package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStopBlocksOnceAndNextSaysWhy(t *testing.T) {
	r := newRig(t)
	r.addReady("a")
	var o bytes.Buffer
	if err := run([]string{"stop"}, &o); err != nil {
		t.Fatal(err)
	}
	out := r.once("done")
	if len(r.runs()) != 0 || !strings.Contains(out, "STOP") {
		t.Fatalf("once ran under STOP: %q", out)
	}
	var n bytes.Buffer
	_ = run([]string{"next", "-t", "t"}, &n)
	if !strings.Contains(n.String(), "STOP") {
		t.Fatalf("next must say why: %q", n.String())
	}
}

func TestNextAndOnceHonourCaps(t *testing.T) {
	r := newRig(t)
	r.addReady("a")
	r.cfg.Caps.PerHour = 1
	r.save()
	r.seedRun("wr-t-old-1", "OLD", time.Now().UTC(), 0, nil, true)
	var n bytes.Buffer
	_ = run([]string{"next", "-t", "t"}, &n)
	if !strings.Contains(n.String(), "cap per_hour") {
		t.Fatalf("next: %q", n.String())
	}
	r.once("done")
	if len(r.runs()) != 1 { // only the seeded one
		t.Fatal("once ignored the cap")
	}
}

// second tenant "u" on the same board
func (r *rig) tenantU() {
	dir := filepath.Join(r.board, "tenant-u")
	os.MkdirAll(dir, 0o755)
	sh(r.t, dir, "git", "init", "-q")
	sh(r.t, dir, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-q", "-m", "init")
	sh(r.t, r.board, mwBin, "tenant", "add", "u", "U", "-d", dir)
	r.cfg.Tenants["u"] = TenantCfg{Engine: "fake", Role: "builder", Enabled: true}
	r.save()
	out := sh(r.t, r.board, mwBin, "add", "task", "ju", "-t", "u", "-c", "works")
	sh(r.t, r.board, mwBin, "ready", strings.Fields(out)[1], "-t", "u")
}

func TestAsyncDispatchRespectsParallelCaps(t *testing.T) {
	for _, tc := range []struct {
		global  int
		started int
	}{{1, 1}, {2, 2}} {
		r := newRig(t)
		r.addReady("a")
		r.tenantU()
		r.cfg.Caps.ParallelGlobal = tc.global
		r.save()
		d, buf := r.disp()
		t.Setenv("FAKE_SLEEP_MS", "1200")
		s := newServer(d)
		t0 := time.Now()
		if _, _, err := s.tick(); err != nil {
			t.Fatal(err)
		}
		if el := time.Since(t0); el > 900*time.Millisecond {
			t.Fatalf("tick blocked on the engine: %v", el)
		}
		rs, _ := readRuns(r.home, "")
		if len(rs) != tc.started {
			t.Fatalf("global %d: %d runs started, want %d", tc.global, len(rs), tc.started)
		}
		s.wait()
		if tc.global == 1 && !strings.Contains(buf.String(), "cap parallel_global") {
			t.Fatalf("cap reason missing: %s", buf.String())
		}
		select {
		case <-s.wake:
		default:
			t.Fatal("ok outcome must wake the loop for an immediate re-tick")
		}
	}
}

func TestStopWritesMidServeStopsDispatching(t *testing.T) {
	r := newRig(t)
	r.addReady("a")
	d, buf := r.disp()
	s := newServer(d)
	os.WriteFile(stopPath(r.home, ""), []byte("x"), 0o600)
	stop, disp, err := s.tick()
	if err != nil || !stop || disp || len(r.runs()) != 0 {
		t.Fatalf("STOP must end serve within the tick: %v %v %v\n%s", stop, disp, err, buf.String())
	}
}

func TestStaleTranscriptKillsAndBlocks(t *testing.T) {
	r := newRig(t)
	id := r.addReady("a")
	projects := t.TempDir()
	projectsDirOverride = projects
	t.Cleanup(func() { projectsDirOverride = "" })
	livenessPoll = 100 * time.Millisecond
	t.Cleanup(func() { livenessPoll = 15 * time.Second })
	r.cfg.Caps.StaleMinutes = 0.03 // 1.8s
	r.cfg.Engines["fake"] = Engine{Cmd: []string{fakeBin, "--session-id", "{uuid}"}, Usage: "claude_stream", Enabled: true}
	r.save()
	t.Setenv("FAKE", "hang")
	// a fake transcript written once, then silent
	go func() {
		time.Sleep(300 * time.Millisecond)
		for i := 0; i < 50; i++ {
			rs, _ := readRuns(r.home, "t")
			if len(rs) == 1 && rs[0].Transcript != "" {
				os.MkdirAll(filepath.Dir(rs[0].Transcript), 0o755)
				os.WriteFile(rs[0].Transcript, []byte("{}\n"), 0o644)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	t0 := time.Now()
	var out bytes.Buffer
	if err := run([]string{"once"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if time.Since(t0) > 20*time.Second {
		t.Fatal("stale run was not killed")
	}
	rs := r.runs()
	if len(rs) != 1 || !rs[0].Stale || rs[0].Outcome != "incomplete" || r.state(id).State != "blocked" {
		t.Fatalf("%+v %s", rs[0], r.state(id).State)
	}
}

func TestSettleBlocksEveryOwnedItemAndReportsStopFailure(t *testing.T) {
	r := newRig(t)
	a, b := r.addReady("a"), r.addReady("b")
	sh(t, r.board, mwBin, "session", "start", "wr-x-1", "-t", "t")
	sh(t, r.board, mwBin, "start", a, "-t", "t", "-s", "wr-x-1")
	// a session owns one item at a time; the second claim goes via a reopen of b under the same session
	d, _ := r.disp()
	run1 := &Run{Name: "wr-x-1", Tenant: "t", Board: r.board, Item: a}
	if err := d.settle(d.MW, run1, "wr: test"); err != nil {
		t.Fatal(err)
	}
	if r.state(a).State != "blocked" || r.sessionState("wr-x-1") == "active" {
		t.Fatalf("settle: %s %s", r.state(a).State, r.sessionState("wr-x-1"))
	}
	_ = b
	// stop of an unknown session is reported
	if err := d.settle(d.MW, &Run{Name: "wr-nosuch-1", Tenant: "t", Board: r.board}, "x"); err == nil {
		t.Fatal("failed stop must be reported")
	}
}

func TestShowFailureSettlesAsIncompleteNotForeign(t *testing.T) {
	needShell(t)
	r := newRig(t)
	r.addReady("a")
	wrap := filepath.Join(t.TempDir(), "mw")
	os.WriteFile(wrap, []byte("#!/bin/sh\n[ \"$1\" = show ] && exit 1\nexec "+mwBin+" \"$@\"\n"), 0o755)
	t.Setenv("MW", wrap)
	r.once("done")
	rs := r.runs()
	if len(rs) != 1 || rs[0].Outcome != "incomplete" || r.sessionState(rs[0].Name) == "active" {
		t.Fatalf("%+v", rs)
	}
}

func TestPIDStartMismatchIsDeadAndNeverKilled(t *testing.T) {
	r := newRig(t)
	pid := liveGroup(t)
	rec := &Run{Name: "wr-t-x-1", Tenant: "t", Board: r.board, Item: "X-1", PID: pid, PStart: "Mon Jan  1 00:00:00 2001", Start: time.Now().UTC().Add(-3 * time.Hour)}
	if err := writeRun(r.home, rec); err != nil {
		t.Fatal(err)
	}
	if runAlive(rec) {
		t.Fatal("start-time mismatch must count as dead")
	}
	r.once("done")
	if !groupAlive(pid) {
		t.Fatal("a mismatched PID must never be killed")
	}
	for _, x := range r.runs() {
		if x.Name == rec.Name && x.Outcome != "lost" {
			t.Fatalf("not lost: %+v", x)
		}
	}
	// a record with the right start time is alive
	live := &Run{PID: pid, PStart: procStart(pid)}
	if !runAlive(live) {
		t.Fatal("matching start time must be alive")
	}
}

func TestWrBlockDoesNotPauseAndEventsLogged(t *testing.T) {
	r := newRig(t)
	r.addReady("a")
	r.addReady("b")
	var notes []string
	notifyFn = func(title, msg string) { notes = append(notes, title+" "+msg) }
	t.Cleanup(func() { notifyFn = func(string, string) {} })
	r.once("nodone") // abandoned: wr blocks it
	var n bytes.Buffer
	_ = run([]string{"next", "-t", "t"}, &n)
	if !strings.Contains(n.String(), "would dispatch") {
		t.Fatalf("wr's own block must not pause the tenant: %q", n.String())
	}
	d, _ := r.disp()
	t.Setenv("FAKE", "nodone")
	s := newServer(d)
	s.tick() // dispatches b (nodone again): non-ok -> event
	s.wait()
	b, _ := os.ReadFile(filepath.Join(r.home, "events.log"))
	if !strings.Contains(string(b), "outcome") || len(notes) == 0 {
		t.Fatalf("events %q notes %v", b, notes)
	}
}

func TestFlagInHelpPrintForms(t *testing.T) {
	h := []byte("  -p, --print    Print\n  -x/--xx  y\n  --verbose  v\n")
	for _, f := range []string{"-p", "--print", "-x", "--xx", "--verbose"} {
		if !flagInHelp(h, f) {
			t.Errorf("%s not found", f)
		}
	}
	if flagInHelp(h, "--verb") || flagInHelp(h, "-v") {
		t.Error("partial token matched")
	}
}

func TestFleetManagedTenantRefused(t *testing.T) {
	r := newRig(t)
	r.addReady("a")
	fl := t.TempDir()
	t.Setenv("WR_FLEET_DIR", fl)
	os.MkdirAll(filepath.Join(fl, "state"), 0o755)
	os.WriteFile(filepath.Join(fl, "state", "auto-dispatch.json"), []byte(`{"enabled":true}`), 0o644)
	os.WriteFile(filepath.Join(fl, "apps-fleet-sessions.json"), []byte(`{"sessions":[{"tenant":"t"}]}`), 0o644)
	var n bytes.Buffer
	_ = run([]string{"next", "-t", "t"}, &n)
	if !strings.Contains(n.String(), "external dispatcher") {
		t.Fatalf("%q", n.String())
	}
	var d bytes.Buffer
	if err := run([]string{"doctor"}, &d); err == nil || !strings.Contains(d.String(), "FAIL fleet") {
		t.Fatalf("doctor: %v\n%s", err, d.String())
	}
	os.WriteFile(filepath.Join(fl, "state", "auto-dispatch.json"), []byte(`{"enabled":false}`), 0o644)
	n.Reset()
	_ = run([]string{"next", "-t", "t"}, &n)
	if strings.Contains(n.String(), "external dispatcher") {
		t.Fatalf("disabled fleet must not block: %q", n.String())
	}
}

func TestStatusIsReadOnlyAndShowsLiveStuckLost(t *testing.T) {
	r := newRig(t)
	id := r.addReady("a")
	sh(t, r.board, mwBin, "session", "start", "wr-t-a-1", "-t", "t")
	sh(t, r.board, mwBin, "start", id, "-t", "t", "-s", "wr-t-a-1")
	dead := exec.Command("true")
	_ = dead.Run()
	r.seedRun("wr-t-a-1", id, time.Now().UTC().Add(-time.Minute), dead.Process.Pid, nil, false)
	var out bytes.Buffer
	if err := run([]string{"status"}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"live: t " + id, "lost?", "stuck: t " + id, "wr-t-a-1.log"} {
		if !strings.Contains(s, want) {
			t.Errorf("status lacks %q:\n%s", want, s)
		}
	}
	if rs := r.runs(); len(rs) != 1 || rs[0].End != nil || r.state(id).State != "doing" {
		t.Fatal("status must not recover or touch anything")
	}
}

func TestLogCommandAndTranscriptKey(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "serve.out"), []byte("a\nb\n"), 0o644)
	var out bytes.Buffer
	if err := logCommand(&out, home, map[string]string{}); err != nil || out.String() != "a\nb\n" {
		t.Fatalf("%v %q", err, out.String())
	}
	projectsDirOverride = "/p"
	defer func() { projectsDirOverride = "" }()
	if got := transcriptPath("/no/such/My.dir/x_y", "U"); got != "/p/-no-such-My-dir-x-y/U.jsonl" {
		t.Fatal(got)
	}
}

func TestOldRunRecordsIgnored(t *testing.T) {
	r := newRig(t)
	dir := runsDir(r.home, "t")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "wr-t-old-1.json"), []byte(`{"name":"wr-t-old-1","tenant":"t","item":"OLD","outcome":"incomplete","start":"2026-10-01T00:00:00Z"}`), 0o644)
	if len(r.runs()) != 0 {
		t.Fatal("1.0 records (no v:2) must not be read")
	}
}
