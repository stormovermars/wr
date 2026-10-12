package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const version = "0.2.0"

var flagAliases = map[string]string{"t": "tenant", "n": "count", "j": "json", "b": "board"}
var valueFlags = map[string]bool{"tenant": true, "count": true, "board": true, "show": true, "max-ticks": true, "last": true, "addr": true, "export": true}
var boolFlags = map[string]bool{"json": true, "once": true, "f": true, "web": true, "open": true}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "wr:", err)
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.Code)
		}
		os.Exit(1)
	}
}

// options splits args into flags and positionals (mw-style: -t T, -j, --long).
func options(args []string) (map[string]string, []string, error) {
	o := map[string]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		k := strings.TrimLeft(a, "-")
		if full, ok := flagAliases[k]; ok && !strings.HasPrefix(a, "--") {
			k = full
		}
		if boolFlags[k] {
			o[k] = "true"
			continue
		}
		if !valueFlags[k] {
			return nil, nil, fmt.Errorf("unknown flag %s", a)
		}
		if i+1 >= len(args) {
			return nil, nil, fmt.Errorf("flag %s needs a value", a)
		}
		i++
		o[k] = args[i]
	}
	return o, pos, nil
}

func usage(out io.Writer, topic string) error {
	help := map[string]string{
		"next":    "wr next [-t TENANT] [-b BOARD_DIR]\nShow the item wr would dispatch per tenant, or why not (config, fleet, STOP/PAUSE, pause rules, caps, dirty tree, retries). Read-only.",
		"once":    "wr once [-t TENANT] [-n COUNT] [-b BOARD_DIR]\nDispatch up to COUNT items (default 1) per enabled tenant, under the same gates as next: session, claim, engine, outcome by board.",
		"runs":    "wr runs [-t TENANT] [-j] [--show NAME]\nList run records, or print one run and its engine log tail.",
		"status":  "wr status [-j] [--last N]\nRead-only: live runs (age, transcript age, log), last N outcomes (default 5), stuck items, per-tenant skip/pause reasons, caps, cost by day. Never recovers or kills; shows lost? for dead runs.",
		"ui":      "wr ui [--once] [--export PATH] [--web [--addr 127.0.0.1:5052] [--open]]\n--export PATH writes the page with its data inlined and exits.\nRead-only dashboard: live runs, tenant columns (counts, doing/blocked/ready/inbox items, pause reason), activity feed. Default is the terminal view (redraws every 3 s, q or Ctrl-C quits); --once prints one frame. --web (or config \"ui\": {\"web\": true, \"addr\": \"...\"}) also serves a page on loopback; without it no socket is opened.",
		"log":     "wr log [-f]\nTail ~/.wr/serve.out (last 50 lines); -f follows. Without serve.out: the last runs and events.log.",
		"doctor":  "wr doctor [-j]\nChecks: mw >= 0.2.0, a board with schema 8 (from mw version -j), config, claude --help flags per engine, the optional WR_FLEET_DIR overlap check, no-git-repo notes, WR_HOME writable, lock, disk floor. Exit 1 on any failure.",
		"version": "wr version [-j]",
		"serve":   "wr serve [--once] [--max-ticks N] [-b BOARD_DIR]\nEach tick starts dispatches in the background under the caps (parallel_global, parallel_tenant, per_hour...), then returns; an ok run triggers an immediate re-tick. Exits at the next tick when ~/.wr/STOP exists (running engines finish first). Holds ~/.wr/wr.lock.",
		"stop":    "wr stop [-t TENANT]\nWrite ~/.wr/STOP (serve exits) or ~/.wr/STOP.<tenant> (that tenant skipped).",
		"start":   "wr start [-t TENANT]\nRemove the STOP file; with -t also clear a quality-guard pause for that tenant.",
		"service": "wr service install|remove\nWrite (and load) ~/Library/LaunchAgents/ro.nfnt.wr.plist on macOS or ~/.config/systemd/user/wr.service on linux, running `wr serve`. Other OS: run wr serve by hand.",
	}
	if t, ok := help[topic]; ok {
		fmt.Fprintln(out, t)
		return nil
	}
	fmt.Fprintln(out, `wr - wave rider: runs sessions that burn an mw board down

  wr next | once | runs | status | ui | log | doctor | version
  wr serve | stop | start | service install|remove
  wr help [COMMAND]

Needs mw >= 0.2.0 (schema 8). Config ~/.wr/config.json (WR_HOME overrides ~/.wr). Board dir: -b, MW_BOARD, or cwd.
STOP files: ~/.wr/STOP (global), ~/.wr/STOP.<tenant>.`)
	return nil
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return usage(out, "")
	}
	cmd, rest := args[0], args[1:]
	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		t := ""
		if len(rest) > 0 {
			t = rest[0]
		}
		return usage(out, t)
	}
	for _, a := range rest {
		if a == "--help" || a == "-h" {
			return usage(out, cmd)
		}
	}
	switch cmd {
	case "next", "once", "runs", "status", "log", "version", "serve", "stop", "start", "service", "doctor", "ui":
	case "role", "engines", "cost":
		return fmt.Errorf("%q was removed (see USAGE.md)", cmd)
	default:
		return fmt.Errorf("unknown command %q (try wr help)", cmd)
	}
	o, pos, err := options(rest)
	if err != nil {
		return err
	}
	if len(pos) > 0 && cmd != "service" {
		return fmt.Errorf("unexpected argument %q", pos[0])
	}
	if cmd == "version" {
		if o["json"] == "true" {
			return printJSON(out, map[string]string{"version": version})
		}
		fmt.Fprintln(out, "wr", version)
		return nil
	}
	home, err := wrHome()
	if err != nil {
		return err
	}
	if cmd == "doctor" {
		return doctorCommand(out, home, o)
	}
	cfg, err := loadConfig(home)
	if err != nil {
		return err
	}
	switch cmd {
	case "log":
		return logCommand(out, home, o)
	case "service":
		return serviceCommand(out, cfg, home, runtime.GOOS, pos, func(name string, args ...string) error {
			return exec.Command(name, args...).Run()
		})
	case "stop":
		return stopCommand(&Dispatcher{Home: home, Out: out}, o)
	case "start":
		return startCommand(&Dispatcher{Home: home, Out: out}, o)
	case "runs":
		return runsCommand(out, home, o)
	}
	m := newMW(o["board"])
	if err := m.checkVersion(); err != nil {
		return err
	}
	d := &Dispatcher{Home: home, Cfg: cfg, MW: m, Out: out}
	if cmd == "next" || cmd == "once" || cmd == "serve" {
		if err := m.requireSchema(); err != nil {
			return err
		}
	}
	switch cmd {
	case "next":
		return nextCommand(d, o)
	case "once":
		return onceCommand(d, o)
	case "status":
		return statusCommand(d, o)
	case "ui":
		return uiCommand(d, o)
	case "serve":
		return serveCommand(d, o)
	}
	return nil
}

func printJSON(out io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		fmt.Fprintln(out, string(b))
	}
	return err
}

// tenantList: -t TENANT or every tenant in config, sorted.
func tenantList(cfg *Config, o map[string]string) []string {
	if t := o["tenant"]; t != "" {
		return []string{t}
	}
	var ts []string
	for t := range cfg.Tenants {
		ts = append(ts, t)
	}
	sort.Strings(ts)
	return ts
}

// allErrored: every enabled tenant in ts failed (errs counts them); wr next and
// wr status exit 1 then, so a broken mw or board is not a green run.
func allErrored(d *Dispatcher, ts []string, errs int) error {
	enabled := 0
	for _, t := range ts {
		if d.Cfg.Tenants[t].Enabled {
			enabled++
		}
	}
	if enabled > 0 && errs >= enabled {
		return errors.New("every enabled tenant errored")
	}
	return nil
}

func nextCommand(d *Dispatcher, o map[string]string) error {
	ts := tenantList(d.Cfg, o)
	errs := 0
	for _, t := range ts {
		item, why, err := d.plan(t)
		if err != nil {
			fmt.Fprintf(d.Out, "%s: error: %v\n", t, err)
			if d.Cfg.Tenants[t].Enabled {
				errs++
			}
			continue
		}
		if item == nil {
			fmt.Fprintf(d.Out, "%s: skip - %s\n", t, why)
			continue
		}
		fmt.Fprintf(d.Out, "%s: would dispatch %s %s (attempt %d)\n", t, item.ID, item.Title, attemptFor(d.Home, t, item.ID))
	}
	return allErrored(d, ts, errs)
}

func onceCommand(d *Dispatcher, o map[string]string) error {
	count := 1
	if v := o["count"]; v != "" {
		if _, err := fmt.Sscanf(v, "%d", &count); err != nil || count < 1 {
			return fmt.Errorf("-n wants a positive number, got %q", v)
		}
	}
	unlock, err := lockHome(d.Home)
	if err != nil {
		return err
	}
	defer unlock()
	if err := d.recoverLost(); err != nil {
		return err
	}
	errored := false
	for _, t := range tenantList(d.Cfg, o) {
		for i := 0; i < count; i++ {
			item, why, err := d.plan(t)
			if err != nil {
				fmt.Fprintf(d.Out, "%s: error: %v\n", t, err)
				errored = true
				break
			}
			if item == nil {
				fmt.Fprintf(d.Out, "%s: idle - %s\n", t, why)
				break
			}
			ts, err := d.tenantSetup(t)
			if err != nil {
				fmt.Fprintf(d.Out, "%s: error: %v\n", t, err)
				errored = true
				break
			}
			rec, err := d.dispatch(ts, item)
			d.MW.fresh()
			if errors.Is(err, errNothingToClaim) {
				fmt.Fprintf(d.Out, "%s: idle - nothing ready\n", t)
				break
			}
			if err != nil {
				fmt.Fprintf(d.Out, "%s: error dispatching %s: %v\n", t, item.ID, err)
				errored = true
				break
			}
			ex := -1
			if rec.Exit != nil {
				ex = *rec.Exit
			}
			fmt.Fprintf(d.Out, "%s: %s %s -> %s (exit %d%s)\n", t, rec.Name, item.ID, rec.Outcome, ex, map[bool]string{true: ", timeout"}[rec.Timeout])
		}
	}
	d.flushPublish()
	if errored {
		return errors.New("one or more tenants errored")
	}
	return nil
}

// lockHome takes ~/.wr/wr.lock without blocking: a second wr must not double-dispatch.
func lockHome(home string) (func(), error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, "wr.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flockFile(f); err != nil {
		f.Close()
		return nil, errors.New("another wr holds " + filepath.Join(home, "wr.lock"))
	}
	return func() { f.Close() }, nil // closing releases the flock
}

// writeLockPid records this process in wr.lock so `wr ui` can show who serves.
// Only serve calls it; the file is truncated first, the lock itself is untouched.
func writeLockPid(home string) {
	_ = os.WriteFile(filepath.Join(home, "wr.lock"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// readLockPid returns the pid written by serve (0 when absent or unreadable).
func readLockPid(home string) int {
	b, err := os.ReadFile(filepath.Join(home, "wr.lock"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func runsCommand(out io.Writer, home string, o map[string]string) error {
	if n := o["show"]; n != "" {
		runs, _ := readRuns(home, o["tenant"])
		for _, r := range runs {
			if r.Name != n {
				continue
			}
			if err := printJSON(out, r); err != nil {
				return err
			}
			if b, err := os.ReadFile(filepath.Join(runsDir(home, r.Tenant), r.Name+".log")); err == nil {
				if len(b) > 4096 {
					b = b[len(b)-4096:]
				}
				fmt.Fprintf(out, "--- log tail ---\n%s\n", b)
			}
			return nil
		}
		return fmt.Errorf("no run named %s", n)
	}
	runs, _ := readRuns(home, o["tenant"])
	if o["json"] == "true" {
		if runs == nil {
			runs = []*Run{}
		}
		return printJSON(out, runs)
	}
	for _, r := range runs {
		oc := r.Outcome
		if oc == "" {
			oc = "running"
		}
		fmt.Fprintf(out, "%s  %-9s %s  %s\n", r.Start.Local().Format("2006-01-02 15:04"), oc, r.Item, r.Name)
	}
	return nil
}

// flagInHelp matches a whole token, so --foo does not match --foobar.
func flagInHelp(help []byte, flag string) bool {
	if i := strings.Index(flag, "="); i > 0 {
		flag = flag[:i]
	}
	return regexp.MustCompile(`(?m)(^|[\s,/|\[])` + regexp.QuoteMeta(flag) + `(\s|=|,|/|\||\]|$)`).Match(help)
}
