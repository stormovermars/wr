package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ReapMW is the first mw with `session stop --release` and `session reap`.
const ReapMW = "0.2.3"

// hasReap reports whether the mw found by checkVersion can release and reap sessions.
func (m *MW) hasReap() bool { return m.Version != "" && !semverLess(m.Version, ReapMW) }

// ReapCfg: `"reap": {"enabled": true, "older_than": "4h"}`. Each serve tick runs, for every
// wr-enabled tenant, `mw session reap -t SLUG --older-than <older_than> --except <live runs> -j`
// (mw >= 0.2.3 only). Absent = enabled, 4h. Tenants wr does not dispatch are never reaped.
type ReapCfg struct {
	Enabled   *bool  `json:"enabled,omitempty"`
	OlderThan string `json:"older_than,omitempty"`
}

const (
	defaultReapOlder = "4h"
	maxReapDays      = 365
	reapSlack        = 5 * time.Minute
)

func (c *Config) reapEnabled() bool {
	return c.Reap == nil || c.Reap.Enabled == nil || *c.Reap.Enabled
}

// reapOlder is the configured --older-than value (90m, 2h, 1d) or the default.
func (c *Config) reapOlder() string {
	if c.Reap != nil && validReapAge(c.Reap.OlderThan) {
		return c.Reap.OlderThan
	}
	return defaultReapOlder
}

// reapFloor is the least older_than that cannot reap a healthy live run:
// run_timeout_min + stale_minutes + 5m.
func (c *Config) reapFloor() time.Duration {
	return time.Duration(c.timeout()) + c.stale() + reapSlack
}

// reapOlderEff is the --older-than actually used: reapOlder raised to reapFloor.
// clamped reports that the floor applied.
func (c *Config) reapOlderEff() (older string, clamped bool) {
	older = c.reapOlder()
	d, _ := parseReapAge(older)
	if f := c.reapFloor(); d < f {
		return fmt.Sprintf("%dm", int((f+time.Minute-1)/time.Minute)), true
	}
	return older, false
}

// parseReapAge reads 90m, 2h, 1d: finite, > 0 after conversion, at most 365d.
func parseReapAge(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	var d time.Duration
	if n, ok := strings.CutSuffix(s, "d"); ok {
		v, err := strconv.ParseFloat(n, 64)
		if err != nil || !(v > 0) || v > maxReapDays {
			return 0, false
		}
		d = time.Duration(v * float64(24*time.Hour))
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, false
		}
	}
	return d, d > 0 && d <= maxReapDays*24*time.Hour
}

func validReapAge(s string) bool {
	_, ok := parseReapAge(s)
	return ok
}

type reapedSession struct {
	Session string   `json:"session"`
	Tenant  string   `json:"tenant"`
	Items   []string `json:"items"`
}

// reapState is ~/.wr/reap.json: the last reap call, shown by `wr status`.
type reapState struct {
	At        string          `json:"at"`
	OlderThan string          `json:"older_than"`
	Reaped    []reapedSession `json:"reaped"`
	Error     string          `json:"error,omitempty"`
}

func reapPath(home string) string { return filepath.Join(home, "reap.json") }

func readReapState(home string) *reapState {
	b, err := os.ReadFile(reapPath(home))
	if err != nil {
		return nil
	}
	var s reapState
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	if s.Reaped == nil {
		s.Reaped = []reapedSession{}
	}
	return &s
}

// reap runs `mw session reap -t TENANT --older-than OLDER [--except NAMES] -j` and returns
// what it released. except are session names that must not be reaped (wr's live runs).
func (m *MW) reap(tenant, older string, except []string) ([]reapedSession, error) {
	args := []string{"session", "reap", "-t", tenant, "--older-than", older}
	if len(except) > 0 {
		args = append(args, "--except", strings.Join(except, ","))
	}
	args = append(args, "-j")
	out, err := retryLock(func() (string, error) { return m.must(args...) })
	if err != nil {
		return nil, err
	}
	var v struct {
		Reaped []reapedSession `json:"reaped"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("bad session reap json: %w", err)
	}
	return v.Reaped, nil
}

// liveRuns maps tenant -> board and the names of wr's own runs that are still alive.
func (d *Dispatcher) liveRuns() (boards map[string]string, names []string) {
	boards = map[string]string{}
	runs, _ := readRuns(d.Home, "")
	for _, r := range runs {
		if r.End != nil || !(isInflight(r.Name) || runAlive(r)) {
			continue
		}
		names = append(names, r.Name)
		if r.Board != "" {
			boards[r.Tenant] = r.Board
		}
	}
	sort.Strings(names)
	return boards, names
}

// reapTick is called once per serve tick: it frees the items of sessions that went
// silent (killed controller, lost context) and records the result. Only wr-enabled
// tenants are reaped, once per (board, tenant), and wr's own live runs are skipped.
func (d *Dispatcher) reapTick() {
	if !d.Cfg.reapEnabled() || !d.MW.hasReap() {
		return
	}
	var tenants []string
	for _, t := range d.Cfg.tenantsInOrder() {
		if d.Cfg.Tenants[t].Enabled {
			tenants = append(tenants, t)
		}
	}
	if len(tenants) == 0 {
		return
	}
	older, clamped := d.Cfg.reapOlderEff()
	if clamped && !d.reapClampLogged {
		d.reapClampLogged = true
		msg := fmt.Sprintf("reap.older_than %s is below run_timeout_min + stale_minutes + 5m; using %s", d.Cfg.reapOlder(), older)
		fmt.Fprintf(d.Out, "reap: %s\n", msg)
		d.event("reap-clamped", "", msg, false)
	}
	boards, except := d.liveRuns()
	st := reapState{At: d.now().UTC().Format(time.RFC3339), OlderThan: older, Reaped: []reapedSession{}}
	var errs []string
	for _, t := range tenants {
		m := d.MW
		if b := boards[t]; b != "" && b != d.MW.Board {
			m = &MW{Bin: d.MW.Bin, Board: b, Version: d.MW.Version}
		}
		reaped, err := m.reap(t, older, except)
		if err != nil {
			errs = append(errs, t+": "+err.Error())
			fmt.Fprintf(d.Out, "reap: %s: error: %v\n", t, err)
			d.event("reap-failed", t, err.Error(), false)
			continue
		}
		if len(reaped) > 0 {
			m.fresh()
			d.MW.fresh()
		}
		st.Reaped = append(st.Reaped, reaped...)
		for _, r := range reaped {
			fmt.Fprintf(d.Out, "reap: %s (%s) released %s\n", r.Session, r.Tenant, strings.Join(r.Items, ", "))
			d.event("reaped", r.Tenant, r.Session+" released "+strings.Join(r.Items, ","), false)
		}
	}
	st.Error = strings.Join(errs, "; ")
	if b, merr := json.Marshal(st); merr == nil {
		_ = writeFileAtomic(reapPath(d.Home), append(b, '\n'), 0o600)
	}
}
