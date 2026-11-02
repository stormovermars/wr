//go:build windows

package wr

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsRenameOverOpenFile(t *testing.T) {
	home := t.TempDir()
	if err := writeRun(home, &Run{Name: "r", Tenant: "t"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(runsDir(home, "t"), "r.json"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(50 * time.Millisecond); f.Close() }()
	if err := writeRun(home, &Run{Name: "r", Tenant: "t", Item: "X"}); err != nil {
		t.Fatalf("rename over an open run record: %v", err)
	}
}
