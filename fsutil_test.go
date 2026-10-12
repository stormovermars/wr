package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRenameRetry(t *testing.T) {
	old, ob, oon := renameFn, renameBackoff, renameRetryOn
	t.Cleanup(func() { renameFn, renameBackoff, renameRetryOn = old, ob, oon })
	renameRetryOn = true
	renameBackoff = time.Millisecond
	calls := 0
	renameFn = func(a, b string) error {
		if calls++; calls < 3 {
			return errors.New("Access is denied.")
		}
		return os.Rename(a, b)
	}
	home := t.TempDir()
	if err := writeRun(home, &Run{Name: "r1", Tenant: "t"}); err != nil || calls != 3 {
		t.Fatalf("err %v calls %d", err, calls)
	}
	calls = 0
	renameFn = func(a, b string) error { calls++; return errors.New("Access is denied.") }
	if err := saveConfig(home, &Config{}); err == nil || calls != renameTries {
		t.Fatalf("want failure after %d tries, got %v after %d", renameTries, err, calls)
	}
	calls = 0
	renameFn = func(a, b string) error { calls++; return os.Rename(a, b) }
	if err := renameRetry(filepath.Join(home, "missing"), filepath.Join(home, "x")); err == nil || calls != 1 {
		t.Fatalf("missing source retried: %v, %d", err, calls)
	}
}

func TestEngineEnvDropsMwKeys(t *testing.T) {
	base := []string{"MW_SESSION=old", "PATH=/bin", "=C:=C:\\x", "Mw_Tenant=old"}
	got := strings.Join(engineEnv(base, "MW_SESSION=new"), " ")
	if strings.Contains(got, "MW_SESSION=old") || !strings.Contains(got, "PATH=/bin") || !strings.Contains(got, "=C:=C:\\x") {
		t.Fatalf("env %q", got)
	}
	// Windows env keys are case-insensitive: Mw_Tenant is MW_TENANT there, a different key elsewhere
	if has := strings.Contains(got, "Mw_Tenant=old"); has == (runtime.GOOS == "windows") {
		t.Fatalf("case handling on %s: %q", runtime.GOOS, got)
	}
}

// Claude Code names the project folder after the cwd with every non-alphanumeric as a
// dash, so a Windows drive path C:\Users\a\my.app becomes C--Users-a-my-app.
func TestTranscriptPathWindowsCwd(t *testing.T) {
	projectsDirOverride = t.TempDir()
	t.Cleanup(func() { projectsDirOverride = "" })
	got := transcriptPath(`C:\Users\a\my.app`, "u1")
	if want := filepath.Join(projectsDirOverride, "C--Users-a-my-app", "u1.jsonl"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

// A shorter pid overwrites a longer one without truncating the locked file.
func TestLockPidOverwriteShorter(t *testing.T) {
	home := t.TempDir()
	unlock, err := lockHome(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	os.WriteFile(filepath.Join(home, "wr.lock"), []byte("123456789012\n"), 0o600)
	writeLockPid(home)
	if readLockPid(home) != os.Getpid() {
		t.Fatalf("pid %d", readLockPid(home))
	}
}

// Off Windows a failed rename is a single attempt.
func TestRenameSingleAttemptOffWindows(t *testing.T) {
	calls := 0
	old, oldOn := renameFn, renameRetryOn
	t.Cleanup(func() { renameFn, renameRetryOn = old, oldOn })
	renameRetryOn = false
	renameFn = func(a, b string) error { calls++; return errors.New("denied") }
	if err := renameRetry("a", "b"); err == nil || calls != 1 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
}
