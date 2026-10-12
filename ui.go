package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed ui.html
var uiPage []byte

const uiDefaultAddr = "127.0.0.1:5052"

type uiServe struct {
	Running bool     `json:"running"`
	PID     int      `json:"pid,omitempty"`
	Stop    []string `json:"stop_files"`
}

type uiCap struct {
	In    int `json:"in_use"`
	Limit int `json:"limit"`
}

type uiCaps struct {
	Global  uiCap            `json:"global"`
	Tenant  map[string]uiCap `json:"tenant"`
	PerHour uiCap            `json:"per_hour"`
}

type uiCost struct {
	Today float64            `json:"today"`
	Days  map[string]float64 `json:"last7"`
}

type uiRun struct {
	Tenant        string   `json:"tenant"`
	Item          string   `json:"item"`
	Title         string   `json:"title"`
	AgeS          int      `json:"age_s"`
	TranscriptAge int      `json:"transcript_age_s"`
	Cost          *float64 `json:"cost"`
	Lost          bool     `json:"lost"`
}

type uiLast struct {
	Time     string  `json:"time"`
	Tenant   string  `json:"tenant"`
	Item     string  `json:"item"`
	Outcome  string  `json:"outcome"`
	Duration int     `json:"duration_s"`
	Cost     float64 `json:"cost"`
	Note     string  `json:"note"`
}

type uiItem struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Title string `json:"title"`
}

type uiTenant struct {
	Slug   string         `json:"slug"`
	Tags   []string       `json:"tags,omitempty"`
	Counts map[string]int `json:"counts"`
	Done   int            `json:"done"`
	Total  int            `json:"total"`
	Reason string         `json:"reason"`
	Items  []uiItem       `json:"items"`
	More   int            `json:"more"`
}

type uiEvent struct {
	TS     string `json:"ts"`
	Tenant string `json:"tenant"`
	Item   string `json:"item"`
	Action string `json:"action"`
	By     string `json:"by"`
	Note   string `json:"note"`
}

type uiDoc struct {
	Refreshed string     `json:"refreshed"`
	Serve     uiServe    `json:"serve"`
	Caps      uiCaps     `json:"caps"`
	Cost      uiCost     `json:"cost"`
	Runs      []uiRun    `json:"runs"`
	Last      []uiLast   `json:"last"`
	Tenants   []uiTenant `json:"tenants"`
	Archived  int        `json:"archived_hidden,omitempty"` // archived tenants left out
	Activity  []uiEvent  `json:"activity"`
}

func (b boardItem) title() string {
	var s string
	_ = json.Unmarshal(b.Raw["title"], &s)
	return s
}

// uiData is read-only; it reuses statusData for everything wr status shows.
func uiData(d *Dispatcher) (*uiDoc, error) {
	st, err := statusData(d, map[string]string{})
	if err != nil {
		return nil, err
	}
	doc := &uiDoc{Refreshed: time.Now().Format(time.RFC3339), Runs: []uiRun{}, Last: []uiLast{}, Tenants: []uiTenant{}, Activity: []uiEvent{}}
	doc.Serve = uiServe{Stop: st.Stop}
	if doc.Serve.Stop == nil {
		doc.Serve.Stop = []string{}
	}
	if un, err := lockHome(d.Home); err != nil {
		doc.Serve.Running = true // somebody holds wr.lock
		doc.Serve.PID = readLockPid(d.Home)
	} else {
		un()
	}
	c := d.Cfg.Caps
	doc.Caps = uiCaps{Global: uiCap{st.Caps.ParallelGlobal, c.ParallelGlobal}, PerHour: uiCap{st.Caps.Hour, c.PerHour}, Tenant: map[string]uiCap{}}
	for t, n := range st.Caps.LiveByTenant {
		doc.Caps.Tenant[t] = uiCap{n, c.ParallelTenant}
	}
	doc.Cost = uiCost{Days: map[string]float64{}}
	today := time.Now().Local().Format("2006-01-02")
	cut := time.Now().AddDate(0, 0, -6).Local().Format("2006-01-02")
	for _, r := range st.Cost {
		if r.Day >= cut {
			doc.Cost.Days[r.Day] += r.USD
		}
		if r.Day == today {
			doc.Cost.Today += r.USD
		}
	}
	runs, _ := readRuns(d.Home, "")
	byName := map[string]*Run{}
	for _, r := range runs {
		byName[r.Name] = r
	}
	seen := map[string]bool{}
	var order []string
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			order = append(order, t)
		}
	}
	for _, t := range d.Cfg.tenantsInOrder() {
		if d.Cfg.Tenants[t].Enabled {
			add(t)
		}
	}
	if ts, err := d.MW.tenants(); err == nil {
		for _, t := range ts {
			add(t.Tenant)
		}
	}
	order, doc.Archived = d.shown(order)
	reason := map[string]string{}
	for _, s := range st.Skips {
		reason[s.Tenant] = s.Reason
	}
	for _, p := range st.Paused {
		reason[p.Tenant] = p.Reason
	}
	titles := map[string]string{}
	items := map[string][]boardItem{}
	for _, t := range order {
		items[t], _ = d.MW.listItems(t)
		for _, it := range items[t] {
			titles[t+"/"+it.ID] = it.title()
		}
	}
	for _, l := range st.Live {
		u := uiRun{l.Tenant, l.Item, titles[l.Tenant+"/"+l.Item], l.AgeSec, l.TranscriptSec, nil, l.Lost}
		if r := byName[l.Name]; r != nil {
			u.Cost = r.Cost
		}
		doc.Runs = append(doc.Runs, u)
	}
	for i := len(runs) - 1; i >= 0 && len(doc.Last) < 10; i-- {
		r := runs[i]
		if r.Outcome == "" {
			continue
		}
		l := uiLast{Time: r.Start.Local().Format("2006-01-02 15:04"), Tenant: r.Tenant, Item: r.Item, Outcome: r.Outcome}
		if r.End != nil {
			l.Duration = int(r.End.Sub(r.Start).Seconds())
		}
		if r.Cost != nil {
			l.Cost = *r.Cost
		}
		switch {
		case r.Timeout:
			l.Note = "timeout"
		case r.Stale:
			l.Note = "stale transcript"
		}
		doc.Last = append(doc.Last, l)
	}
	rank := map[string]int{"doing": 0, "blocked": 1, "ready": 2, "inbox": 3}
	limit := map[string]int{"doing": 99, "blocked": 99, "ready": 6, "inbox": 3}
	for _, t := range order {
		ut := uiTenant{Slug: t, Tags: d.MW.meta(t).Tags, Counts: map[string]int{}, Reason: reason[t], Items: []uiItem{}}
		its := append([]boardItem(nil), items[t]...)
		sort.SliceStable(its, func(i, j int) bool { return rank[its[i].State] < rank[its[j].State] })
		shown := map[string]int{}
		for _, it := range its {
			ut.Counts[it.State]++
			ut.Total++
			if it.State == "done" {
				ut.Done++
			}
			if _, ok := rank[it.State]; !ok {
				continue
			}
			if shown[it.State] >= limit[it.State] {
				ut.More++
				continue
			}
			shown[it.State]++
			ut.Items = append(ut.Items, uiItem{it.ID, it.State, it.title()})
		}
		doc.Tenants = append(doc.Tenants, ut)
	}
	if out, err := d.MW.must("log", "-A", "-n", "40", "-j"); err == nil {
		_ = json.Unmarshal([]byte(out), &doc.Activity)
	}
	return doc, nil
}

func dur(sec int) string {
	switch {
	case sec < 0:
		return "-"
	case sec < 90:
		return fmt.Sprintf("%ds", sec)
	case sec < 5400:
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func clock(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Local().Format("01-02 15:04")
	}
	return ts
}

// uiFrame renders the three bands as text; width picks side-by-side or stacked tenants.
func uiFrame(doc *uiDoc, width int) string {
	var b strings.Builder
	serve := "serve: not running"
	if doc.Serve.Running {
		serve = "serve: running"
		if doc.Serve.PID > 0 {
			serve += fmt.Sprintf(" (pid %d)", doc.Serve.PID)
		}
	}
	if n := len(doc.Serve.Stop); n > 0 {
		serve += fmt.Sprintf(" (%d STOP files)", n)
	}
	fmt.Fprintf(&b, "wr ui  %s | live %d/%d, %d/h of %d | USD today %.2f | refreshed %s\n\n", serve, doc.Caps.Global.In, doc.Caps.Global.Limit,
		doc.Caps.PerHour.In, doc.Caps.PerHour.Limit, doc.Cost.Today, clock(doc.Refreshed))
	b.WriteString("LIVE RUNS\n")
	if len(doc.Runs) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, r := range doc.Runs {
		flag, cost := "", "-"
		if r.Lost {
			flag = "  lost?"
		}
		if r.Cost != nil {
			cost = fmt.Sprintf("%.2f", *r.Cost)
		}
		fmt.Fprintf(&b, "  %-12s %-8s %-40s age %-6s transcript %-6s USD %s%s\n", r.Tenant, r.Item, clip(r.Title, 40), dur(r.AgeS), dur(r.TranscriptAge), cost, flag)
	}
	b.WriteString("\nTENANTS\n")
	if s := archivedNote(doc.Archived); s != "" {
		b.WriteString(s + "\n")
	}
	var blocks [][]string
	for _, t := range doc.Tenants {
		var cs []string
		for _, s := range []string{"doing", "ready", "blocked", "inbox", "done"} {
			if n := t.Counts[s]; n > 0 {
				cs = append(cs, fmt.Sprintf("%s %d", s, n))
			}
		}
		head := t.Slug + tagText(t.Tags)
		if t.Total > 0 {
			head = fmt.Sprintf("%s  %d/%d done", head, t.Done, t.Total)
		}
		l := []string{head, strings.Join(cs, ", ")}
		if t.Reason != "" {
			l = append(l, "("+clip(t.Reason, 36)+")")
		}
		for _, it := range t.Items {
			l = append(l, fmt.Sprintf("%-7s %s %s", it.State, it.ID, clip(it.Title, 20)))
		}
		if t.More > 0 {
			l = append(l, fmt.Sprintf("+%d more", t.More))
		}
		blocks = append(blocks, l)
	}
	per := 1
	if width >= 120 {
		per = width / 40
	}
	for i := 0; i < len(blocks); i += per {
		grp := blocks[i:min(i+per, len(blocks))]
		rows := 0
		for _, g := range grp {
			rows = max(rows, len(g))
		}
		for r := 0; r < rows; r++ {
			var line strings.Builder
			for _, g := range grp {
				cell := ""
				if r < len(g) {
					cell = clip(g[r], 38)
				}
				if per > 1 {
					fmt.Fprintf(&line, "  %-38s", cell)
				} else {
					line.WriteString("  " + cell)
				}
			}
			b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("ACTIVITY\n")
	for i, e := range doc.Activity {
		if i == 15 {
			break
		}
		fmt.Fprintf(&b, "  %s %-12s %-8s %-8s %s %s\n", clock(e.TS), e.Tenant, e.Item, e.Action, e.By, clip(e.Note, 40))
	}
	return b.String()
}

func termWidth() int {
	var w int
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	if out, err := cmd.Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) == 2 {
			fmt.Sscanf(f[1], "%d", &w)
		}
	}
	if w == 0 {
		fmt.Sscanf(os.Getenv("COLUMNS"), "%d", &w)
	}
	if w == 0 {
		w = 100
	}
	return w
}

// exportUI is the explicit `wr ui --export`: it always writes.
func exportUI(d *Dispatcher, path string) error {
	_, _, err := exportUIAt(d, path, true)
	return err
}

// exportUIAt writes the ui page to path with the data inlined (no fetch), atomically,
// plus the sidecar <path>.hash ("<unix time>\n<sha256 of the data without its refresh time>").
// Unless force it skips an unchanged page and a write less than the interval after the
// last one; wait is then the time left until the interval ends (0 for unchanged).
func exportUIAt(d *Dispatcher, path string, force bool) (wrote bool, wait time.Duration, err error) {
	doc, err := uiData(d)
	if err != nil {
		return false, 0, err
	}
	stamp := doc.Refreshed
	doc.Refreshed = ""
	hb, _ := json.Marshal(doc)
	doc.Refreshed = stamp
	sum := sha256.Sum256(hb)
	hash := hex.EncodeToString(sum[:])
	if !force {
		if b, e := os.ReadFile(path + ".hash"); e == nil {
			if f := strings.Fields(string(b)); len(f) == 2 {
				at, _ := strconv.ParseInt(f[0], 10, 64)
				if f[1] == hash {
					return false, 0, nil
				}
				if left := d.Cfg.Publish.interval() - d.now().Sub(time.Unix(at, 0)); left > 0 {
					return false, left, nil
				}
			}
		}
	}
	b, err := uiExportPage(doc)
	if err != nil {
		return false, 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, 0, err
	}
	for _, f := range []struct { // the page first, then its hash
		p    string
		data []byte
	}{{path, b}, {path + ".hash", []byte(strconv.FormatInt(d.now().Unix(), 10) + "\n" + hash + "\n")}} {
		tf, err := os.CreateTemp(filepath.Dir(f.p), filepath.Base(f.p)+".*.tmp")
		if err != nil {
			return false, 0, err
		}
		tmp := tf.Name()
		_, werr := tf.Write(f.data)
		cerr := tf.Close()
		if werr != nil || cerr != nil {
			os.Remove(tmp)
			return false, 0, errors.Join(werr, cerr)
		}
		_ = os.Chmod(tmp, 0o644)
		if err := renameRetry(tmp, f.p); err != nil {
			os.Remove(tmp)
			return false, 0, err
		}
	}
	d.pubWrites++
	return true, 0, nil
}

// uiExportPage is uiPage with the doc as window.__WR_DATA__ before the page script.
func uiExportPage(doc *uiDoc) ([]byte, error) {
	j, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	j = bytes.ReplaceAll(j, []byte("<"), []byte(`\u003c`)) // no </script> breakout
	tag := []byte("<script>\nconst $=")
	i := bytes.Index(uiPage, tag)
	if i < 0 {
		return nil, errors.New("ui page template changed: no script marker")
	}
	var out bytes.Buffer
	out.Write(uiPage[:i])
	out.WriteString("<script>window.__WR_DATA__=")
	out.Write(j)
	out.WriteString(";</script>\n")
	out.Write(uiPage[i:])
	return out.Bytes(), nil
}

func uiCommand(d *Dispatcher, o map[string]string) error {
	if p := o["export"]; p != "" {
		return exportUI(d, p)
	}
	web := o["web"] == "true" || o["open"] == "true" || d.Cfg.UI.Web
	addr := o["addr"]
	if addr == "" {
		addr = d.Cfg.UI.Addr
	}
	if addr == "" {
		addr = uiDefaultAddr
	}
	if web { // the only place a socket is ever opened
		ln, err := listenLoopback(addr)
		if err != nil {
			return err
		}
		url := "http://" + ln.Addr().String() + "/"
		fmt.Fprintln(os.Stderr, "wr ui: page at", url)
		go http.Serve(ln, uiHandler(d))
		if o["open"] == "true" {
			switch runtime.GOOS {
			case "darwin":
				_ = exec.Command("open", url).Start()
			case "windows":
				_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
			default:
				_ = exec.Command("xdg-open", url).Start()
			}
		}
	}
	if o["once"] == "true" {
		doc, err := uiData(d)
		if err != nil {
			return err
		}
		fmt.Fprint(d.Out, uiFrame(doc, termWidth()))
		return nil
	}
	vt := d.Out != os.Stdout || enableVT()
	quit := make(chan struct{}, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		stty := func(a ...string) error { c := exec.Command("stty", a...); c.Stdin = os.Stdin; return c.Run() }
		if stty("-icanon", "-echo") == nil {
			defer stty("icanon", "echo")
			go func() {
				buf := make([]byte, 1)
				for {
					if n, err := os.Stdin.Read(buf); n == 0 || err != nil || buf[0] == 'q' {
						quit <- struct{}{}
						return
					}
				}
			}()
		}
	}
	for {
		frame := ""
		d.MW.fresh()
		if doc, err := uiData(d); err != nil {
			frame = "wr ui: " + err.Error() + "\n"
		} else {
			frame = uiFrame(doc, termWidth())
		}
		clear := "\x1b[H\x1b[2J"
		if !vt {
			clear = "\n" // a Windows console without VT support: frames scroll instead
		}
		fmt.Fprint(d.Out, clear+frame+"\nq to quit\n")
		select {
		case <-quit:
			return nil
		case <-sig:
			return nil
		case <-time.After(uiRefresh):
		}
	}
}

func listenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("wr ui binds loopback only, got " + addr)
	}
	return net.Listen("tcp", addr)
}

func uiHandler(d *Dispatcher) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		d.MW.fresh()
		doc, err := uiData(d)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = printJSON(w, doc)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(uiPage)
	})
	return mux
}
