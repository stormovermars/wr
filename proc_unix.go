//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func setProcGroup(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killGroup kills the engine's whole process group.
func killGroup(c *exec.Cmd) {
	if c.Process != nil {
		killPgid(c.Process.Pid)
	}
}

// killPgid kills a whole process group by its leader's pid.
func killPgid(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// groupAlive reports whether any process of the group still exists.
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}

func flockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func diskFreeGB(dir string) (float64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0, err
	}
	return float64(s.Bavail) * float64(s.Bsize) / (1 << 30), nil
}

// procStart is the process start time as `ps -o lstart=` prints it; "" if unknown.
func procStart(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// enableVT: Unix terminals speak ANSI already.
func enableVT() bool { return true }
