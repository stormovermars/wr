//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsPidAliveAndStart(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Fatal("own pid not alive")
	}
	s := procStart(os.Getpid())
	if s == "" || s != procStart(os.Getpid()) {
		t.Fatalf("procStart unstable: %q", s)
	}
	c := sleepCmd(30)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	if !groupAlive(pid) {
		t.Fatal("child not alive")
	}
	killGroup(c)
	_ = c.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatal("killed child still alive")
	}
}

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

func TestWindowsEnableVTDoesNotPanic(t *testing.T) { _ = enableVT() }
