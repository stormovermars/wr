package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const forbidPrefix = "Forbidden for this session (never touch): "

// logCap is the per-run engine log size cap (2 MB); a var so tests can shrink it.
var logCap int64 = 16 << 20

// Run is one run record: ~/.wr/runs/<tenant>/<NAME>.json.
type Run struct {
	V          int             `json:"v"` // record format; readRuns ignores anything but runVersion
	Name       string          `json:"name"`
	Tenant     string          `json:"tenant"`
	Board      string          `json:"board"`
	Item       string          `json:"item"`
	Engine     string          `json:"engine"`
	UUID       string          `json:"uuid"`
	CmdHash    string          `json:"cmd_hash"`
	PID        int             `json:"pid"`
	PStart     string          `json:"pstart,omitempty"`     // process start time (ps lstart), guards PID reuse
	Transcript string          `json:"transcript,omitempty"` // claude transcript whose mtime is the liveness signal
	Stale      bool            `json:"stale,omitempty"`
	StopErr    string          `json:"stop_error,omitempty"`
	Phase      string          `json:"phase,omitempty"` // "starting" until the engine is exec'd
	Start      time.Time       `json:"start"`
	End        *time.Time      `json:"end,omitempty"`
	Exit       *int            `json:"exit,omitempty"`
	Timeout    bool            `json:"timeout,omitempty"`
	Outcome    string          `json:"outcome,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`
	Cost       *float64        `json:"cost_usd,omitempty"`
}

const runVersion = 2

func runsDir(home, tenant string) string { return filepath.Join(home, "runs", tenant) }

func writeRun(home string, r *Run) error {
	dir := runsDir(home, r.Tenant)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	r.V = runVersion
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, r.Name+".json")
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return renameRetry(p+".tmp", p)
}

func readRuns(home, tenant string) ([]*Run, error) {
	var tenants []string
	if tenant != "" {
		tenants = []string{tenant}
	} else {
		ents, _ := os.ReadDir(filepath.Join(home, "runs"))
		for _, e := range ents {
			if e.IsDir() {
				tenants = append(tenants, e.Name())
			}
		}
	}
	var out []*Run
	for _, t := range tenants {
		files, _ := filepath.Glob(filepath.Join(runsDir(home, t), "*.json"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var r Run
			if json.Unmarshal(b, &r) == nil && r.V == runVersion {
				out = append(out, &r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugPattern is mw's validID shape.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func slugPart(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// runName builds wr-<tenant>-<attempt>-<rnd>, truncating the tenant part so the
// whole name is a valid mw session id (<= 64, lowercase, single dashes). The item
// is not known before the claim, so a short random tail keeps names unique.
func runName(tenant string, attempt int, rnd string) string {
	tail := "-" + strconv.Itoa(attempt) + "-" + rnd
	room := 64 - len("wr-") - len(tail)
	tp := slugPart(tenant)
	if room < 1 {
		room = 1
	}
	if len(tp) > room {
		tp = strings.Trim(tp[:room], "-")
	}
	if tp == "" {
		tp = "t"
	}
	return "wr-" + tp + tail
}

// shortRand is 6 random hex characters.
func shortRand() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// attemptFor = 1 + run files already written for this tenant+item.
func attemptFor(home, tenant, id string) int {
	n := 0
	runs, _ := readRuns(home, tenant)
	for _, r := range runs {
		if r.Item == id {
			n++
		}
	}
	return n + 1
}

func isFailed(outcome string) bool {
	return outcome == "incomplete" || outcome == "lost" || outcome == "foreign"
}

// failedAttempts counts failed/incomplete runs for an item (retry limit).
func failedAttempts(home, tenant, id string) int {
	n := 0
	runs, _ := readRuns(home, tenant)
	for _, r := range runs {
		if r.Item == id && isFailed(r.Outcome) {
			n++
		}
	}
	return n
}

func lastOutcome(home, tenant string) string {
	runs, _ := readRuns(home, tenant)
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].Outcome != "" {
			return runs[i].Outcome
		}
	}
	return ""
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// expandArgv substitutes only {uuid} {usd} {name} {id}; argv stays an array.
func expandArgv(tmpl []string, uuid, usd, name, id string) []string {
	r := strings.NewReplacer("{uuid}", uuid, "{usd}", usd, "{name}", name, "{id}", id)
	out := make([]string, len(tmpl))
	for i, a := range tmpl {
		out[i] = r.Replace(a)
	}
	return out
}

func cmdHash(argv []string) string {
	b, _ := json.Marshal(argv)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])[:12]
}

// buildPrompt is mw's headless session prompt verbatim, the claimed item's
// `mw next --claim -j` brief inline, and the forbid rule.
func buildPrompt(base, brief, id, name string, forbid []string) string {
	p := strings.TrimRight(base, "\n") + "\n\nHeadless run: nobody answers questions. Session " + name + " already owns " + id + ".\n"
	p += "Your item (mw next -j brief):\n" + brief + "\n\nWork only " + id + ": finish with mw done " + id + " CHECK --evidence FILE, or mw block " + id + " REASON, then stop.\n"
	if len(forbid) > 0 {
		p += forbidPrefix + strings.Join(forbid, ", ") + ".\n"
	}
	return p
}

// capWriter writes at most max bytes to w, then discards (and notes truncation).
type capWriter struct {
	mu        sync.Mutex
	w         io.Writer
	max, n    int64
	truncated bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - c.n
	if room > 0 {
		k := int64(len(p))
		if k > room {
			k = room
		}
		if _, err := c.w.Write(p[:k]); err != nil {
			return 0, err
		}
		c.n += k
		if k < int64(len(p)) {
			c.truncated = true
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

// tailBuf keeps the last max bytes written to it.
type tailBuf struct {
	b   []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

// parseUsage pulls total_cost_usd and usage from the last `"type":"result"` line of
// claude --output-format stream-json output.
func parseUsage(kind string, tail []byte) (json.RawMessage, *float64) {
	if kind != "claude_stream" {
		return nil, nil
	}
	lines := bytes.Split(bytes.TrimSpace(tail), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var v struct {
			Type  string          `json:"type"`
			Cost  *float64        `json:"total_cost_usd"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(bytes.TrimSpace(lines[i]), &v) == nil && v.Type == "result" {
			return v.Usage, v.Cost
		}
	}
	return nil, nil
}

type Dispatcher struct {
	Home string
	Cfg  *Config
	MW   *MW
	Out  io.Writer

	Now   func() time.Time    // nil = time.Now (tests)
	Sleep func(time.Duration) // nil = wait on Ctx (tests)
	Tick  time.Duration       // nonzero overrides tick_min (tests)

	Ctx  context.Context // serve cancels it on SIGTERM/SIGINT; nil = never
	Tags string          // --tag filter for status and ui (comma-joined, AND)
	Dry  bool            // `wr next`: plan only, never change the board (no gate-level apply)

	hookMu     sync.Mutex // serializes the published page
	pubPending bool       // a change waits for the interval to end
	pubTimer   bool       // a delayed write is scheduled
	pubWrites  int        // page writes so far (tests)

	lvlMu    sync.Mutex        // gate-level apply bookkeeping
	lvlTried map[string]bool   // tenants already attempted since the last reset
	lvlErr   map[string]string // last apply error per tenant (one event per distinct error)

	reapClampLogged bool // the older_than clamp was logged once
}

// afterFunc schedules the delayed publish (tests replace it).
var afterFunc = time.AfterFunc

// publishInterval is publish.interval in seconds: default 60, never below 5.
func (c PublishCfg) interval() time.Duration {
	n := c.Interval
	if n == 0 {
		n = 60
	}
	if n < 5 {
		n = 5
	}
	return time.Duration(n) * time.Second
}

// publishPage writes the ui page with its data inlined when config publish.html is
// set, only when the data changed since the last write (hash in <page>.hash) and
// at most every publish.interval seconds. A change inside the interval schedules
// one write at the interval's end, so the page is never stale for longer than that.
// A failure is an event.
func (d *Dispatcher) publishPage() {
	p := d.Cfg.Publish.HTML
	if p == "" {
		return
	}
	d.hookMu.Lock()
	defer d.hookMu.Unlock()
	p = expandHome(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(d.MW.Board, p)
	}
	d.MW.fresh()
	wrote, wait, err := exportUIAt(d, p, false)
	switch {
	case err != nil:
		d.event("publish-error", "", err.Error(), false)
	case wrote:
		d.pubPending = false
	case wait > 0:
		d.pubPending = true
		if !d.pubTimer { // one timer at a time; it reschedules itself while the interval is not over
			d.pubTimer = true
			afterFunc(wait, func() {
				d.hookMu.Lock()
				d.pubTimer = false
				pending := d.pubPending
				d.hookMu.Unlock()
				if pending {
					d.publishPage()
				}
			})
		}
	}
}

// expandHome is mw's htmlPath rule for a leading ~/.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// flushPublish writes a page still pending at exit (once, serve shutdown) now.
func (d *Dispatcher) flushPublish() {
	d.hookMu.Lock()
	pending := d.pubPending
	d.pubPending = false
	d.hookMu.Unlock()
	if !pending || d.Cfg.Publish.HTML == "" {
		return
	}
	p := expandHome(d.Cfg.Publish.HTML)
	if !filepath.IsAbs(p) {
		p = filepath.Join(d.MW.Board, p)
	}
	d.MW.fresh()
	if _, _, err := exportUIAt(d, p, true); err != nil {
		d.event("publish-error", "", err.Error(), false)
	}
}

// afterSettle is the hook after a run settles: publish the page.
func (d *Dispatcher) afterSettle() {
	d.publishPage()
}

func (d *Dispatcher) ctx() context.Context {
	if d.Ctx != nil {
		return d.Ctx
	}
	return context.Background()
}

type tenantSetup struct {
	Slug string
	Cfg  TenantCfg
	Dir  string // tenant working dir
}

func (d *Dispatcher) tenantSetup(slug string) (*tenantSetup, error) {
	tc, ok := d.Cfg.Tenants[slug]
	if !ok {
		return nil, fmt.Errorf("tenant %q is not in config (add it to %s)", slug, configPath(d.Home))
	}
	dir := ""
	if tc.Dir != nil {
		dir = *tc.Dir
	} else {
		ts, err := d.MW.tenants()
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			if t.Tenant == slug {
				dir = t.Path
			}
		}
	}
	if dir == "" {
		return nil, fmt.Errorf("tenant %q has no directory (set \"dir\" in config)", slug)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(d.MW.Board, dir)
	}
	if !underDir(d.MW.Board, dir) {
		return nil, fmt.Errorf("tenant dir outside board: %s", dir)
	}
	return &tenantSetup{slug, tc, dir}, nil
}

// underDir reports whether dir is board or inside it (symlinks resolved), so mw
// run from the tenant dir discovers the same board.
func underDir(board, dir string) bool {
	if r, err := filepath.EvalSymlinks(board); err == nil {
		board = r
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	rel, err := filepath.Rel(board, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// errNotRepo: the tenant dir is not inside a git repo (the dirty-tree guard is off).
var errNotRepo = errors.New("not a git repo")

// mwOwnFile reports files mw and wr create next to the board: the board, its
// archive, the lock, tmp and pid files, and wr's state (.wr/).
func mwOwnFile(p string) bool {
	base := filepath.Base(strings.TrimSuffix(p, "/"))
	return strings.HasPrefix(base, ".miniwaves.") || strings.HasPrefix(p, ".miniwaves/") || strings.Contains(p, "/.miniwaves/") ||
		strings.HasPrefix(p, ".wr/") || strings.Contains(p, "/.wr/")
}

// gitDirty: true when `git status --porcelain=v2 -z` (the only git call) has an
// entry outside ignore (absolute paths) and outside mw's own files and wr's
// state (.wr/). errNotRepo when dir is not in a git repo; other errors when git fails.
func gitDirty(dir string, ignore ...string) (bool, error) {
	gitC := func(args ...string) *exec.Cmd {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "LC_ALL=C")
		return c
	}
	cmd := gitC("status", "--porcelain=v2", "-z", "--untracked-files=all")
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		if strings.Contains(se.String(), "not a git repository") {
			return false, errNotRepo
		}
		return false, err
	}
	// ignore paths are made relative to the repo root (status paths are root-relative); the
	// root is only asked for when an entry survives the mw/wr own-file filter
	var ign map[string]bool
	ignored := func(p string) bool {
		if len(ignore) == 0 {
			return false
		}
		if ign == nil {
			ign = map[string]bool{}
			out, err := gitC("rev-parse", "--show-toplevel").Output()
			if err != nil {
				return false
			}
			top := filepath.FromSlash(strings.TrimSpace(string(out))) // C:/x on Windows
			if r, err := filepath.EvalSymlinks(top); err == nil {
				top = r
			}
			for _, ip := range ignore {
				if r, err := filepath.EvalSymlinks(filepath.Dir(ip)); err == nil {
					ip = filepath.Join(r, filepath.Base(ip))
				}
				if rel, err := filepath.Rel(top, ip); err == nil {
					ign[filepath.ToSlash(rel)] = true
				}
			}
		}
		if ign[p] || ign[strings.TrimSuffix(p, ".hash")] {
			return true
		}
		if strings.HasSuffix(p, ".tmp") { // <page>.<random>.tmp from the atomic write
			q := strings.TrimSuffix(p, ".tmp")
			if i := strings.LastIndex(q, "."); i > 0 {
				return ign[q[:i]] || ign[q[:i]+".hash"]
			}
		}
		return false
	}
	toks := strings.Split(so.String(), "\x00")
	for i := 0; i < len(toks); i++ {
		l := toks[i]
		if len(l) < 3 || l[0] == '#' {
			continue
		}
		var p string
		switch l[0] {
		case '?', '!':
			p = l[2:]
		case '1':
			if f := strings.SplitN(l, " ", 9); len(f) == 9 {
				p = f[8]
			}
		case '2':
			if f := strings.SplitN(l, " ", 10); len(f) == 10 {
				p = f[9]
			}
			i++ // the original path follows as its own token
		case 'u':
			if f := strings.SplitN(l, " ", 11); len(f) == 11 {
				p = f[10]
			}
		}
		if p == "" || mwOwnFile(p) || ignored(p) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// ignoredPaths are the files a tick itself writes in a tenant repo: the published page.
func (d *Dispatcher) ignoredPaths() []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		p = expandHome(p)
		if !filepath.IsAbs(p) {
			p = filepath.Join(d.MW.Board, p)
		}
		out = append(out, p)
	}
	add(d.Cfg.Publish.HTML)
	if b, err := os.ReadFile(filepath.Join(d.MW.Board, ".miniwaves.json")); err == nil {
		var v struct {
			Publish struct {
				HTML string `json:"html"`
			} `json:"publish"`
		}
		if json.Unmarshal(b, &v) == nil {
			add(v.Publish.HTML)
		}
	}
	return out
}

// inflight names runs this process is preparing or executing (the record exists
// before the engine pid does, and recoverLost must not mistake them for lost).
var inflight = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

func setInflight(name string, on bool) {
	inflight.Lock()
	defer inflight.Unlock()
	if on {
		inflight.m[name] = true
	} else {
		delete(inflight.m, name)
	}
}

func isInflight(name string) bool {
	inflight.Lock()
	defer inflight.Unlock()
	return inflight.m[name]
}

// runAlive: the engine group exists and is the same process that was started
// (start time matches; a recycled PID counts as dead and is never killed).
func runAlive(r *Run) bool {
	if r.PID <= 0 || !groupAlive(r.PID) {
		return false
	}
	return r.PStart == "" || procStart(r.PID) == r.PStart
}

// runLive: an unfinished run that is still running (or just being set up).
func runLive(r *Run, now time.Time) bool {
	if r.End != nil {
		return false
	}
	if r.PID == 0 {
		return isInflight(r.Name) || (r.Phase == "starting" && now.Sub(r.Start) < 2*time.Minute)
	}
	return runAlive(r)
}

// plan decides what wr would dispatch for a tenant: an item, or a skip reason.
// next, once and serve all go through it, so every gate (config, fleet, STOP,
// PAUSE, pause rules, caps, dirty tree, retries) applies to all three.
func (d *Dispatcher) plan(slug string) (*NextItem, string, error) { return d.planMode(slug, false) }

// planMode is plan; peek answers "anything ready?" from the item list already
// loaded instead of asking `mw next` (wr status: no extra call per tenant).
func (d *Dispatcher) planMode(slug string, peek bool) (*NextItem, string, error) {
	tc, ok := d.Cfg.Tenants[slug]
	if !ok {
		return nil, "not in config", nil
	}
	if !tc.Enabled {
		return nil, "not enabled", nil
	}
	mt, merr := d.MW.metaErr(slug)
	if merr != nil {
		return nil, "", fmt.Errorf("cannot read tenant metadata: %w", merr) // fail closed
	}
	if mt.Archived {
		return nil, "archived in mw (mw tenant restore " + slug + ")", nil
	}
	if why := d.blockReason(slug, tc); why != "" {
		return nil, why, nil
	}
	if !peek && !d.Dry {
		d.applyLevel(slug, tc) // only for a tenant wr would run (a cap below is temporary)
	}
	u := d.usage()
	if c := d.globalCap(u); c != "" {
		return nil, c, nil
	}
	if c := d.tenantCap(u, slug); c != "" {
		return nil, c, nil
	}
	ts, err := d.tenantSetup(slug)
	if err != nil {
		return nil, "", err
	}
	items, lerr := d.planItems(slug, peek)
	if lerr == nil {
		if rs, _, _ := d.pauseReasons(items, nil, true, true); len(rs) > 0 {
			return nil, "paused - " + strings.Join(rs, "; "), nil
		}
	}
	dirty, gerr := gitDirty(ts.Dir, d.ignoredPaths()...)
	if gerr != nil && !errors.Is(gerr, errNotRepo) {
		return nil, "dirty-tree guard: git failed in " + ts.Dir, nil
	}
	if dirty {
		if lo := lastOutcome(d.Home, slug); lo == "" {
			return nil, "dirty tree and no prior run", nil
		} else if lo != "ok" {
			return nil, "dirty tree and last run was " + lo, nil
		}
	}
	if lerr != nil {
		return nil, "", lerr
	}
	var first *boardItem
	ready := 0
	for i := range items {
		// the kinds `mw next` skips without -k (acceptance never skips a work kind)
		if items[i].State == "ready" && items[i].Kind != "wave" && items[i].Kind != "prompt" && items[i].Kind != "story" {
			if first == nil {
				first = &items[i]
			}
			ready++
		}
	}
	if first == nil {
		return nil, "nothing ready", nil
	}
	if peek {
		return nil, fmt.Sprintf("%d ready (wr next names the item)", ready), nil
	}
	// the first ready item on the board stands for the one `mw next --claim` will
	// give: wr never peeks with a separate `mw next`
	n := &NextItem{ID: first.ID, Title: first.Title}
	if f := failedAttempts(d.Home, slug, n.ID); f >= d.Cfg.maxAttempts() {
		return nil, fmt.Sprintf("retries: %s failed %d times (max %d)", n.ID, f, d.Cfg.maxAttempts()), nil
	}
	return n, "", nil
}

// blockReason is why wr refuses to run an enabled tenant (self-dispatch off, fleet-managed,
// engine, STOP, STOP.<slug>, PAUSE), or "".
func (d *Dispatcher) blockReason(slug string, tc TenantCfg) string {
	if isSelfTenant(slug) && !d.Cfg.SelfDispatch {
		return "self-dispatch off (self_dispatch: false)"
	}
	if fleetManaged()[slug] {
		return "an external dispatcher has this tenant enabled (" + filepath.Join(fleetDir(), "state", "auto-dispatch.json") + "); wr refuses it"
	}
	eng, ok := d.Cfg.Engines[tc.Engine]
	if !ok || !eng.Enabled || len(eng.Cmd) == 0 {
		return "engine " + tc.Engine + " not enabled or has no cmd"
	}
	if p := stopPath(d.Home, ""); fileExists(p) {
		return "STOP: " + p + " present (wr start)"
	}
	if p := stopPath(d.Home, slug); fileExists(p) {
		return "paused - STOP." + slug + " present (wr start -t " + slug + ")"
	}
	if p := pausePath(d.Home, slug); fileExists(p) {
		return "paused - " + readReason(p) + " (wr start -t " + slug + ")"
	}
	return ""
}

// planItems is the tenant's item list for planning: the full list when it is
// cached (status, serve) or asked for, else the lean list, upgraded to the full
// one only when a pause rule could trip on fields the lean list lacks (a blocked
// item's note, a done item's judged stamp).
func (d *Dispatcher) planItems(slug string, full bool) ([]boardItem, error) {
	if full {
		return d.MW.listItems(slug)
	}
	if items, ok := d.MW.cachedFull(slug); ok {
		return items, nil
	}
	items, err := d.MW.listLean(slug)
	if err != nil {
		return nil, err
	}
	blocked, done := 0, 0
	for _, it := range items {
		switch it.State {
		case "blocked":
			blocked++
		case "done":
			done++
		}
	}
	if (d.Cfg.Pause.OnBlocked && blocked > 0) || done > d.Cfg.Pause.Unjudged {
		return d.MW.listItems(slug)
	}
	return items, nil
}

// recoverLost marks runs with no end and a dead engine as lost, blocking what the
// session still owns and stopping it. Live runs are left; an overdue live one is
// reaped. A PID whose start time differs from the record is dead and never killed.
func (d *Dispatcher) recoverLost() error {
	runs, _ := readRuns(d.Home, "")
	for _, r := range runs {
		if r.End != nil || isInflight(r.Name) {
			continue
		}
		if runAlive(r) {
			if time.Since(r.Start) < time.Duration(d.Cfg.timeout())+5*time.Minute {
				continue
			}
			killPgid(r.PID) // overdue: its owner is gone, reap the group, then mark lost
		}
		m := &MW{Bin: d.MW.Bin, Board: r.Board}
		if err := d.settle(m, r, "wr: session lost (engine process gone)"); err != nil {
			r.StopErr = err.Error()
			d.event("stop-failed", r.Tenant, r.Name+": "+err.Error(), false)
		}
		now := time.Now().UTC()
		r.End, r.Outcome = &now, "lost"
		if err := writeRun(d.Home, r); err != nil {
			return err
		}
		fmt.Fprintf(d.Out, "lost: %s (%s) marked lost\n", r.Name, r.Item)
		d.event("lost", r.Tenant, r.Name+" "+r.Item, true)
	}
	return nil
}

// settle blocks every item still doing under the session (not only the dispatched
// one) and stops the session: one `mw list -s NAME -A -j`, then a block per item.
// The error is the failed list or stop, if any.
func (d *Dispatcher) settle(m *MW, r *Run, why string) error {
	items, err := m.sessionItems(r.Name)
	return d.settleItems(m, r, why, items, err)
}

func (d *Dispatcher) settleItems(m *MW, r *Run, why string, items []sessionItem, listErr error) error {
	for _, it := range items {
		if it.State == "doing" {
			t := it.Tenant
			if t == "" {
				t = r.Tenant
			}
			_ = m.block(t, it.ID, why, r.Name)
		}
	}
	m.fresh()
	var errs []error
	if listErr != nil {
		errs = append(errs, fmt.Errorf("session list failed: %w", listErr))
	}
	released, err := m.sessionStopRelease(r.Tenant, r.Name)
	if err != nil {
		errs = append(errs, fmt.Errorf("session stop failed: %w", err))
	} else if m.hasReap() && len(released) > 0 {
		d.event("released", r.Tenant, fmt.Sprintf("%s: session stopped with --release, %d item(s) back to ready %s", r.Name, len(released), strings.Join(released, ",")), false)
	}
	return errors.Join(errs...)
}

type prepared struct {
	ts     *tenantSetup
	item   *NextItem
	rec    *Run
	prompt string
	argv   []string
	eng    Engine
}

var projectsDirOverride string

// transcriptPath is where claude writes the session transcript:
// ~/.claude/projects/<cwd with non-alphanumerics as dashes>/<uuid>.jsonl.
func transcriptPath(cwd, uuid string) string {
	root := projectsDirOverride
	if root == "" {
		if root = os.Getenv("WR_CLAUDE_PROJECTS"); root == "" {
			h, _ := os.UserHomeDir()
			root = filepath.Join(h, ".claude", "projects")
		}
	}
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	return filepath.Join(root, regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(cwd, "-"), uuid+".jsonl")
}

// dispatch = prepare (session, claim: synchronous) then execute (engine run).
func (d *Dispatcher) dispatch(ts *tenantSetup, item *NextItem) (*Run, error) {
	p, err := d.prepare(ts, item)
	if err != nil {
		if p != nil {
			return p.rec, err
		}
		return nil, err
	}
	return d.execute(p)
}

// errNothingToClaim: mw had no ready item to give (a null claim): not an error, nothing ran.
var errNothingToClaim = errors.New("nothing to claim")

// prepare writes the record and claims the item with one `mw next --claim --prompt`
// (which also registers the session and returns the headless prompt). The run is marked in flight; execute (or a
// failed prepare) clears it.
func (d *Dispatcher) prepare(ts *tenantSetup, item *NextItem) (*prepared, error) {
	eng := d.Cfg.Engines[ts.Cfg.Engine]
	attempt := attemptFor(d.Home, ts.Slug, item.ID)
	role := ts.Cfg.Role
	if role == "" {
		role = "builder"
	}
	name := runName(ts.Slug, attempt, shortRand())
	usd := "5"
	if v, ok := d.Cfg.Caps.USDPerDay[ts.Cfg.Engine]; ok && v > 0 {
		usd = strconv.FormatFloat(v/4, 'f', -1, 64)
	}
	uuid := newUUID()
	argv := expandArgv(eng.Cmd, uuid, usd, name, item.ID)
	rec := &Run{Name: name, Tenant: ts.Slug, Board: d.MW.Board, Item: item.ID, Engine: ts.Cfg.Engine,
		UUID: uuid, CmdHash: cmdHash(eng.Cmd), Phase: "starting", Start: time.Now().UTC()}
	for _, a := range eng.Cmd {
		if strings.Contains(a, "{uuid}") {
			rec.Transcript = transcriptPath(ts.Dir, uuid)
		}
	}
	setInflight(name, true)
	p := &prepared{ts, item, rec, "", argv, eng}
	// the record exists before the claim, so a crash in between is recoverable
	if err := writeRun(d.Home, rec); err != nil {
		setInflight(name, false)
		return p, err
	}
	claimed, err := d.MW.claimNext(ts.Slug, name, role)
	d.MW.fresh()
	if err == nil && claimed == nil {
		// raced: another runner took the item between plan and claim; nothing is claimed
		// no run happened: drop the record, so it counts for nothing (attempts, lastOutcome)
		setInflight(name, false)
		_ = os.Remove(filepath.Join(runsDir(d.Home, ts.Slug), name+".json"))
		return nil, errNothingToClaim
	}
	if err != nil {
		setInflight(name, false)
		return p, d.abort(rec, err)
	}
	if claimed.ID != item.ID { // the board's order moved: run what mw actually gave us
		rec.Item, p.item = claimed.ID, claimed
		argv = expandArgv(eng.Cmd, uuid, usd, name, claimed.ID)
		p.argv = argv
		if err := writeRun(d.Home, rec); err != nil {
			setInflight(name, false)
			return p, d.abort(rec, err)
		}
	}
	if claimed.Prompt == "" {
		return p, d.abort(rec, errors.New("mw next --claim returned no prompt (needs mw >= "+MinMW+")"))
	}
	if f := failedAttempts(d.Home, ts.Slug, claimed.ID); f >= d.Cfg.maxAttempts() {
		return p, d.abort(rec, fmt.Errorf("retries: %s failed %d times (max %d)", claimed.ID, f, d.Cfg.maxAttempts()))
	}
	p.prompt = buildPrompt(claimed.Prompt, claimed.Brief, claimed.ID, name, ts.Cfg.Forbid)
	return p, nil
}

// Timings the test rig lowers (like retryDelay and livenessPoll).
var (
	engineWaitDelay = 10 * time.Second // grace for an engine's I/O after it exits or is killed
	followPoll      = time.Second      // wr log -f
	uiRefresh       = 3 * time.Second  // wr ui terminal refresh
)

// livenessPoll is how often a running engine's transcript mtime is checked.
var livenessPoll = 15 * time.Second

// execute runs the engine and settles the board. Liveness is the transcript's
// mtime: no write for stale_minutes kills the run and blocks the item.
func (d *Dispatcher) execute(p *prepared) (*Run, error) {
	ts, item, rec, eng := p.ts, p.item, p.rec, p.eng
	defer d.afterSettle()
	defer setInflight(rec.Name, false)
	logf, err := os.Create(filepath.Join(runsDir(d.Home, ts.Slug), rec.Name+".log"))
	if err != nil {
		return rec, d.abort(rec, err)
	}
	defer logf.Close()
	lw := &capWriter{w: logf, max: logCap}
	tail := &tailBuf{max: 256 << 10}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(d.Cfg.timeout()))
	defer cancel()
	cmd := exec.Command(p.argv[0], p.argv[1:]...)
	cmd.Dir = ts.Dir
	cmd.Stdin = strings.NewReader(p.prompt)
	cmd.Stdout = io.MultiWriter(lw, tail)
	cmd.Stderr = lw
	cmd.WaitDelay = engineWaitDelay
	cmd.Env = engineEnv(os.Environ(), "WR_BOARD="+d.MW.Board, "WR_TENANT="+ts.Slug, "WR_ITEM="+item.ID,
		"WR_SESSION="+rec.Name, "MW_SESSION="+rec.Name, "MW_TENANT="+ts.Slug)
	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		d.finish(rec, -1, false)
		d.settleRec(rec, "wr: engine failed to start: "+err.Error())
		rec.Outcome = "incomplete"
		return rec, writeRun(d.Home, rec)
	}
	rec.PID, rec.Phase, rec.PStart = cmd.Process.Pid, "running", procStart(cmd.Process.Pid)
	if err := writeRun(d.Home, rec); err != nil {
		killGroup(cmd)
		_ = cmd.Wait()
		return rec, d.abort(rec, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var werr error
	timedOut, canceled := false, false
	var tick <-chan time.Time
	if rec.Transcript != "" {
		tk := time.NewTicker(livenessPoll)
		defer tk.Stop()
		tick = tk.C
	}
wait:
	for {
		select {
		case werr = <-done:
			break wait
		case <-ctx.Done():
			timedOut = true
			killGroup(cmd)
			werr = <-done
			break wait
		case <-d.ctx().Done():
			canceled = true
			killGroup(cmd)
			werr = <-done
			break wait
		case <-tick:
			last := rec.Start
			if fi, err := os.Stat(rec.Transcript); err == nil && fi.ModTime().After(last) {
				last = fi.ModTime()
			}
			if time.Since(last) > d.Cfg.stale() {
				rec.Stale = true
				killGroup(cmd)
				werr = <-done
				break wait
			}
		}
	}
	killGroup(cmd) // sweep stragglers left in the group
	code := 0
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		code = ee.ExitCode()
	} else if werr != nil {
		code = -1
	}
	usage, cost := parseUsage(eng.Usage, tail.b)
	d.finish(rec, code, timedOut)
	rec.Usage, rec.Cost = usage, cost
	if canceled {
		d.settleRec(rec, "wr: serve stopped (signal)")
		rec.Outcome = "incomplete"
		return rec, writeRun(d.Home, rec)
	}
	if lw.truncated {
		fmt.Fprintf(logf, "\n[wr: log truncated at %d bytes]\n", logCap)
	}
	rec.Outcome = d.outcome(rec, code)
	return rec, writeRun(d.Home, rec)
}

// settleRec settles on the dispatcher's board and records a failed stop.
func (d *Dispatcher) settleRec(r *Run, why string) {
	if err := d.settle(d.MW, r, why); err != nil {
		r.StopErr = err.Error()
		d.event("stop-failed", r.Tenant, r.Name+": "+err.Error(), true)
	}
}

// engineEnv is base without wr/mw routing variables, plus extra.
func engineEnv(base []string, extra ...string) []string {
	var out []string
	for _, e := range base {
		if envKeyIs(e, "MW_BOARD") || envKeyIs(e, "MW_SESSION") || envKeyIs(e, "MW_TENANT") {
			continue
		}
		out = append(out, e)
	}
	return append(out, extra...)
}

// abort settles a run that failed after its record was written (block what is
// claimed, stop the session), closes the record as incomplete, and returns cause.
func (d *Dispatcher) abort(rec *Run, cause error) error {
	d.finish(rec, -1, false)
	d.settleRec(rec, "wr: dispatch failed: "+cause.Error())
	rec.Outcome = "incomplete"
	if err := writeRun(d.Home, rec); err != nil {
		return fmt.Errorf("%w (and the record: %v)", cause, err)
	}
	return cause
}

func (d *Dispatcher) finish(r *Run, code int, timeout bool) {
	now := time.Now().UTC()
	r.End, r.Exit, r.Timeout = &now, &code, timeout
}

// outcome reads the board, not the exit code, then settles: one
// `mw list -s NAME -A -j` gives the item's state and what else the session still
// owns; every doing item is blocked and the session stopped (a failed stop is recorded).
func (d *Dispatcher) outcome(r *Run, code int) string {
	var items []sessionItem
	var err error
	for i := 0; i < 3; i++ {
		if items, err = d.MW.sessionItems(r.Name); err == nil {
			break
		}
		time.Sleep(retryDelay)
	}
	oc, why := "incomplete", fmt.Sprintf("wr: session ended without done (exit %d)", code)
	switch {
	case r.Stale:
		why = fmt.Sprintf("wr: session stalled (no transcript writes for %s), killed", d.Cfg.stale())
	case r.Timeout:
		why = "wr: run timed out"
	}
	state, found := "", false
	for _, it := range items {
		if it.ID == r.Item && (it.Tenant == r.Tenant || it.Tenant == "") {
			state, found = it.State, true
		}
	}
	if err == nil && !found { // no longer owned (reopened, archived...): ask the item itself
		if it, serr := d.MW.showRetry(r.Tenant, r.Item); serr == nil {
			state, found = it.State, true
			if it.State == "doing" && it.Session != r.Name {
				state = "foreign"
			}
		} else {
			err = serr
		}
	}
	switch {
	case err != nil:
		why = "wr: cannot read item after run: " + err.Error()
	case state == "done":
		oc = "ok"
	case state == "blocked":
		oc = "blocked"
	case state == "doing":
	default:
		oc = "foreign"
	}
	d.settleRec2(r, why, items, err)
	return oc
}

func (d *Dispatcher) settleRec2(r *Run, why string, items []sessionItem, listErr error) {
	if err := d.settleItems(d.MW, r, why, items, listErr); err != nil {
		r.StopErr = err.Error()
		d.event("stop-failed", r.Tenant, r.Name+": "+err.Error(), true)
	}
}

// applyLevel sets the board's gate level for a tenant wr would run to the configured one
// (one log line per change; an mw error is logged and one gate-level-failed event is
// written per distinct error, never fatal). At most one attempt per tenant until
// resetLevelTries (serve: per tick; once: per invocation). An unknown value is
// logged once per process and ignored; values match case-insensitively.
func (d *Dispatcher) applyLevel(slug string, tc TenantCfg) {
	if d.Dry || !tc.Enabled || tc.GateLevel == "" {
		return
	}
	want := normLevel(tc.GateLevel)
	if !validGateLevel(want) {
		if _, dup := levelWarned.LoadOrStore(slug+"="+tc.GateLevel, true); !dup {
			fmt.Fprintf(d.Out, "%s: gate_level %s unknown, ignored\n", slug, tc.GateLevel)
		}
		return
	}
	if d.blockReason(slug, tc) != "" {
		return
	}
	d.lvlMu.Lock()
	if d.lvlTried[slug] {
		d.lvlMu.Unlock()
		return
	}
	if d.lvlTried == nil {
		d.lvlTried = map[string]bool{}
	}
	d.lvlTried[slug] = true
	d.lvlMu.Unlock()
	changed, from, err := d.MW.applyGateLevel(slug, want)
	switch {
	case err != nil:
		fmt.Fprintf(d.Out, "%s: gate level %s not applied: %v\n", slug, want, err)
		d.lvlMu.Lock()
		if d.lvlErr == nil {
			d.lvlErr = map[string]string{}
		}
		dup := d.lvlErr[slug] == err.Error()
		d.lvlErr[slug] = err.Error()
		d.lvlMu.Unlock()
		if !dup {
			d.event("gate-level-failed", slug, want+": "+err.Error(), false)
		}
	case changed:
		d.lvlMu.Lock()
		delete(d.lvlErr, slug)
		d.lvlMu.Unlock()
		fmt.Fprintf(d.Out, "%s: gate level %s -> %s\n", slug, from, want)
		d.event("gate-level", slug, from+" -> "+want, false)
	}
}

// resetLevelTries lets the next plan or tick try applying gate levels again.
func (d *Dispatcher) resetLevelTries() {
	d.lvlMu.Lock()
	d.lvlTried = nil
	d.lvlMu.Unlock()
}

var levelWarned sync.Map
