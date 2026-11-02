package wr

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
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
		if u.Scheme == "https" && (u.Host == "github.com" || u.Host == "objects.githubusercontent.com" || u.Host == "release-assets.githubusercontent.com") {
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

func latestRelease() (*ghRelease, error) { return releaseFrom(releaseAPI) }

func releaseFrom(api string) (*ghRelease, error) {
	b, err := fetchURL(api)
	if err != nil {
		return nil, err
	}
	var r ghRelease
	if err := json.Unmarshal(b, &r); err != nil || r.Tag == "" {
		return nil, fmt.Errorf("unexpected release data from %s", api)
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

// assetName is the release.yml name for this platform, e.g. wr_Darwin_arm64.tar.gz.
func assetName(goos, goarch string) string { return assetFor(repoName, goos, goarch) }

// assetFor is the release asset name of repo (wr or miniwaves) for a platform.
func assetFor(repo, goos, goarch string) string {
	osn := map[string]string{"darwin": "Darwin", "linux": "Linux", "windows": "Windows"}[goos]
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[goarch]
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return repo + "_" + osn + "_" + arch + ext
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
	exe, err := exePath()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	changed, err := updateSelf(out, exe, check)
	// mw is updated with wr (latest release of each); its failure is a note, never wr's exit status.
	if updatePartner(out, exe, check) {
		changed = true
	}
	if !check {
		mwHint(out, MinMW) // still too old (or missing) after a failed mw download
	}
	if err != nil {
		return err
	}
	if !changed { // --check: 0 when something can be updated
		return &exitError{Code: 2, Msg: fmt.Sprintf("wr %s and mw are current", version)}
	}
	return nil
}

// updateSelf replaces exe with the latest wr release and reports whether it did
// (check: only says whether one exists).
func updateSelf(out io.Writer, exe string, check bool) (bool, error) {
	rel, err := latestRelease()
	if err != nil {
		return false, fmt.Errorf("cannot read the latest release (%v); see %s", err, releasesURL)
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	if !newerVersion(version, latest) {
		fmt.Fprintf(out, "wr %s is current (latest release %s)\n", version, latest)
		return false, nil
	}
	if check {
		fmt.Fprintf(out, "update available: %s → %s (wr update)\n", version, latest)
		return true, nil
	}
	bin, err := fetchVerified(rel, repoName, binName)
	if err != nil {
		return false, err
	}
	if err := replaceExe(exe, bin); err != nil {
		return false, fmt.Errorf("cannot replace %s (%v); install the new version with:\n%s", exe, err, installHint)
	}
	fmt.Fprintf(out, "wr %s → %s (%s)\n", version, latest, exe)
	return true, nil
}

// The partner binary: wr and mw install and update together, each at its latest release.
const partnerRepo, partnerBin = "miniwaves", "mw"

var partnerAPI = "https://api.github.com/repos/stormovermars/" + partnerRepo + "/releases/latest"

// updatePartner installs or updates mw next to exe, where the installers put it
// (best effort: a failure is one note line).
// It reports whether it changed mw (check: whether it would).
func updatePartner(w io.Writer, exe string, check bool) bool {
	p := filepath.Join(filepath.Dir(exe), partnerBin+filepath.Ext(exe))
	have := binVersion(p) // "" when missing or not runnable
	rel, err := releaseFrom(partnerAPI)
	if err != nil {
		fmt.Fprintf(w, "mw: not updated: cannot read the latest release (%v); retry: wr update\n", err)
		return false
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	switch {
	case have != "" && !newerVersion(have, latest):
		fmt.Fprintf(w, "mw %s is current (latest release %s)\n", have, latest)
		return false
	case check && have == "":
		fmt.Fprintf(w, "mw %s available, not installed at %s (wr update installs it)\n", latest, p)
		return true
	case check:
		fmt.Fprintf(w, "update available: mw %s → %s (wr update)\n", have, latest)
		return true
	}
	bin, err := fetchVerified(rel, partnerRepo, partnerBin)
	if err == nil {
		err = replaceExe(p, bin)
	}
	if err != nil {
		fmt.Fprintf(w, "mw: not updated: %s; retry: wr update\n", strings.ReplaceAll(err.Error(), "\n", " "))
		return false
	}
	if have == "" {
		have = "(new)"
	}
	fmt.Fprintf(w, "mw %s → %s (%s)\n", have, latest, p)
	return true
}

// binVersion is the version a wr or mw binary reports in `version -j` ("" when it cannot say).
func binVersion(p string) string {
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, p, "version", "-j").Output()
	var v struct {
		Version string `json:"version"`
	}
	if err != nil || json.Unmarshal(b, &v) != nil {
		return ""
	}
	return v.Version
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

// fetchVerified downloads repo's asset for this platform and checksums.txt, checks the sha256
// and returns the binary bin from the archive.
func fetchVerified(rel *ghRelease, repo, bin string) ([]byte, error) {
	name := assetFor(repo, runtime.GOOS, runtime.GOARCH)
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	if urls[name] == "" || urls["checksums.txt"] == "" {
		return nil, fmt.Errorf("release %s has no %s (or checksums.txt); see https://github.com/stormovermars/%s/releases", rel.Tag, name, repo)
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
	return extractBinary(arc, strings.HasSuffix(name, ".zip"), bin)
}

func extractBinary(arc []byte, isZip bool, binName string) ([]byte, error) {
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
	if _, err := os.Stat(exe); swapOld && err == nil {
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
