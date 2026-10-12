//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// openSharedRead opens p like a well-behaved reader: read, write and delete sharing allowed.
func openSharedRead(t *testing.T, p string) *os.File {
	t.Helper()
	u, _ := syscall.UTF16PtrFromString(p)
	h, err := syscall.CreateFile(u, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(h), p)
}

// MoveFileEx cannot replace a target open with FILE_SHARE_DELETE; the POSIX rename can, at
// once (no retry), and the open reader keeps the old contents.
func TestWindowsReplaceWhileSharedReaderOpen(t *testing.T) {
	dir := t.TempDir()
	p, tmp := filepath.Join(dir, "run.json"), filepath.Join(dir, "run.json.tmp")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openSharedRead(t, p)
	defer f.Close()
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(tmp, p); err != nil {
		t.Fatalf("replace over a shared-open file: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("content %q", b)
	}
	buf := make([]byte, 8)
	if n, _ := f.Read(buf); string(buf[:n]) != "old" {
		t.Fatalf("open reader sees %q, want the old contents", buf[:n])
	}
}

// With the POSIX rename unavailable (pre-1809, FAT) replaceFile falls back to MoveFileEx.
func TestWindowsReplaceFallback(t *testing.T) {
	defer posixRenameOff.Store(posixRenameOff.Load())
	posixRenameOff.Store(true)
	dir := t.TempDir()
	p, tmp := filepath.Join(dir, "x.json"), filepath.Join(dir, "x.json.tmp")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(tmp, p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("content %q", b)
	}
	if !posixUnsupported(errInvalidParameter) || posixUnsupported(syscall.ERROR_ACCESS_DENIED) {
		t.Fatal("posixUnsupported classification")
	}
}
