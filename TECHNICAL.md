# wr technical notes

## Files

- `main.go` flag parsing, commands (next, once, runs, version), the home lock, the home lock.
- `serve.go` serve loop (`Server.tick`, goroutine dispatch, wake channel), caps usage, pause rules, quality guard, STOP/PAUSE files, stop/start, service files.
- `status.go` `status` (read-only) and `log`. `fleet.go` the optional `WR_FLEET_DIR` overlap check, `events.log`, notifications.
- `config.go` config structs, default literal (verbatim from the spec), load/save, `WR_HOME`.
- `mw.go` the only mw surface: exec `mw` with cwd = board dir, JSON decode. Never reads `.miniwaves.json` (the board file path and schema come from `mw version -j`).
- `run.go` records, naming, planning guards, dispatch sequence, outcome, lost recovery, log cap, usage parse.
- `proc_unix.go` / `proc_windows.go` setpgid + group kill, pid probe, flock. Windows is stubbed; W3 list: Windows lock (LockFileEx), a job object for group kill, and `diskFreeGB`.
- `serve_test.go` caps, pauses, STOP, cooldown, service files, 
- `testdata/fakeengine/main.go` test engine, built by `TestMain` into a temp dir.

wr never writes into a repo or the board file; it changes the board only through mw.

## Records

```
~/.wr/config.json
~/.wr/wr.lock
~/.wr/STOP, STOP.<tenant>     kill switches (empty marker files)
~/.wr/PAUSE.<tenant>          quality-guard trip, reason inside; removed by `wr start -t T`
~/.wr/state.json              tenant -> done item ids seen at the last tick (guard baseline)
~/.wr/runs/<tenant>/<NAME>.json   written at start (pid, uuid, item, engine, cmd_hash, start, board), rewritten at end (end, exit, timeout, outcome, usage, cost_usd)
~/.wr/runs/<tenant>/<NAME>.log    engine stdout+stderr, capped at 2 MB
```

Spend is the sum of run files per local day; there is no second store.
Writes are tmp+rename.

## Outcome rules

After the engine exits or times out, `mw show ID -j -v`:

| board | outcome | wr action |
|---|---|---|
| done | ok | stop session |
| blocked | blocked | stop session |
| doing, session = NAME | incomplete | `mw block ID "wr: session ended without done (exit X)" -s NAME`, stop session |
| anything else | foreign | stop session |

Exit code and timeout are recorded only. A run whose engine cannot start is
incomplete. Lost: at the start of `once` (and in `status`), a record with no
`end` and a dead pid becomes `lost`, with the same block and session stop; a
live pid is left alone.

Retry: attempt = 1 + existing run files for the item; `failed` outcomes
(incomplete, lost, foreign) at or above `max_attempts` skip the item.
Dirty-tree guard: skip when `git status --porcelain` is non-empty and the last
run of the tenant was not ok, or git errors.

## Lock

`once` and `serve` take a non-blocking exclusive `flock` on `~/.wr/wr.lock` and refuses to
run if another wr holds it (so two dispatchers cannot double-dispatch). `status`
takes it only to recover lost runs.

## Deviations from the spec text

- `mw get-prompt session NAME` refuses an existing session (exit 3, `{"error":"session_exists"}`), so the prompt is fetched before the claim.
- `mw show -j` carries the owner and state without `-v`.
- `-n` on `once` is read as a dispatch count per tenant.

## serve (W2)

`Server.tick` order: global STOP (exit) -> `recoverLost` -> usage snapshot
(`Dispatcher.usage`: runs in the last 60 min, live groups, USD today per engine,
statfs of `WR_HOME`) -> per enabled tenant in config-file order (`Config.Order`,
read by scanning the `tenants` keys; tenants absent from it follow sorted):
`mw list -t T -j -v`, pause rules (`pauseReasons`), STOP.T / PAUSE.T, then only if
nothing was dispatched yet this tick and no global cap hit: cooldown, tenant caps,
`plan` (dirty tree, `mw next`, max_attempts) and `dispatch`. Pause lines print
even when a cap would have blocked. Cooldown is in-memory per `serve` process.

Quality guard needs a baseline: `state.json` holds the done ids per tenant as of
the previous tick; an id done now and not in it, with empty `evidence` (if the
item has that key) or empty `note`, trips the guard. The dispatch's own item is
therefore judged on the next tick. A tenant's first tick seeds the baseline.

`judged`/`evidence` are read as raw JSON (absent, null, "", false, [], {} = empty)
(schema 8 only).

Known limit: with the evidence gate off for a tenant, mw never records evidence, so the quality guard would trip on its first done item; keep the gate on (`mw gate evidence on`) for dispatched tenants. Tests: `realmw_test.go` runs the loop against the binary in `MW` (skipped when unset); the fake engine writes proof into `<evidence>/<tenant>/<ID>/`, and `rig.gateOn()` turns the gate on.

Tests inject `Dispatcher.Now`/`Sleep`, call `Server.tick` directly, and point
`WR_SERVICE_DIR` at a temp dir (then no launchctl/systemctl is run).

## Windows

`proc_windows.go`: the lock is `LockFileEx` (one byte at offset 1<<30, so the pid text stays readable) and free disk is `GetDiskFreeSpaceEx`, both via `syscall.NewLazyDLL` (stdlib only). Job-object group kill, service install and process start time are not implemented (the engine process is killed, not its tree).


- Schema comes from `mw version -j` (`{version, schema}`); mw < 0.2.0 or schema != 8 is refused (`checkVersion`/`requireSchema`), no fallback.
- `serve` runs under `signal.NotifyContext(SIGTERM, SIGINT)`: a cancel kills the engine's process group, blocks the item and marks the run `incomplete`, then serve exits; the inter-tick sleep wakes on cancel.
- The quality-guard baseline (`state.json`) is not advanced when the PAUSE file cannot be written, so the trip is re-found next tick; disabled tenants are dropped from `state.json`.
- `disk_free_gb` fails closed: if free space cannot be read, dispatch is skipped with the reason printed. Service files carry `PATH` (install-time plus the dirs of `mw` and the engines) and launchd `KeepAlive` restarts only on non-zero exit.


- `plan()` (run.go) is the single gate for next/once/serve. `tick` only keeps the quality-guard baseline and writes PAUSE, then calls `plan` and `prepare` (record, session, claim: synchronous, so caps see the run) and `execute` in a goroutine. `inflight` names runs being prepared/executed so `recoverLost` never reaps them.
- Run record `"v":2`: adds `pstart` (`ps -o lstart=`), `transcript`, `stale`, `stop_error`. `readRuns` skips other versions.
- Retry code kept: wr's own block notes start with `wr:`; `pauseReasons` does not count them, so `max_attempts`/cooldown act on items a human reopens.
- Tests are host-independent: rig sets `disk_free_gb: 0`; `WR_FLEET_DIR` points at nothing; notifications are stubbed.

## Internals

- `MW.tenants()` comes from `mw version -j` (`tenants` array, kept by `checkVersion`); only an mw without that array is asked with `tenant list`.
- Dispatch: `planMode` (lean list, pause rules, dirty guard; the first ready item stands for the claim) -> `prepare` (name `wr-<tenant>-<attempt>-<rand>`, write the record, one `next --claim --prompt -j`; the prompt is split from the brief) -> `execute` -> `outcome`. `planItems` upgrades the lean list to the full one only when a blocked item or more than `pause.unjudged` done items could trip a pause rule; it reuses a cached full list (status, serve) without a call.
- `gitDirty`: one `git status --porcelain=v2 -z --untracked-files=all` run in the tenant dir (no `rev-parse`); entries named `.miniwaves.*`, under `.wr/`, or equal to a published page (matched by path suffix, since status paths are repo-relative) are skipped; "not a git repository" is `errNotRepo` and counts as clean.
- Timings the tests lower: `retryDelay`, `livenessPoll`, `engineWaitDelay`, `followPoll`, `uiRefresh`.
- `serve` keeps `hinted` (tenant -> day) for the daily "mw archive -t T" hint at more than 1000 done items.


- `MW` keeps a call log (`Calls()`) and a per-tick memo (`fresh()` clears it): tenant list, item list per tenant, gate per tenant. `tick`, `once` (after each dispatch), each ui frame and each `/api` request start fresh. Tests assert call counts from the log (`calls_test.go`).
- Dispatch: `plan` (next, a peek) -> `prepare` (get-prompt, write the record, `next --claim`; a different item than planned is run as claimed, a null claim closes the record as `raced`) -> `execute` -> `outcome` (`list -s NAME -A`, then `show` only when the item is no longer owned; done and blocked items release the session).
- `wr status` plans with `peek`: ready counts from the loaded list instead of `mw next`.
- Hooks: `afterSettle` = `publishPage`, serialized by `hookMu` (hash sidecar, `publish.interval` throttle, one delayed write via `afterFunc`, `flushPublish` at exit); `tick` runs `publishPage` last. `wr ui --export` = `uiExportPage`: the embedded page plus `window.__WR_DATA__` (`<` escaped), so the page renders without `fetch`.
- `gitDirty` used `status --porcelain -uall` and drops `.miniwaves*.json`, `.wr/`, and the published paths (wr config `publish.html` and the board's own `publish.html`).
- `wr.lock`: serve writes its pid after taking the lock; `wr ui` reads it only while the lock is held.
