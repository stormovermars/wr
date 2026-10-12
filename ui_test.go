package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// uiRig: one live run on tenant t (item J), plus ready/inbox items.
func uiRig(t *testing.T) *rig {
	r := newRig(t)
	id := r.addReady("ship the thing")
	sh(t, r.board, mwBin, "add", "task", "idea", "-t", "t", "-c", "works")
	r.seedRun("wr-t-live-1", id, time.Now().Add(-2*time.Minute), liveGroup(t), nil, false)
	return r
}

func TestUITerminalOnce(t *testing.T) {
	r := uiRig(t)
	var out bytes.Buffer
	if err := run([]string{"ui", "--once"}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, w := range []string{"LIVE RUNS", "TENANTS", "ACTIVITY", "ship the thing", "ready", "inbox", "live 1/"} {
		if !strings.Contains(s, w) {
			t.Fatalf("frame lacks %q:\n%s", w, s)
		}
	}
	_ = r
}

func TestUIAPI(t *testing.T) {
	r := uiRig(t)
	d, _ := r.disp()
	rec := httptest.NewRecorder()
	uiHandler(d).ServeHTTP(rec, httptest.NewRequest("GET", "/api", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var doc uiDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Runs) != 1 || doc.Runs[0].Tenant != "t" || doc.Runs[0].Title != "ship the thing" || doc.Runs[0].Lost || doc.Runs[0].AgeS < 100 {
		t.Fatalf("runs: %+v", doc.Runs)
	}
	if doc.Caps.Global.In != 1 || doc.Caps.Tenant["t"].In != 1 || doc.Serve.Running || doc.Cost.Days == nil {
		t.Fatalf("caps/serve/cost: %+v %+v %+v", doc.Caps, doc.Serve, doc.Cost)
	}
	var tt *uiTenant
	for i := range doc.Tenants {
		if doc.Tenants[i].Slug == "t" {
			tt = &doc.Tenants[i]
		}
	}
	if tt == nil || tt.Counts["ready"] != 1 || tt.Counts["inbox"] != 1 || len(tt.Items) != 2 || tt.Items[0].State != "ready" {
		t.Fatalf("tenant t: %+v", tt)
	}
	if len(doc.Activity) == 0 || doc.Activity[0].Tenant != "t" {
		t.Fatalf("activity: %+v", doc.Activity)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, k := range []string{"serve", "caps", "cost", "runs", "last", "tenants", "activity"} {
		if raw[k] == nil {
			t.Fatalf("missing %q", k)
		}
	}
}

func TestUIPageServed(t *testing.T) {
	r := newRig(t)
	d, _ := r.disp()
	rec := httptest.NewRecorder()
	uiHandler(d).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>wr ui</title>") {
		t.Fatalf("%d %.200s", rec.Code, rec.Body)
	}
	if _, err := listenLoopback("0.0.0.0:0"); err == nil {
		t.Fatal("non-loopback bind must be refused")
	}
}
