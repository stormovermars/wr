package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetryLock(t *testing.T) {
	old := lockRetryDelay
	lockRetryDelay = time.Millisecond
	defer func() { lockRetryDelay = old }()
	locked := errors.New("mw next --claim: mw: board locked (pid 7) — retry")
	n := 0
	out, err := retryLock(func() (string, error) {
		if n++; n < 3 {
			return "", locked
		}
		return "ok", nil
	})
	if err != nil || out != "ok" || n != 3 {
		t.Fatalf("two timeouts then ok: %q %v after %d calls", out, err, n)
	}
	n = 0
	if _, err := retryLock(func() (string, error) { n++; return "", locked }); !isLockTimeout(err) || n != 1+lockRetries {
		t.Fatalf("always locked: %v after %d calls, want %d", err, n, 1+lockRetries)
	}
	n = 0
	if _, err := retryLock(func() (string, error) { n++; return "", errors.New("mw next: tenant x not found") }); err == nil || n != 1 {
		t.Fatalf("other errors are not retried: %v after %d calls", err, n)
	}
	// the 0.2.2 wording still matches
	if !isLockTimeout(errors.New("mw: board locked (pid 9): lock wait timed out after 15s — retry, or raise MW_LOCK_WAIT (seconds)")) {
		t.Fatal("0.2.2 lock timeout not recognised")
	}
}

// claimNext repeats a claim that hit mw's lock wait timeout and counts each timeout in LockWaits.
func TestClaimNextRetriesLockTimeout(t *testing.T) {
	needShell(t)
	old := lockRetryDelay
	lockRetryDelay = time.Millisecond
	defer func() { lockRetryDelay = old }()
	dir := t.TempDir()
	cnt := filepath.Join(dir, "n")
	script := "#!/bin/sh\necho x >> '" + cnt + "'\n" +
		"[ $(wc -l < '" + cnt + "') -lt 3 ] && echo 'mw: board locked (pid 1) — retry' >&2 && exit 1\n" +
		"echo '{\"id\":\"TSK-001\",\"title\":\"T\",\"prompt\":\"P\"}'\n"
	bin := filepath.Join(dir, "mw")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &MW{Bin: bin, Board: dir}
	it, err := m.claimNext("t", "s", "builder")
	if err != nil || it == nil || it.ID != "TSK-001" {
		t.Fatalf("claim after two lock timeouts: %+v %v", it, err)
	}
	if m.LockWaits != 2 {
		t.Fatalf("LockWaits %d, want 2", m.LockWaits)
	}
}

// A failing mw with an empty stderr still yields a reason (its exit code).
func TestMustNamesExitCodeWithoutStderr(t *testing.T) {
	needShell(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "mw")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := (&MW{Bin: bin, Board: dir}).must("list", "-j")
	if err == nil || !strings.Contains(err.Error(), "exit code 5") {
		t.Fatalf("got %v", err)
	}
}

// sessionStop repeats a stop that failed transiently (a Windows rename refused while another
// runner reads the tenant file), so a settled run never leaves its session idle; a refusal is final.
func TestSessionStopRetriesTransientFailure(t *testing.T) {
	needShell(t)
	old := retryDelay
	retryDelay = time.Millisecond
	defer func() { retryDelay = old }()
	dir := t.TempDir()
	cnt := filepath.Join(dir, "n")
	script := "#!/bin/sh\necho x >> '" + cnt + "'\n" +
		"[ $(wc -l < '" + cnt + "') -lt 3 ] && echo 'mw: rename t.json.1.tmp t.json: Access is denied.' >&2 && exit 1\n" +
		"echo 'stopped session s'\n"
	bin := filepath.Join(dir, "mw")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &MW{Bin: bin, Board: dir}
	if err := m.sessionStop("t", "s"); err != nil {
		t.Fatalf("stop after two transient failures: %v", err)
	}
	if b, _ := os.ReadFile(cnt); strings.Count(string(b), "x") != 3 {
		t.Fatalf("calls %q, want 3", b)
	}
	refuse := filepath.Join(dir, "mw2")
	os.Remove(cnt)
	if err := os.WriteFile(refuse, []byte("#!/bin/sh\necho x >> '"+cnt+"'\necho 'mw: session s still owns TSK-001; block, finish, or reopen it first' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.Bin = refuse
	if err := m.sessionStop("t", "s"); err == nil {
		t.Fatal("a refused stop must fail")
	}
	if b, _ := os.ReadFile(cnt); strings.Count(string(b), "x") != 1 {
		t.Fatalf("refusal retried: calls %q", b)
	}
}
