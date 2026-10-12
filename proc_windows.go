//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx     = kernel32.NewProc("LockFileEx")
	procDiskFree       = kernel32.NewProc("GetDiskFreeSpaceExW")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

const (
	processQueryLimited = 0x1000 // PROCESS_QUERY_LIMITED_INFORMATION
	stillActive         = 259    // STILL_ACTIVE
)

// Windows has no process groups: the engine is started normally and killGroup kills its
// process tree with `taskkill /T /F` (falling back to killing the engine alone). A child
// that outlived the engine itself is no longer reachable from its pid (documented caveat).
func setProcGroup(c *exec.Cmd) {}

func killGroup(c *exec.Cmd) {
	if c.Process != nil {
		killPgid(c.Process.Pid)
	}
}

func killPgid(pgid int) {
	if exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pgid)).Run() != nil {
		if p, err := os.FindProcess(pgid); err == nil {
			_ = p.Kill()
			_ = p.Release()
		}
	}
}

// pidAlive: the process can be opened and has not exited (an exited process whose handle
// someone still holds reports an exit code other than STILL_ACTIVE).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimited, false, uint32(pid))
	if err != nil {
		return err == syscall.ERROR_ACCESS_DENIED // exists, owned by someone else
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if syscall.GetExitCodeProcess(h, &code) != nil {
		return true
	}
	return code == stillActive
}

func groupAlive(pgid int) bool { return pidAlive(pgid) }

// flockFile takes an exclusive, non-blocking LockFileEx on one byte far past any
// text in the file (a pid is written at the start; locked bytes cannot be read).
func flockFile(f *os.File) error {
	ov := syscall.Overlapped{Offset: 1 << 30}
	const exclusiveFail = 0x2 | 0x1 // LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY
	r, _, err := procLockFileEx.Call(f.Fd(), exclusiveFail, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
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
	if r, _, e := procDiskFree.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), 0, 0); r == 0 {
		return 0, e
	}
	return float64(free) / (1 << 30), nil
}

// procStart is the process creation time (GetProcessTimes, RFC 3339 UTC); "" if unknown.
// It guards PID reuse like `ps -o lstart` does on Unix.
func procStart(pid int) string {
	h, err := syscall.OpenProcess(processQueryLimited, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)
	var c, e, k, u syscall.Filetime
	if syscall.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return ""
	}
	return time.Unix(0, c.Nanoseconds()).UTC().Format(time.RFC3339Nano)
}

// enableVT turns on ANSI escape processing (ENABLE_VIRTUAL_TERMINAL_PROCESSING) for the
// console on stdout; false when stdout is not a console or the console is too old.
func enableVT() bool {
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		return false
	}
	var mode uint32
	if syscall.GetConsoleMode(h, &mode) != nil {
		return false
	}
	const vt = 0x0004
	if mode&vt != 0 {
		return true
	}
	r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode|vt))
	return r != 0
}
