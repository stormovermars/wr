# wr — wave rider (spec v2 after Opus audit, 2026-10-11)

`wr` is the **active** half of the radical engineering flow: it runs the sessions that burn an `mw` board down. `mw` is the ledger and stays passive; `wr` rides its waves. Name check: no `wr` command in Homebrew, Debian, AUR (only libraries on crates/npm/PyPI; `mr` is taken).

## Contract
wr talks to mw **only** through these surfaces: `mw tenant list -j -v`, `mw list -t T -j` (`{items, waves}`), `mw list -t T -j -v` (board reads for pauses), `mw get-prompt <role> -j` (dispatcher/coach), `mw next -t T -j`, `mw show ID -t T -j`, `mw session list -j -v`, `mw session start|stop`, `mw start ID -s NAME`, `mw block ID TEXT -s NAME`, `mw get-prompt session NAME -t T -a ROLE -j`, `mw version -j` (`{version, schema}`; only a binary that predates it makes wr fall back to the `version` field of `.miniwaves.json`; wr dispatches on schemas 6 and 7; 7 is mw 0.5.0 and only adds the `events` list), `mw gate -t T -j` (read-only; `[]{name, effective}`, tells the quality guard whether the evidence gate is on). `mw next` exiting non-zero with `nothing ready` = idle, not an error. wr never edits the board file, never imports mw code; humans-only commands (`gate`, `accept`, `reject`, `edit -c`) are never run by wr. wr's own writes use the fixed session name `wr`.

## Commands (v1)
- `wr next [-t]` — the item wr would dispatch per tenant, with the reason or the skip reason (not enabled, cap, paused, dirty, retries, nothing ready). Dry run of `once`.
- `wr once [-t] [-n]` — **dispatch sequence**: NAME = `wr-<tenant>-<id-lower>-<attempt>` (attempt = 1 + run files for that ID; tenant part truncated so NAME passes mw's `validID`, ≤64 lowercase, single dashes) → `mw session start NAME -t T -a ROLE` → `mw start ID -t T -s NAME` (wr is the claimer) → prompt = `get-prompt session NAME` + the line `Work only ID (already claimed by you). After mw done ID or mw block ID, stop.` + the tenant's `forbid` rule → run the engine with the prompt on stdin, cwd = tenant dir, stdin then closed, wall-clock timeout (default 45 min) kills the whole process group (setpgid / job object). **Outcome = board, not exit code**: after exit or timeout, `mw show ID -j`: `done` → ok; `blocked` → blocked; `doing` owned by NAME → `incomplete` and wr runs `mw block ID "wr: session ended without done (exit X)" -s NAME`; anything else → `foreign`. Then `mw session stop NAME`. Exit code and timeout are recorded only.
- `wr serve` — one dispatch per tick (default 15 min) across enabled tenants, under caps: rolling-60-minute sessions (default 4), parallel per tenant (1) and global (3), USD per day per engine (sum of `total_cost_usd` from run files; engines without usage count 0, said in `wr cost`), run timeout, max attempts per item (2 failed/incomplete → skipped, printed), cooldown per tenant after a failure (default 1 tick), disk-free floor (default 5 GB). **Pause** (per tenant, printed every tick): any `blocked` item; more than K `done` items with empty `judged` (default 5; schema 5 boards without `judged` → blocked-only); quality-guard trip = an item went `done` with no `evidence` (schema 6) or no note (schema 5). Kill switches: `wr stop [-t]` writes `~/.wr/STOP` or `~/.wr/STOP.<tenant>`, `wr start [-t]` removes it; serve exits on the global STOP at the next tick. `wr service install|remove` writes a launchd plist / systemd unit; Windows: run `wr serve` by hand. **`wr serve` on real tenants only on the owner's explicit go; it replaces fleet-auto-dispatch per tenant and both must never be enabled for one tenant.**
- `wr runs [-t] [-j] [--show NAME]`, `wr status [-j]` (live runs, recent outcomes, stuck items, skip reasons, cost), `wr log [-f]`, `wr doctor`, `wr version`.
- `mw next -t T -j` prints the full item brief, or `null` with exit 0 when nothing is ready (the only contract; wr needs mw >= 0.2.0, schema 8).
- `wr next` prints per tenant `would dispatch ID` or the reason: not enabled, self-dispatch off, an external dispatcher owns it, STOP, PAUSE/pause rules, a cap, dirty tree, retries, nothing ready. `once` and `serve` use the same gates.

## Engines
Config-defined argv arrays, never shell strings; placeholders only `{uuid}` `{usd}` `{name}` `{id}`. Shipped: `claude` and a generic `cmd`. Default claude template: `["claude","-p","--permission-mode","bypassPermissions","--output-format","stream-json","--verbose","--session-id","{uuid}","--max-budget-usd","{usd}"]` (flags verified against `claude --help` at build time; drop any that are absent and say so in `wr engines`). This runs **unsandboxed**: the forbid list is advisory prompt text, so device safety depends on the session prompt and the dirty-tree guard; stated in USAGE.

## Guards
- Tenants are opt-in (`enabled: false` default). Self-dispatch of `wr`, `miniwaves` and any `fleet*` tenant only with `self_dispatch: true`.
- Dirty-tree guard: skip a tenant when `git status --porcelain` is non-empty and the last run on it was not `ok`, or when git errors (read-only git only).
- `~/.wr/wr.lock` (flock / LockFileEx, copy mw's lock) so two `serve` or a `once` during `serve` cannot double-dispatch.
- Lost runs: a run file with no `end` whose PID is dead → `lost`, same block + session stop; a live PID is left alone.

## Config `~/.wr/config.json`
```json
{"version":2,"tick_min":15,
 "caps":{"per_hour":4,"parallel_global":3,"parallel_tenant":1,"usd_per_day":{"claude":20},"run_timeout_min":45,"stale_minutes":20,"max_attempts":2,"disk_free_gb":5},
 "pause":{"unjudged":5,"on_blocked":true,"quality_guard":true},
 "engines":{"claude":{"cmd":["claude","-p","--permission-mode","bypassPermissions","--output-format","stream-json","--verbose","--session-id","{uuid}","--max-budget-usd","{usd}"],"usage":"claude_stream","enabled":true},
            "cmd":{"cmd":[],"usage":"none","enabled":false}},
 "tenants":{"example":{"engine":"claude","role":"builder","forbid":["device-serial-1"],"dir":null,"enabled":false}},
 "self_dispatch":false}
```

## Records
`~/.wr/runs/<tenant>/<NAME>.json` written at start (pid, name, uuid, item, engine, cmd hash, start) and completed at end (end, exit, outcome, usage, cost); engine stdout/stderr at `<NAME>.log`, size-capped (default 2 MB). Spend = sum of run files per day; no second store. wr never writes into a repo or the board.

## Tests
Fake engine = `testdata/fakeengine` (Go test binary) driven by `FAKE=done|block|crash|hang|nodone`, running the real `mw` (path from `MW`, default PATH) against a temp board. Required: each outcome in the dispatch sequence, lost recovery, caps/pause/STOP, retry limit, name collision, prompt contains the forbid line, config round-trip, `nothing ready` = idle. No test calls real `claude`.

## Non-goals (v1)
No HTTP, no `--apply`, no multi-engine probes, no Windows service, no board editing, no new kinds, no git writes, no cloud, no multi-machine. Multi-user later = `--by` identity on mw plus a shared board lock; out of v1.

## Build plan (Go, single binary, mw-style repo)
- W1 (Sonnet ≤60k, Opus review): `main.go`, `config.go`, `mw.go` (exec + JSON client), `run.go` (engine exec, records, outcome, lost), `wr next|once|runs|cost|status|engines|version`, fake engine + tests, `build.sh`, `USAGE.md`, `README.md`, `TECHNICAL.md`.
- W2 (Sonnet ≤40k, Opus review): `serve` (one dispatch per tick), caps, pauses, STOP files, `service`, `role` (print; `--send` with one API).
Each wave dogfoods on the wr board itself; no push until the owner says.

Asks on mw (radical Wave A, `mw version -j` since shipped); `mw next -j` prints `null` with exit 0 when nothing is ready.

## v1 additions (W3)

- `wr doctor [-j]`: mw on PATH + version/schema, claude flag check, config validity, `~/.wr` writable, lock free (held by serve = ok), disk floor; exit 1 on failure.
- `wr once --dry` is an alias of `wr next`; `wr serve --once` runs exactly one tick (not combinable with `--max-ticks`); serve re-checks the mw schema every tick.
- Quality guard (schema 6): with the evidence gate effective (`mw gate -t T -j`; error = on) a newly done item must show `"e":"E"` in `mw list -j -v`; with the gate off it needs a non-empty note.
