package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type statusDoc struct {
	Tenants  []statusRow `json:"tenants"`
	Archived int         `json:"archived_hidden,omitempty"` // archived tenants left out
	Live     []liveRow   `json:"live"`
	Recent   []recentRow `json:"recent"`
	Stuck    []stuckRow  `json:"stuck"`
	Skips    []pausedRow `json:"skips"`
	Caps     Usage       `json:"caps"`
	Paused   []pausedRow `json:"paused"`
	Cost     []costRow   `json:"cost"`
	Stop     []string    `json:"stop_files"`
	MW       mwInfo      `json:"mw"`
	Reap     *reapState  `json:"reap,omitempty"` // last `mw session reap` of wr serve
}

type mwInfo struct {
	Schema int    `json:"schema"`
	Source string `json:"source"`
}

type statusRow struct {
	Tenant  string   `json:"tenant"`
	Tags    []string `json:"tags,omitempty"`
	Enabled bool     `json:"enabled"`
	Engine  string   `json:"engine"`
	Last    string   `json:"last_outcome"`
	Running int      `json:"running"`
	Today   float64  `json:"usd_today"`
	Done    int      `json:"done"`
	Total   int      `json:"total"`
	Doing   int      `json:"doing"`
	Blocked int      `json:"blocked"`
	// GateLevel is the board's level (mw >= 0.2.5), GateLevelWanted the configured one.
	GateLevel       string `json:"gate_level,omitempty"`
	GateLevelWanted string `json:"gate_level_wanted,omitempty"`
}

type pausedRow struct {
	Tenant string `json:"tenant"`
	Reason string `json:"reason"`
}

type liveRow struct {
	Tenant        string `json:"tenant"`
	Item          string `json:"item"`
	Name          string `json:"name"`
	AgeSec        int    `json:"age_sec"`
	TranscriptSec int    `json:"transcript_age_sec"` // -1: no transcript
	Log           string `json:"log"`
	Lost          bool   `json:"lost"`
}

type recentRow struct {
	At      string `json:"at"`
	Tenant  string `json:"tenant"`
	Item    string `json:"item"`
	Outcome string `json:"outcome"`
	Name    string `json:"name"`
}

type stuckRow struct {
	Tenant  string `json:"tenant"`
	Item    string `json:"item"`
	Session string `json:"session"`
}

type costRow struct {
	Day    string  `json:"day"`
	Engine string  `json:"engine"`
	USD    float64 `json:"usd"`
}

func costRows(runs []*Run) []costRow {
	sum := map[[2]string]float64{}
	for _, r := range runs {
		if r.Cost != nil {
			sum[[2]string{r.Start.Local().Format("2006-01-02"), r.Engine}] += *r.Cost
		}
	}
	var rows []costRow
	for k, v := range sum {
		rows = append(rows, costRow{k[0], k[1], v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Day != rows[j].Day {
			return rows[i].Day < rows[j].Day
		}
		return rows[i].Engine < rows[j].Engine
	})
	return rows
}

func age(sec int) string {
	if sec < 0 {
		return "-"
	}
	return (time.Duration(sec) * time.Second).String()
}

// statusData gathers everything `wr status` and `wr ui` show. It is read-only:
// it never recovers, settles or kills anything.
func statusData(d *Dispatcher, o map[string]string) (statusDoc, error) {
	now := time.Now()
	today := now.Local().Format("2006-01-02")
	doc := statusDoc{Tenants: []statusRow{}, Live: []liveRow{}, Recent: []recentRow{}, Stuck: []stuckRow{}, Skips: []pausedRow{},
		Paused: []pausedRow{}, Stop: stopFiles(d.Home), Caps: d.usage(), MW: mwInfo{Schema: d.MW.Schema, Source: d.MW.Source}}
	ts := tenantList(d.Cfg, o)
	if o["tenant"] == "" {
		ts, doc.Archived = d.shown(d.Cfg.tenantsInOrder())
	}
	all, _ := readRuns(d.Home, "")
	liveNames := map[string]bool{}
	for _, x := range all {
		if x.End == nil {
			lr := liveRow{Tenant: x.Tenant, Item: x.Item, Name: x.Name, AgeSec: int(now.Sub(x.Start).Seconds()), TranscriptSec: -1,
				Log: filepath.Join(runsDir(d.Home, x.Tenant), x.Name+".log"), Lost: !runLive(x, now)}
			if x.Transcript != "" {
				if fi, err := os.Stat(x.Transcript); err == nil {
					lr.TranscriptSec = int(now.Sub(fi.ModTime()).Seconds())
				}
			}
			if !lr.Lost {
				liveNames[x.Name] = true
			}
			doc.Live = append(doc.Live, lr)
		}
	}
	n := 5
	if v := o["last"]; v != "" {
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 {
			return doc, fmt.Errorf("--last wants a positive number, got %q", v)
		}
	}
	for i := len(all) - 1; i >= 0 && len(doc.Recent) < n; i-- {
		if x := all[i]; x.Outcome != "" {
			doc.Recent = append(doc.Recent, recentRow{x.Start.Local().Format("2006-01-02 15:04"), x.Tenant, x.Item, x.Outcome, x.Name})
		}
	}
	doc.Cost = costRows(all)
	for _, t := range ts {
		tc := d.Cfg.Tenants[t]
		r := statusRow{Tenant: t, Tags: d.MW.meta(t).Tags, Enabled: tc.Enabled, Engine: tc.Engine, Last: lastOutcome(d.Home, t), GateLevelWanted: normLevel(tc.GateLevel)}
		for _, x := range all {
			if x.Tenant != t {
				continue
			}
			if x.End == nil && liveNames[x.Name] {
				r.Running++
			}
			if x.Cost != nil && x.Start.Local().Format("2006-01-02") == today {
				r.Today += *x.Cost
			}
		}
		if !tc.Enabled {
			doc.Tenants = append(doc.Tenants, r)
			continue
		}
		if validGateLevel(tc.GateLevel) { // no extra mw call for tenants without a gate_level
			r.GateLevel = d.MW.gateLevelOf(t)
		}
		items, lerr := d.MW.listItems(t)
		if lerr == nil {
			for _, it := range items {
				switch it.State {
				case "done":
					r.Done++
				case "doing":
					r.Doing++
				case "blocked":
					r.Blocked++
				}
			}
			r.Total = len(items)
		}
		doc.Tenants = append(doc.Tenants, r)
		if lerr == nil {
			for _, it := range items {
				if it.State == "doing" && strings.HasPrefix(it.session(), "wr-") && !liveNames[it.session()] {
					doc.Stuck = append(doc.Stuck, stuckRow{t, it.ID, it.session()})
				}
			}
		}
		if item, why, err := d.planMode(t, true); err != nil {
			doc.Skips = append(doc.Skips, pausedRow{t, "error: " + err.Error()})
		} else if item != nil {
			doc.Skips = append(doc.Skips, pausedRow{t, "would dispatch " + item.ID})
		} else {
			doc.Skips = append(doc.Skips, pausedRow{t, why})
		}
	}
	doc.Paused = d.pausedNow(ts)
	doc.Reap = readReapState(d.Home)
	return doc, nil
}

// statusErrored counts enabled tenants whose plan errored.
func statusErrored(doc statusDoc) (n, enabled int) {
	skip := map[string]string{}
	for _, s := range doc.Skips {
		skip[s.Tenant] = s.Reason
	}
	for _, r := range doc.Tenants {
		if r.Enabled {
			enabled++
			if strings.HasPrefix(skip[r.Tenant], "error: ") {
				n++
			}
		}
	}
	return
}

func statusCommand(d *Dispatcher, o map[string]string) error {
	doc, err := statusData(d, o)
	if err != nil {
		return err
	}
	failed := func() error {
		if n, en := statusErrored(doc); en > 0 && n >= en {
			return errors.New("every enabled tenant errored")
		}
		return nil
	}
	if o["json"] == "true" {
		if err := printJSON(d.Out, doc); err != nil {
			return err
		}
		return failed()
	}
	for _, r := range doc.Tenants {
		fmt.Fprintf(d.Out, "%-14s enabled=%-5v engine=%-7s last=%-10s running=%d usd_today=%.4f done=%d/%d doing=%d blocked=%d%s%s\n", r.Tenant, r.Enabled, r.Engine, r.Last, r.Running, r.Today, r.Done, r.Total, r.Doing, r.Blocked, gateText(r.GateLevel, r.GateLevelWanted), tagText(r.Tags))
	}
	if s := archivedNote(doc.Archived); s != "" {
		fmt.Fprintln(d.Out, s)
	}
	fmt.Fprintf(d.Out, "mw: schema %d via %s", doc.MW.Schema, doc.MW.Source)
	if !schemaOK(d.MW.Schema) {
		fmt.Fprintf(d.Out, " - WR WILL NOT DISPATCH (needs schema %d)", MinSchema)
	}
	fmt.Fprintln(d.Out)
	for _, l := range doc.Live {
		lost := ""
		if l.Lost {
			lost = " lost?"
		}
		fmt.Fprintf(d.Out, "live: %s %s age %s transcript %s log %s%s\n", l.Tenant, l.Item, age(l.AgeSec), age(l.TranscriptSec), l.Log, lost)
	}
	for _, r := range doc.Recent {
		fmt.Fprintf(d.Out, "recent: %s %s %s %s\n", r.At, r.Tenant, r.Item, r.Outcome)
	}
	for _, s := range doc.Stuck {
		fmt.Fprintf(d.Out, "stuck: %s %s doing under %s with no live run\n", s.Tenant, s.Item, s.Session)
	}
	for _, s := range doc.Skips {
		fmt.Fprintf(d.Out, "tenant %s: %s\n", s.Tenant, s.Reason)
	}
	u := doc.Caps
	fmt.Fprintf(d.Out, "caps: sessions/60min %d/%d, live %d/%d, disk %.1f GB (floor %d)\n", u.Hour, d.Cfg.Caps.PerHour, u.ParallelGlobal, d.Cfg.Caps.ParallelGlobal, u.DiskFreeGB, u.DiskFloorGB)
	var engs []string
	for e := range u.USDToday {
		engs = append(engs, e)
	}
	sort.Strings(engs)
	for _, e := range engs {
		fmt.Fprintf(d.Out, "caps: engine %s USD %.4f / %.2f today\n", e, u.USDToday[e], d.Cfg.Caps.USDPerDay[e])
	}
	for _, c := range doc.Cost {
		fmt.Fprintf(d.Out, "cost: %s %-8s USD %.4f\n", c.Day, c.Engine, c.USD)
	}
	for _, p := range doc.Paused {
		fmt.Fprintf(d.Out, "paused: %s - %s\n", p.Tenant, p.Reason)
	}
	for _, f := range doc.Stop {
		fmt.Fprintf(d.Out, "stop file: %s\n", f)
	}
	if r := doc.Reap; r != nil {
		switch {
		case r.Error != "":
			fmt.Fprintf(d.Out, "reap: %s older-than %s error: %s\n", r.At, r.OlderThan, r.Error)
		case len(r.Reaped) == 0:
			fmt.Fprintf(d.Out, "reap: %s older-than %s nothing reaped\n", r.At, r.OlderThan)
		default:
			for _, x := range r.Reaped {
				fmt.Fprintf(d.Out, "reap: %s older-than %s %s (%s) released %s\n", r.At, r.OlderThan, x.Session, x.Tenant, strings.Join(x.Items, ", "))
			}
		}
	}
	return failed()
}

// logCommand tails ~/.wr/serve.out: the last 50 lines, then (-f) new output.
func logCommand(out io.Writer, home string, o map[string]string) error {
	p := filepath.Join(home, "serve.out")
	b, err := os.ReadFile(p)
	if err != nil {
		return logFallback(out, home, p)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}
	fmt.Fprintln(out, strings.Join(lines, "\n"))
	if o["f"] != "true" {
		return nil
	}
	pos := int64(len(b))
	for {
		time.Sleep(followPoll)
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		if fi, _ := f.Stat(); fi.Size() < pos {
			pos = 0
		}
		f.Seek(pos, io.SeekStart)
		n, _ := io.Copy(out, f)
		pos += n
		f.Close()
	}
}

// logFallback: serve.out is missing (serve never ran as a service), so show the
// last runs and the tail of events.log instead.
func logFallback(out io.Writer, home, p string) error {
	runs, _ := readRuns(home, "")
	ev, _ := os.ReadFile(filepath.Join(home, "events.log"))
	if len(runs) == 0 && len(bytes.TrimSpace(ev)) == 0 {
		return fmt.Errorf("no log yet: %s (no runs or events either)", p)
	}
	fmt.Fprintf(out, "no %s; last runs and events:\n", p)
	if len(runs) > 10 {
		runs = runs[len(runs)-10:]
	}
	for _, r := range runs {
		oc := r.Outcome
		if oc == "" {
			oc = "running"
		}
		fmt.Fprintf(out, "run  %s  %-9s %s  %s\n", r.Start.Local().Format("2006-01-02 15:04"), oc, r.Item, r.Name)
	}
	lines := strings.Split(strings.TrimRight(string(ev), "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	if len(bytes.TrimSpace(ev)) > 0 {
		fmt.Fprintln(out, strings.Join(lines, "\n"))
	}
	return nil
}

// gateText is the status column for the board's gate level ("" without mw support).
func gateText(l, wanted string) string {
	if l == "" {
		return ""
	}
	if wanted != "" && wanted != l {
		return " gate " + l + " (wants " + wanted + ")"
	}
	return " gate " + l
}
