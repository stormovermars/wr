// fakeengine stands in for claude in wr's tests; FAKE selects the behaviour.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mw(args ...string) {
	c := exec.Command(env("MW", "mw"), args...)
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	if err := c.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeengine: mw failed:", err)
		os.Exit(9)
	}
}

func main() {
	in, _ := io.ReadAll(os.Stdin) // the prompt
	if p := os.Getenv("FAKE_STDIN"); p != "" {
		_ = os.WriteFile(p, in, 0o600)
	}
	id := env("FAKE_ID", os.Getenv("WR_ITEM"))
	tenant := os.Getenv("WR_TENANT")
	if n, _ := strconv.Atoi(os.Getenv("FAKE_NOISE")); n > 0 {
		fmt.Print(strings.Repeat("x", n))
	}
	if n, _ := strconv.Atoi(os.Getenv("FAKE_SLEEP_MS")); n > 0 {
		time.Sleep(time.Duration(n) * time.Millisecond)
	}
	switch os.Getenv("FAKE") {
	case "done":
		evidence(tenant, id)
		mw("done", id, "fake ok") // no -t/-s, no cwd override: MW_SESSION/MW_TENANT + board discovery from cwd
		fmt.Println(`{"type":"system"}`)
		fmt.Println(`{"type":"result","total_cost_usd":0.25,"usage":{"input_tokens":10,"output_tokens":5}}`)
	case "block":
		mw("block", id, "fake blocked")
	case "reopen":
		mw("reopen", id, "-t", tenant)
	case "crash":
		os.Exit(3)
	case "hang":
		c := exec.Command("sleep", "60")
		_ = c.Start()
		if p := os.Getenv("FAKE_PIDFILE"); p != "" {
			_ = os.WriteFile(p, []byte(fmt.Sprintf("%d %d", os.Getpid(), c.Process.Pid)), 0o600)
		}
		time.Sleep(60 * time.Second)
	case "nodone":
	}
}

// evidence writes a proof file where schema-6 boards expect it (<evidence>/<tenant>/<ID>/),
// so the quality guard sees a properly finished item. Boards without "dirs" skip it.
func evidence(tenant, id string) {
	out, err := exec.Command(env("MW", "mw"), "dirs", "-j").Output()
	if err != nil {
		return
	}
	var d map[string]string
	if json.Unmarshal(out, &d) != nil || d["evidence"] == "" {
		return
	}
	dir := filepath.Join(d["evidence"], tenant, id)
	if os.MkdirAll(dir, 0o755) == nil {
		_ = os.WriteFile(filepath.Join(dir, "proof.txt"), []byte("fake proof\n"), 0o644)
	}
}
