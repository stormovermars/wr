package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stressResult struct {
	dispatches, calls []int
	lat               []time.Duration
	lockWaits         int64
	timings           map[string][]time.Duration
	errs              []string
	wall              time.Duration
}

// stressBoard makes one shared workspace with `tenants` tenants of `items` ready tasks each.
func stressBoard(t *testing.T, tenants []string, items int, sleepMS string) string {
	t.Helper()
	board := t.TempDir()
	t.Setenv("MW", mwBin)
	t.Setenv("MW_BOARD", board)
	t.Setenv("FAKE", "done")
	t.Setenv("FAKE_SLEEP_MS", sleepMS)
	t.Setenv("WR_SERVICE_DIR", t.TempDir())
	sh(t, board, mwBin, "init", "stress")
	sh(t, board, mwBin, "dirs", "set", "evidence", filepath.Join(board, "evidence"))
	sh(t, board, mwBin, "gate", "evidence", "off", "--by", "t")
	for _, tn := range tenants {
		if err := os.MkdirAll(filepath.Join(board, tn), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return board
}

func newRunner(t *testing.T, board string, tenants []string) *Dispatcher {
	home := t.TempDir()
	cfg, err := loadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Caps.PerHour, cfg.Caps.ParallelGlobal, cfg.Caps.ParallelTenant, cfg.Caps.DiskFreeGB = 100000, 100000, 100000, 0
	cfg.Pause.Unjudged, cfg.Pause.OnBlocked, cfg.Pause.QualityGuard = 1<<30, false, false
	cfg.Tenants = map[string]TenantCfg{}
	for _, tn := range tenants {
		cfg.Tenants[tn] = TenantCfg{Engine: "fake", Role: "builder", Enabled: true}
	}
	cfg.Engines = map[string]Engine{"fake": {Cmd: []string{fakeBin}, Usage: "claude_stream", Enabled: true}}
	m := newMW(board)
	if err := m.checkVersion(); err != nil {
		t.Fatal(err)
	}
	return &Dispatcher{Home: home, Cfg: cfg, MW: m, Out: io.Discard}
}

// drain is the once loop: plan, dispatch, repeat until nothing is ready on its tenants.
func drain(d *Dispatcher, tenants []string, res *stressResult, mu *sync.Mutex, deadline time.Time) {
	n := 0
	for time.Now().Before(deadline) {
		progress := false
		for _, tn := range tenants {
			d.MW.fresh()
			item, _, err := d.plan(tn)
			if err != nil {
				mu.Lock()
				res.errs = append(res.errs, "plan "+tn+": "+err.Error())
				mu.Unlock()
				continue
			}
			if item == nil {
				continue
			}
			ts, err := d.tenantSetup(tn)
			if err != nil {
				mu.Lock()
				res.errs = append(res.errs, err.Error())
				mu.Unlock()
				continue
			}
			t0 := time.Now()
			_, err = d.dispatch(ts, item)
			lat := time.Since(t0)
			if errors.Is(err, errNothingToClaim) {
				continue
			}
			if err != nil {
				mu.Lock()
				res.errs = append(res.errs, "dispatch "+item.ID+": "+err.Error())
				mu.Unlock()
				continue
			}
			n++
			progress = true
			mu.Lock()
			res.lat = append(res.lat, lat)
			mu.Unlock()
		}
		if !progress {
			break
		}
	}
	mu.Lock()
	res.dispatches = append(res.dispatches, n)
	res.calls = append(res.calls, len(d.MW.Calls()))
	res.lockWaits += d.MW.LockWaits
	if res.timings == nil {
		res.timings = map[string][]time.Duration{}
	}
	for k, v := range d.MW.Timings {
		res.timings[k] = append(res.timings[k], v...)
	}
	mu.Unlock()
}

func pct(l []time.Duration, p float64) time.Duration {
	if len(l) == 0 {
		return 0
	}
	sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
	return l[int(float64(len(l)-1)*p)]
}

// runStress seeds the board, runs the runners (runner i works on assign[i]) and checks the invariants.
func runStress(t *testing.T, label string, tenants []string, items int, assign [][]string, limit time.Duration) {
	sleep := "20"
	if len(assign) > 2 {
		sleep = "50"
	}
	board := stressBoard(t, tenants, items, sleep)
	for _, tn := range tenants {
		sh(t, board, mwBin, "tenant", "add", tn, tn, "-d", filepath.Join(board, tn))
		for i := 0; i < items; i++ {
			sh(t, board, mwBin, "add", "task", fmt.Sprintf("%s job %d", tn, i), "-t", tn, "-c", "works", "-S", "ready")
		}
	}
	var runners []*Dispatcher
	for _, a := range assign {
		runners = append(runners, newRunner(t, board, a))
	}
	res := &stressResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := time.Now()
	deadline := start.Add(limit)
	for i, d := range runners {
		wg.Add(1)
		go func(d *Dispatcher, a []string) {
			defer wg.Done()
			drain(d, a, res, &mu, deadline)
		}(d, assign[i])
	}
	wg.Wait()
	res.wall = time.Since(start)
	total := len(tenants) * items
	done, doneEvents := 0, map[string]int{}
	for _, tn := range tenants {
		var v struct {
			Items []struct{ ID, State string } `json:"items"`
		}
		_ = json.Unmarshal([]byte(sh(t, board, mwBin, "list", "-t", tn, "-j")), &v)
		for _, it := range v.Items {
			switch it.State {
			case "done":
				done++
				var s struct {
					Events []struct{ Action string } `json:"events"`
				}
				_ = json.Unmarshal([]byte(sh(t, board, mwBin, "show", it.ID, "-t", tn, "-j")), &s)
				for _, e := range s.Events {
					if e.Action == "done" {
						doneEvents[tn+"/"+it.ID]++
					}
				}
			case "doing":
				t.Errorf("%s: %s/%s left doing", label, tn, it.ID)
			}
		}
	}
	if done != total {
		t.Errorf("%s: %d of %d items done (deadline %s)", label, done, total, limit)
	}
	for k, n := range doneEvents {
		if n != 1 {
			t.Errorf("%s: %s has %d done events", label, k, n)
		}
	}
	var ss []struct{ Session, State string }
	_ = json.Unmarshal([]byte(sh(t, board, mwBin, "session", "list", "-j", "-v")), &ss)
	for _, s := range ss {
		if s.State != "stopped" {
			t.Errorf("%s: session %s left %s", label, s.Session, s.State)
		}
	}
	for _, e := range res.errs {
		t.Errorf("%s: %s", label, e)
	}
	for _, d := range runners {
		if b, _ := os.ReadFile(filepath.Join(d.Home, "events.log")); strings.Contains(string(b), "error") || strings.Contains(string(b), "lock") {
			t.Errorf("%s: events.log of %s:\n%s", label, d.Home, b)
		}
		rs, _ := readRuns(d.Home, "")
		for _, r := range rs {
			if r.Outcome != "ok" {
				t.Errorf("%s: run %s %s -> %s", label, r.Name, r.Item, r.Outcome)
			}
		}
	}
	nd, nc := 0, 0
	for i := range res.dispatches {
		nd += res.dispatches[i]
		nc += res.calls[i]
	}
	per := 0.0
	if nd > 0 {
		per = float64(nc) / float64(nd)
	}
	var verbs []string
	for k, v := range res.timings {
		verbs = append(verbs, fmt.Sprintf("%s n=%d p50 %s p95 %s", k, len(v), pct(v, .5).Round(time.Millisecond), pct(v, .95).Round(time.Millisecond)))
	}
	sort.Strings(verbs)
	t.Logf("%s: per-call mw timings under load: %s", label, strings.Join(verbs, " | "))
	dup := 0
	for _, n := range doneEvents {
		if n != 1 {
			dup++
		}
	}
	stressLine(strings.NewReplacer(", ", "-", " ", "-").Replace(label), done, total, dup, res.lockWaits, res.lat, res.wall)
	t.Logf("%s: %d runners, %d tenants x %d items; dispatches per runner %v; %d total; mw calls/dispatch %.1f; dispatch latency p50 %s p95 %s; lock-timeout retries %d; wall %s",
		label, len(runners), len(tenants), items, res.dispatches, nd, per,
		pct(res.lat, .5).Round(time.Millisecond), pct(res.lat, .95).Round(time.Millisecond), res.lockWaits, res.wall.Round(time.Millisecond))
}

func TestTwoRunnersConcurrent(t *testing.T) {
	if mwBin == "" {
		t.Skip("mw not on PATH")
	}
	both := []string{"a", "b"}
	t.Run("split", func(t *testing.T) {
		runStress(t, "2 runners, one tenant each", both, 10, [][]string{{"a"}, {"b"}}, 60*time.Second)
	})
	t.Run("both-on-both", func(t *testing.T) {
		runStress(t, "2 runners, both tenants each", both, 10, [][]string{both, both}, 60*time.Second)
	})
}

// TestFortyRunnersConcurrent: 40 runners. Gated by WR_STRESS=1 (it takes well over 20 s).
func TestFortyRunnersConcurrent(t *testing.T) {
	if mwBin == "" {
		t.Skip("mw not on PATH")
	}
	if os.Getenv("WR_STRESS") != "1" {
		t.Skip("set WR_STRESS=1: 40 runners take longer than 20 s")
	}
	var ts40 []string
	var assign40 [][]string
	for i := 0; i < 40; i++ {
		tn := fmt.Sprintf("t%02d", i)
		ts40 = append(ts40, tn)
		assign40 = append(assign40, []string{tn})
	}
	t.Run("40 tenants x 10", func(t *testing.T) {
		runStress(t, "40 runners, one tenant each", ts40, 10, assign40, 5*time.Minute)
	})
	four := []string{"w", "x", "y", "z"}
	var all [][]string
	for i := 0; i < 40; i++ {
		all = append(all, four)
	}
	t.Run("4 tenants x 100", func(t *testing.T) {
		runStress(t, "40 runners, all on 4 tenants", four, 100, all, 5*time.Minute)
	})
}

// TestThousandTenants: 1000 tenants x 2 items on one board, 40 runners each enabled on all of them
// (each starts at its own tenant and walks round). Gated by WR_STRESS=1.
func TestThousandTenants(t *testing.T) {
	if mwBin == "" {
		t.Skip("mw not on PATH")
	}
	if os.Getenv("WR_STRESS") != "1" {
		t.Skip("set WR_STRESS=1")
	}
	const nT, perT, nR = 1000, 2, 40
	board := t.TempDir()
	t.Setenv("MW", mwBin)
	t.Setenv("MW_BOARD", board)
	t.Setenv("FAKE", "done")
	t.Setenv("FAKE_SLEEP_MS", "10")
	t.Setenv("WR_SERVICE_DIR", t.TempDir())
	sh(t, board, mwBin, "init", "big")
	sh(t, board, mwBin, "dirs", "set", "evidence", filepath.Join(board, "evidence"))
	sh(t, board, mwBin, "gate", "evidence", "off", "--by", "t")
	sh(t, board, mwBin, "tenant", "add", "proto", "proto", "-d", filepath.Join(board, "proto"))
	for i := 0; i < perT; i++ {
		sh(t, board, mwBin, "add", "task", fmt.Sprintf("job %d", i), "-t", "proto", "-c", "works", "-S", "ready")
	}
	// replicate the proto tenant 1000 times: its tenant file and an index entry each
	bp := filepath.Join(board, ".miniwaves.json")
	raw, _ := os.ReadFile(bp)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	protoFile, err := os.ReadFile(filepath.Join(board, ".miniwaves", "proto.json"))
	if err != nil {
		t.Fatal(err)
	}
	var all []map[string]string
	var slugs []string
	for i := 0; i < nT; i++ {
		slug := fmt.Sprintf("t%04d", i)
		slugs = append(slugs, slug)
		all = append(all, map[string]string{"slug": slug, "name": slug, "path": filepath.Join(board, slug)})
		_ = os.MkdirAll(filepath.Join(board, slug), 0o755)
		if err := os.WriteFile(filepath.Join(board, ".miniwaves", slug+".json"), protoFile, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	doc["tenants"], _ = json.Marshal(all)
	nb, _ := json.Marshal(doc)
	if err := os.WriteFile(bp, append(nb, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	var boardSize int64
	_ = filepath.Walk(board, func(p string, fi os.FileInfo, e error) error {
		if e == nil && !fi.IsDir() && (strings.HasSuffix(p, ".json") && (strings.Contains(p, "/.miniwaves/") || filepath.Base(p) == ".miniwaves.json")) {
			boardSize += fi.Size()
		}
		return nil
	})
	// idle latency of single calls on the big board
	timeIt := func(args ...string) time.Duration {
		best := time.Hour
		for i := 0; i < 5; i++ {
			t0 := time.Now()
			sh(t, board, mwBin, args...)
			if d := time.Since(t0); d < best {
				best = d
			}
		}
		return best
	}
	idle := fmt.Sprintf("idle best-of-5: version -j %s, list -t t0500 -j %s", timeIt("version", "-j").Round(time.Millisecond), timeIt("list", "-t", "t0500", "-j").Round(time.Millisecond))

	var runners []*Dispatcher
	for i := 0; i < nR; i++ {
		runners = append(runners, newRunner(t, board, slugs))
	}
	var doneCount int64
	res := &stressResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := time.Now()
	deadline := start.Add(10 * time.Minute)
	for i, d := range runners {
		wg.Add(1)
		go func(i int, d *Dispatcher) {
			defer wg.Done()
			n := 0
			for k := 0; k < nT && time.Now().Before(deadline) && atomic.LoadInt64(&doneCount) < nT*perT; k++ {
				tn := slugs[(i*nT/nR+k)%nT]
				for {
					d.MW.fresh()
					item, _, err := d.plan(tn)
					if err != nil || item == nil {
						if err != nil {
							mu.Lock()
							res.errs = append(res.errs, "plan "+tn+": "+err.Error())
							mu.Unlock()
						}
						break
					}
					ts, err := d.tenantSetup(tn)
					if err != nil {
						mu.Lock()
						res.errs = append(res.errs, err.Error())
						mu.Unlock()
						break
					}
					t0 := time.Now()
					_, err = d.dispatch(ts, item)
					if errors.Is(err, errNothingToClaim) {
						break
					}
					if err != nil {
						mu.Lock()
						res.errs = append(res.errs, "dispatch "+item.ID+": "+err.Error())
						mu.Unlock()
						break
					}
					n++
					atomic.AddInt64(&doneCount, 1)
					mu.Lock()
					res.lat = append(res.lat, time.Since(t0))
					mu.Unlock()
				}
			}
			mu.Lock()
			res.dispatches = append(res.dispatches, n)
			res.calls = append(res.calls, len(d.MW.Calls()))
			res.lockWaits += d.MW.LockWaits
			if res.timings == nil {
				res.timings = map[string][]time.Duration{}
			}
			for k, v := range d.MW.Timings {
				res.timings[k] = append(res.timings[k], v...)
			}
			mu.Unlock()
		}(i, d)
	}
	wg.Wait()
	wall := time.Since(start)
	// verify: every item done exactly once
	done, bad := 0, 0
	for _, slug := range slugs {
		fb, _ := os.ReadFile(filepath.Join(board, ".miniwaves", slug+".json"))
		var v struct {
			Items []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Events []struct{ Action string }
			} `json:"items"`
		}
		_ = json.Unmarshal(fb, &v)
		for _, it := range v.Items {
			if it.Status == "done" {
				done++
			}
			n := 0
			for _, e := range it.Events {
				if e.Action == "done" {
					n++
				}
			}
			if it.Status == "done" && n != 1 || it.Status == "doing" {
				bad++
			}
		}
	}
	nd := 0
	for _, x := range res.dispatches {
		nd += x
	}
	var vs []string
	for _, k := range []string{"version", "list", "next", "session", "show"} {
		if l := res.timings[k]; len(l) > 0 {
			vs = append(vs, fmt.Sprintf("%s n=%d p50 %s p95 %s", k, len(l), pct(l, .5).Round(time.Millisecond), pct(l, .95).Round(time.Millisecond)))
		}
	}
	t.Logf("1000 tenants: board %d KB in 1001 files; %s", boardSize/1024, idle)
	t.Logf("1000 tenants: %d runners on all tenants; %d dispatches; %d/%d done, %d not exactly-once or left doing; lock timeouts %d; dispatch p50 %s p95 %s; wall %s; under load: %s",
		nR, nd, done, nT*perT, bad, res.lockWaits, pct(res.lat, .5).Round(time.Millisecond), pct(res.lat, .95).Round(time.Millisecond), wall.Round(time.Second), strings.Join(vs, " | "))
	stressLine("1000-tenants", done, nT*perT, bad, res.lockWaits, res.lat, wall)
	if done != nT*perT || bad > 0 || res.lockWaits > 0 || len(res.errs) > 0 {
		t.Errorf("done %d/%d bad %d lock timeouts %d errs %d %v", done, nT*perT, bad, res.lockWaits, len(res.errs), res.errs[:min(3, len(res.errs))])
	}
}

// stressLine prints the one-line summary the stress workflow copies to $GITHUB_STEP_SUMMARY.
func stressLine(variant string, done, total, dup int, timeouts int64, lat []time.Duration, wall time.Duration) {
	fmt.Printf("STRESS %s done=%d/%d dup=%d timeouts=%d p50=%d p95=%d wall=%.0f\n", variant, done, total, dup, timeouts,
		pct(lat, .5).Milliseconds(), pct(lat, .95).Milliseconds(), wall.Seconds())
}
