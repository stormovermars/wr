//go:build !windows

package osutil

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func SetProcGroup(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// KillGroup kills the engine's whole process group.
func KillGroup(c *exec.Cmd) {
	if c.Process != nil {
		KillPgid(c.Process.Pid)
	}
}

// KillPgid kills a whole process group by its leader's pid.
func KillPgid(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }

func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// GroupAlive reports whether any process of the group still exists.
func GroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}

func FlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func DiskFreeGB(dir string) (float64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0, err
	}
	return float64(s.Bavail) * float64(s.Bsize) / (1 << 30), nil
}

// ProcStart is the process start time as `ps -o lstart=` prints it; "" if unknown.
func ProcStart(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// EnableVT: Unix terminals speak ANSI already.
func EnableVT() bool { return true }
