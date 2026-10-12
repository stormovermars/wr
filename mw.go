package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MW is the only surface wr uses to talk to the board: the mw binary.
type MW struct {
	Bin   string // from MW env, default "mw"
	Board string // directory holding .miniwaves.json; mw runs with cwd = Board

	Schema    int    // board schema, set by checkVersion (0 = no board)
	Source    string // where Schema came from
	Version   string
	BoardFile string // the board file mw found, from `mw version -j`

	mu        sync.Mutex
	calls     []string                   // every mw invocation (args joined), for tests and doctor
	memo      map[string]any             // per-tick cache: item lists and gate states
	Timings   map[string][]time.Duration // per mw verb, every call (tests report where time goes)
	LockWaits int64                      // calls that failed with "board locked" (mw waited its 5 s)
	tens      []MWTenant                 // tenants from `mw version -j` (nil: ask `mw tenant list`)
}

// Calls returns a copy of the mw call log.
func (m *MW) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// ResetCalls clears the call log (tests count calls per action).
func (m *MW) ResetCalls() {
	m.mu.Lock()
	m.calls = nil
	m.mu.Unlock()
}

// fresh drops the cached tenant list and item lists: call at the start of a tick
// or command, and after the board changed under the cache (claim, settle).
func (m *MW) fresh() {
	m.mu.Lock()
	m.memo = nil
	m.mu.Unlock()
}

func (m *MW) cached(key string, load func() (any, error)) (any, error) {
	m.mu.Lock()
	if v, ok := m.memo[key]; ok {
		m.mu.Unlock()
		return v, nil
	}
	m.mu.Unlock()
	v, err := load()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.memo == nil {
		m.memo = map[string]any{}
	}
	m.memo[key] = v
	m.mu.Unlock()
	return v, nil
}

func newMW(board string) *MW {
	bin := os.Getenv("MW")
	if bin == "" {
		bin = "mw"
	}
	if board == "" {
		board = os.Getenv("MW_BOARD")
	}
	if board == "" {
		board, _ = os.Getwd()
	}
	return &MW{Bin: bin, Board: board}
}

type mwResult struct {
	Out, Err string
	Code     int
}

// mwInstallHint is what to tell a user whose mw is missing (doctor and every mw call).
const mwInstallHint = "install it: curl -fsSL https://raw.githubusercontent.com/stormovermars/miniwaves/main/install.sh | sh (or see https://github.com/stormovermars/miniwaves)"

func (m *MW) exec(args ...string) (mwResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, strings.Join(args, " "))
	m.mu.Unlock()
	t0 := time.Now()
	defer func() {
		m.mu.Lock()
		if m.Timings == nil {
			m.Timings = map[string][]time.Duration{}
		}
		m.Timings[args[0]] = append(m.Timings[args[0]], time.Since(t0))
		m.mu.Unlock()
	}()
	cmd := exec.Command(m.Bin, args...)
	cmd.Dir = m.Board
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	r := mwResult{Out: o.String(), Err: strings.TrimSpace(e.String())}
	if strings.Contains(r.Err, "board locked") {
		atomic.AddInt64(&m.LockWaits, 1)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		r.Code = ee.ExitCode()
		return r, nil
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return r, fmt.Errorf("mw not found (%s): %s", m.Bin, mwInstallHint)
	}
	return r, err // could not start mw at all
}

func (m *MW) must(args ...string) (string, error) {
	r, err := m.exec(args...)
	if err != nil {
		return "", err
	}
	if r.Code != 0 {
		return "", fmt.Errorf("mw %s: %s", strings.Join(args, " "), r.Err)
	}
	return r.Out, nil
}

// wr needs mw >= MinMW and board schema 8 only (no compatibility with older mw).
const (
	MinSchema = 8
	MaxSchema = 8
	MinMW     = "0.2.1"
)

func schemaOK(s int) bool { return s >= MinSchema && s <= MaxSchema }

// semverLess reports a < b for dotted numeric versions.
func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &x)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &y)
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// checkVersion asks `mw version -j` for the mw version and the board schema
// (board_schema and board, both null when no board is found); it refuses mw older
// than MinMW. m.Schema (0 = no board), m.Source and m.BoardFile record the result.
func (m *MW) checkVersion() error {
	r, err := m.exec("version", "-j")
	if err != nil {
		return err
	}
	var v struct {
		Version     string  `json:"version"`
		Schema      *int    `json:"schema"`
		BoardSchema *int    `json:"board_schema"`
		Board       *string `json:"board"`
		Tenants     []struct {
			Slug     string   `json:"slug"`
			Name     string   `json:"name"`
			Dir      string   `json:"dir"`
			Tags     []string `json:"tags"`
			Archived bool     `json:"archived"`
		} `json:"tenants"`
	}
	if r.Code != 0 || json.Unmarshal([]byte(r.Out), &v) != nil || v.Schema == nil {
		return fmt.Errorf("mw too old: `mw version -j` unavailable; wr needs mw >= %s (see MIGRATION.md)", MinMW)
	}
	m.Version, m.Source, m.Schema, m.BoardFile = v.Version, "mw version -j", 0, ""
	if v.BoardSchema != nil {
		m.Schema = *v.BoardSchema
	}
	if v.Board != nil {
		m.BoardFile = *v.Board
	}
	m.mu.Lock()
	m.tens = nil
	for _, t := range v.Tenants {
		m.tens = append(m.tens, MWTenant{Tenant: t.Slug, Name: t.Name, Path: t.Dir, Tags: t.Tags, Archived: t.Archived})
	}
	m.mu.Unlock()
	if semverLess(v.Version, MinMW) {
		return fmt.Errorf("mw %s is too old; wr needs mw >= %s (see MIGRATION.md)", v.Version, MinMW)
	}
	return nil
}

// requireSchema refuses to run without a board, or on a schema other than 8.
func (m *MW) requireSchema() error {
	if m.BoardFile == "" && m.Schema == 0 {
		return fmt.Errorf("no mw board found from %s: run `mw init` there, or pass -b / set MW_BOARD", m.Board)
	}
	if !schemaOK(m.Schema) {
		return fmt.Errorf("board schema %d (from %s), wr needs schema %d: run `mw migrate` (see MIGRATION.md)", m.Schema, m.Source, MinSchema)
	}
	return nil
}

type MWTenant struct {
	Tenant   string   `json:"tenant"`
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Tags     []string `json:"tags,omitempty"`
	Archived bool     `json:"archived,omitempty"`
}

// tenants come from `mw version -j` (no extra call); an mw that does not list
// them is asked with `mw tenant list`, cached until fresh().
func (m *MW) tenants() ([]MWTenant, error) {
	m.mu.Lock()
	tens := m.tens
	m.mu.Unlock()
	if tens != nil {
		return tens, nil
	}
	v, err := m.cached("tenants", func() (any, error) {
		out, err := m.must("tenant", "list", "-j", "-v")
		if err != nil {
			return nil, err
		}
		var t []MWTenant
		return t, json.Unmarshal([]byte(out), &t)
	})
	if err != nil {
		return nil, err
	}
	return v.([]MWTenant), nil
}

type NextItem struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Acceptance string `json:"acceptance"`
	Prompt     string `json:"prompt"` // --prompt: the headless session prompt
	Brief      string `json:"-"`      // the full `mw next --claim -j` text
}

func parseNext(out string) (*NextItem, error) {
	if strings.TrimSpace(out) == "null" {
		return nil, nil
	}
	var n NextItem
	if err := json.Unmarshal([]byte(out), &n); err != nil {
		return nil, err
	}
	n.Brief = strings.TrimSpace(out)
	if n.Prompt != "" { // the brief is the item; the prompt travels separately
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(out), &m) == nil {
			delete(m, "prompt")
			if b, err := json.MarshalIndent(m, "", "  "); err == nil {
				n.Brief = string(b)
			}
		}
	}
	return &n, nil
}

// claimNext atomically claims the next ready item for session NAME (registering
// it with ROLE) and returns the headless session prompt with it: one call replaces
// next + get-prompt + session start + start. nil when nothing is ready (mw prints
// null, exit 0).
func (m *MW) claimNext(tenant, name, role string) (*NextItem, error) {
	out, err := m.must("next", "--claim", "-t", tenant, "-s", name, "-a", role, "--prompt", "-j")
	if err != nil {
		return nil, err
	}
	return parseNext(out)
}

type ItemInfo struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Session string `json:"session"`
	Title   string `json:"title"`
}

func (m *MW) show(tenant, id string) (*ItemInfo, error) {
	out, err := m.must("show", id, "-t", tenant, "-j")
	if err != nil {
		return nil, err
	}
	var it ItemInfo
	return &it, json.Unmarshal([]byte(out), &it)
}

// showRetry reads an item up to 3 times (1s apart) before giving up.
func (m *MW) showRetry(tenant, id string) (*ItemInfo, error) {
	var it *ItemInfo
	var err error
	for i := 0; i < 3; i++ {
		if it, err = m.show(tenant, id); err == nil {
			return it, nil
		}
		time.Sleep(retryDelay)
	}
	return nil, err
}

var retryDelay = time.Second

// sessionItem is one row of `mw list -s NAME -A -j`: what a session owns, any tenant.
type sessionItem struct {
	Tenant string `json:"tenant"`
	ID     string `json:"id"`
	State  string `json:"state"`
	Title  string `json:"title"`
}

// sessionItems lists every item the session owns across all tenants in one call.
func (m *MW) sessionItems(name string) ([]sessionItem, error) {
	out, err := m.must("list", "-s", name, "-A", "-j")
	if err != nil {
		return nil, err
	}
	var v struct {
		Items []sessionItem `json:"items"`
	}
	return v.Items, json.Unmarshal([]byte(out), &v)
}

func (m *MW) sessionStop(tenant, name string) error {
	// -t keeps the command on one tenant file (no lock on every tenant)
	_, err := m.must("session", "stop", name, "-t", tenant)
	return err
}

func (m *MW) block(tenant, id, text, name string) error {
	_, err := m.must("block", id, text, "-t", tenant, "-s", name)
	return err
}

// evidenceGate reports whether the evidence gate is effective for tenant t
// (`mw gate -t T -j`, read-only, cached until fresh()). Any error returns true:
// the strict check stays.
func (m *MW) evidenceGate(t string) bool {
	v, err := m.cached("gate/"+t, func() (any, error) {
		out, err := m.must("gate", "-t", t, "-j")
		if err != nil {
			return nil, err
		}
		var gs []struct {
			Name      string `json:"name"`
			Effective bool   `json:"effective"`
		}
		if json.Unmarshal([]byte(out), &gs) != nil {
			return nil, errors.New("bad gate json")
		}
		for _, g := range gs {
			if g.Name == "evidence" {
				return g.Effective, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return true
	}
	return v.(bool)
}
