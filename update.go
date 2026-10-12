package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Self-update from GitHub releases: stdlib only, no token.
const (
	repoName    = "wr"
	binName     = "wr"
	releasesURL = "https://github.com/stormovermars/" + repoName + "/releases"
	installHint = "curl -fsSL https://raw.githubusercontent.com/stormovermars/wr/main/install.sh | sh   (Windows: irm https://raw.githubusercontent.com/stormovermars/wr/main/install.ps1 | iex)\nor: claude plugin marketplace update wr && claude plugin update wr@wr"
)

var (
	releaseAPI   = "https://api.github.com/repos/stormovermars/" + repoName + "/releases/latest"
	updateClient = &http.Client{Timeout: 5 * time.Second}
	exePath      = os.Executable
	// assetClient has a long overall timeout: the asset is several MB behind a redirect.
	assetClient = &http.Client{Timeout: 120 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return assetURLCheck(r.URL)
	}}
	// assetURLCheck pins download URLs to GitHub (tests swap it for the httptest host).
	assetURLCheck = func(u *url.URL) error {
		if u.Scheme == "https" && (u.Host == "github.com" || u.Host == "objects.githubusercontent.com") {
			return nil
		}
		return fmt.Errorf("refusing download from %s", u.Redacted())
	}
	// swapOld: Windows cannot overwrite the running exe, so it moves to exe.old first.
	swapOld = runtime.GOOS == "windows"
)

type ghRelease struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func latestRelease() (*ghRelease, error) {
	b, err := fetchURL(releaseAPI)
	if err != nil {
		return nil, err
	}
	var r ghRelease
	if err := json.Unmarshal(b, &r); err != nil || r.Tag == "" {
		return nil, fmt.Errorf("unexpected release data from %s", releaseAPI)
	}
	return &r, nil
}

func fetchURL(url string) ([]byte, error) { return getWith(updateClient, url) }

// getAsset downloads a release asset: GitHub hosts only (redirects included), long timeout.
func getAsset(u string) ([]byte, error) {
	pu, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	if err := assetURLCheck(pu); err != nil {
		return nil, err
	}
	return getWith(assetClient, u)
}

func getWith(c *http.Client, url string) ([]byte, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// assetName is the release.yml name for this platform, e.g. miniwaves_Darwin_arm64.tar.gz.
func assetName(goos, goarch string) string {
	osn := map[string]string{"darwin": "Darwin", "linux": "Linux", "windows": "Windows"}[goos]
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[goarch]
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return repoName + "_" + osn + "_" + arch + ext
}

// newerVersion reports whether b is a newer x.y.z than a.
func newerVersion(a, b string) bool {
	return semverLess(strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v"))
}

func updateCommand(out io.Writer, args []string) error {
	check := len(args) == 1 && args[0] == "--check"
	if len(args) > 0 && !check {
		return errors.New("usage: wr update [--check]")
	}
	rel, err := latestRelease()
	if err != nil {
		return fmt.Errorf("cannot read the latest release (%v); see %s", err, releasesURL)
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	if !newerVersion(version, latest) {
		mwHint(out, MinMW)
		return &exitError{Code: 2, Msg: fmt.Sprintf("wr %s is current (latest release %s)", version, latest)}
	}
	if check {
		fmt.Fprintf(out, "update available: %s → %s (wr update)\n", version, latest)
		return nil
	}
	bin, err := fetchVerified(rel, assetName(runtime.GOOS, runtime.GOARCH))
	if err != nil {
		return err
	}
	exe, err := exePath()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if err := replaceExe(exe, bin); err != nil {
		return fmt.Errorf("cannot replace %s (%v); install the new version with:\n%s", exe, err, installHint)
	}
	fmt.Fprintf(out, "wr %s → %s (%s)\n", version, latest, exe)
	// the new wr may need a newer mw: ask it for its min_mw
	var nv struct {
		MinMW string `json:"min_mw"`
	}
	if b, err := exec.Command(exe, "version", "-j").Output(); err == nil && json.Unmarshal(b, &nv) == nil && nv.MinMW != "" {
		mwHint(out, nv.MinMW)
	}
	return nil
}

// mwHint tells the user to update mw when the installed one is older than min.
func mwHint(out io.Writer, min string) {
	b, err := exec.Command(newMW("").Bin, "version", "-j").Output()
	var v struct {
		Version string `json:"version"`
	}
	if err != nil || json.Unmarshal(b, &v) != nil || v.Version == "" {
		fmt.Fprintf(out, "mw not found: wr needs mw >= %s; %s\n", min, mwInstallHint)
	} else if semverLess(v.Version, min) {
		fmt.Fprintf(out, "update mw too: mw %s is older than %s (run: mw update)\n", v.Version, min)
	}
}

// updateNotice is wr doctor's line: a newer release, checked at most once a day (cached in
// WR_HOME/update-check.json, failures too; WR_NO_UPDATE_CHECK=1 skips it); "" when current, offline or on any error.
func updateNotice(home string) string {
	if os.Getenv("WR_NO_UPDATE_CHECK") == "1" {
		return ""
	}
	p := filepath.Join(home, "update-check.json")
	today := time.Now().Format("2006-01-02")
	var c struct{ Date, Latest string }
	if b, err := os.ReadFile(p); err != nil || json.Unmarshal(b, &c) != nil || c.Date != today {
		c.Date, c.Latest = today, ""
		if rel, err := latestRelease(); err == nil {
			c.Latest = strings.TrimPrefix(rel.Tag, "v")
		}
		if b, err := json.Marshal(c); err == nil { // failures are cached too: one attempt a day
			_ = os.WriteFile(p, b, 0o600)
		}
	}
	if c.Latest != "" && newerVersion(version, c.Latest) {
		return fmt.Sprintf("update available: %s → %s (wr update)", version, c.Latest)
	}
	return ""
}

// fetchVerified downloads the asset and checksums.txt, checks the sha256 and returns the binary.
func fetchVerified(rel *ghRelease, name string) ([]byte, error) {
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	if urls[name] == "" || urls["checksums.txt"] == "" {
		return nil, fmt.Errorf("release %s has no %s (or checksums.txt); see %s", rel.Tag, name, releasesURL)
	}
	arc, err := getAsset(urls[name])
	if err != nil {
		return nil, err
	}
	sums, err := getAsset(urls["checksums.txt"])
	if err != nil {
		return nil, err
	}
	want := ""
	for _, l := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(l); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = strings.ToLower(f[0])
		}
	}
	got := sha256.Sum256(arc)
	if want == "" || want != hex.EncodeToString(got[:]) {
		return nil, fmt.Errorf("checksum mismatch for %s: not installed", name)
	}
	return extractBinary(arc, strings.HasSuffix(name, ".zip"))
}

func extractBinary(arc []byte, isZip bool) ([]byte, error) {
	if isZip {
		zr, err := zip.NewReader(bytes.NewReader(arc), int64(len(arc)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if f.Name == binName+".exe" {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, errors.New(binName + ".exe not in the archive")
	}
	gz, err := gzip.NewReader(bytes.NewReader(arc))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errors.New(binName + " not in the archive")
		}
		if h.Name == binName || h.Name == "./"+binName {
			return io.ReadAll(tr)
		}
	}
}

// replaceExe writes the new binary next to exe and renames it over; on Windows the running
// exe cannot be overwritten but can be renamed, so it moves to exe.old first.
func replaceExe(exe string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(exe), "."+filepath.Base(exe)+".*.new")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o755)
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	if swapOld {
		_ = os.Remove(exe + ".old")
		if err := renameRetry(exe, exe+".old"); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := renameRetry(tmp, exe); err != nil {
			os.Remove(tmp)
			if rerr := renameRetry(exe+".old", exe); rerr != nil {
				return fmt.Errorf("%v; restoring %s.old failed too: %v", err, exe, rerr)
			}
			return err
		}
		return nil
	}
	if err := renameRetry(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
