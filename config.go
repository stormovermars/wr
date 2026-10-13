package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// defaultConfig is the literal from docs/specs/wr.md; written on first run.
const defaultConfig = `{"version":2,"tick_min":15,
 "caps":{"per_hour":4,"parallel_global":3,"parallel_tenant":1,"usd_per_day":{"claude":20},"run_timeout_min":45,"stale_minutes":20,"max_attempts":2,"disk_free_gb":5},
 "pause":{"unjudged":5,"on_blocked":true,"quality_guard":true},
 "engines":{"claude":{"cmd":["claude","-p","--permission-mode","bypassPermissions","--output-format","stream-json","--verbose","--session-id","{uuid}","--max-budget-usd","{usd}"],"usage":"claude_stream","enabled":true},
            "cmd":{"cmd":[],"usage":"none","enabled":false}},
 "tenants":{"example":{"engine":"claude","role":"builder","forbid":[],"dir":null,"enabled":false}},
 "self_dispatch":false}
`

type Caps struct {
	PerHour        int                `json:"per_hour"`
	ParallelGlobal int                `json:"parallel_global"`
	ParallelTenant int                `json:"parallel_tenant"`
	USDPerDay      map[string]float64 `json:"usd_per_day"`
	RunTimeoutMin  float64            `json:"run_timeout_min"`
	StaleMinutes   float64            `json:"stale_minutes"`
	MaxAttempts    int                `json:"max_attempts"`
	DiskFreeGB     int                `json:"disk_free_gb"`
}

type Pause struct {
	Unjudged     int  `json:"unjudged"`
	OnBlocked    bool `json:"on_blocked"`
	QualityGuard bool `json:"quality_guard"`
}

type Engine struct {
	Cmd     []string `json:"cmd"`
	Usage   string   `json:"usage"`
	Enabled bool     `json:"enabled"`
}

type TenantCfg struct {
	Engine  string   `json:"engine"`
	Role    string   `json:"role"`
	Forbid  []string `json:"forbid"`
	Dir     *string  `json:"dir"`
	Enabled bool     `json:"enabled"`
}

// UICfg: `"ui": {"web": true, "addr": "127.0.0.1:5052"}` turns the web page on for `wr ui`.
type UICfg struct {
	Web  bool   `json:"web,omitempty"`
	Addr string `json:"addr,omitempty"`
}

// PublishCfg: `"publish": {"html": "PATH"}` writes the `wr ui` page (data inlined)
// to PATH after every tick and after each run settles. Off by default.
type PublishCfg struct {
	HTML string `json:"html,omitempty"`
	// Interval: seconds between page writes (default 60, minimum 5; lower values clamp to 5, doctor says so).
	Interval int `json:"interval,omitempty"`
}

type Config struct {
	Version      int                  `json:"version"`
	TickMin      int                  `json:"tick_min"`
	Caps         Caps                 `json:"caps"`
	Pause        Pause                `json:"pause"`
	Engines      map[string]Engine    `json:"engines"`
	Tenants      map[string]TenantCfg `json:"tenants"`
	SelfDispatch bool                 `json:"self_dispatch"`
	UI           UICfg                `json:"ui"`
	Publish      PublishCfg           `json:"publish"`
	Reap         *ReapCfg             `json:"reap,omitempty"`
	Order        []string             `json:"-"` // tenant keys in file order
}

// wrHome returns the state directory: WR_HOME or ~/.wr.
func wrHome() (string, error) {
	if h := os.Getenv("WR_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".wr"), nil
}

func configPath(home string) string { return filepath.Join(home, "config.json") }

// loadConfig reads the config, writing the default first if it is absent.
func loadConfig(home string) (*Config, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	p := configPath(home)
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(p, []byte(defaultConfig), 0o600); err != nil {
			return nil, err
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if c.Version != 2 {
		return nil, fmt.Errorf("%s: config version %d unsupported, wr needs version 2", p, c.Version)
	}
	for t := range c.Tenants {
		if !slugPattern.MatchString(t) || len(t) > 40 {
			return nil, fmt.Errorf("%s: tenant name %q must be a lowercase slug (a-z, 0-9, dashes)", p, t)
		}
	}
	c.Order = tenantOrder(b)
	return &c, nil
}

// tenantOrder returns the keys of the top-level "tenants" object in file order
// (a Go map forgets it).
func tenantOrder(b []byte) []string {
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(top["tenants"]))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			break
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			break
		}
	}
	return keys
}

// tenantsInOrder is Order plus any tenant (e.g. test-built) missing from it, sorted.
func (c *Config) tenantsInOrder() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range c.Order {
		if _, ok := c.Tenants[t]; ok && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	var rest []string
	for t := range c.Tenants {
		if !seen[t] {
			rest = append(rest, t)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func saveConfig(home string, c *Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := configPath(home) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return renameRetry(tmp, configPath(home))
}

func (c *Config) timeout() int64 {
	m := c.Caps.RunTimeoutMin
	if m <= 0 {
		m = 45
	}
	return int64(m * 60 * 1e9)
}

// stale is the no-transcript-write limit after which a run is killed.
func (c *Config) stale() time.Duration {
	m := c.Caps.StaleMinutes
	if m <= 0 {
		m = 20
	}
	return time.Duration(m * float64(time.Minute))
}

func (c *Config) maxAttempts() int {
	if c.Caps.MaxAttempts <= 0 {
		return 2
	}
	return c.Caps.MaxAttempts
}

// isSelfTenant reports tenants wr will not dispatch on unless self_dispatch.
func isSelfTenant(slug string) bool {
	return slug == "wr" || slug == "miniwaves" || (len(slug) >= 5 && slug[:5] == "fleet")
}
