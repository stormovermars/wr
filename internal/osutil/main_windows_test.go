//go:build windows

package osutil

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestMain lets the test binary double as a portable `sleep N` (see sleepCmd).
func TestMain(m *testing.M) {
	if s := os.Getenv("WR_TEST_SLEEP"); s != "" {
		n, _ := strconv.Atoi(s)
		time.Sleep(time.Duration(n) * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// sleepCmd re-runs this test binary as a child that sleeps secs seconds.
func sleepCmd(secs int) *exec.Cmd {
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), "WR_TEST_SLEEP="+strconv.Itoa(secs))
	return c
}
