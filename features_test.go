package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failingMW: version works, every other call exits 2 (a board that cannot be read).
func failingEnv(t *testing.T) string {
	t.Helper()
	m := stubMW(t, `{"version":"0.2.1","schema":8,"board_schema":8,"board":"/b/.miniwaves.json"}`)
	tdir := filepath.Join(m.Board, "tenant")
	os.MkdirAll(tdir, 0o755)
	sh(t, tdir, "git", "init", "-q")
	home := t.TempDir()
	cfg, err := loadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Caps.DiskFreeGB = 0
	cfg.Tenants = map[string]TenantCfg{"t": {Engine: "fake", Role: "builder", Enabled: true}, "u": {Engine: "fake", Role: "builder", Enabled: true}}
	cfg.Engines = map[string]Engine{"fake": {Cmd: []string{fakeBin}, Usage: "claude_stream", Enabled: true}}
	d := tdir
	for k, v := range cfg.Tenants {
		v.Dir = &d
		cfg.Tenants[k] = v
	}
	_ = saveConfig(home, cfg)
	t.Setenv("WR_HOME", home)
	t.Setenv("MW", m.Bin)
	t.Setenv("MW_BOARD", m.Board)
	return home
}

func TestNextAndStatusExitOneWhenEveryTenantErrored(t *testing.T) {
	failingEnv(t)
	for _, cmd := range []string{"next", "status"} {
		var out bytes.Buffer
		err := run([]string{cmd}, &out)
		if err == nil || !strings.Contains(err.Error(), "every enabled tenant errored") {
			t.Fatalf("%s: want exit 1, got %v\n%s", cmd, err, out.String())
		}
	}
	// one tenant fine, one broken: not a failure
	r := newRig(t)
	r.cfg.Tenants["x"] = TenantCfg{Engine: "fake", Role: "builder", Enabled: true, Dir: ptr(filepath.Join(t.TempDir(), "nope"))}
	r.save()
	var out bytes.Buffer
	if err := run([]string{"next"}, &out); err != nil {
		t.Fatalf("partial failure must stay exit 0: %v\n%s", err, out.String())
	}
}

func ptr(s string) *string { return &s }

func TestClaimNextCarriesPromptVerbatim(t *testing.T) {
	needShell(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "mw")
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '{\"id\":\"T-1\",\"title\":\"x\",\"claimed\":true,\"prompt\":\"P line1\\nLoop: x\"}'\n"), 0o755)
	m := &MW{Bin: bin, Board: dir}
	n, err := m.claimNext("t", "x", "builder")
	if err != nil || n == nil || n.Prompt != "P line1\nLoop: x" {
		t.Fatalf("prompt must be verbatim: %+v %v", n, err)
	}
	if strings.Contains(n.Brief, "Loop: x") || !strings.Contains(n.Brief, `"id": "T-1"`) {
		t.Fatalf("brief must be the item without the prompt:\n%s", n.Brief)
	}
	if got := buildPrompt(n.Prompt, n.Brief, "T-1", "x", nil); !strings.HasPrefix(got, n.Prompt) || strings.Count(got, "Loop: x") != 1 {
		t.Fatalf("buildPrompt must keep the mw prompt verbatim, once:\n%s", got)
	}
	if got := m.Calls(); len(got) != 1 || !strings.Contains(got[0], "--prompt") {
		t.Fatalf("claim must ask for the prompt: %v", got)
	}
}

func TestGitDirtyIgnoresBoardAndWrFiles(t *testing.T) {
	dir := t.TempDir()
	sh(t, dir, "git", "init", "-q")
	sh(t, dir, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-q", "-m", "i")
	os.MkdirAll(filepath.Join(dir, "site"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".wr"), 0o755)
	for _, f := range []string{".miniwaves.json", ".miniwaves.archive.json", "site/index.html", ".wr/state"} {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
	}
	if d, err := gitDirty(dir, filepath.Join(dir, "site", "index.html")); err != nil || d {
		t.Fatalf("board files, published page and .wr/ must not count: dirty=%v err=%v", d, err)
	}
	if d, _ := gitDirty(dir); !d {
		t.Fatal("without the published path ignored, site/index.html is dirt")
	}
	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("x"), 0o644)
	if d, _ := gitDirty(dir, filepath.Join(dir, "site", "index.html")); !d {
		t.Fatal("a real change must still be dirty")
	}
}

func TestLogFallsBackToRunsAndEvents(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	if err := logCommand(&out, home, map[string]string{}); err == nil {
		t.Fatal("nothing at all: still an error")
	}
	now := time.Now().UTC()
	writeRun(home, &Run{Name: "wr-t-x-1", Tenant: "t", Item: "X-1", Start: now, End: &now, Outcome: "ok"})
	os.WriteFile(filepath.Join(home, "events.log"), []byte("2026-10-12T10:00:00\tpause\tt\treason\n"), 0o600)
	out.Reset()
	if err := logCommand(&out, home, map[string]string{}); err != nil || !strings.Contains(out.String(), "wr-t-x-1") || !strings.Contains(out.String(), "pause") {
		t.Fatalf("fallback: %v\n%s", err, out.String())
	}
}

func TestServePidInLockFile(t *testing.T) {
	home := t.TempDir()
	unlock, err := lockHome(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	writeLockPid(home)
	if readLockPid(home) != os.Getpid() {
		t.Fatalf("pid %d", readLockPid(home))
	}
	if _, err := lockHome(home); err == nil { // still locked: writing the pid did not drop the lock
		t.Fatal("second lock must fail")
	}
}
