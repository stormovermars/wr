package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// verbs counts the mw calls by their first words ("next --claim", "list", ...).
func verbs(calls []string) string {
	return strings.Join(calls, " | ")
}

// TestOnceCallBudget runs a whole `wr once` (version, planning, dispatch, settle) and
// counts every mw call it makes: version -j, list, next --claim --prompt, list -s,
// show (only because the item left the session), session stop.
func TestOnceCallBudget(t *testing.T) {
	r := newRig(t)
	id := r.addReady("job")
	t.Setenv("FAKE", "done")
	var out bytes.Buffer
	m := newMW(r.board)
	d := &Dispatcher{Home: r.home, Cfg: r.cfg, MW: m, Out: &out}
	start := time.Now()
	if err := m.checkVersion(); err != nil {
		t.Fatal(err)
	}
	if err := onceCommand(d, map[string]string{}); err != nil {
		t.Fatalf("once: %v\n%s", err, out.String())
	}
	took := time.Since(start)
	calls := m.Calls()
	if rs := r.runs(); len(rs) != 1 || rs[0].Outcome != "ok" || rs[0].Item != id {
		t.Fatalf("once did not finish %s: %+v\n%s", id, rs, out.String())
	}
	if len(calls) > 6 {
		t.Fatalf("once used %d mw calls, limit 6: %s", len(calls), verbs(calls))
	}
	for _, c := range calls {
		for _, gone := range []string{"session start", "start ", "get-prompt", "tenant list", "next -t", "list -t t -j -v"} {
			if strings.HasPrefix(c, gone) {
				t.Errorf("dropped call is back: %s", c)
			}
		}
	}
	if !strings.HasPrefix(calls[0], "version") || !strings.HasPrefix(calls[1], "list -t t -j") || strings.Contains(calls[1], "-v") || !strings.HasPrefix(calls[2], "next --claim") {
		t.Errorf("unexpected call order: %s", verbs(calls))
	}
	t.Logf("once: %d calls in %s: %s", len(calls), took, verbs(calls))
}

func TestStatusCallBudget(t *testing.T) {
	r := newRig(t)
	r.addReady("job")
	var out bytes.Buffer
	m := newMW(r.board)
	if err := m.checkVersion(); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{Home: r.home, Cfg: r.cfg, MW: m, Out: &out}
	if err := statusCommand(d, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	calls := m.Calls()
	// status: one list per tenant (the version call came before; tenants come from it)
	if limit := 1 + len(r.cfg.Tenants); len(calls) > limit {
		t.Fatalf("status used %d mw calls, limit %d: %s", len(calls), limit, verbs(calls))
	}
	if !strings.Contains(out.String(), "done=0/1 doing=0 blocked=0") {
		t.Fatalf("per-tenant progress missing:\n%s", out.String())
	}
	t.Logf("status: %s", verbs(calls))
}
