//go:build windows

package osutil

import (
	"os"
	"testing"
	"time"
)

func TestWindowsPidAliveAndStart(t *testing.T) {
	if !PidAlive(os.Getpid()) {
		t.Fatal("own pid not alive")
	}
	s := ProcStart(os.Getpid())
	if s == "" || s != ProcStart(os.Getpid()) {
		t.Fatalf("ProcStart unstable: %q", s)
	}
	c := sleepCmd(30)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	if !GroupAlive(pid) {
		t.Fatal("child not alive")
	}
	KillGroup(c)
	_ = c.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for PidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if PidAlive(pid) {
		t.Fatal("killed child still alive")
	}
}

func TestWindowsEnableVTDoesNotPanic(t *testing.T) { _ = EnableVT() }
