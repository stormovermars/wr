package wr

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginFiles(t *testing.T) {
	for _, f := range []string{".claude-plugin/marketplace.json", "plugins/wr/.claude-plugin/plugin.json", "plugins/wr/hooks/hooks.json"} {
		b, err := os.ReadFile(repo(f))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	var pj struct{ Version string }
	b, _ := os.ReadFile(repo("plugins/wr/.claude-plugin/plugin.json"))
	_ = json.Unmarshal(b, &pj)
	if pj.Version != version {
		t.Errorf("plugin version %s != wr %s", pj.Version, version)
	}
	sc, _ := os.ReadFile(repo("plugins/wr/scripts/ensure-wr.sh"))
	if !strings.Contains(string(sc), `want="`+version+`"`) || !strings.Contains(string(sc), `min_mw="`+MinMW+`"`) {
		t.Error("ensure-wr.sh versions drifted from version/MinMW")
	}
	if out, err := exec.Command("bash", "-n", repo("plugins/wr/scripts/ensure-wr.sh")).CombinedOutput(); err != nil {
		t.Errorf("bash -n: %v\n%s", err, out)
	}
	for _, f := range []string{"install.sh", "uninstall.sh", "scripts/install-mw.sh"} {
		if out, err := exec.Command("sh", "-n", repo(f)).CombinedOutput(); err != nil {
			t.Errorf("sh -n %s: %v\n%s", f, err, out)
		}
	}
	sk, _ := os.ReadFile(repo("plugins/wr/skills/wr/SKILL.md"))
	if n := strings.Count(string(sk), "\n"); n > 50 {
		t.Errorf("SKILL.md has %d lines, max 50", n)
	}
	rd, _ := os.ReadFile(repo("README.md"))
	if n := strings.Count(string(rd), "\n"); n > 50 {
		t.Errorf("README.md has %d lines, max 50", n)
	}
}

// repo resolves a repo-root path from this package's directory (internal/wr).
func repo(p string) string { return filepath.Join("..", "..", p) }
