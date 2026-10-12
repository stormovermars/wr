package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (r *rig) disp() (*Dispatcher, *bytes.Buffer) {
	var buf bytes.Buffer
	r.t.Setenv("FAKE", "done")
	return &Dispatcher{Home: r.home, Cfg: r.cfg, MW: newMW(r.board), Out: &buf}, &buf
}

func (r *rig) tick(s *Server, buf *bytes.Buffer) string {
	r.t.Helper()
	buf.Reset()
	if _, _, err := s.tick(); err != nil {
		r.t.Fatal(err)
	}
	s.wait()
	return buf.String()
}

func TestServeOneDispatchPerTick(t *testing.T) {
	r := newRig(t)
	r.addReady("a")
	r.addReady("b")
	d, buf := r.disp()
	s := newServer(d)
	out := r.tick(s, buf)
	if n := len(r.runs()); n != 1 || !strings.Contains(out, "-> ok") {
		t.Fatalf("tick 1: %d runs\n%s", n, out)
	}
	out2 := r.tick(s, buf)
	if n := len(r.runs()); n != 2 {
		t.Fatalf("tick 2: %d runs\n%s", n, out2)
	}
	if out := r.tick(s, buf); !strings.Contains(out, "nothing ready") || len(r.runs()) != 2 {
		t.Fatalf("tick 3: %s", out)
	}
}

func TestServeLoopSleepsBetweenTicksAndStopsOnGlobalSTOP(t *testing.T) {
	r := newRig(t)
	d, buf := r.disp()
	var slept []time.Duration
	d.Sleep = func(x time.Duration) { slept = append(slept, x) }
	r.cfg.TickMin = 7
	if err := newServer(d).serveLoop(3, d.Sleep); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 2 || slept[0] != 7*time.Minute {
		t.Fatalf("sleeps %v", slept)
	}
	// global STOP: exits at the next tick, no further sleep
	var o bytes.Buffer
	if err := run([]string{"stop"}, &o); err != nil || !fileExists(stopPath(r.home, "")) {
		t.Fatalf("stop: %v", err)
	}
	slept = nil
	buf.Reset()
	r.addReady("a")
	if err := newServer(d).serveLoop(5, d.Sleep); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 0 || len(r.runs()) != 0 || !strings.Contains(buf.String(), "STOP") {
		t.Fatalf("serve ignored STOP: sleeps %v runs %d %q", slept, len(r.runs()), buf.String())
	}
	if err := run([]string{"start"}, &o); err != nil || fileExists(stopPath(r.home, "")) {
		t.Fatalf("start: %v", err)
	}
}

func TestServeCommandLockAndMaxTicks(t *testing.T) {
	r := newRig(t)
	r.addReady("a")
	t.Setenv("FAKE", "done")
	var out bytes.Buffer
	if err := run([]string{"serve", "--max-ticks", "1"}, &out); err != nil || len(r.runs()) != 1 {
		t.Fatalf("serve: %v runs %d\n%s", err, len(r.runs()), out.String())
	}
	unlock, err := lockHome(r.home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := run([]string{"serve", "--max-ticks", "1"}, &out); err == nil {
		t.Fatal("second serve must refuse while the lock is held")
	}
}

func TestTenantSTOPFile(t *testing.T) {
	r := newRig(t)
	r.addReady("a")
	d, buf := r.disp()
	var o bytes.Buffer
	if err := run([]string{"stop", "-t", "t"}, &o); err != nil || !fileExists(stopPath(r.home, "t")) {
		t.Fatalf("stop -t: %v", err)
	}
	out := r.tick(newServer(d), buf)
	if len(r.runs()) != 0 || !strings.Contains(out, "t: skip - paused - STOP.t") {
		t.Fatalf("tenant STOP ignored: %q", out)
	}
	run([]string{"start", "-t", "t"}, &o)
	r.tick(newServer(d), buf)
	if len(r.runs()) != 1 {
		t.Fatal("not dispatched after start")
	}
}

// liveGroup starts a sleep in its own process group, to count as a live run.
func liveGroup(t *testing.T) int {
	t.Helper()
	c := sleepCmd(60)
	setProcGroup(c)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killGroup(c); _ = c.Wait() })
	return c.Process.Pid
}

func (r *rig) seedRun(name, item string, start time.Time, pid int, cost *float64, ended bool) {
	rec := &Run{Name: name, Tenant: "t", Board: r.board, Item: item, Engine: "fake", PID: pid, Start: start, Cost: cost}
	if ended {
		e := start.Add(time.Second)
		rec.End, rec.Outcome = &e, "ok"
	}
	if err := writeRun(r.home, rec); err != nil {
		r.t.Fatal(err)
	}
}

func TestCaps(t *testing.T) {
	cases := []struct {
		name string
		set  func(r *rig)
		want string
	}{
		{"per_hour", func(r *rig) {
			r.cfg.Caps.PerHour = 2
			r.seedRun("wr-t-x-1", "X-1", time.Now().Add(-10*time.Minute), 0, nil, true)
			r.seedRun("wr-t-x-2", "X-2", time.Now().Add(-59*time.Minute), 0, nil, true)
			r.seedRun("wr-t-x-3", "X-3", time.Now().Add(-61*time.Minute), 0, nil, true) // outside the window
		}, "cap per_hour: 2 sessions"},
		{"parallel_tenant", func(r *rig) {
			r.cfg.Caps.ParallelTenant = 1
			r.seedRun("wr-t-x-1", "X-1", time.Now(), liveGroup(t), nil, false)
		}, "cap parallel_tenant: 1 live"},
		{"parallel_global", func(r *rig) {
			r.cfg.Caps.ParallelTenant = 9
			r.cfg.Caps.ParallelGlobal = 1
			r.seedRun("wr-t-x-1", "X-1", time.Now(), liveGroup(t), nil, false)
		}, "cap parallel_global: 1 live"},
		{"usd_per_day", func(r *rig) {
			c := 3.5
			r.cfg.Caps.USDPerDay = map[string]float64{"fake": 3}
			r.seedRun("wr-t-x-1", "X-1", time.Now(), 0, &c, true)
		}, "cap usd_per_day: engine fake spent USD 3.50 of 3.00"},
		{"disk_free_gb", func(r *rig) { r.cfg.Caps.DiskFreeGB = 1 << 30 }, "cap disk_free_gb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.addReady("a")
			r.cfg.Caps.PerHour, r.cfg.Caps.ParallelGlobal, r.cfg.Caps.DiskFreeGB = 100, 100, 0
			c.set(r)
			d, buf := r.disp()
			before := len(r.runs())
			out := r.tick(newServer(d), buf)
			if !strings.Contains(out, c.want) || len(r.runs()) != before {
				t.Fatalf("want skip %q, runs %d->%d\n%s", c.want, before, len(r.runs()), out)
			}
			// the same board dispatches once the cap is lifted
			r.cfg.Caps = Caps{PerHour: 100, ParallelGlobal: 100, ParallelTenant: 100}
			r.tick(newServer(d), buf)
			if len(r.runs()) != before+1 {
				t.Fatalf("not dispatched without the cap\n%s", buf.String())
			}
		})
	}
}

func TestPauseBlockedItem(t *testing.T) {
	r := newRig(t)
	id := r.addReady("a")
	sh(t, r.board, mwBin, "block", id, "nope", "-t", "t")
	r.addReady("b")
	d, buf := r.disp()
	out := r.tick(newServer(d), buf)
	if len(r.runs()) != 0 || !strings.Contains(out, "t: skip - paused - blocked item(s): "+id) {
		t.Fatalf("blocked pause: %q", out)
	}
	r.cfg.Pause.OnBlocked = false
	r.tick(newServer(d), buf)
	if len(r.runs()) != 1 {
		t.Fatal("on_blocked=false must not pause")
	}
}

// writeBoard writes a board file of the given schema; fakeMW prints list JSON.
func fakeBoard(t *testing.T, schema int, listJSON string) *MW {
	needShell(t)
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".miniwaves.json"), []byte(`{"version":`+strconv.Itoa(schema)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "mw")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat <<'EOF'\n"+listJSON+"\nEOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &MW{Bin: bin, Board: dir}
}

// v6 items as mw >= schema 6 prints them: judged/evidence omitted when empty.
const v6List = `{"items":[
 {"id":"I-1","state":"done","note":"n"},
 {"id":"I-2","state":"done","note":"n"},
 {"id":"I-3","state":"done","note":"n"},
 {"id":"I-4","state":"done","e":"E","judged":{"ok":true}},
 {"id":"I-5","state":"done","note":"has note","evidence":[]}],"waves":[]}`

func TestPauseUnjudgedSchema6(t *testing.T) {
	m := fakeBoard(t, 6, v6List)
	d := &Dispatcher{Cfg: &Config{Pause: Pause{Unjudged: 2, OnBlocked: true, QualityGuard: true}}, MW: m}
	its, err := m.listItems("t")
	if err != nil {
		t.Fatal(err)
	}
	r, trip, _ := d.pauseReasons(its, nil, true, true)
	if len(r) != 1 || !strings.Contains(r[0], "4 done items unjudged (limit 2)") || trip != "" {
		t.Fatalf("v6 absent judged = unjudged: %v %q", r, trip)
	}
	d.Cfg.Pause.Unjudged = 4
	if r, _, _ := d.pauseReasons(its, nil, true, true); len(r) != 0 {
		t.Fatalf("within limit: %v", r)
	}
	// newly done: I-4 was seen, the others are new; absent or empty evidence trips
	_, trip, _ = d.pauseReasons(its, []string{"I-4"}, false, true)
	for _, id := range []string{"I-1", "I-2", "I-3", "I-5"} {
		if !strings.Contains(trip, id) {
			t.Fatalf("trip lacks %s: %q", id, trip)
		}
	}
	if _, trip, _ = d.pauseReasons(its, []string{"I-1", "I-2", "I-3", "I-5"}, false, true); trip != "" {
		t.Fatalf("clean item tripped: %q", trip)
	}
}

// wr's own blocks do not pause the tenant; a human block does; the no-evidence
// waiver note is a warning, not a quality-guard trip.
func TestOwnBlocksAndWaiver(t *testing.T) {
	r := newRig(t)
	sh(t, r.board, mwBin, "session", "start", "s", "-t", "t")
	a, b, c := r.addReady("a"), r.addReady("b"), r.addReady("c")
	sh(t, r.board, mwBin, "start", a, "-t", "t", "-s", "s")
	sh(t, r.board, mwBin, "block", a, "wr: session ended without done", "-t", "t", "-s", "s")
	sh(t, r.board, mwBin, "start", b, "-t", "t", "-s", "s")
	sh(t, r.board, mwBin, "block", b, "need a decision", "-t", "t", "-s", "s")
	sh(t, r.board, mwBin, "start", c, "-t", "t", "-s", "s")
	sh(t, r.board, mwBin, "done", c, "ok", "--no-evidence", "pure refactor", "-t", "t", "-s", "s")
	m := newMW(r.board)
	its, err := m.listItems("t")
	if err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{Cfg: &Config{Pause: Pause{Unjudged: 50, OnBlocked: true, QualityGuard: true}}, MW: m}
	rs, trip, warns := d.pauseReasons(its, nil, false, true)
	if len(rs) != 1 || strings.Contains(rs[0], a) || !strings.Contains(rs[0], b) {
		t.Fatalf("blocked reasons: %v", rs)
	}
	if trip != "" || len(warns) != 1 || warns[0] != c {
		t.Fatalf("waiver must warn only: trip %q warns %v", trip, warns)
	}
}

// blankNote empties an item's note in the board file, as a session that skipped its evidence would.
func blankNote(t *testing.T, board, id string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(board, ".miniwaves", "*.json"))
	hit := false
	for _, p := range files {
		if strings.HasSuffix(p, ".archive.json") {
			continue
		}
		b, _ := os.ReadFile(p)
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		items, _ := doc["items"].([]any)
		for _, it := range items {
			if m := it.(map[string]any); m["id"] == id {
				m["note"], m["notes"], hit = "", nil, true
			}
		}
		if hit {
			nb, _ := json.Marshal(doc)
			if err := os.WriteFile(p, nb, 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("item not found")
}

func TestQualityGuardPausesUntilStart(t *testing.T) {
	r := newRig(t)
	d, buf := r.disp()
	s := newServer(d)
	r.tick(s, buf) // baseline: no done items
	id := r.addReady("a")
	sh(t, r.board, mwBin, "session", "start", "h", "-t", "t")
	sh(t, r.board, mwBin, "start", id, "-t", "t", "-s", "h")
	sh(t, r.board, mwBin, "done", id, "checked", "--no-evidence", "x", "-t", "t", "-s", "h")
	blankNote(t, r.board, id)
	r.addReady("b")
	out := r.tick(s, buf)
	if len(r.runs()) != 0 || !strings.Contains(out, "quality guard") || !fileExists(pausePath(r.home, "t")) {
		t.Fatalf("guard did not trip: %q", out)
	}
	if out := r.tick(s, buf); !strings.Contains(out, "(wr start -t t)") || len(r.runs()) != 0 {
		t.Fatalf("pause must persist: %q", out)
	}
	var st bytes.Buffer
	if err := run([]string{"status"}, &st); err != nil || !strings.Contains(st.String(), "paused: t - quality guard") {
		t.Fatalf("status lacks the pause: %v %q", err, st.String())
	}
	var o bytes.Buffer
	run([]string{"start", "-t", "t"}, &o)
	r.tick(s, buf)
	if len(r.runs()) != 1 {
		t.Fatalf("not dispatched after wr start -t t\n%s", buf.String())
	}
}

func TestCooldownAfterNonOkRun(t *testing.T) {
	r := newRig(t)
	r.cfg.Pause.OnBlocked = false // the abandoned item ends blocked; isolate the cooldown
	r.addReady("a")
	r.addReady("b")
	d, buf := r.disp()
	t.Setenv("FAKE", "nodone")
	s := newServer(d)
	r.tick(s, buf)
	if len(r.runs()) != 1 || r.runs()[0].Outcome != "incomplete" {
		t.Fatalf("setup: %+v", r.runs())
	}
	if out := r.tick(s, buf); len(r.runs()) != 1 || !strings.Contains(out, "cooldown") {
		t.Fatalf("no cooldown: %q", out)
	}
	r.tick(s, buf)
	if len(r.runs()) != 2 {
		t.Fatal("cooldown must last exactly one tick")
	}
}

func TestServeMaxAttemptsSkipPrinted(t *testing.T) {
	r := newRig(t)
	r.cfg.Pause.OnBlocked = false
	r.addReady("a")
	d, buf := r.disp()
	t.Setenv("FAKE", "nodone")
	s := newServer(d)
	for i := 0; i < 2; i++ {
		r.tick(s, buf)
		sh(t, r.board, mwBin, "ready", "TSK-001", "-t", "t") // re-ready after wr blocked it
		s.cooldown = map[string]bool{}
	}
	if out := r.tick(s, buf); !strings.Contains(out, "retries: TSK-001 failed 2 times") {
		t.Fatalf("%q", out)
	}
}

func TestStatusShowsCapsPausedAndStop(t *testing.T) {
	r := newRig(t)
	sh(t, r.board, mwBin, "block", r.addReady("a"), "x", "-t", "t")
	var o bytes.Buffer
	run([]string{"stop", "-t", "t"}, &o)
	var out bytes.Buffer
	if err := run([]string{"status", "-j"}, &out); err != nil {
		t.Fatal(err)
	}
	var doc statusDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Paused) != 1 || !strings.Contains(doc.Paused[0].Reason, "blocked") || !strings.Contains(doc.Paused[0].Reason, "STOP.t") {
		t.Fatalf("paused: %+v", doc.Paused)
	}
	if len(doc.Stop) != 1 || doc.Caps.PerHour != 4 || len(doc.Tenants) != 1 {
		t.Fatalf("%+v", doc)
	}
}

func TestTenantOrderFollowsConfigFile(t *testing.T) {
	r := newRig(t)
	cfg := `{"version":2,"tenants":{"zed":{"enabled":true},"alpha":{"enabled":true},"mid":{}}}`
	if err := os.WriteFile(configPath(r.home), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(r.home)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.tenantsInOrder(), ","); got != "zed,alpha,mid" {
		t.Fatalf("order %s", got)
	}
}

func TestServiceFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WR_SERVICE_DIR", dir)
	home := filepath.Join(t.TempDir(), "wrhome")
	t.Setenv("PATH", "/opt/marker/bin:/usr/bin")
	called := false
	runner := func(string, ...string) error { called = true; return nil }
	var out bytes.Buffer
	if err := serviceCommand(&out, nil, home, "darwin", []string{"install"}, runner); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "ro.nfnt.wr.plist"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"<string>ro.nfnt.wr</string>", "<string>serve</string>", "<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>", "<key>PATH</key><string>", "/opt/marker/bin:/usr/bin", home + "/serve.out", home + "/serve.err", "<key>WR_HOME</key>"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("plist lacks %q:\n%s", w, b)
		}
	}
	if err := serviceCommand(&out, nil, home, "linux", []string{"install"}, runner); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "wr.service"))
	if !strings.Contains(string(b), " serve\n") || !strings.Contains(string(b), "WantedBy=default.target") || !strings.Contains(string(b), `ExecStart="`) || !strings.Contains(string(b), `Environment="PATH=`) || !strings.Contains(string(b), "/opt/marker/bin") {
		t.Errorf("unit:\n%s", b)
	}
	if called {
		t.Fatal("an overridden service dir must not call launchctl/systemctl")
	}
	if err := serviceCommand(&out, nil, home, "darwin", []string{"remove"}, runner); err != nil || fileExists(filepath.Join(dir, "ro.nfnt.wr.plist")) {
		t.Fatalf("remove: %v", err)
	}
	out.Reset()
	if err := serviceCommand(&out, nil, home, "windows", []string{"install"}, runner); err != nil || !strings.Contains(out.String(), "run `wr serve` by hand") {
		t.Fatalf("windows: %v %q", err, out.String())
	}
	if err := serviceCommand(&out, nil, home, "darwin", []string{"bogus"}, runner); err == nil {
		t.Fatal("bad subcommand accepted")
	}
}

func TestSystemdQuotesAndEscapesPercent(t *testing.T) {
	u := systemdUnit("/opt/100%/wr bin", "/h%ome", "/a:/b")
	if !strings.Contains(u, `ExecStart="/opt/100%%/wr bin" serve`) || !strings.Contains(u, `Environment="WR_HOME=/h%%ome"`) {
		t.Fatalf("unit:\n%s", u)
	}
}

func TestServeCtxCancelKillsDispatchAndExits(t *testing.T) {
	r := newRig(t)
	id := r.addReady("slow")
	d, buf := r.disp()
	t.Setenv("FAKE", "hang")
	ctx, cancel := context.WithCancel(context.Background())
	d.Ctx = ctx
	d.Tick = 10 * time.Millisecond
	time.AfterFunc(1500*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() {
		done <- newServer(d).serveLoop(0, func(x time.Duration) {
			select {
			case <-time.After(x):
			case <-ctx.Done():
			}
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not exit after cancel")
	}
	runs := r.runs()
	if len(runs) != 1 || runs[0].Outcome != "incomplete" || runs[0].End == nil {
		t.Fatalf("runs: %+v\n%s", runs, buf.String())
	}
	if st := r.state(id); st.State != "blocked" {
		t.Fatalf("item must be settled (blocked), got %s", st.State)
	}
}

func TestPauseWriteFailureKeepsBaseline(t *testing.T) {
	r := newRig(t)
	id := r.addReady("x")
	sh(t, r.board, mwBin, "session", "start", "s", "-t", "t")
	sh(t, r.board, mwBin, "start", id, "-t", "t", "-s", "s")
	sh(t, r.board, mwBin, "done", id, "verified", "-t", "t", "-s", "s")
	blankNote(t, r.board, id)
	d, buf := r.disp()
	s := newServer(d)
	r.tick(s, buf) // first look: seeds the baseline with no done items
	saveSeen(d.Home, map[string][]string{"t": {}})
	// a dangling symlink where PAUSE.t should go makes the write fail
	if err := os.Symlink(filepath.Join(d.Home, "nodir", "x"), pausePath(d.Home, "t")); err != nil {
		t.Fatal(err)
	}
	out := r.tick(s, buf)
	if !strings.Contains(out, "cannot write PAUSE") {
		t.Fatalf("out: %q", out)
	}
	if got := loadSeen(d.Home)["t"]; len(got) != 0 {
		t.Fatalf("baseline advanced past an unrecorded trip: %v", got)
	}
}

func TestDisabledTenantForgottenAndDiskFailsClosed(t *testing.T) {
	d := &Dispatcher{Cfg: &Config{Caps: Caps{DiskFreeGB: 5}}}
	if c := d.globalCap(Usage{DiskFreeGB: -1, diskErr: "boom"}); !strings.Contains(c, "failing closed") || !strings.Contains(c, "boom") {
		t.Fatalf("disk: %q", c)
	}
	r := newRig(t)
	d2, buf := r.disp()
	saveSeen(d2.Home, map[string][]string{"t": {"X"}})
	r.cfg.Tenants["t"] = TenantCfg{Engine: "fake", Enabled: false}
	r.tick(newServer(d2), buf)
	if _, ok := loadSeen(d2.Home)["t"]; ok {
		t.Fatal("disabled tenant kept in state.json")
	}
}

func TestTenantNamesValidated(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(configPath(home), []byte(`{"version":2,"tenants":{"../evil":{"engine":"x"}}}`), 0o600)
	if _, err := loadConfig(home); err == nil {
		t.Fatal("bad tenant name accepted")
	}
	var out bytes.Buffer
	if err := stopCommand(&Dispatcher{Home: home, Out: &out}, map[string]string{"tenant": "../x"}); err == nil {
		t.Fatal("stop accepted a bad tenant")
	}
	if err := startCommand(&Dispatcher{Home: home, Out: &out}, map[string]string{"tenant": "A/b"}); err == nil {
		t.Fatal("start accepted a bad tenant")
	}
}

// Gate off (mw records no evidence): an ok run with a note must not trip the guard.
// Gate on: the done item must carry e == "E", which the fake engine's proof file gives it.
func TestQualityGuardFollowsEvidenceGate(t *testing.T) {
	for _, gate := range []bool{false, true} {
		r := newRig(t)
		if gate {
			r.gateOn()
		}
		r.addReady("a")
		r.addReady("b")
		d, buf := r.disp()
		if d.MW.evidenceGate("t") != gate {
			t.Fatalf("evidenceGate != %v", gate)
		}
		s := newServer(d)
		r.tick(s, buf) // dispatches a (seeds baseline)
		r.tick(s, buf) // sees a done; must dispatch b, no trip
		if n := len(r.runs()); n != 2 || fileExists(pausePath(r.home, "t")) {
			t.Fatalf("gate=%v: %d runs, pause=%v", gate, n, fileExists(pausePath(r.home, "t")))
		}
	}
}

func TestServeOnceRejectsMaxTicks(t *testing.T) {
	var out bytes.Buffer
	t.Setenv("WR_HOME", t.TempDir())
	if err := run([]string{"serve", "--once", "--max-ticks", "2"}, &out); err == nil {
		t.Fatal("want error")
	}
}
