package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---- STOP / PAUSE files ----------------------------------------------------

func stopPath(home, tenant string) string {
	if tenant == "" {
		return filepath.Join(home, "STOP")
	}
	return filepath.Join(home, "STOP."+tenant)
}

// pausePath holds a quality-guard trip; only `wr start -t T` clears it.
func pausePath(home, tenant string) string { return filepath.Join(home, "PAUSE."+tenant) }

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func readReason(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func checkTenantFlag(t string) error {
	if t != "" && !slugPattern.MatchString(t) {
		return fmt.Errorf("tenant %q must be a lowercase slug (a-z, 0-9, dashes)", t)
	}
	return nil
}

func stopCommand(d *Dispatcher, o map[string]string) error {
	if err := checkTenantFlag(o["tenant"]); err != nil {
		return err
	}
	if err := os.MkdirAll(d.Home, 0o700); err != nil {
		return err
	}
	p := stopPath(d.Home, o["tenant"])
	if err := os.WriteFile(p, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(d.Out, "stop written:", p)
	return nil
}

func startCommand(d *Dispatcher, o map[string]string) error {
	t := o["tenant"]
	if err := checkTenantFlag(t); err != nil {
		return err
	}
	removed := 0
	ps := []string{stopPath(d.Home, t)}
	if t != "" {
		ps = append(ps, pausePath(d.Home, t))
	}
	for _, p := range ps {
		if err := os.Remove(p); err == nil {
			fmt.Fprintln(d.Out, "removed:", p)
			removed++
		}
	}
	if removed == 0 {
		fmt.Fprintln(d.Out, "nothing to remove")
	}
	return nil
}

// ---- board reading for pauses ---------------------------------------------

type boardItem struct {
	ID    string
	Kind  string
	Title string
	State string
	Note  string
	Raw   map[string]json.RawMessage
}

// emptyField: absent, null, "", false, [] or {} counts as empty.
func emptyField(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null" || s == `""` || s == "false" || s == "[]" || s == "{}"
}

func (b boardItem) eIsE() bool {
	var e string
	return json.Unmarshal(b.Raw["e"], &e) == nil && e == "E"
}

func (b boardItem) session() string {
	var v string
	_ = json.Unmarshal(b.Raw["session"], &v)
	return v
}

// waived: done with `mw done --no-evidence REASON` (leaves a "no-evidence:" note).
func (b boardItem) waived() bool {
	var notes []string
	_ = json.Unmarshal(b.Raw["notes"], &notes)
	for _, n := range notes {
		if strings.Contains(n, "no-evidence:") {
			return true
		}
	}
	return false
}

func (b boardItem) empty(k string) bool {
	return emptyField(b.Raw[k])
}

// listItems is `mw list -t T -j -v`, cached until fresh(): one call per tenant per
// tick, reused by the pause rules, the caps, settle and status.
func (m *MW) listItems(tenant string) ([]boardItem, error) { return m.listItemsMode(tenant, true) }

// listLean is `mw list -t T -j` (id, type, state, title only): enough to find the
// next ready item and to count; notes, sessions and judged need listItems.
func (m *MW) listLean(tenant string) ([]boardItem, error) { return m.listItemsMode(tenant, false) }

// cachedFull returns the full list when it is already cached (no mw call).
func (m *MW) cachedFull(tenant string) ([]boardItem, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.memo["list/"+tenant]
	if !ok {
		return nil, false
	}
	return append([]boardItem(nil), v.([]boardItem)...), true
}

func (m *MW) listItemsMode(tenant string, full bool) ([]boardItem, error) {
	key, args := "lean/"+tenant, []string{"list", "-t", tenant, "-j"}
	if full {
		key, args = "list/"+tenant, append(args, "-v")
	}
	v, err := m.cached(key, func() (any, error) {
		out, err := m.must(args...)
		if err != nil {
			return nil, err
		}
		var v struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			return nil, err
		}
		var items []boardItem
		for _, r := range v.Items {
			var it boardItem
			it.Raw = r
			_ = json.Unmarshal(r["id"], &it.ID)
			_ = json.Unmarshal(r["title"], &it.Title)
			_ = json.Unmarshal(r["type"], &it.Kind)
			_ = json.Unmarshal(r["state"], &it.State)
			_ = json.Unmarshal(r["note"], &it.Note)
			items = append(items, it)
		}
		return items, nil
	})
	if err != nil {
		return nil, err
	}
	return append([]boardItem(nil), v.([]boardItem)...), nil
}

// state.json: the done-set seen at the last tick, per tenant, so the quality
// guard can tell which items went done since then.
func statePath(home string) string { return filepath.Join(home, "state.json") }

func loadSeen(home string) map[string][]string {
	m := map[string][]string{}
	if b, err := os.ReadFile(statePath(home)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func saveSeen(home string, m map[string][]string) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := statePath(home) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return renameRetry(tmp, statePath(home))
}

// pauseReasons evaluates the board-derived pause rules for a tenant. The
// quality guard compares against seen (first = first look: seed, no trip).
// A done item without `judged` is unjudged; a newly done item without evidence
// trips the guard, unless it was finished with the --no-evidence waiver, which
// only warns. wr's own blocks (note starting "wr:") do not pause the tenant.
func (d *Dispatcher) pauseReasons(items []boardItem, seen []string, first bool, gate bool) (reasons []string, trip string, warns []string) {
	var blocked, unjudged []string
	for _, it := range items {
		if it.State == "blocked" && !strings.HasPrefix(strings.TrimSpace(it.Note), "wr:") {
			blocked = append(blocked, it.ID)
		}
		if it.State == "done" && it.empty("judged") {
			unjudged = append(unjudged, it.ID)
		}
	}
	if d.Cfg.Pause.OnBlocked && len(blocked) > 0 {
		reasons = append(reasons, "blocked item(s): "+strings.Join(blocked, ", "))
	}
	if len(unjudged) > d.Cfg.Pause.Unjudged {
		reasons = append(reasons, fmt.Sprintf("%d done items unjudged (limit %d)", len(unjudged), d.Cfg.Pause.Unjudged))
	}
	if d.Cfg.Pause.QualityGuard && !first {
		was := map[string]bool{}
		for _, s := range seen {
			was[s] = true
		}
		var bad []string
		for _, it := range items {
			if it.State != "done" || was[it.ID] {
				continue
			}
			switch {
			case it.waived():
				warns = append(warns, it.ID)
			case gate && !it.eIsE(), !gate && strings.TrimSpace(it.Note) == "":
				bad = append(bad, it.ID)
			}
		}
		if len(bad) > 0 {
			trip = "quality guard: done without evidence/note: " + strings.Join(bad, ", ")
		}
	}
	return reasons, trip, warns
}

func doneIDs(items []boardItem) []string {
	var ids []string
	for _, it := range items {
		if it.State == "done" {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// ---- caps ------------------------------------------------------------------

// Usage is the current consumption against the caps.
type Usage struct {
	Hour           int                `json:"sessions_last_60min"`
	PerHour        int                `json:"per_hour"`
	ParallelGlobal int                `json:"parallel_global_live"`
	LiveByTenant   map[string]int     `json:"parallel_by_tenant"`
	USDToday       map[string]float64 `json:"usd_today"`
	DiskFreeGB     float64            `json:"disk_free_gb"`
	DiskFloorGB    int                `json:"disk_floor_gb"`
	diskErr        string
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dispatcher) usage() Usage {
	u := Usage{PerHour: d.Cfg.Caps.PerHour, LiveByTenant: map[string]int{}, USDToday: map[string]float64{}, DiskFloorGB: d.Cfg.Caps.DiskFreeGB}
	now := d.now()
	today := now.Local().Format("2006-01-02")
	runs, _ := readRuns(d.Home, "")
	for _, r := range runs {
		if now.Sub(r.Start) < time.Hour {
			u.Hour++
		}
		if runLive(r, now) {
			u.ParallelGlobal++
			u.LiveByTenant[r.Tenant]++
		}
		if r.Cost != nil && r.Start.Local().Format("2006-01-02") == today {
			u.USDToday[r.Engine] += *r.Cost
		}
	}
	if free, err := diskFreeGB(d.Home); err == nil {
		u.DiskFreeGB = free
	} else {
		u.DiskFreeGB = -1
		u.diskErr = err.Error()
	}
	return u
}

// globalCap is the first global cap that forbids a dispatch, or "".
func (d *Dispatcher) globalCap(u Usage) string {
	c := d.Cfg.Caps
	switch {
	case c.PerHour > 0 && u.Hour >= c.PerHour:
		return fmt.Sprintf("cap per_hour: %d sessions in the last 60 min (limit %d)", u.Hour, c.PerHour)
	case c.ParallelGlobal > 0 && u.ParallelGlobal >= c.ParallelGlobal:
		return fmt.Sprintf("cap parallel_global: %d live (limit %d)", u.ParallelGlobal, c.ParallelGlobal)
	case c.DiskFreeGB > 0 && u.DiskFreeGB < 0:
		return fmt.Sprintf("cap disk_free_gb: cannot read free space (%s); failing closed", u.diskErr)
	case c.DiskFreeGB > 0 && u.DiskFreeGB < float64(c.DiskFreeGB):
		return fmt.Sprintf("cap disk_free_gb: %.1f GB free (floor %d)", u.DiskFreeGB, c.DiskFreeGB)
	}
	return ""
}

// tenantCap is the first per-tenant cap that forbids a dispatch, or "".
func (d *Dispatcher) tenantCap(u Usage, tenant string) string {
	tc := d.Cfg.Tenants[tenant]
	c := d.Cfg.Caps
	if c.ParallelTenant > 0 && u.LiveByTenant[tenant] >= c.ParallelTenant {
		return fmt.Sprintf("cap parallel_tenant: %d live (limit %d)", u.LiveByTenant[tenant], c.ParallelTenant)
	}
	if lim := c.USDPerDay[tc.Engine]; lim > 0 && u.USDToday[tc.Engine] >= lim {
		return fmt.Sprintf("cap usd_per_day: engine %s spent USD %.2f of %.2f today", tc.Engine, u.USDToday[tc.Engine], lim)
	}
	return ""
}

// ---- serve -----------------------------------------------------------------

// Server holds the cross-tick state of one `wr serve`.
type Server struct {
	D        *Dispatcher
	mu       sync.Mutex
	cooldown map[string]bool   // tenant skipped once after a non-ok run
	notified map[string]string // tenant -> last pause reason notified
	hinted   map[string]string // tenant -> day the "mw archive" hint was logged
	wg       sync.WaitGroup    // in-flight engine runs
	wake     chan struct{}     // an ok run finished: tick again now
}

// syncWriter serialises writes from the tick and the run goroutines.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func newServer(d *Dispatcher) *Server {
	if _, ok := d.Out.(*syncWriter); !ok {
		d.Out = &syncWriter{w: d.Out}
	}
	return &Server{D: d, cooldown: map[string]bool{}, notified: map[string]string{}, wake: make(chan struct{}, 1)}
}

// wait blocks until every in-flight run has finished.
func (s *Server) wait() { s.wg.Wait() }

// noteReason notifies once per change of a tenant's pause reason.
func (s *Server) noteReason(t, reason string) {
	s.mu.Lock()
	prev := s.notified[t]
	s.notified[t] = reason
	s.mu.Unlock()
	if reason != "" && reason != prev {
		s.D.event("pause", t, reason, true)
	}
}

// tick runs one serve iteration: dispatches are started in goroutines (caps in
// plan() bound them) and tick returns at once. stop = the global STOP file asked
// serve to exit.
// archiveHint logs "mw archive -t T" once per day for a tenant with more than 1000 done items.
func (s *Server) archiveHint(t string, items []boardItem) {
	done := 0
	for _, it := range items {
		if it.State == "done" {
			done++
		}
	}
	if done <= 1000 {
		return
	}
	day := s.D.now().Format("2006-01-02")
	s.mu.Lock()
	if s.hinted == nil {
		s.hinted = map[string]string{}
	}
	seenToday := s.hinted[t] == day
	s.hinted[t] = day
	s.mu.Unlock()
	if seenToday {
		return
	}
	fmt.Fprintf(s.D.Out, "%s: hint - %d done items slow every mw call; run: mw archive -t %s\n", t, done, t)
	s.D.event("hint", t, fmt.Sprintf("%d done items; mw archive -t %s", done, t), false)
}

func (s *Server) tick() (stop bool, dispatched bool, err error) {
	d := s.D
	if fileExists(stopPath(d.Home, "")) {
		fmt.Fprintf(d.Out, "STOP: %s present, serve exits\n", stopPath(d.Home, ""))
		return true, false, nil
	}
	d.MW.fresh()
	d.resetLevelTries()
	if err := d.recoverLost(); err != nil {
		return false, false, err
	}
	d.reapTick()
	seen := loadSeen(d.Home)
	for _, t := range d.Cfg.tenantsInOrder() {
		tc := d.Cfg.Tenants[t]
		if !tc.Enabled {
			delete(seen, t)
			continue
		}
		if mt, merr := d.MW.metaErr(t); merr != nil {
			fmt.Fprintf(d.Out, "%s: cannot read tenant metadata: %v\n", t, merr)
			continue
		} else if mt.Archived {
			delete(seen, t)
			continue
		}
		d.applyLevel(t, tc) // before the quality guard reads the evidence state
		items, lerr := d.MW.listItems(t)
		if lerr != nil {
			fmt.Fprintf(d.Out, "%s: error: %v\n", t, lerr)
			continue
		}
		s.archiveHint(t, items)
		prev, had := seen[t]
		reasons, trip, warns := d.pauseReasons(items, prev, !had, d.MW.evidenceGate(t))
		if len(warns) > 0 {
			d.event("warn", t, "done with --no-evidence waiver: "+strings.Join(warns, ", "), false)
		}
		pauseFailed := false
		if trip != "" && !fileExists(pausePath(d.Home, t)) {
			if werr := os.WriteFile(pausePath(d.Home, t), []byte(trip+"\n"), 0o600); werr != nil {
				pauseFailed = true
				fmt.Fprintf(d.Out, "%s: error: cannot write PAUSE (%v); will retry next tick\n", t, werr)
				reasons = append(reasons, trip)
			}
		}
		if !pauseFailed {
			seen[t] = doneIDs(items) // keep the old baseline so the trip is re-found
		}
		cur := strings.Join(reasons, "; ")
		if fileExists(pausePath(d.Home, t)) {
			cur = readReason(pausePath(d.Home, t))
		}
		s.noteReason(t, cur)
		s.mu.Lock()
		cool := s.cooldown[t]
		delete(s.cooldown, t)
		s.mu.Unlock()
		if cool {
			fmt.Fprintf(d.Out, "%s: skip - cooldown after a non-ok run\n", t)
			continue
		}
		item, why, perr := d.plan(t)
		if perr != nil {
			fmt.Fprintf(d.Out, "%s: error: %v\n", t, perr)
			continue
		}
		if item == nil {
			fmt.Fprintf(d.Out, "%s: skip - %s\n", t, why)
			continue
		}
		ts, terr := d.tenantSetup(t)
		if terr != nil {
			fmt.Fprintf(d.Out, "%s: error: %v\n", t, terr)
			continue
		}
		p, derr := d.prepare(ts, item)
		if errors.Is(derr, errNothingToClaim) {
			fmt.Fprintf(d.Out, "%s: skip - nothing ready\n", t)
			continue
		}
		dispatched = true
		if derr != nil {
			s.mu.Lock()
			s.cooldown[t] = true
			s.mu.Unlock()
			fmt.Fprintf(d.Out, "%s: error dispatching %s: %v\n", t, item.ID, derr)
			d.event("dispatch-error", t, item.ID+": "+derr.Error(), true)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			rec, rerr := d.execute(p)
			if rerr != nil || rec.Outcome != "ok" {
				s.mu.Lock()
				s.cooldown[t] = true
				s.mu.Unlock()
				oc := "error"
				if rec != nil && rec.Outcome != "" {
					oc = rec.Outcome
				}
				d.event("outcome", t, p.item.ID+" -> "+oc, true)
			} else {
				select {
				case s.wake <- struct{}{}: // next ready item without waiting a tick
				default:
				}
			}
			if rec == nil {
				return
			}
			ex := -1
			if rec.Exit != nil {
				ex = *rec.Exit
			}
			fmt.Fprintf(d.Out, "%s: %s %s -> %s (exit %d%s)\n", t, rec.Name, p.item.ID, rec.Outcome, ex, map[bool]string{true: ", timeout"}[rec.Timeout])
		}()
	}
	if err := saveSeen(d.Home, seen); err != nil {
		return false, dispatched, err
	}
	d.publishPage()
	return false, dispatched, nil
}

// serveLoop ticks until STOP or maxTicks (0 = forever), then waits for in-flight
// runs. sleep is injectable; nil sleeps until the tick, a signal, or an ok run.
func (s *Server) serveLoop(maxTicks int, sleep func(time.Duration)) error {
	d := s.D
	defer s.wait()
	tick := time.Duration(d.Cfg.TickMin) * time.Minute
	if d.Cfg.TickMin <= 0 {
		tick = 15 * time.Minute
	}
	if d.Tick > 0 {
		tick = d.Tick
	}
	if sleep == nil {
		sleep = func(dur time.Duration) {
			t := time.NewTimer(dur)
			defer t.Stop()
			select {
			case <-t.C:
			case <-s.wake:
			case <-d.ctx().Done():
			}
		}
	}
	ctx := d.ctx()
	for n := 0; maxTicks == 0 || n < maxTicks; n++ {
		if n > 0 {
			sleep(tick)
		}
		if ctx.Err() != nil {
			d.flushPublish()
			fmt.Fprintln(d.Out, "serve: signal received, exiting")
			return nil
		}
		if d.MW != nil {
			if err := d.MW.checkVersion(); err != nil {
				return err
			}
			if err := d.MW.requireSchema(); err != nil {
				return err
			}
		}
		stop, _, err := s.tick()
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
		if ctx.Err() != nil {
			d.flushPublish()
			fmt.Fprintln(d.Out, "serve: signal received, exiting")
			return nil
		}
	}
	return nil
}

func serveCommand(d *Dispatcher, o map[string]string) error {
	unlock, err := lockHome(d.Home)
	if err != nil {
		return err
	}
	defer unlock()
	writeLockPid(d.Home)
	max := 0
	if o["once"] == "true" {
		if o["max-ticks"] != "" {
			return errors.New("--once and --max-ticks are mutually exclusive")
		}
		max = 1
	}
	if v := o["max-ticks"]; v != "" {
		if _, err := fmt.Sscanf(v, "%d", &max); err != nil || max < 1 {
			return fmt.Errorf("--max-ticks wants a positive number, got %q", v)
		}
	}
	fmt.Fprintf(d.Out, "serve: tick every %d min, global STOP %s\n", d.Cfg.TickMin, stopPath(d.Home, ""))
	ctx, stopSig := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSig()
	if d.Ctx == nil {
		d.Ctx = ctx
	}
	return newServer(d).serveLoop(max, d.Sleep)
}

// ---- status ----------------------------------------------------------------

// pausedNow lists paused enabled tenants without touching the guard baseline.
func (d *Dispatcher) pausedNow(tenants []string) []pausedRow {
	var out []pausedRow
	seen := loadSeen(d.Home)
	for _, t := range tenants {
		if !d.Cfg.Tenants[t].Enabled {
			continue
		}
		var rs []string
		if fileExists(stopPath(d.Home, t)) {
			rs = append(rs, "STOP."+t)
		}
		if fileExists(pausePath(d.Home, t)) {
			rs = append(rs, readReason(pausePath(d.Home, t)))
		}
		if items, err := d.MW.listItems(t); err == nil {
			r, _, _ := d.pauseReasons(items, seen[t], true, true)
			rs = append(rs, r...)
		}
		if len(rs) > 0 {
			out = append(out, pausedRow{t, strings.Join(rs, "; ")})
		}
	}
	return out
}

func stopFiles(home string) []string {
	fs, _ := filepath.Glob(filepath.Join(home, "STOP*"))
	sort.Strings(fs)
	return fs
}

// ---- service ---------------------------------------------------------------

const plistLabel = "ro.nfnt.wr"

func xmlEsc(s string) string {
	var b bytes.Buffer
	_ = xmlEscape(&b, s)
	return b.String()
}

func xmlEscape(w io.Writer, s string) error {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	_, err := io.WriteString(w, r.Replace(s))
	return err
}

// servicePATH builds the PATH a service needs: install-time $PATH plus the
// dirs of mw and of the engine binaries, de-duplicated.
func servicePATH(extra ...string) string {
	var parts []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
	}
	for _, p := range extra {
		add(p)
	}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		add(p)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// binDirs: the dirs holding mw and each engine's cmd[0] (when resolvable).
func binDirs(cfg *Config) []string {
	var dirs []string
	names := []string{"mw"}
	if m := os.Getenv("MW"); m != "" {
		names = []string{m}
	}
	if cfg != nil {
		for _, e := range cfg.Engines {
			if len(e.Cmd) > 0 {
				names = append(names, e.Cmd[0])
			}
		}
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				dirs = append(dirs, filepath.Dir(abs))
			}
		}
	}
	return dirs
}

func launchdPlist(bin, home, path string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
 "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + plistLabel + `</string>
  <key>ProgramArguments</key>
  <array><string>` + xmlEsc(bin) + `</string><string>serve</string></array>
  <key>EnvironmentVariables</key>
  <dict><key>WR_HOME</key><string>` + xmlEsc(home) + `</string>
    <key>PATH</key><string>` + xmlEsc(path) + `</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>` + xmlEsc(filepath.Join(home, "serve.out")) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEsc(filepath.Join(home, "serve.err")) + `</string>
</dict></plist>
`
}

// sdQuote quotes one systemd word and escapes % (specifier) as %%.
func sdQuote(v string) string {
	v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(v)
	return `"` + v + `"`
}

func systemdUnit(bin, home, path string) string {
	pct := func(v string) string { return strings.ReplaceAll(v, "%", "%%") }
	return "[Unit]\nDescription=wr wave rider serve loop\n\n[Service]\nExecStart=" + sdQuote(bin) + " serve\n" +
		"Environment=" + sdQuote("WR_HOME="+home) + "\nEnvironment=" + sdQuote("PATH="+path) +
		"\nRestart=on-failure\nStandardOutput=append:" + pct(filepath.Join(home, "serve.out")) +
		"\nStandardError=append:" + pct(filepath.Join(home, "serve.err")) + "\n\n[Install]\nWantedBy=default.target\n"
}

// servicePath: WR_SERVICE_DIR (tests) overrides the real per-user location.
func servicePath(goos string) (string, bool) {
	name := map[string]string{"darwin": plistLabel + ".plist", "linux": "wr.service"}[goos]
	if name == "" {
		return "", false
	}
	if dir := os.Getenv("WR_SERVICE_DIR"); dir != "" {
		return filepath.Join(dir, name), true
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	if goos == "darwin" {
		return filepath.Join(h, "Library", "LaunchAgents", name), true
	}
	return filepath.Join(h, ".config", "systemd", "user", name), true
}

func serviceCommand(out io.Writer, cfg *Config, home, goos string, pos []string, run func(name string, args ...string) error) error {
	if len(pos) != 1 || (pos[0] != "install" && pos[0] != "remove") {
		return errors.New("usage: wr service install|remove")
	}
	p, ok := servicePath(goos)
	if !ok {
		fmt.Fprintln(out, "no service support on "+goos+": run `wr serve` by hand")
		return nil
	}
	real := os.Getenv("WR_SERVICE_DIR") == "" // only a real install talks to launchctl/systemctl
	uid := fmt.Sprint(os.Getuid())
	if pos[0] == "remove" {
		if real {
			if goos == "darwin" {
				_ = run("launchctl", "bootout", "gui/"+uid+"/"+plistLabel)
			} else {
				_ = run("systemctl", "--user", "disable", "--now", "wr.service")
			}
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Fprintln(out, "removed:", p)
		return nil
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	path := servicePATH(binDirs(cfg)...)
	body := launchdPlist(bin, home, path)
	if goos == "linux" {
		body = systemdUnit(bin, home, path)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(out, "written:", p)
	if real {
		if goos == "darwin" {
			_ = run("launchctl", "bootout", "gui/"+uid+"/"+plistLabel)
			if err := run("launchctl", "bootstrap", "gui/"+uid, p); err != nil {
				return fmt.Errorf("launchctl bootstrap: %w", err)
			}
		} else if err := run("systemctl", "--user", "enable", "--now", "wr.service"); err != nil {
			return fmt.Errorf("systemctl: %w", err)
		}
	}
	fmt.Fprintln(out, "logs:", filepath.Join(home, "serve.out"), filepath.Join(home, "serve.err"))
	return nil
}

// exitError carries a specific process exit code.
type exitError struct {
	Code int
	Msg  string
}

func (e *exitError) Error() string { return e.Msg }
