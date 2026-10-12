package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func httpGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, b)
	}
	return string(b)
}

var doneRe = regexp.MustCompile(`"done":\s*1`)

func exportedData(t *testing.T, path string) []byte {
	t.Helper()
	page, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)
	i := strings.Index(s, "window.__WR_DATA__=")
	if i < 0 {
		t.Fatalf("export has no inlined data: %.200s", s)
	}
	s = s[i+len("window.__WR_DATA__="):]
	return []byte(s[:strings.Index(s, ";</script>")])
}

// TestConfigurationMatrix runs every combination of mw publish.auto x wr publish.html
// x wr ui.web: once with the fake engine, then every view that applies.
func TestConfigurationMatrix(t *testing.T) {
	yn := func(b bool) string { return map[bool]string{true: "on", false: "off"}[b] }
	var rows []string
	for _, auto := range []bool{false, true} {
		for _, pub := range []bool{false, true} {
			for _, web := range []bool{false, true} {
				name := fmt.Sprintf("mw.auto=%s wr.publish=%s ui.web=%s", yn(auto), yn(pub), yn(web))
				t.Run(name, func(t *testing.T) {
					r := newRig(t)
					id := r.addReady("job")
					if auto {
						bp := filepath.Join(r.board, ".miniwaves.json")
						b, _ := os.ReadFile(bp)
						var raw map[string]any
						_ = json.Unmarshal(b, &raw)
						raw["publish"] = map[string]any{"html": "site/index.html", "auto": true, "interval": 5}
						nb, _ := json.Marshal(raw)
						if err := os.WriteFile(bp, nb, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					wrPage := filepath.Join(r.board, "site", "wr.html")
					if pub {
						r.cfg.Publish.HTML, r.cfg.Publish.Interval = wrPage, 5
					}
					r.cfg.UI.Web = web
					r.save()
					out := r.once("done")
					if !strings.Contains(out, "-> ok") || r.state(id).State != "done" {
						t.Fatalf("once did not finish the item:\n%s", out)
					}
					checked := []string{"once"}
					if !web { // with ui.web on, `wr ui` starts the web server: the handler is exercised below
						var ui bytes.Buffer
						if err := run([]string{"ui", "--once"}, &ui); err != nil || ui.Len() == 0 {
							t.Fatalf("ui --once: %v %q", err, ui.String())
						}
						checked = append(checked, "ui --once")
					}
					if pub {
						if !strings.Contains(string(exportedData(t, wrPage)), `"done":1`) {
							t.Fatal("wr's published page misses the done item")
						}
						exp := filepath.Join(t.TempDir(), "e.html")
						if err := run([]string{"ui", "--export", exp}, io.Discard); err != nil {
							t.Fatal(err)
						}
						if !strings.Contains(string(exportedData(t, exp)), `"done":1`) {
							t.Fatal("ui --export misses the done item")
						}
						checked = append(checked, "wr page", "ui --export")
					}
					if web {
						d := &Dispatcher{Home: r.home, Cfg: r.cfg, MW: newMW(r.board), Out: io.Discard}
						srv := httptest.NewServer(uiHandler(d))
						defer srv.Close()
						if p := httpGet(t, srv.URL+"/"); !strings.Contains(p, "<html") {
							t.Fatalf("/ is not a page: %.100s", p)
						}
						if a := httpGet(t, srv.URL+"/api"); !doneRe.MatchString(a) {
							t.Fatalf("/api misses the done item: %.300s", a)
						}
						checked = append(checked, "/", "/api")
					}
					if auto {
						pg, err := os.ReadFile(filepath.Join(r.board, "site", "index.html"))
						if err != nil || !strings.Contains(string(pg), id) {
							t.Fatalf("mw publish.auto page missing or stale: %v", err)
						}
						checked = append(checked, "mw page")
					}
					if ev, _ := os.ReadFile(filepath.Join(r.home, "events.log")); strings.Contains(string(ev), "error") {
						t.Fatalf("errors in events.log:\n%s", ev)
					}
					rows = append(rows, fmt.Sprintf("%-44s ok  %s", name, strings.Join(checked, ", ")))
				})
			}
		}
	}
	t.Logf("configuration matrix (8 rows):\n%s", strings.Join(rows, "\n"))
}

// TestPublishThrottle: 3 exports inside the interval write once, an unchanged page never
// again, a change after the interval writes again, and a change inside it lands at the
// interval's end through the scheduled write.
func TestPublishThrottle(t *testing.T) {
	r := newRig(t)
	page := filepath.Join(r.board, "site", "wr.html")
	r.cfg.Publish.HTML, r.cfg.Publish.Interval = page, 1 // clamps to 5 s
	if r.cfg.Publish.interval() != 5*time.Second || (PublishCfg{}).interval() != 60*time.Second {
		t.Fatal("interval default/clamp")
	}
	clock := time.Now()
	var fired []func()
	var delays []time.Duration
	old := afterFunc
	afterFunc = func(dl time.Duration, f func()) *time.Timer {
		delays = append(delays, dl)
		fired = append(fired, f)
		return nil
	}
	defer func() { afterFunc = old }()
	d := &Dispatcher{Home: r.home, Cfg: r.cfg, MW: newMW(r.board), Out: io.Discard, Now: func() time.Time { return clock }}
	if err := d.MW.checkVersion(); err != nil {
		t.Fatal(err)
	}
	d.publishPage()
	for i := 0; i < 2; i++ {
		d.publishPage()
	}
	if d.pubWrites != 1 {
		t.Fatalf("3 publishes inside the interval wrote %d pages", d.pubWrites)
	}
	r.addReady("change inside the interval")
	d.publishPage()
	d.publishPage()
	if d.pubWrites != 1 || len(fired) != 1 || delays[0] <= 0 || delays[0] > 5*time.Second {
		t.Fatalf("a change inside the interval must schedule exactly one write: writes=%d timers=%v", d.pubWrites, delays)
	}
	fired[0]() // fires early (interval not over yet): it must reschedule, not drop the change
	if len(fired) != 2 || d.pubWrites != 1 {
		t.Fatalf("early timer must reschedule: timers=%d writes=%d", len(fired), d.pubWrites)
	}
	clock = clock.Add(6 * time.Second)
	fired[1]() // the scheduled write at the interval's end
	if d.pubWrites != 2 {
		t.Fatalf("scheduled write did not land: %d", d.pubWrites)
	}
	if !strings.Contains(string(exportedData(t, page)), "change inside the interval") {
		t.Fatal("page is stale after the scheduled write")
	}
	clock = clock.Add(time.Minute)
	d.publishPage() // unchanged data: nothing
	if d.pubWrites != 2 {
		t.Fatalf("an unchanged board wrote the page: %d", d.pubWrites)
	}
	r.addReady("after the interval")
	d.publishPage() // interval long over: immediate
	if d.pubWrites != 3 {
		t.Fatalf("a change after the interval must write now: %d", d.pubWrites)
	}
	exp := filepath.Join(t.TempDir(), "x.html")
	if err := exportUI(d, exp); err != nil || d.pubWrites != 4 {
		t.Fatalf("explicit export must always write: %v %d", err, d.pubWrites)
	}
	// a pending write is flushed at exit
	r.addReady("pending at exit")
	clock = clock.Add(time.Second)
	d.publishPage()
	d.flushPublish()
	if d.pubWrites != 6 && d.pubWrites != 5 {
		t.Fatalf("flush: %d", d.pubWrites)
	}
	if !strings.Contains(string(exportedData(t, page)), "pending at exit") {
		t.Fatal("pending page not flushed")
	}
}
