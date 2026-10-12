# wr

wr (wave rider) runs the coding sessions that burn a [miniwaves](https://github.com/stormovermars/miniwaves) (`mw`) board down, one claimed item at a time, and reads each outcome from the board. Single Go binary, stdlib only, no server unless you ask for the dashboard page.

## Install

Claude Code plugin (installs the CLI too):

```sh
claude plugin marketplace add stormovermars/wr && claude plugin install wr@wr
```

Or the CLI alone (needs mw >= 0.2.0):

```sh
curl -fsSL https://raw.githubusercontent.com/stormovermars/wr/main/install.sh | sh
go install github.com/stormovermars/wr@latest
```

## Use

```sh
wr doctor      # environment checks, exit 1 on failure
wr next        # what would be dispatched, or why not (read-only)
wr status      # live runs, stuck items, caps, cost (read-only)
wr ui          # dashboard in the terminal; --web opts into a loopback page
wr serve       # the loop; dispatches sessions, see the warning below
```

Warning: engines run unsandboxed and spend money. Tenants are off until you set `enabled` in `~/.wr/config.json`, and `wr serve` / `wr once` only run when you start them. `wr stop` ends it.

Optional, off by default: `publish` (a static copy of the dashboard, throttled by `publish.interval`) in the config. Git belongs to the coding agent.

Runs on macOS and Linux (tested). Windows binaries are built but not yet verified.

## Links

- [USAGE.md](USAGE.md): commands, config, caps, changelog
- [TECHNICAL.md](TECHNICAL.md): internals
- [MIGRATION.md](MIGRATION.md): upgrade notes
- [miniwaves](https://github.com/stormovermars/miniwaves): the board wr drives
