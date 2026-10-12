package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTagsAndArchivedTenants(t *testing.T) {
	r := newRig(t)
	r.addReady("tagged work")
	sh(t, r.board, mwBin, "tenant", "add", "gone", "Gone", "-d", r.tdir)
	sh(t, r.board, mwBin, "tenant", "tag", "t", "+android", "+store")
	sh(t, r.board, mwBin, "tenant", "archive", "gone")
	r.cfg.Tenants["gone"] = TenantCfg{Engine: "fake", Role: "builder", Enabled: true}
	r.save()
	d, _ := r.disp()
	rec := httptest.NewRecorder()
	uiHandler(d).ServeHTTP(rec, httptest.NewRequest("GET", "/api", nil))
	var doc uiDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err, rec.Body)
	}
	tags := map[string]string{}
	for _, x := range doc.Tenants {
		tags[x.Slug] = strings.Join(x.Tags, ",")
	}
	if _, gone := tags["gone"]; doc.Archived != 1 || gone || tags["t"] != "android,store" {
		t.Fatalf("api tenants %+v archived %d", doc.Tenants, doc.Archived)
	}
	if !strings.Contains(rec.Body.String(), `"tags"`) {
		t.Fatal("api lacks tags")
	}
	if _, why, _ := d.plan("gone"); !strings.Contains(why, "archived") {
		t.Fatalf("archived tenant planned: %q", why)
	}
	var out bytes.Buffer
	if err := run([]string{"status"}, &out); err != nil && !strings.Contains(err.Error(), "exit") {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "#android #store") || !strings.Contains(s, "archived: 1 tenant hidden") || strings.Contains(s, "\ngone ") {
		t.Fatalf("status:\n%s", s)
	}
	out.Reset()
	_ = run([]string{"ui", "--once", "--tag", "ios"}, &out)
	if s := out.String(); strings.Contains(s, "tagged work") {
		t.Fatalf("--tag ios still shows t:\n%s", s)
	}
	out.Reset()
	_ = run([]string{"ui", "--once", "--tag", "android", "--tag", "store"}, &out)
	if s := out.String(); !strings.Contains(s, "t #android #store") || !strings.Contains(s, "archived: 1 tenant hidden") {
		t.Fatalf("ui --tag AND:\n%s", s)
	}
}
