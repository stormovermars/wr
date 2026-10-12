package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// fleetDir is the optional external registry dir named by WR_FLEET_DIR ("" = the
// overlap check is off).
func fleetDir() string { return os.Getenv("WR_FLEET_DIR") }

// fleetManaged returns the tenants another dispatcher owns: when
// <WR_FLEET_DIR>/state/auto-dispatch.json has enabled=true, every tenant listed in
// <WR_FLEET_DIR>/apps-fleet-sessions.json. Empty when WR_FLEET_DIR is unset, or
// the registry is off or unreadable.
func fleetManaged() map[string]bool {
	dir := fleetDir()
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "state", "auto-dispatch.json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		Enabled bool `json:"enabled"`
	}
	if json.Unmarshal(b, &cfg) != nil || !cfg.Enabled {
		return nil
	}
	b, err = os.ReadFile(filepath.Join(dir, "apps-fleet-sessions.json"))
	if err != nil {
		return nil
	}
	var reg struct {
		Sessions []struct {
			Tenant string `json:"tenant"`
		} `json:"sessions"`
	}
	if json.Unmarshal(b, &reg) != nil {
		return nil
	}
	m := map[string]bool{}
	for _, s := range reg.Sessions {
		if s.Tenant != "" {
			m[s.Tenant] = true
		}
	}
	return m
}

// ---- events log and notifications ------------------------------------------

// notifyFn raises a macOS notification; tests replace it.
var notifyFn = func(title, msg string) {
	if runtime.GOOS == "darwin" {
		esc := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
		_ = exec.Command("osascript", "-e", "display notification "+esc(msg)+" with title "+esc(title)).Run()
	}
}

// event appends to ~/.wr/events.log and, when notify, raises a notification.
func (d *Dispatcher) event(kind, tenant, msg string, notify bool) {
	if f, err := os.OpenFile(filepath.Join(d.Home, "events.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s\t%s\t%s\t%s\n", d.now().Format("2006-01-02T15:04:05"), kind, tenant, msg)
		f.Close()
	}
	if notify {
		notifyFn("wr: "+kind+" "+tenant, msg)
	}
}
