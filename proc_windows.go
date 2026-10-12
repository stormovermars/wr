//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// Windows: job objects are not implemented; the lock is LockFileEx.
func setProcGroup(c *exec.Cmd) {}

func killGroup(c *exec.Cmd) {
	if c.Process != nil {
		_ = c.Process.Kill()
	}
}

func killPgid(pgid int) {
	if p, err := os.FindProcess(pgid); err == nil {
		_ = p.Kill()
	}
}

func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p != nil && pid > 0
}

func groupAlive(pgid int) bool { return pidAlive(pgid) }

// flockFile takes an exclusive, non-blocking LockFileEx on one byte far past any
// text in the file (a pid is written at the start; locked bytes cannot be read).
func flockFile(f *os.File) error {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	ov := syscall.Overlapped{Offset: 1 << 30}
	const exclusiveFail = 0x2 | 0x1 // LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY
	r, _, err := proc.Call(f.Fd(), exclusiveFail, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	if r == 0 {
		return err
	}
	return nil // closing the file releases it
}

func diskFreeGB(dir string) (float64, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free uint64
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	if r, _, e := proc.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), 0, 0); r == 0 {
		return 0, e
	}
	return float64(free) / (1 << 30), nil
}

func procStart(pid int) string { return "" }
