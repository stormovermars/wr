package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLevelMW is a #!/bin/sh mw: `version -j` answers version, `gate -t T -j` answers gateOut,
// `gate level` is only logged. Calls go to calls.log.
func fakeLevelMW(t *testing.T, version, gateOut string) (m *MW, calls string) {
	t.Helper()
	needShell(t)
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\n" +
		"case \"$1 $2\" in\n" +
		"'version -j') echo '{\"version\":\"" + version + "\",\"schema\":8,\"board_schema\":8,\"board\":\"x\",\"tenants\":[{\"slug\":\"t\",\"name\":\"t\",\"dir\":\"" + dir + "\"}]}';;\n" +
		"'gate -t') echo '" + gateOut + "';;\n" +
		"'list -t') echo '{\"items\":[]}';;\n" +
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

const gateObj = `{"level":"proof","gates":[{"name":"evidence","setting":"on","effective":true,"reason":""}]}`

func TestGateLevelConfigParse(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"tenants":{"a":{"gate_level":"spec"},"b":{}}}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Tenants["a"].GateLevel != "spec" || c.Tenants["b"].GateLevel != "" {
		t.Fatalf("%+v", c.Tenants)
	}
	for _, l := range []string{"trust", "proof", "spec", "judged"} {
		if !validGateLevel(l) {
			t.Fatal(l)
		}
	}
	if validGateLevel("strict") || validGateLevel("") {
		t.Fatal("accepted junk")
	}
}

func TestParseGateShapes(t *testing.T) {
	g, err := parseGate(gateObj)
	if err != nil || g.Level != "proof" || !g.Evidence {
		t.Fatalf("%+v %v", g, err)
	}
	g, err = parseGate(`[{"name":"evidence","effective":false}]`)
	if err != nil || g.Level != "" || g.Evidence {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestApplyGateLevelDecision(t *testing.T) {
	m, calls := fakeLevelMW(t, "0.2.5", gateObj)
	// none configured, or equal: no write
	for _, w := range []string{"", "proof"} {
		if ch, _, err := m.applyGateLevel("t", w); ch || err != nil {
			t.Fatalf("want=%q changed=%v err=%v", w, ch, err)
		}
	}
	if strings.Contains(readCalls(t, calls), "gate level") {
		t.Fatal("wrote a level without need")
	}
	ch, from, err := m.applyGateLevel("t", "judged")
	if err != nil || !ch || from != "proof" {
		t.Fatalf("%v %q %v", ch, from, err)
	}
	if c := readCalls(t, calls); !strings.Contains(c, "gate level judged -t t --by wr") {
		t.Fatalf("calls:\n%s", c)
	}
	// unknown value never reaches mw
	if ch, _, _ := m.applyGateLevel("t", "bogus"); ch {
		t.Fatal("applied bogus")
	}
}

func TestApplyGateLevelOldMW(t *testing.T) {
	m, calls := fakeLevelMW(t, "0.2.4", `[{"name":"evidence","effective":true}]`)
	if ch, _, err := m.applyGateLevel("t", "judged"); ch || err != nil {
		t.Fatal(ch, err)
	}
	if m.gateLevelOf("t") != "" || strings.Contains(readCalls(t, calls), "gate") {
		t.Fatalf("old mw was asked about levels:\n%s", readCalls(t, calls))
	}
	if !m.evidenceGate("t") {
		t.Fatal("evidence gate parse")
	}
}

func TestStatusGateLevel(t *testing.T) {
	for _, c := range []struct{ ver, gate, want string }{
		{"0.2.5", gateObj, " gate proof"},
		{"0.2.4", `[{"name":"evidence","effective":true}]`, ""},
	} {
		m, _ := fakeLevelMW(t, c.ver, c.gate)
		var out bytes.Buffer
		cfg := &Config{Tenants: map[string]TenantCfg{"t": {Enabled: true, GateLevel: "spec"}}}
		d := &Dispatcher{Home: t.TempDir(), Cfg: cfg, MW: m, Out: &out}
		doc, err := statusData(d, map[string]string{})
		if err != nil {
			t.Fatal(err)
		}
		r := doc.Tenants[0]
		if (r.GateLevel != "") != (c.want != "") || r.GateLevelWanted != "spec" {
			t.Fatalf("%s: %+v", c.ver, r)
		}
		if err := statusCommand(d, map[string]string{}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), " gate ") != (c.want != "") || !strings.Contains(out.String(), c.want) {
			t.Fatalf("%s: %s", c.ver, out.String())
		}
		// status never writes the level
		if strings.Contains(strings.Join(m.Calls(), "\n"), "gate level") {
			t.Fatal("status changed the board")
		}
	}
}

func TestPlanAppliesLevelNextDoesNot(t *testing.T) {
	m, calls := fakeLevelMW(t, "0.2.5", gateObj)
	cfg := lvlCfg("spec")
	var out bytes.Buffer
	d := &Dispatcher{Home: t.TempDir(), Cfg: cfg, MW: m, Out: &out, Dry: true}
	d.plan("t")
	if strings.Contains(readCalls(t, calls), "gate level") {
		t.Fatal("dry plan wrote")
	}
	d.Dry = false
	d.plan("t")
	if !strings.Contains(readCalls(t, calls), "gate level spec -t t --by wr") || !strings.Contains(out.String(), "gate level proof -> spec") {
		t.Fatalf("calls:\n%s\nout:%s", readCalls(t, calls), out.String())
	}
}

func lvlCfg(level string) *Config {
	return &Config{
		Engines: map[string]Engine{"e": {Enabled: true, Cmd: []string{"true"}}},
		Tenants: map[string]TenantCfg{"t": {Enabled: true, Engine: "e", GateLevel: level}},
	}
}

func TestLevelNotAppliedWhenWrRefuses(t *testing.T) {
	// STOP.t present
	m, calls := fakeLevelMW(t, "0.2.5", gateObj)
	d := &Dispatcher{Home: t.TempDir(), Cfg: lvlCfg("spec"), MW: m, Out: &bytes.Buffer{}}
	os.WriteFile(stopPath(d.Home, "t"), nil, 0o600)
	d.plan("t")
	if strings.Contains(readCalls(t, calls), "gate level") {
		t.Fatal("applied under STOP.t")
	}
	// PAUSE present
	os.Remove(stopPath(d.Home, "t"))
	os.WriteFile(pausePath(d.Home, "t"), []byte("x\n"), 0o600)
	d.plan("t")
	if strings.Contains(readCalls(t, calls), "gate level") {
		t.Fatal("applied under PAUSE")
	}
	// fleet-managed
	os.Remove(pausePath(d.Home, "t"))
	fl := t.TempDir()
	os.MkdirAll(filepath.Join(fl, "state"), 0o755)
	os.WriteFile(filepath.Join(fl, "state", "auto-dispatch.json"), []byte(`{"enabled":true}`), 0o600)
	os.WriteFile(filepath.Join(fl, "apps-fleet-sessions.json"), []byte(`{"sessions":[{"tenant":"t"}]}`), 0o600)
	t.Setenv("WR_FLEET_DIR", fl)
	d.plan("t")
	if strings.Contains(readCalls(t, calls), "gate level") {
		t.Fatal("applied to a fleet-managed tenant")
	}
}

func TestLevelCaseInsensitiveAndInvalidWarnedOnce(t *testing.T) {
	m, calls := fakeLevelMW(t, "0.2.5", gateObj)
	var out bytes.Buffer
	d := &Dispatcher{Home: t.TempDir(), Cfg: lvlCfg("SPEC"), MW: m, Out: &out}
	d.plan("t")
	if !strings.Contains(readCalls(t, calls), "gate level spec -t t --by wr") {
		t.Fatalf("calls:\n%s", readCalls(t, calls))
	}
	var o2 bytes.Buffer
	d2 := &Dispatcher{Home: t.TempDir(), Cfg: lvlCfg("strictx"), MW: m, Out: &o2}
	d2.plan("t")
	d2.resetLevelTries()
	d2.plan("t")
	if strings.Count(o2.String(), "gate_level strictx unknown, ignored") != 1 {
		t.Fatalf("out: %s", o2.String())
	}
}

func TestLevelFailureOneEventOneTry(t *testing.T) {
	m, calls := fakeLevelMW(t, "0.2.5", gateObj)
	// make `gate level` fail
	b, _ := os.ReadFile(m.Bin)
	nb := strings.Replace(string(b), "case \"$1 $2\" in\n", "case \"$1 $2\" in\n'gate level') echo 'refused: for humans' >&2; exit 2;;\n", 1)
	os.WriteFile(m.Bin, []byte(nb), 0o755)
	var out bytes.Buffer
	home := t.TempDir()
	d := &Dispatcher{Home: home, Cfg: lvlCfg("spec"), MW: m, Out: &out}
	d.plan("t")
	d.plan("t")
	if n := strings.Count(readCalls(t, calls), "gate level"); n != 1 {
		t.Fatalf("%d attempts:\n%s", n, readCalls(t, calls))
	}
	d.resetLevelTries() // next serve tick: retried, same error, no second event
	d.plan("t")
	ev, _ := os.ReadFile(filepath.Join(home, "events.log"))
	if strings.Count(string(ev), "gate-level-failed") != 1 || !strings.Contains(string(ev), "refused") {
		t.Fatalf("events: %s", ev)
	}
}

func TestStatusGateDriftAndNoCallWithoutConfig(t *testing.T) {
	if got := gateText("proof", "spec"); got != " gate proof (wants spec)" {
		t.Fatal(got)
	}
	m, calls := fakeLevelMW(t, "0.2.5", gateObj)
	d := &Dispatcher{Home: t.TempDir(), Cfg: lvlCfg(""), MW: m, Out: &bytes.Buffer{}}
	if _, err := statusData(d, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readCalls(t, calls), "gate -t") {
		t.Fatalf("status asked mw gate without gate_level:\n%s", readCalls(t, calls))
	}
}
