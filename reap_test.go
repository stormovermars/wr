package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeMW writes a #!/bin/sh mw that logs every call to calls.log and answers
// `version -j` with the given version, `session reap` with reapOut and `session stop` with stopOut.
func fakeMW(t *testing.T, version, reapOut, stopOut string) (m *MW, calls string) {
	t.Helper()
	needShell(t)
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\n" +
		"case \"$1 $2\" in\n" +
		"'version -j') echo '{\"version\":\"" + version + "\",\"schema\":8,\"board_schema\":8,\"board\":\"x\"}';;\n" +
		"'session reap') echo '" + reapOut + "';;\n" +
		"'session stop') printf '%s' '" + stopOut + "';;\n" +
		"'list -s') echo '{\"items\":[]}';;\n" +
		"esac\n"
	bin := filepath.Join(dir, "mw")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m = &MW{Bin: bin, Board: dir}
	if err := m.checkVersion(); err != nil {
		t.Fatal(err)
	}
	return m, calls
}

func readCalls(t *testing.T, p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestSettleReleasesSessionOnNewMW(t *testing.T) {
	m, calls := fakeMW(t, "0.2.3", `{"reaped":[]}`, "stopped session s\n  released TSK-001\n")
	home := t.TempDir()
	var out bytes.Buffer
	d := &Dispatcher{Home: home, Cfg: &Config{}, MW: m, Out: &out}
	if err := d.settle(m, &Run{Name: "wr-t-1-aaaaaa", Tenant: "t"}, "wr: stuck"); err != nil {
		t.Fatal(err)
	}
	if c := readCalls(t, calls); !strings.Contains(c, "session stop wr-t-1-aaaaaa -t t --release") {
		t.Fatalf("calls:\n%s", c)
	}
	ev, _ := os.ReadFile(filepath.Join(home, "events.log"))
	if !strings.Contains(string(ev), "\treleased\tt\t") || !strings.Contains(string(ev), "TSK-001") {
		t.Fatalf("events:\n%s", ev)
	}
}

func TestSettleFallsBackOnOldMW(t *testing.T) {
	m, calls := fakeMW(t, "0.2.1", `{}`, "stopped session s\n")
	home := t.TempDir()
	d := &Dispatcher{Home: home, Cfg: &Config{}, MW: m, Out: &bytes.Buffer{}}
	if err := d.settle(m, &Run{Name: "wr-t-1-aaaaaa", Tenant: "t"}, "wr: stuck"); err != nil {
		t.Fatal(err)
	}
	c := readCalls(t, calls)
	if strings.Contains(c, "--release") || !strings.Contains(c, "session stop wr-t-1-aaaaaa -t t") {
		t.Fatalf("calls:\n%s", c)
	}
	if ev, _ := os.ReadFile(filepath.Join(home, "events.log")); strings.Contains(string(ev), "released") {
		t.Fatalf("released event on old mw:\n%s", ev)
	}
}

func TestReapTickPerTickAndSkippedOnOldMW(t *testing.T) {
	m, calls := fakeMW(t, "0.2.3", `{"reaped":[{"session":"dead-1","tenant":"t","items":["TSK-009"]}]}`, "")
	home := t.TempDir()
	var out bytes.Buffer
	cfg := &Config{Reap: &ReapCfg{OlderThan: "90m"}, Tenants: map[string]TenantCfg{"t": {Enabled: true}}}
	d := &Dispatcher{Home: home, Cfg: cfg, MW: m, Out: &out, Now: time.Now}
	d.reapTick()
	d.reapTick()
	c := readCalls(t, calls)
	if n := strings.Count(c, "session reap -t t --older-than 90m -j"); n != 2 {
		t.Fatalf("reap calls %d, want 2:\n%s", n, c)
	}
	ev, _ := os.ReadFile(filepath.Join(home, "events.log"))
	if !strings.Contains(string(ev), "\treaped\tt\tdead-1 released TSK-009") {
		t.Fatalf("events:\n%s", ev)
	}
	st := readReapState(home)
	if st == nil || len(st.Reaped) != 1 || st.OlderThan != "90m" {
		t.Fatalf("state %+v", st)
	}
	// default older_than and the disabled switch
	d.Cfg = &Config{Tenants: map[string]TenantCfg{"t": {Enabled: true}}}
	d.reapTick()
	if !strings.Contains(readCalls(t, calls), "--older-than 4h -j") {
		t.Fatal("default 4h not used")
	}
	off := false
	d.Cfg = &Config{Reap: &ReapCfg{Enabled: &off}}
	before := readCalls(t, calls)
	d.reapTick()
	if readCalls(t, calls) != before {
		t.Fatal("reap ran while disabled")
	}
	// mw older than 0.2.3: no call at all
	old, ocalls := fakeMW(t, "0.2.1", `{}`, "")
	d2 := &Dispatcher{Home: t.TempDir(), Cfg: &Config{}, MW: old, Out: &bytes.Buffer{}, Now: time.Now}
	d2.reapTick()
	if strings.Contains(readCalls(t, ocalls), "reap") {
		t.Fatalf("reap called on mw 0.2.1:\n%s", readCalls(t, ocalls))
	}
	if readReapState(d2.Home) != nil {
		t.Fatal("state written on old mw")
	}
}

func TestReapConfigMatrixRow(t *testing.T) {
	on, off := true, false
	rows := []struct {
		cfg     *ReapCfg
		enabled bool
		older   string
	}{
		{nil, true, "4h"},
		{&ReapCfg{}, true, "4h"},
		{&ReapCfg{Enabled: &on, OlderThan: "30m"}, true, "30m"},
		{&ReapCfg{Enabled: &off, OlderThan: "1d"}, false, "1d"},
		{&ReapCfg{OlderThan: "banana"}, true, "4h"},
		{&ReapCfg{OlderThan: "-1h"}, true, "4h"},
		{&ReapCfg{OlderThan: "1e9d"}, true, "4h"},
		{&ReapCfg{OlderThan: "NaNd"}, true, "4h"},
		{&ReapCfg{OlderThan: "Infd"}, true, "4h"},
		{&ReapCfg{OlderThan: "366d"}, true, "4h"},
	}
	for _, r := range rows {
		c := &Config{Reap: r.cfg}
		if c.reapEnabled() != r.enabled || c.reapOlder() != r.older {
			t.Errorf("%+v: enabled=%v older=%q", r.cfg, c.reapEnabled(), c.reapOlder())
		}
	}
}

func TestStatusShowsLastReap(t *testing.T) {
	m, _ := fakeMW(t, "0.2.3", `{"reaped":[{"session":"dead-1","tenant":"t","items":["TSK-009"]}]}`, "")
	home := t.TempDir()
	d := &Dispatcher{Home: home, Cfg: &Config{Tenants: map[string]TenantCfg{"t": {Enabled: true}}}, MW: m, Out: &bytes.Buffer{}, Now: time.Now}
	d.reapTick()
	d.Cfg = &Config{}
	var out bytes.Buffer
	d.Out = &out
	doc, err := statusData(d, map[string]string{})
	if err != nil || doc.Reap == nil || len(doc.Reap.Reaped) != 1 {
		t.Fatalf("%v %+v", err, doc.Reap)
	}
	if err := statusCommand(d, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "reap: ") || !strings.Contains(out.String(), "dead-1 (t) released TSK-009") {
		t.Fatalf("status:\n%s", out.String())
	}
}

func enabledCfg(tenants ...string) *Config {
	c := &Config{Tenants: map[string]TenantCfg{}}
	for _, t := range tenants {
		c.Tenants[t] = TenantCfg{Enabled: true}
	}
	return c
}

func TestReapOnlyEnabledTenantsAndSkipsLiveRuns(t *testing.T) {
	m, calls := fakeMW(t, "0.2.3", `{"reaped":[]}`, "")
	home := t.TempDir()
	cfg := enabledCfg("a", "b")
	cfg.Tenants["off"] = TenantCfg{Enabled: false}
	d := &Dispatcher{Home: home, Cfg: cfg, MW: m, Out: &bytes.Buffer{}, Now: time.Now}
	setInflight("wr-a-1-live", true)
	defer setInflight("wr-a-1-live", false)
	if err := writeRun(home, &Run{Name: "wr-a-1-live", Tenant: "a", Board: m.Board, Item: "TSK-1"}); err != nil {
		t.Fatal(err)
	}
	d.reapTick()
	c := readCalls(t, calls)
	if strings.Contains(c, "-A") || strings.Contains(c, "-t off") {
		t.Fatalf("reaped more than the enabled tenants:\n%s", c)
	}
	for _, tn := range []string{"a", "b"} {
		if !strings.Contains(c, "session reap -t "+tn+" --older-than 4h --except wr-a-1-live -j") {
			t.Fatalf("no skip of the live run for %s:\n%s", tn, c)
		}
	}
	if _, err := os.Stat(reapPath(home) + ".tmp"); err == nil {
		t.Fatal("reap.json.tmp left behind")
	}
}

func TestReapOlderClampedToRunTimeoutFloor(t *testing.T) {
	m, calls := fakeMW(t, "0.2.3", `{"reaped":[]}`, "")
	home := t.TempDir()
	cfg := enabledCfg("a")
	cfg.Reap = &ReapCfg{OlderThan: "30m"}
	cfg.Caps.RunTimeoutMin = 180
	cfg.Version = 2
	var out bytes.Buffer
	d := &Dispatcher{Home: home, Cfg: cfg, MW: m, Out: &out, Now: time.Now}
	d.reapTick()
	d.reapTick()
	// 180m + 20m (default stale) + 5m
	if n := strings.Count(readCalls(t, calls), "--older-than 205m"); n != 2 {
		t.Fatalf("clamp not applied:\n%s", readCalls(t, calls))
	}
	if n := strings.Count(out.String(), "using 205m"); n != 1 {
		t.Fatalf("clamp logged %d times:\n%s", n, out.String())
	}
	var dout bytes.Buffer
	cfgHome := t.TempDir()
	if err := saveConfig(cfgHome, cfg); err != nil {
		t.Fatal(err)
	}
	_ = doctorCommand(&dout, cfgHome, map[string]string{})
	if !strings.Contains(dout.String(), "WARNING: older_than 30m") {
		t.Fatalf("doctor:\n%s", dout.String())
	}
}

func TestReapReleasedEventOnlyWithItems(t *testing.T) {
	m, _ := fakeMW(t, "0.2.3", `{"reaped":[]}`, "stopped session s\n")
	home := t.TempDir()
	d := &Dispatcher{Home: home, Cfg: &Config{}, MW: m, Out: &bytes.Buffer{}}
	if err := d.settle(m, &Run{Name: "wr-t-1-aaaaaa", Tenant: "t"}, "x"); err != nil {
		t.Fatal(err)
	}
	if ev, _ := os.ReadFile(filepath.Join(home, "events.log")); strings.Contains(string(ev), "released") {
		t.Fatalf("released event with 0 items:\n%s", ev)
	}
}

func TestStopFallsBackWhenMWRejectsRelease(t *testing.T) {
	needShell(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\n" +
		"case \"$1 $2\" in\n" +
		"'version -j') echo '{\"version\":\"0.2.3\",\"schema\":8,\"board_schema\":8,\"board\":\"x\"}';;\n" +
		"'session stop') case \"$*\" in *--release*) echo 'mw: unknown option --release' >&2; exit 1;; *) echo 'stopped session s';; esac;;\n" +
		"esac\n"
	bin := filepath.Join(dir, "mw")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &MW{Bin: bin, Board: dir}
	if err := m.checkVersion(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.sessionStopRelease("t", "s"); err != nil {
		t.Fatal(err)
	}
	if c := readCalls(t, calls); strings.Count(c, "session stop") != 2 {
		t.Fatalf("calls:\n%s", c)
	}
}

func TestParseReapAgeLimits(t *testing.T) {
	for _, bad := range []string{"1e9d", "NaNd", "Infd", "-1d", "0d", "366d", "9999h", "0s", ""} {
		if validReapAge(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"90m", "2h", "1d", "365d", "0.5d"} {
		if !validReapAge(good) {
			t.Errorf("%q rejected", good)
		}
	}
}
