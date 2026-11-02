package wr

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeRelease(t *testing.T, tag string, payload []byte, badSum bool) {
	t.Helper()
	oldAPI, oldPartner, oldExe, oldChk := releaseAPI, partnerAPI, exePath, assetURLCheck
	t.Cleanup(func() { releaseAPI, partnerAPI, exePath, assetURLCheck = oldAPI, oldPartner, oldExe, oldChk })
	assetURLCheck = func(u *url.URL) error {
		if u.Hostname() == "127.0.0.1" {
			return nil
		}
		return fmt.Errorf("refusing %s", u.Host)
	}
	releaseAPI = fakeServer(t, repoName, binName, tag, payload, badSum)
}

// fakePartner serves an mw release the same way (fakeRelease must run first).
func fakePartner(t *testing.T, tag string, payload []byte, badSum bool) {
	partnerAPI = fakeServer(t, partnerRepo, partnerBin, tag, payload, badSum)
}

// fakeServer serves one release of repo with bin inside; it returns the /latest URL.
func fakeServer(t *testing.T, repo, bin, tag string, payload []byte, badSum bool) string {
	t.Helper()
	name := assetFor(repo, runtime.GOOS, runtime.GOARCH)
	var arc bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&arc)
		w, _ := zw.Create(bin + ".exe")
		w.Write(payload)
		zw.Close()
	} else {
		gz := gzip.NewWriter(&arc)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: bin, Mode: 0o755, Size: int64(len(payload))})
		tw.Write(payload)
		tw.Close()
		gz.Close()
	}
	sum := sha256.Sum256(arc.Bytes())
	if badSum {
		sum[0] ^= 0xff
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":"%s/a"},{"name":"checksums.txt","browser_download_url":"%s/sums"}]}`, tag, name, srv.URL, srv.URL)
		case "/a":
			w.Write(arc.Bytes())
		case "/sums":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/latest"
}

func fakeExe(t *testing.T) string {
	exe := filepath.Join(t.TempDir(), binName)
	os.WriteFile(exe, []byte("old"), 0o755)
	exePath = func() (string, error) { return exe, nil }
	return exe
}

func TestUpdate(t *testing.T) {
	fakeRelease(t, "v9.9.9", []byte("new binary"), false)
	exe := fakeExe(t)
	var out bytes.Buffer
	if err := run([]string{"update", "--check"}, &out); err != nil || !strings.HasPrefix(out.String(), "update available: "+version+" → 9.9.9 (wr update)\n") {
		t.Fatalf("--check: %v %q", err, out.String())
	}
	out.Reset()
	if err := run([]string{"update"}, &out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" || !strings.Contains(out.String(), "→ 9.9.9") {
		t.Fatalf("binary %q out %q", b, out.String())
	}
}

func TestUpdateCurrentAndBadChecksum(t *testing.T) {
	fakeRelease(t, "v"+version, []byte("x"), false)
	fakeExe(t)
	var ee *exitError
	if err := run([]string{"update"}, &bytes.Buffer{}); !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("want exit 2, got %v", err)
	}
	fakeRelease(t, "v9.9.9", []byte("evil"), true)
	exe := fakeExe(t)
	if err := run([]string{"update"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("replaced despite bad checksum")
	}
}

// wr update installs a missing mw next to wr even when wr itself is current.
func TestUpdateInstallsPartner(t *testing.T) {
	fakeRelease(t, "v"+version, []byte("x"), false)
	fakePartner(t, "v9.9.9", []byte("mw binary"), false)
	exe := fakeExe(t)
	mw := filepath.Join(filepath.Dir(exe), partnerBin+filepath.Ext(exe))
	var out bytes.Buffer
	if err := run([]string{"update", "--check"}, &out); err != nil || !strings.Contains(out.String(), "mw 9.9.9 available, not installed") {
		t.Fatalf("--check: %v %q", err, out.String())
	}
	if _, err := os.Stat(mw); err == nil {
		t.Fatal("--check installed mw")
	}
	out.Reset()
	if err := run([]string{"update"}, &out); err != nil || !strings.Contains(out.String(), "mw (new) → 9.9.9") {
		t.Fatalf("update: %v %q", err, out.String())
	}
	if b, _ := os.ReadFile(mw); string(b) != "mw binary" {
		t.Fatalf("mw %q", b)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("current wr replaced")
	}
}

// A failed mw download is one note line; wr's own update and exit status stand.
func TestUpdatePartnerFailureIsANote(t *testing.T) {
	for _, partner := range []string{"unreachable", "bad checksum"} {
		fakeRelease(t, "v9.9.9", []byte("new binary"), false)
		if partner == "bad checksum" {
			fakePartner(t, "v9.9.9", []byte("evil"), true)
		}
		exe := fakeExe(t)
		var out bytes.Buffer
		if err := run([]string{"update"}, &out); err != nil || !strings.Contains(out.String(), "mw: not updated: ") || !strings.Contains(out.String(), "retry: wr update") {
			t.Fatalf("%s: %v %q", partner, err, out.String())
		}
		if b, _ := os.ReadFile(exe); string(b) != "new binary" {
			t.Fatalf("%s: wr %q", partner, b)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(exe), partnerBin+filepath.Ext(exe))); err == nil {
			t.Fatalf("%s: mw written", partner)
		}
	}
}

// wr's own failure is the exit status; mw is still updated.
func TestUpdateSelfFailureStillUpdatesPartner(t *testing.T) {
	fakeRelease(t, "v9.9.9", []byte("evil"), true)
	fakePartner(t, "v9.9.9", []byte("mw binary"), false)
	exe := fakeExe(t)
	if err := run([]string{"update"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want wr's checksum error, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(exe), partnerBin+filepath.Ext(exe))); string(b) != "mw binary" {
		t.Fatalf("mw %q", b)
	}
}

func TestDoctorUpdateNotice(t *testing.T) {
	home := t.TempDir()
	if s := updateNotice(home); s != "" { // offline (TestMain points at a closed port): quiet
		t.Fatalf("offline notice %q", s)
	}
	if _, err := os.Stat(filepath.Join(home, "update-check.json")); err != nil {
		t.Fatalf("failure not cached: %v", err)
	}
	os.Remove(filepath.Join(home, "update-check.json"))
	fakeRelease(t, "v9.9.9", nil, false)
	t.Setenv("WR_NO_UPDATE_CHECK", "1")
	if s := updateNotice(home); s != "" {
		t.Fatalf("opt-out ignored: %q", s)
	}
	t.Setenv("WR_NO_UPDATE_CHECK", "")
	if s := updateNotice(home); s != "update available: "+version+" → 9.9.9 (wr update)" {
		t.Fatalf("notice %q", s)
	}
	releaseAPI = "http://127.0.0.1:1/latest"
	if s := updateNotice(home); !strings.Contains(s, "9.9.9") {
		t.Fatalf("cached notice lost: %q", s)
	}
}

func TestAssetURLPinned(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://github.com/x/y/releases/download/v1/a.tar.gz": true,
		"https://objects.githubusercontent.com/a":              true,
		"https://release-assets.githubusercontent.com/a":       true,
		"http://github.com/a":                                  false,
		"https://evil.example/a":                               false,
		"https://github.com.evil.example/a":                    false,
	} {
		pu, _ := url.Parse(u)
		if err := assetURLCheck(pu); (err == nil) != ok {
			t.Errorf("%s: err %v, want ok=%v", u, err, ok)
		}
	}
	if _, err := getAsset("https://evil.example/a"); err == nil {
		t.Error("getAsset accepted a foreign host")
	}
}

// A failed second rename puts the old exe back.
func TestReplaceExeRollback(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tool.exe")
	os.WriteFile(exe, []byte("old"), 0o755)
	oldFn, oldSwap, oldOn := renameFn, swapOld, renameRetryOn
	t.Cleanup(func() { renameFn, swapOld, renameRetryOn = oldFn, oldSwap, oldOn })
	swapOld, renameRetryOn = true, false
	n := 0
	renameFn = func(a, b string) error {
		if n++; n == 2 {
			return errors.New("sharing violation")
		}
		return os.Rename(a, b)
	}
	if err := replaceExe(exe, []byte("new")); err == nil {
		t.Fatal("want error")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("exe %q after failed swap", b)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.new")); len(m) != 0 {
		t.Fatalf("temp left: %v", m)
	}
}
