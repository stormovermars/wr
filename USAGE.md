# wr usage

Build with `./build.sh` (output `build/wr`) and put it on PATH. Needs `mw` >= 0.2.0 on PATH
(or `MW=/path/to/mw`) and a board whose `mw version -j` reports schema 8. wr refuses to run
(`next`, `once`, `serve`, `doctor`) on an older mw or any other schema.

## Warning: engines are unsandboxed

The default claude engine runs with `--permission-mode bypassPermissions`. Nothing
confines it. The tenant `forbid` list is advisory text appended to the prompt, so
safety rests on the session prompt and the dirty-tree guard. Enable a tenant only
when you accept that. **Run `wr serve` on real tenants only on an explicit
go from the owner**; it replaces other auto-dispatchers per tenant, and two must
never be enabled for one tenant.

## Commands

| command | does |
|---|---|
| `wr next [-t T]` | per tenant, the item wr would dispatch, or why not. The same gates as `once`/`serve` (one `plan()`): not in config, not enabled, self-dispatch off, an external dispatcher (WR_FLEET_DIR) owns the tenant, engine off, global/tenant STOP, PAUSE, pause rules, caps (`per_hour`, parallel, `usd_per_day`, disk), dirty tree, retries, nothing ready. Read-only. |
| `wr once [-t T] [-n N]` | dispatch up to N items (default 1) per enabled tenant. Without `-t`, every tenant in the config. |
| `wr runs [-t T] [-j] [--show NAME]` | list run records; `--show` prints one record plus the last 4 KB of its log. |
| `wr serve` | loop: each tick (`tick_min`) starts one dispatch per tenant (in config order) in a background goroutine under the caps and pauses, then returns, so `parallel_global`/`parallel_tenant` really run in parallel. An `ok` outcome triggers an immediate re-tick. Takes `~/.wr/wr.lock`. Exits at the next tick when `~/.wr/STOP` exists (running engines finish first). `--max-ticks N` stops after N ticks; `--once` runs one tick, waits for its runs, and exits. |
| `wr stop [-t T]` | write `~/.wr/STOP` (serve exits) or `~/.wr/STOP.T` (that tenant is skipped, printed each tick). |
| `wr start [-t T]` | remove the STOP file; with `-t` also clears a quality-guard pause for T. |
| `wr service install\|remove` | write `~/Library/LaunchAgents/ro.nfnt.wr.plist` (macOS, loaded with launchctl) or `~/.config/systemd/user/wr.service` (linux, `systemctl --user enable --now`) running `wr serve`; logs `~/.wr/serve.out`, `serve.err`. Other OS: run `wr serve` by hand. |
| `wr status [-j] [--last N]` | read-only (never recovers, settles or kills): per tenant enabled/last outcome/running/spend today/done of total/doing/blocked (one `mw list` per tenant, ready counts come from it; `wr next` names the item). Exit 1 when every enabled tenant errored; `live:` one row per running engine (tenant, item, age, transcript-mtime age, log path, `lost?` when its process is gone); `recent:` last N outcomes (default 5); `stuck:` items `doing` under a `wr-*` session with no live run; per-tenant skip/pause reason; mw schema; caps usage; `cost:` USD per engine per day; STOP files. `-j` prints `{tenants, live, recent, stuck, skips, caps, paused, cost, stop_files, mw}`. |
| `wr ui [--once] [--web [--addr A] [--open]] [--export PATH]` | read-only dashboard, see [wr ui](#wr-ui). `--export PATH` writes the page with its data inlined and exits. |
| `wr log [-f]` | tail `~/.wr/serve.out` (last 50 lines); `-f` follows. Without serve.out: the last runs and `events.log`. |
| `wr doctor [-j]` | checks: mw on PATH, >= 0.2.0, schema 8; config; each enabled claude engine's flags against `claude --help` (absent ones listed, never edited; also warns on `--output-format json`); no enabled tenant is also owned by fleet-auto-dispatch; `~/.wr` writable; lock; disk floor. Exit 1 on any failure. A lock held by a running serve is reported as held. This replaces `wr engines`. |
| `wr version [-j]` | version. |

Board directory: `-b DIR`, else `MW_BOARD`, else the current directory (wr runs
`mw` with that cwd). Flags: `-t` tenant, `-n` count, `-j` JSON, `-b` board.
Also `--max-ticks N` (serve), `--last N` (status), `-f` (log).

## Config

`~/.wr/config.json` (override the directory with `WR_HOME`). Written with the
spec defaults on first run; edit it by hand. Keys: `tick_min`, `caps`
(`per_hour parallel_global parallel_tenant usd_per_day run_timeout_min
max_attempts disk_free_gb stale_minutes`), `pause`, `engines`, `tenants`, `self_dispatch`, `ui`, `publish`.
`usd_per_day` also sets the per-run `--max-budget-usd` (a quarter of the day cap).

Engine = argv array, never a shell string. Placeholders: `{uuid}` `{usd}`
`{name}` `{id}`. `usage: "claude_stream"` parses `total_cost_usd` and `usage` from the last `{"type":"result"}` line of claude's `--output-format stream-json --verbose` output (the default cmd), so the run log fills live;
`"none"` records nothing.

Tenant: `engine`, `role`, `forbid` (strings added to the prompt), `dir` (null =
the tenant path from `mw tenant list`), `enabled` (default false). The tenants
`wr`, `miniwaves` and any `fleet*` are dispatched only with `self_dispatch: true`.

Optional, off by default (errors are events in `events.log`, never fatal):
`"publish": {"html": "PATH", "interval": 60}` writes the `wr ui` page with its data inlined (`wr ui --export PATH`, which always writes). After ticks and settles the page is rewritten only when its data changed (a hash is kept in `PATH.hash`) and at most every `interval` seconds (default 60, minimum 5; lower values clamp to 5, `wr doctor` says so). A change inside the interval is written at the interval's end, and a pending write is flushed when `once` or `serve` exits, so the page is never stale longer than the interval. Git is the coding agent's job: wr never runs git or a git sync command (the git sync command was removed in mw 0.2.0 and the old wr `sync` key is gone).
The dirty-tree guard ignores `.miniwaves.json`, `.miniwaves.archive.json`, `.wr/` and the published page, so a board kept in the tenant repo does not block dispatch.

## A dispatch

For ready item ID of tenant T: `mw next -t T -j` (plan), prompt from
`mw get-prompt session NAME --headless -j` (before the claim: exit 3 /
`session_exists` bumps the attempt number), `mw next --claim -s NAME -a ROLE -j`
(one atomic call that claims and registers the session), then the engine with the
prompt on stdin in the tenant dir under a wall-clock timeout (default 45 min)
that kills the whole process group. NAME is `wr-<tenant>-<id>-<attempt>`.
Afterwards the board decides: `done` is ok, `blocked` is blocked, still `doing`
under NAME is incomplete (wr blocks it), anything else is foreign. wr's own blocks (note starting `wr:`) do not trigger the tenant pause, so `max_attempts` and the cooldown apply to items a human reopens. The session
is then stopped. An item that failed `max_attempts` times is skipped.

Engine env: `WR_BOARD WR_TENANT WR_ITEM WR_SESSION MW_SESSION MW_TENANT` (no `MW_BOARD`: mw finds the board from the tenant dir).
The prompt is mw's headless session prompt verbatim, plus the claimed item's brief inline and the forbid rule.

Liveness: for engines with a `{uuid}` placeholder (claude), the transcript `~/.claude/projects/<cwd, non-alphanumerics as dashes>/<uuid>.jsonl` must be written at least every `stale_minutes` (default 20); otherwise wr kills the group and blocks the item. After every run wr blocks every item still `doing` under the session, then `mw session stop`; a failed stop is recorded in the run (`stop_error`) and `events.log`. Settle is one `mw list -s NAME -A -j` (what the session still owns) plus a `show` when the item is no longer owned. If those reads fail 3 times the run settles as `incomplete`. The engine PID is stored with its start time; a PID whose start time differs is dead and never killed.
Alerts: pauses and non-ok outcomes append to `~/.wr/events.log` and raise a macOS notification.
Optional overlap check: only when `WR_FLEET_DIR` names an external registry directory, wr refuses the tenants another dispatcher owns (its `state/auto-dispatch.json` has `enabled: true`: every tenant in its `apps-fleet-sessions.json`). Unset (the default): no check.

## serve: caps and pauses

Every tick: global STOP check, lost-run recovery, then per enabled tenant (config
file order) at most one new dispatch. Skips are printed with the reason.

Caps: `per_hour` (sessions started in the last 60 min, all tenants),
`parallel_global` and `parallel_tenant` (run files without `end` whose process
group is alive and matches the recorded start time), `usd_per_day` (per engine, sum of `cost_usd` of today's runs),
`disk_free_gb` (free space on the `WR_HOME` volume), `max_attempts` per item.
After a run whose outcome is not `ok` the tenant is skipped for one tick (cooldown).

Pause (printed every tick, nothing dispatched): any `blocked` item
(`pause.on_blocked`); more than `pause.unjudged` `done` items with an empty
`judged` field ; quality
guard (`pause.quality_guard`): an item went `done` since the previous tick without
evidence (a `done --no-evidence REASON` waiver note is only a warning in `events.log`). A guard trip
writes `~/.wr/PAUSE.<tenant>` and holds until `wr start -t T`. The done-set seen
at the last tick is kept in `~/.wr/state.json`; the first tick for a tenant only
records it.

With `WR_FLEET_DIR` set, wr refuses tenants the other dispatcher has enabled.

## wr ui

Read-only dashboard, no actions. Default is the terminal view: clears and redraws every 3 s (plain ANSI, `q` or Ctrl-C quits) with a header (serve state, caps, cost today, refreshed), live runs, tenant columns (side by side at 120+ columns, else stacked) and the last 15 activity events. `wr ui --once` prints one frame and exits.

The web page is opt-in: `wr ui --web [--addr 127.0.0.1:5052] [--open]`, or config `"ui": {"web": true, "addr": "127.0.0.1:5052"}`. It serves one embedded page at `/` and JSON at `/api` (serve state, caps, cost, `runs`, `last`, `tenants`, `activity`), loopback only, polled every 3 s. Without the flag or key wr opens no socket. Tenants shown: enabled ones in config plus every tenant `mw tenant list` reports.

## Changelog

### 0.2.0 (mw >= 0.2.0, board schema 8)

The first public release of wr after the 0.1.0 line of mw. It needs mw >= 0.2.0 (`mw next --claim --prompt`, `tenants` in `mw version -j`); run `mw migrate` on every workspace first.

- Dispatch path: `mw version -j` (tenants included) -> `mw list -t T -j` -> `mw next --claim -s SESSION -a ROLE --prompt -j` -> run -> `mw list -s SESSION -A -j` (+ `show` only when the item is no longer owned) -> `mw session stop -t TENANT`.
- Session names are `wr-<tenant>-<attempt>-<6 hex>`; the retry limit is checked on the first ready item and again on the claimed one.
- Board schema 8: `mw version -j` must report `board_schema` 8, otherwise wr refuses with the `mw migrate` hint. Git is the agent's job: wr has no sync key; the dirty-tree guard ignores `.miniwaves.*`, `.miniwaves/`, the published page and `.wr/`.
- `wr ui` (terminal dashboard, `--web` page), `wr status`, `wr log [-f]`, `wr doctor`, `wr serve` under `parallel_global`/`parallel_tenant` caps, PAUSE/STOP files and pause rules shared by `next`, `once` and `serve`.
- Optional `publish.html` page with data inlined, throttled to `publish.interval` seconds (default 60, minimum 5).
- Claude Code plugin (`claude plugin marketplace add stormovermars/wr && claude plugin install wr@wr`), installers, CI (`go vet`, `go test -race`, Windows `go test`), release notes from `releases/<version>.md` when it exists.
- Removed commands: `wr role`, `--send`, `wr engines` (now `wr doctor`), `wr once --dry` (use `wr next`), `wr cost` (now `wr status`).
