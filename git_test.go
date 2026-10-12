package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitDirtyIgnoresLockTmpPidArchiveAndPage(t *testing.T) {
	dir := t.TempDir()
	sh(t, dir, "git", "init", "-q")
	sh(t, dir, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-q", "-m", "i")
	os.MkdirAll(filepath.Join(dir, "site"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".miniwaves"), 0o755)
	for _, f := range []string{".miniwaves.json", ".miniwaves.json.tmp", ".miniwaves/t.lock", ".miniwaves/t.lock.pid", ".miniwaves/t.json.tmp", ".miniwaves/t.archive.json", "site/index.html"} {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
	}
	if d, err := gitDirty(dir, filepath.Join(dir, "site", "index.html")); err != nil || d {
		t.Fatalf("mw's own files and the page must not count: dirty=%v err=%v", d, err)
	}
	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("x"), 0o644)
	if d, _ := gitDirty(dir); !d {
		t.Fatal("a real untracked file must count")
	}
	// a path with a space and a rename are parsed too
	os.Remove(filepath.Join(dir, "real.txt"))
	os.WriteFile(filepath.Join(dir, "my file.txt"), []byte("x"), 0o644)
	if d, _ := gitDirty(dir); !d {
		t.Fatal("a file with a space must count")
	}
}

func TestGitDirtyNotARepoIsClean(t *testing.T) {
	dir := t.TempDir()
	if d, err := gitDirty(dir); d || !errors.Is(err, errNotRepo) {
		t.Fatalf("dirty=%v err=%v", d, err)
	}
}

func TestOnceDispatchesWhenOnlyTheLockFileIsUntracked(t *testing.T) {
	if mwBin == "" {
		t.Skip("mw not on PATH")
	}
	board := t.TempDir()
	home := t.TempDir()
	t.Setenv("WR_HOME", home)
	t.Setenv("MW", mwBin)
	t.Setenv("MW_BOARD", board)
	sh(t, board, mwBin, "init", "b")
	sh(t, board, mwBin, "dirs", "set", "evidence", filepath.Join(board, "evidence"))
	sh(t, board, "git", "init", "-q")
	sh(t, board, "git", "add", ".miniwaves.json", ".miniwaves")
	sh(t, board, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "board")
	id := strings.Fields(sh(t, board, mwBin, "add", "task", "job", "-c", "works"))[1]
	sh(t, board, mwBin, "ready", id)
	if st := sh(t, board, "git", "status", "--porcelain"); !strings.Contains(st, ".miniwaves/") {
		t.Fatalf("setup: lock file should be untracked:\n%s", st)
	}
	cfg, err := loadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Caps.DiskFreeGB = 0
	cfg.Tenants = map[string]TenantCfg{"b": {Engine: "fake", Role: "builder", Enabled: true}}
	cfg.Engines = map[string]Engine{"fake": {Cmd: []string{fakeBin}, Usage: "claude_stream", Enabled: true}}
	if err := saveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE", "done")
	var out bytes.Buffer
	if err := run([]string{"once"}, &out); err != nil {
		t.Fatalf("once: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "-> ok") {
		t.Fatalf("a board with only its lock file untracked must dispatch:\n%s", out.String())
	}
}

func TestDoctorNotesNoGitRepo(t *testing.T) {
	r := newRig(t)
	os.RemoveAll(filepath.Join(r.tdir, ".git"))
	var out bytes.Buffer
	_ = run([]string{"doctor"}, &out)
	if !strings.Contains(out.String(), "no git repo: dirty-tree guard off") {
		t.Fatalf("doctor must say the guard is off:\n%s", out.String())
	}
}

func TestGitDirtyRenameIsParsed(t *testing.T) {
	dir := t.TempDir()
	sh(t, dir, "git", "init", "-q")
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644)
	sh(t, dir, "git", "add", "x.txt")
	sh(t, dir, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "i")
	sh(t, dir, "git", "mv", "x.txt", "y.txt")
	if d, err := gitDirty(dir); err != nil || !d {
		t.Fatalf("a staged rename must count: dirty=%v err=%v", d, err)
	}
	// a renamed-to mw file is ignored, the original path token must not be read as an entry
	sh(t, dir, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "r")
	sh(t, dir, "git", "mv", "y.txt", ".miniwaves.json")
	if d, err := gitDirty(dir); err != nil || d {
		t.Fatalf("rename into an mw file: dirty=%v err=%v", d, err)
	}
}

func TestPublishedPageMatchedExactlyFromRepoRoot(t *testing.T) {
	dir := t.TempDir()
	sh(t, dir, "git", "init", "-q")
	os.MkdirAll(filepath.Join(dir, "a", "site"), 0o755)
	os.MkdirAll(filepath.Join(dir, "site"), 0o755)
	os.WriteFile(filepath.Join(dir, "site", "index.html"), []byte("x"), 0o644)
	// the page is a/site/index.html; the untracked site/index.html is a different file
	if d, _ := gitDirty(dir, filepath.Join(dir, "a", "site", "index.html")); !d {
		t.Fatal("a same-named file elsewhere must still count")
	}
	os.Remove(filepath.Join(dir, "site", "index.html"))
	os.WriteFile(filepath.Join(dir, "a", "site", "index.html"), []byte("x"), 0o644)
	if d, _ := gitDirty(filepath.Join(dir, "a"), filepath.Join(dir, "a", "site", "index.html")); d {
		t.Fatal("the page itself must not count, also from a subdirectory")
	}
}

func TestStoryOnlyBoardIsNothingReadyAndLeavesNoRecord(t *testing.T) {
	r := newRig(t)
	id := strings.Fields(sh(t, r.board, mwBin, "add", "story", "outcome", "-t", "t", "-c", "works"))[1]
	sh(t, r.board, mwBin, "ready", id, "-t", "t")
	out := r.once("done")
	if !strings.Contains(out, "nothing ready") || len(r.runs()) != 0 {
		t.Fatalf("a story is not dispatchable:\n%s runs=%d", out, len(r.runs()))
	}
	if lastOutcome(r.home, "t") != "" {
		t.Fatal("lastOutcome changed")
	}
	if b, _ := os.ReadFile(filepath.Join(r.home, "events.log")); strings.Contains(string(b), "error") {
		t.Fatalf("error event logged:\n%s", b)
	}
}

func TestNullClaimIsNotAnErrorAndDropsTheRecord(t *testing.T) {
	r := newRig(t)
	r.addReady("job")
	d := &Dispatcher{Home: r.home, Cfg: r.cfg, MW: newMW(r.board), Out: &bytes.Buffer{}}
	if err := d.MW.checkVersion(); err != nil {
		t.Fatal(err)
	}
	ts, _ := d.tenantSetup("t")
	// another runner took the only item between plan and claim
	item, _, _ := d.plan("t")
	sh(t, r.board, mwBin, "start", item.ID, "-t", "t")
	_, err := d.dispatch(ts, item)
	if !errors.Is(err, errNothingToClaim) || len(r.runs()) != 0 {
		t.Fatalf("err=%v runs=%d", err, len(r.runs()))
	}
}

func TestGitDirtyIgnoresPageHashAndTmpAndHomePaths(t *testing.T) {
	dir := t.TempDir()
	sh(t, dir, "git", "init", "-q")
	os.MkdirAll(filepath.Join(dir, "site"), 0o755)
	page := filepath.Join(dir, "site", "index.html")
	for _, f := range []string{"site/index.html", "site/index.html.hash", "site/index.html.482913.tmp"} {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
	}
	if d, err := gitDirty(dir, page); err != nil || d {
		t.Fatalf("page, hash sidecar and temp file must not count: dirty=%v err=%v", d, err)
	}
	os.WriteFile(filepath.Join(dir, "site", "other.html.hash"), []byte("x"), 0o644)
	if d, _ := gitDirty(dir, page); !d {
		t.Fatal("another page's hash file must count")
	}
	if got := expandHome("~/x/y.html"); got == "~/x/y.html" || !filepath.IsAbs(got) {
		t.Fatalf("expandHome: %q", got)
	}
}
