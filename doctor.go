package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// doctorCommand runs the environment checks and fails (exit 1) if any fails.
func doctorCommand(out io.Writer, home string, o map[string]string) error {
	var cs []doctorCheck
	add := func(name string, ok bool, detail string) { cs = append(cs, doctorCheck{name, ok, detail}) }

	m := newMW(o["board"])
	if p, err := exec.LookPath(m.Bin); err != nil {
		add("mw", false, "not found: "+m.Bin+"; "+mwInstallHint)
	} else if err := m.checkVersion(); err != nil {
		add("mw", false, p+": "+err.Error())
	} else {
		add("mw", true, fmt.Sprintf("%s %s (needs >= %s)", p, m.Version, MinMW))
		if err := m.requireSchema(); err != nil {
			add("board", false, err.Error())
		} else {
			add("board", true, fmt.Sprintf("%s schema %d via %s", m.BoardFile, m.Schema, m.Source))
		}
	}

	var cfg *Config
	var cfgErr error
	_, statErr := os.Stat(configPath(home))
	if cfg, cfgErr = loadConfig(home); cfgErr != nil {
		add("config", false, cfgErr.Error())
	} else if statErr != nil {
		add("config", true, "first run: wrote the default config to "+configPath(home)+" (edit it, then enable tenants)")
	} else {
		add("config", true, configPath(home))
	}

	if cfg != nil {
		var names []string
		for n := range cfg.Engines {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			e := cfg.Engines[n]
			if !e.Enabled || len(e.Cmd) == 0 || filepath.Base(e.Cmd[0]) != "claude" {
				continue
			}
			absent, err := absentFlags(e.Cmd)
			switch {
			case err != nil:
				add("engine "+n, false, err.Error())
			case len(absent) > 0:
				add("engine "+n, false, "flags absent from claude --help: "+strings.Join(absent, " "))
			default:
				detail := "all template flags present (engines are unsandboxed: bypassPermissions; forbid is advisory prompt text)"
				if strings.Contains(strings.Join(e.Cmd, " "), "output-format json") {
					detail += "; WARNING: output-format json fills the log only at the end, use stream-json --verbose + usage claude_stream"
				}
				add("engine "+n, true, detail)
			}
		}
	}

	if cfg != nil && m.BoardFile != "" && m.requireSchema() == nil {
		d := &Dispatcher{Home: home, Cfg: cfg, MW: m, Out: io.Discard}
		for _, t := range cfg.tenantsInOrder() {
			if !cfg.Tenants[t].Enabled {
				continue
			}
			if ts, err := d.tenantSetup(t); err == nil {
				if _, gerr := gitDirty(ts.Dir); errors.Is(gerr, errNotRepo) {
					add("git "+t, true, "no git repo: dirty-tree guard off ("+ts.Dir+")")
				}
			}
		}
	}

	if cfg != nil && cfg.Publish.HTML != "" {
		note := fmt.Sprintf("page %s rewritten when the data changed, at most every %s", cfg.Publish.HTML, cfg.Publish.interval())
		if cfg.Publish.Interval != 0 && cfg.Publish.Interval < 5 {
			note += " (publish.interval below 5 s is clamped to 5)"
		}
		add("publish", true, note)
	}

	if cfg != nil && fleetDir() != "" {
		fm := fleetManaged()
		var clash []string
		for _, t := range cfg.tenantsInOrder() {
			if cfg.Tenants[t].Enabled && fm[t] {
				clash = append(clash, t)
			}
		}
		if len(clash) > 0 {
			add("fleet", false, "an external dispatcher (WR_FLEET_DIR) has these enabled too: "+strings.Join(clash, ", ")+" ("+filepath.Join(fleetDir(), "state", "auto-dispatch.json")+")")
		} else {
			add("fleet", true, "no overlap with the registry in WR_FLEET_DIR")
		}
	}

	if err := os.MkdirAll(home, 0o700); err != nil {
		add("home", false, err.Error())
	} else if f, err := os.CreateTemp(home, ".doctor-*"); err != nil {
		add("home", false, home+" not writable: "+err.Error())
	} else {
		f.Close()
		os.Remove(f.Name())
		add("home", true, home+" writable")
	}

	if unlock, err := lockHome(home); err != nil {
		add("lock", true, "held (serve running)")
	} else {
		unlock()
		add("lock", true, "free")
	}

	floor := 5
	if cfg != nil {
		floor = cfg.Caps.DiskFreeGB
	}
	if free, err := diskFreeGB(home); err != nil {
		add("disk", false, "cannot read free space: "+err.Error())
	} else {
		add("disk", free >= float64(floor), fmt.Sprintf("%.1f GB free (floor %d)", free, floor))
	}

	if cfg != nil && cfg.reapEnabled() {
		if _, clamped := cfg.reapOlderEff(); clamped {
			add("reap", true, fmt.Sprintf("WARNING: older_than %s is below run_timeout_min + stale_minutes + 5m (%s); serve raises it to that", cfg.reapOlder(), cfg.reapFloor()))
		} else {
			add("reap", true, "older_than "+cfg.reapOlder())
		}
	}
	if s := updateNotice(home); s != "" {
		add("update", true, s)
	}
	failed := 0
	for _, c := range cs {
		if !c.OK {
			failed++
		}
	}
	if o["json"] == "true" {
		if err := printJSON(out, map[string]any{"ok": failed == 0, "checks": cs}); err != nil {
			return err
		}
	} else {
		for _, c := range cs {
			fmt.Fprintf(out, "%-4s %-14s %s\n", map[bool]string{true: "ok", false: "FAIL"}[c.OK], c.Name, c.Detail)
		}
	}
	if failed > 0 {
		return &exitError{Code: 1, Msg: fmt.Sprintf("doctor: %d check(s) failed", failed)}
	}
	return nil
}

// absentFlags lists template flags missing from `claude --help` (reported, never dropped from the config).
func absentFlags(cmd []string) ([]string, error) {
	help, err := exec.Command(cmd[0], "--help").CombinedOutput()
	if err != nil && len(bytes.TrimSpace(help)) == 0 {
		return nil, fmt.Errorf("cannot run %s --help: %v", cmd[0], err)
	}
	var absent []string
	for _, a := range cmd[1:] {
		if strings.HasPrefix(a, "-") && !flagInHelp(help, a) {
			absent = append(absent, a)
		}
	}
	return absent, nil
}
