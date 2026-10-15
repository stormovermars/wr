package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
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
	LockWaits int64                      // calls that failed with "board locked" (mw's lock wait timed out; claims retry it)
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
		msg := r.Err
		if msg == "" { // never a bare failure: name the exit code (a Windows NTSTATUS shows as 0xC...)
			msg = fmt.Sprintf("exit code %d (0x%X), no stderr", r.Code, uint32(r.Code))
		}
		return "", fmt.Errorf("mw %s: %s", strings.Join(args, " "), msg)
	}
	return r.Out, nil
}

// isLockTimeout: mw gave up waiting for the board lock ("board locked (pid P) ..." from every
// mw >= 0.2.1); nothing was written, so the call can simply be repeated.
func isLockTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "board locked")
}

// lockRetries and lockRetryDelay bound retryLock: up to 3 repeats, each after a jittered
// lockRetryDelay..2*lockRetryDelay pause (spreads the runners that timed out together).
var (
	lockRetries    = 3
	lockRetryDelay = time.Second
)

// retryLock runs call and repeats it while it fails with a lock wait timeout.
func retryLock(call func() (string, error)) (string, error) {
	out, err := call()
	for i := 0; i < lockRetries && isLockTimeout(err); i++ {
		time.Sleep(lockRetryDelay + time.Duration(rand.Int64N(int64(lockRetryDelay)+1)))
		out, err = call()
	}
	return out, err
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
	// a lock wait timeout claimed nothing: retry it before the dispatch counts an error
	out, err := retryLock(func() (string, error) {
		return m.must("next", "--claim", "-t", tenant, "-s", name, "-a", role, "--prompt", "-j")
	})
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

// sessionStop stops the session; a lock timeout or a transient failure (a Windows rename
// refused while another process reads the tenant file) is retried, so a settled run never
// leaves its session idle. A refusal (still owns an item, unknown session) is not retried.
func (m *MW) sessionStop(tenant, name string) error {
	_, err := m.sessionStopRelease(tenant, name)
	return err
}

// sessionStopRelease is sessionStop that, on mw >= 0.2.3, passes --release (items the
// session still owns go back to ready instead of refusing the stop) and returns the
// released item ids; older mw stops without it and returns none.
func (m *MW) sessionStopRelease(tenant, name string) (released []string, err error) {
	// -t keeps the command on one tenant file (no lock on every tenant)
	args := []string{"session", "stop", name, "-t", tenant}
	if m.hasReap() {
		args = append(args, "--release")
	}
	for i := 0; ; i++ {
		var out string
		out, err = retryLock(func() (string, error) { return m.must(args...) })
		if err == nil {
			for _, ln := range strings.Split(out, "\n") {
				if f := strings.Fields(ln); len(f) == 2 && f[0] == "released" {
					released = append(released, f[1])
				}
			}
			return released, nil
		}
		if hasFlag(args, "--release") && strings.Contains(err.Error(), "unknown option") && strings.Contains(err.Error(), "--release") {
			// mw was downgraded under the cached version: one plain stop, not counted as a failure
			args = removeFlag(args, "--release")
			i--
			continue
		}
		if i >= 2 || strings.Contains(err.Error(), "still owns") || strings.Contains(err.Error(), "not found") {
			return nil, err
		}
		time.Sleep(retryDelay)
	}
}

func hasFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f {
			return true
		}
	}
	return false
}

func removeFlag(args []string, f string) []string {
	var out []string
	for _, a := range args {
		if a != f {
			out = append(out, a)
		}
	}
	return out
}

func (m *MW) block(tenant, id, text, name string) error {
	_, err := m.must("block", id, text, "-t", tenant, "-s", name)
	return err
}

// gateState is `mw gate -t T -j`: mw >= 0.2.5 answers {"level":..,"gates":[..]}, older mw a bare array.
type gateState struct {
	Level    string // "" from an mw without levels
	Evidence bool   // evidence gate effective
}

func (m *MW) gateState(t string) (gateState, error) {
	v, err := m.cached("gate/"+t, func() (any, error) {
		out, err := m.must("gate", "-t", t, "-j")
		if err != nil {
			return nil, err
		}
		return parseGate(out)
	})
	if err != nil {
		return gateState{}, err
	}
	return v.(gateState), nil
}

func parseGate(out string) (gateState, error) {
	type gate struct {
		Name      string `json:"name"`
		Effective bool   `json:"effective"`
	}
	var gs []gate
	var gs0 gateState
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		var o struct {
			Level string `json:"level"`
			Gates []gate `json:"gates"`
		}
		if json.Unmarshal([]byte(out), &o) != nil {
			return gs0, errors.New("bad gate json")
		}
		gs, gs0.Level = o.Gates, o.Level
	} else if json.Unmarshal([]byte(out), &gs) != nil {
		return gs0, errors.New("bad gate json")
	}
	gs0.Evidence = true
	for _, g := range gs {
		if g.Name == "evidence" {
			gs0.Evidence = g.Effective
		}
	}
	return gs0, nil
}

// evidenceGate reports whether the evidence gate is effective for tenant t
// (`mw gate -t T -j`, read-only, cached until fresh()). Any error returns true:
// the strict check stays.
func (m *MW) evidenceGate(t string) bool {
	g, err := m.gateState(t)
	if err != nil {
		return true
	}
	return g.Evidence
}

// LevelMW is the first mw with `mw gate level` and the "level" field of `mw gate -j`.
const LevelMW = "0.2.5"

// hasLevel reports whether the mw found by checkVersion supports gate levels.
func (m *MW) hasLevel() bool { return m.Version != "" && !semverLess(m.Version, LevelMW) }

var gateLevels = []string{"trust", "proof", "spec", "judged"}

// normLevel lower-cases a configured level (mw matches case-insensitively).
func normLevel(l string) string { return strings.ToLower(strings.TrimSpace(l)) }

func validGateLevel(l string) bool {
	l = normLevel(l)
	for _, x := range gateLevels {
		if x == l {
			return true
		}
	}
	return false
}

// gateLevelOf is the board's level for tenant t ("" when unknown or mw has no levels).
func (m *MW) gateLevelOf(t string) string {
	if !m.hasLevel() {
		return ""
	}
	g, err := m.gateState(t)
	if err != nil {
		return ""
	}
	return g.Level
}

// applyGateLevel makes the board's gate level for t match the configured one (wanted ""
// = none configured: never touches the board). It reports whether it changed it.
func (m *MW) applyGateLevel(t, wanted string) (changed bool, from string, err error) {
	wanted = normLevel(wanted)
	if wanted == "" || !validGateLevel(wanted) || !m.hasLevel() {
		return false, "", nil
	}
	g, err := m.gateState(t)
	if err != nil {
		return false, "", err
	}
	if g.Level == wanted {
		return false, g.Level, nil
	}
	if _, err := m.must("gate", "level", wanted, "-t", t, "--by", "wr"); err != nil {
		return false, g.Level, err
	}
	m.mu.Lock()
	delete(m.memo, "gate/"+t)
	m.mu.Unlock()
	return true, g.Level, nil
}
