package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubMW is an mw whose `version -j` prints verOut (or fails when empty).
func stubMW(t *testing.T, verOut string) *MW {
	needShell(t)
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 2\n"
	if verOut != "" {
		script = "#!/bin/sh\n[ \"$1\" = version ] && echo '" + verOut + "' && exit 0\nexit 2\n"
	}
	bin := filepath.Join(dir, "mw")
	os.WriteFile(bin, []byte(script), 0o755)
	return &MW{Bin: bin, Board: dir}
}

func TestSchemaGuard(t *testing.T) {
	m := stubMW(t, `{"version":"0.2.1","schema":8,"board_schema":8,"board":"/b/.miniwaves.json"}`)
	if err := m.checkVersion(); err != nil || m.Schema != 8 || m.Source != "mw version -j" || m.requireSchema() != nil || m.BoardFile == "" {
		t.Fatalf("0.2.1/8 must dispatch: %v %+v", err, m)
	}
	// old mw: refused with the migration pointer (an old mw drops the sync/publish keys)
	m = stubMW(t, `{"version":"0.1.0","schema":8,"board_schema":8,"board":"/b/x"}`)
	if err := m.checkVersion(); err == nil || !strings.Contains(err.Error(), "0.2.1") || !strings.Contains(err.Error(), "MIGRATION.md") {
		t.Fatalf("mw 0.1.0 must be refused: %v", err)
	}
	// old board schema (6) and newer (8): refused
	for _, s := range []string{`{"version":"0.2.1","schema":8,"board_schema":7,"board":"/b/x"}`, `{"version":"0.9.0","schema":9,"board_schema":9,"board":"/b/x"}`} {
		m = stubMW(t, s)
		if err := m.checkVersion(); err != nil {
			t.Fatal(err)
		}
		if err := m.requireSchema(); err == nil || !strings.Contains(err.Error(), "mw migrate") {
			t.Fatalf("%s must be refused: %v", s, err)
		}
	}
	// no board found: board_schema and board are null
	m = stubMW(t, `{"version":"0.2.1","schema":8,"board_schema":null,"board":null}`)
	if err := m.checkVersion(); err != nil {
		t.Fatal(err)
	}
	if err := m.requireSchema(); err == nil || !strings.Contains(err.Error(), "mw init") {
		t.Fatalf("no board must be refused with the init hint: %v", err)
	}
	// mw without `version -j`: refused
	m = stubMW(t, "")
	if err := m.checkVersion(); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("no version -j must be refused: %v", err)
	}
}

func TestOnceRefusesWrongSchemaAndStatusSaysPath(t *testing.T) {
	m := stubMW(t, `{"version":"0.2.1","schema":7,"board_schema":9,"board":"/b/x"}`)
	t.Setenv("WR_HOME", t.TempDir())
	t.Setenv("MW", m.Bin)
	t.Setenv("MW_BOARD", m.Board)
	var out bytes.Buffer
	if err := run([]string{"once"}, &out); err == nil || !strings.Contains(err.Error(), "schema 8") {
		t.Fatalf("once must refuse: %v", err)
	}
	out.Reset()
	if err := run([]string{"status"}, &out); err != nil || !strings.Contains(out.String(), "schema 9 via mw version -j") || !strings.Contains(out.String(), "WILL NOT DISPATCH") {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
}

func TestDoctor(t *testing.T) {
	m := stubMW(t, `{"version":"0.2.1","schema":8,"board_schema":8,"board":"/nowhere/.miniwaves.json"}`)
	home := t.TempDir()
	t.Setenv("WR_HOME", home)
	t.Setenv("MW", m.Bin)
	t.Setenv("MW_BOARD", m.Board)
	cfg0, err := loadConfig(home) // default config
	if err != nil {
		t.Fatal(err)
	}
	cfg0.Caps.DiskFreeGB = 0 // not the host disk
	_ = saveConfig(home, cfg0)
	var out bytes.Buffer
	err = run([]string{"doctor", "-j"}, &out)
	var doc struct {
		OK     bool
		Checks []doctorCheck
	}
	if jerr := json.Unmarshal(out.Bytes(), &doc); jerr != nil {
		t.Fatalf("json: %v\n%s", jerr, out.String())
	}
	byName := map[string]doctorCheck{}
	for _, c := range doc.Checks {
		byName[c.Name] = c
	}
	for _, n := range []string{"mw", "board", "config", "home", "lock", "disk"} {
		if !byName[n].OK {
			t.Errorf("%s should pass: %+v", n, byName[n])
		}
	}
	if (err == nil) != doc.OK {
		t.Fatalf("exit %v vs ok=%v", err, doc.OK)
	}
	// failure: no board + held lock + broken config -> exit 1 with the init hint
	m2 := stubMW(t, `{"version":"0.2.1","schema":8,"board_schema":null,"board":null}`)
	t.Setenv("MW", m2.Bin)
	unlock, _ := lockHome(home)
	defer unlock()
	os.WriteFile(configPath(home), []byte("{nope"), 0o600)
	out.Reset()
	err = run([]string{"doctor"}, &out)
	var ee *exitError
	if err == nil || !asExit(err, &ee) || ee.Code != 1 || !strings.Contains(out.String(), "FAIL board") || !strings.Contains(out.String(), "FAIL config") || strings.Contains(out.String(), "FAIL lock") || !strings.Contains(out.String(), "held (serve running)") || !strings.Contains(out.String(), "mw init") {
		t.Fatalf("want exit 1 with FAIL board + FAIL config and lock held ok: %v\n%s", err, out.String())
	}
	// wrong schema: the migrate hint
	m3 := stubMW(t, `{"version":"0.2.1","schema":8,"board_schema":7,"board":"/b/x"}`)
	t.Setenv("MW", m3.Bin)
	out.Reset()
	if err := run([]string{"doctor"}, &out); err == nil || !strings.Contains(out.String(), "mw migrate") {
		t.Fatalf("wrong schema must fail doctor with the migrate hint: %v\n%s", err, out.String())
	}
}

func asExit(err error, ee **exitError) bool {
	e, ok := err.(*exitError)
	*ee = e
	return ok
}

func TestRemovedCommandsAndFlags(t *testing.T) {
	for _, c := range []string{"role", "engines", "cost"} {
		if err := run([]string{c}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "was removed") {
			t.Fatalf("%s: %v", c, err)
		}
	}
	if err := run([]string{"once", "--dry"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("--dry: %v", err)
	}
}

func TestServeOnce(t *testing.T) {
	r := newRig(t)
	id := r.addReady("job")
	t.Setenv("FAKE", "done")
	var nxt bytes.Buffer
	if err := run([]string{"next", "-t", "t"}, &nxt); err != nil || !strings.Contains(nxt.String(), "would dispatch "+id) || len(r.runs()) != 0 {
		t.Fatalf("next: %v\n%s", err, nxt.String())
	}
	var out bytes.Buffer
	if err := run([]string{"serve", "--once"}, &out); err != nil { // returns, so it ran one tick
		t.Fatalf("serve --once: %v\n%s", err, out.String())
	}
	if len(r.runs()) != 1 || r.state(id).State != "done" {
		t.Fatalf("one tick should dispatch exactly one run: %d\n%s", len(r.runs()), out.String())
	}
}
