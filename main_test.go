package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

var fakeBin, mwBin string

func TestMain(m *testing.M) {
	if s := os.Getenv("WR_TEST_SLEEP"); s != "" { // sleepCmd: a portable `sleep N`
		n, _ := strconv.Atoi(s)
		time.Sleep(time.Duration(n) * time.Second)
		os.Exit(0)
	}
	releaseAPI = "http://127.0.0.1:1/latest" // tests never reach GitHub (doctor's update check stays quiet)
	dir, err := os.MkdirTemp("", "wr-fake")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "fakeengine")
	if runtime.GOOS == "windows" {
		fakeBin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", fakeBin, "./testdata/fakeengine").CombinedOutput(); err != nil {
		panic(string(out))
	}
	mwBin, _ = exec.LookPath("mw")
	if mwBin == "" {
		if os.Getenv("CI") != "" {
			fmt.Fprintln(os.Stderr, "wr tests: mw not on PATH and CI is set; install mw >= "+MinMW)
			os.Exit(1)
		}
	} else {
		out, _ := exec.Command(mwBin, "version", "-j").Output()
		var v struct{ Version string }
		_ = json.Unmarshal(out, &v)
		if v.Version == "" || semverLess(v.Version, MinMW) {
			fmt.Fprintf(os.Stderr, "wr tests: need mw >= %s, found %q at %s\n", MinMW, v.Version, mwBin)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "wr tests: using mw %s at %s\n", v.Version, mwBin)
	}
	os.Setenv("WR_FLEET_DIR", filepath.Join(dir, "no-fleet"))
	notifyFn = func(string, string) {}
	retryDelay = 0
	followPoll = 10 * time.Millisecond
	uiRefresh = 50 * time.Millisecond
	engineWaitDelay = time.Second
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type rig struct {
	t     *testing.T
	home  string
	board string
	tdir  string
	cfg   *Config
}

func sh(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// newRig builds a temp WR_HOME, an mw board with tenant "t", and a clean git tenant dir.
func newRig(t *testing.T) *rig {
	t.Helper()
	if mwBin == "" {
		t.Skip("mw not on PATH")
	}
	board := t.TempDir()
	r := &rig{t: t, home: t.TempDir(), board: board, tdir: filepath.Join(board, "tenant")}
	if err := os.MkdirAll(r.tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WR_HOME", r.home)
	t.Setenv("MW", mwBin)
	t.Setenv("MW_BOARD", r.board)
	sh(t, r.tdir, "git", "init", "-q")
	sh(t, r.tdir, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-q", "-m", "init")
	sh(t, r.board, mwBin, "init", "b")
	sh(t, r.board, mwBin, "tenant", "add", "t", "T", "-d", r.tdir)
	sh(t, r.board, mwBin, "dirs", "set", "evidence", filepath.Join(r.board, "evidence")) // keep proof inside the temp board
	cfg, err := loadConfig(r.home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Caps.DiskFreeGB = 0 // tests must not depend on the host disk
	cfg.Tenants = map[string]TenantCfg{"t": {Engine: "fake", Role: "builder", Forbid: []string{"serial-1", "serial-2"}, Enabled: true}}
	cfg.Engines = map[string]Engine{"fake": {Cmd: []string{fakeBin}, Usage: "claude_stream", Enabled: true}}
	r.cfg = cfg
	r.save()
	return r
}

// gateOn turns the evidence gate on for tenant t, as schema-6 boards run with: done then
// records evidence (the fake engine writes proof) and the quality guard can pass.
func (r *rig) gateOn() {
	sh(r.t, r.board, mwBin, "gate", "evidence", "on", "--by", "wr-test", "-t", "t")
}

func (r *rig) save() {
	if err := saveConfig(r.home, r.cfg); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) addReady(title string) string {
	out := sh(r.t, r.board, mwBin, "add", "task", title, "-t", "t", "-c", "works")
	id := strings.Fields(out)[1]
	sh(r.t, r.board, mwBin, "ready", id, "-t", "t")
	return id
}

func (r *rig) state(id string) ItemInfo {
	it, err := newMW(r.board).show("t", id)
	if err != nil {
		r.t.Fatal(err)
	}
	return *it
}

func (r *rig) sessionState(name string) string {
	var ss []struct{ Session, State string }
	_ = json.Unmarshal([]byte(sh(r.t, r.board, mwBin, "session", "list", "-j", "-v")), &ss)
	for _, s := range ss {
		if s.Session == name {
			return s.State
		}
	}
	return ""
}

func (r *rig) once(fake string, extra ...string) string {
	r.t.Helper()
	r.t.Setenv("FAKE", fake)
	var out bytes.Buffer
	if err := run(append([]string{"once"}, extra...), &out); err != nil {
		r.t.Fatalf("once: %v\n%s", err, out.String())
	}
	return out.String()
}

func (r *rig) runs() []*Run {
	rs, _ := readRuns(r.home, "t")
	return rs
}

func TestOutcomes(t *testing.T) {
	cases := []struct{ fake, outcome, state string }{
		{"done", "ok", "done"},
		{"block", "blocked", "blocked"},
		{"nodone", "incomplete", "blocked"}, // wr blocks the abandoned item
		{"crash", "incomplete", "blocked"},
		{"reopen", "foreign", ""},
	}
	for _, c := range cases {
		t.Run(c.fake, func(t *testing.T) {
			r := newRig(t)
			id := r.addReady("job")
			out := r.once(c.fake)
			rs := r.runs()
			if len(rs) != 1 || rs[0].Outcome != c.outcome {
				t.Fatalf("want outcome %s, got %+v\n%s", c.outcome, rs, out)
			}
			if c.state != "" && r.state(id).State != c.state {
				t.Fatalf("item state %s, want %s", r.state(id).State, c.state)
			}
			if c.fake == "crash" && (rs[0].Exit == nil || *rs[0].Exit != 3) {
				t.Fatalf("exit not recorded: %+v", rs[0].Exit)
			}
			if c.fake == "done" && (rs[0].Cost == nil || *rs[0].Cost != 0.25) {
				t.Fatalf("cost not parsed: %+v", rs[0].Cost)
			}
			if st := r.sessionState(rs[0].Name); st != "stopped" {
				t.Fatalf("session %s state %q, want stopped", rs[0].Name, st)
			}
		})
	}
}

func TestNothingReadyIsIdle(t *testing.T) {
	r := newRig(t)
	out := r.once("done")
	if !strings.Contains(out, "idle") || len(r.runs()) != 0 {
		t.Fatalf("want idle and no runs, got %q", out)
	}
	var nb bytes.Buffer
	if err := run([]string{"next"}, &nb); err != nil || !strings.Contains(nb.String(), "nothing ready") {
		t.Fatalf("next: %v %q", err, nb.String())
	}
}

func TestPromptAndRecords(t *testing.T) {
	r := newRig(t)
	id := r.addReady("job")
	stdin := filepath.Join(t.TempDir(), "stdin")
	t.Setenv("FAKE_STDIN", stdin)
	r.once("done")
	b, _ := os.ReadFile(stdin)
	p := string(b)
	for _, want := range []string{"Work only " + id, "Headless run", "Your item (mw next -j brief)", `"id": "` + id + `"`, forbidPrefix + "serial-1, serial-2"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	for _, bad := range []string{"Loop:", "mw session start '", "ping me"} {
		if strings.Contains(p, bad) {
			t.Errorf("prompt still has %q", bad)
		}
	}
	rs := r.runs()
	if len(rs) != 1 {
		t.Fatalf("runs: %d", len(rs))
	}
	x := rs[0]
	if x.PID == 0 || x.UUID == "" || x.CmdHash == "" || x.Item != id || x.Engine != "fake" || x.End == nil || x.Exit == nil || x.Outcome != "ok" {
		t.Fatalf("incomplete record: %+v", x)
	}
	if !regexp.MustCompile(`^wr-t-1-[0-9a-f]{6}$`).MatchString(x.Name) {
		t.Fatalf("name %s", x.Name)
	}
}

func TestRecordWrittenAtStartAndHangKilled(t *testing.T) {
	r := newRig(t)
	r.cfg.Caps.RunTimeoutMin = 2.0 / 60
	r.save()
	r.addReady("job")
	pidfile := filepath.Join(t.TempDir(), "pids")
	t.Setenv("FAKE_PIDFILE", pidfile)
	t.Setenv("FAKE", "hang")
	done := make(chan string, 1)
	go func() {
		var out bytes.Buffer
		_ = run([]string{"once"}, &out)
		done <- out.String()
	}()
	// while the engine hangs, the record exists with a pid and no end
	deadline := time.Now().Add(1500 * time.Millisecond)
	seen := false
	for time.Now().Before(deadline) && !seen {
		for _, x := range r.runs() {
			if x.PID > 0 && x.End == nil {
				seen = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !seen {
		t.Fatal("no start record (pid, no end) while engine ran")
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("timeout did not end the run")
	}
	rs := r.runs()
	if len(rs) != 1 || rs[0].End == nil || !rs[0].Timeout || rs[0].Outcome != "incomplete" {
		t.Fatalf("after timeout: %+v", rs)
	}
	b, _ := os.ReadFile(pidfile)
	f := strings.Fields(string(b))
	if len(f) != 2 {
		t.Fatalf("pidfile %q", b)
	}
	eng, _ := strconv.Atoi(f[0])
	child, _ := strconv.Atoi(f[1])
	time.Sleep(200 * time.Millisecond)
	if pidAlive(eng) || pidAlive(child) || groupAlive(eng) {
		t.Fatalf("process group survived: engine %d child %d", eng, child)
	}
}

func TestLostRecovery(t *testing.T) {
	r := newRig(t)
	id := r.addReady("job")
	name := runName("t", 1, "aaaaaa")
	sh(t, r.board, mwBin, "session", "start", name, "-t", "t", "-a", "builder")
	sh(t, r.board, mwBin, "start", id, "-t", "t", "-s", name)
	dead := exec.Command("true")
	_ = dead.Run() // reaped: its pid is dead
	rec := &Run{Name: name, Tenant: "t", Board: r.board, Item: id, Engine: "fake", PID: dead.Process.Pid, Start: time.Now().UTC()}
	if err := writeRun(r.home, rec); err != nil {
		t.Fatal(err)
	}
	// a live-PID run is left alone
	lp := sleepCmd(30)
	setProcGroup(lp)
	if err := lp.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { killGroup(lp); _ = lp.Wait() }()
	live := &Run{Name: "wr-t-live-1", Tenant: "t", Board: r.board, Item: "X-1", PID: lp.Process.Pid, Start: time.Now().UTC()}
	_ = writeRun(r.home, live)
	out := r.once("done")
	if !strings.Contains(out, "lost") {
		t.Fatalf("no lost report:\n%s", out)
	}
	for _, x := range r.runs() {
		switch x.Name {
		case name:
			if x.Outcome != "lost" || x.End == nil {
				t.Fatalf("not marked lost: %+v", x)
			}
		case live.Name:
			if x.End != nil {
				t.Fatalf("live run touched: %+v", x)
			}
		}
	}
	if r.state(id).State != "blocked" {
		t.Fatalf("lost item not blocked: %+v", r.state(id))
	}
	if st := r.sessionState(name); st != "stopped" {
		t.Fatalf("lost session state %q, want stopped", st)
	}
}

func TestRetryAttemptCounterAndLimit(t *testing.T) {
	r := newRig(t)
	id := r.addReady("job")
	r.once("nodone")
	for n := 2; n <= 2; n++ {
		sh(t, r.board, mwBin, "reopen", id, "-t", "t")
		sh(t, r.board, mwBin, "ready", id, "-t", "t")
		r.once("nodone")
	}
	rs := r.runs()
	if len(rs) != 2 || !strings.Contains(rs[0].Name, "-1-") || !strings.Contains(rs[1].Name, "-2-") {
		t.Fatalf("attempt counter: %+v", rs)
	}
	sh(t, r.board, mwBin, "reopen", id, "-t", "t")
	sh(t, r.board, mwBin, "ready", id, "-t", "t")
	out := r.once("done")
	if !strings.Contains(out, "retries") || len(r.runs()) != 2 {
		t.Fatalf("retry limit not applied:\n%s", out)
	}
}

func TestNameTruncationToValidID(t *testing.T) {
	long := strings.Repeat("Abc_", 30)
	n := runName(long, 12, "0a1b2c")
	if len(n) > 64 || !slugPattern.MatchString(n) || !strings.HasSuffix(n, "-12-0a1b2c") || !strings.HasPrefix(n, "wr-abc-abc") {
		t.Fatalf("bad name %q (%d)", n, len(n))
	}
	if got := runName("web", 1, "0a1b2c"); got != "wr-web-1-0a1b2c" {
		t.Fatalf("got %q", got)
	}
	// truncation landing on a dash must not leave a double or trailing dash
	for k := 1; k < 70; k++ {
		nm := runName(strings.Repeat("a-", k), 1, shortRand())
		if len(nm) > 64 || !slugPattern.MatchString(nm) {
			t.Fatalf("k=%d invalid %q", k, nm)
		}
	}
	if shortRand() == shortRand() {
		t.Fatal("random tail repeats")
	}
}

func TestConfigDefaultWriteAndRoundTrip(t *testing.T) {
	home := filepath.Join(t.TempDir(), "wrhome")
	c, err := loadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(configPath(home))
	if string(b) != defaultConfig {
		t.Fatal("default config was not written verbatim")
	}
	if c.Caps.PerHour != 4 || c.Caps.USDPerDay["claude"] != 20 || c.Engines["claude"].Cmd[2] != "--permission-mode" ||
		c.Tenants["example"].Enabled || c.Tenants["example"].Dir != nil || len(c.Tenants["example"].Forbid) != 0 ||
		strings.Contains(string(b), "serial-") {
		t.Fatalf("default parsed wrong: %+v", c)
	}
	if err := saveConfig(home, c); err != nil {
		t.Fatal(err)
	}
	c2, err := loadConfig(home)
	if err != nil || !reflect.DeepEqual(c, c2) {
		t.Fatalf("round trip differs: %v", err)
	}
	var raw map[string]any
	b, _ = os.ReadFile(configPath(home))
	if json.Unmarshal(b, &raw) != nil || raw["version"].(float64) != 2 {
		t.Fatal("saved config not valid json")
	}
}

func TestLogSizeCap(t *testing.T) {
	r := newRig(t)
	r.addReady("job")
	old := logCap
	logCap = 100
	defer func() { logCap = old }()
	t.Setenv("FAKE_NOISE", "5000")
	r.once("nodone")
	rs := r.runs()
	fi, err := os.Stat(filepath.Join(runsDir(r.home, "t"), rs[0].Name+".log"))
	if err != nil || fi.Size() > 100+80 {
		t.Fatalf("log not capped: %v %v", fi, err)
	}
}

func TestDisabledTenantAndExpandArgv(t *testing.T) {
	r := newRig(t)
	r.addReady("job")
	tc := r.cfg.Tenants["t"]
	tc.Enabled = false
	r.cfg.Tenants["t"] = tc
	r.save()
	out := r.once("done")
	if !strings.Contains(out, "not enabled") || len(r.runs()) != 0 {
		t.Fatalf("disabled tenant dispatched:\n%s", out)
	}
	got := expandArgv([]string{"--session-id", "{uuid}", "$HOME;{usd}", "{name}/{id}"}, "U", "5", "N", "I")
	if !reflect.DeepEqual(got, []string{"--session-id", "U", "$HOME;5", "N/I"}) {
		t.Fatalf("argv %v", got)
	}
}

func TestCrashBetweenClaimAndRecordIsRecovered(t *testing.T) {
	r := newRig(t)
	id := r.addReady("job")
	name := runName("t", 1, "bbbbbb")
	sh(t, r.board, mwBin, "session", "start", name, "-t", "t", "-a", "builder")
	sh(t, r.board, mwBin, "start", id, "-t", "t", "-s", name)
	// what dispatch leaves behind if wr dies right after the claim: PID 0, starting
	if err := writeRun(r.home, &Run{Name: name, Tenant: "t", Board: r.board, Item: id, Engine: "fake", Phase: "starting", Start: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	r.once("done")
	rs := r.runs()
	if len(rs) < 1 || rs[0].Name != name || rs[0].Outcome != "lost" || rs[0].End == nil {
		t.Fatalf("not recovered: %+v", rs)
	}
	if r.state(id).State != "blocked" || r.sessionState(name) != "stopped" {
		t.Fatalf("item %+v session %q", r.state(id), r.sessionState(name))
	}
}

func TestTenantOutsideBoardRefusedAndOnceFails(t *testing.T) {
	r := newRig(t)
	r.addReady("job")
	out := t.TempDir()
	tc := r.cfg.Tenants["t"]
	tc.Dir = &out
	r.cfg.Tenants["t"] = tc
	r.save()
	t.Setenv("FAKE", "done")
	var b bytes.Buffer
	err := run([]string{"once"}, &b)
	if err == nil || !strings.Contains(b.String(), "tenant dir outside board") || len(r.runs()) != 0 {
		t.Fatalf("err %v out %q runs %d", err, b.String(), len(r.runs()))
	}
}

func TestDirtyTreeWithNoPriorRunSkips(t *testing.T) {
	r := newRig(t)
	r.addReady("job")
	if err := os.WriteFile(filepath.Join(r.tdir, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := r.once("done")
	if !strings.Contains(out, "dirty tree") || len(r.runs()) != 0 {
		t.Fatalf("dirty tree dispatched: %q", out)
	}
}

func TestFlagInHelpWholeToken(t *testing.T) {
	help := []byte("  --output-format <f>  x\n  --foobar  y\n")
	if !flagInHelp(help, "--output-format") || flagInHelp(help, "--foo") || !flagInHelp(help, "--foobar") {
		t.Fatal("token match wrong")
	}
}

func TestEngineEnvDropsMWBoard(t *testing.T) {
	got := engineEnv([]string{"A=1", "MW_BOARD=/x", "MW_SESSION=old", "MW_TENANT=old"}, "MW_SESSION=n", "MW_TENANT=t")
	if !reflect.DeepEqual(got, []string{"A=1", "MW_SESSION=n", "MW_TENANT=t"}) {
		t.Fatalf("%v", got)
	}
}

func TestOverdueLiveGroupIsKilledAndLost(t *testing.T) {
	r := newRig(t)
	lp := sleepCmd(30)
	setProcGroup(lp)
	if err := lp.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { killGroup(lp); _ = lp.Wait() }()
	old := time.Now().UTC().Add(-3 * time.Hour)
	_ = writeRun(r.home, &Run{Name: "wr-t-old-1", Tenant: "t", Board: r.board, Item: "X-1", PID: lp.Process.Pid, Start: old})
	r.once("done")
	rs := r.runs()
	if len(rs) != 1 || rs[0].Outcome != "lost" {
		t.Fatalf("overdue not lost: %+v", rs)
	}
	_ = lp.Wait()
	if groupAlive(lp.Process.Pid) {
		t.Fatal("overdue group survived")
	}
}

// sleepCmd is `sleep secs` as a child process on every OS (Windows has no sleep binary):
// the test binary re-executed in WR_TEST_SLEEP mode.
func sleepCmd(secs int) *exec.Cmd {
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), "WR_TEST_SLEEP="+strconv.Itoa(secs))
	return c
}

// needShell skips a test whose fake mw is a #!/bin/sh script: Windows cannot exec one.
// Those tests cover schema, doctor and pause logic that has no OS-specific code.
func needShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake mw is a #!/bin/sh script; Windows cannot exec it (logic is OS-independent, covered on macOS/Linux)")
	}
}
