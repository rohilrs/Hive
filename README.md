# Hive

Terminal-first orchestrator for Claude Code.

## What is Hive?

Hive is a Go daemon that orchestrates Claude Code subprocesses through pipeline state machines (Plan / Build / Debug / Finish-branch), with a Bubbletea TUI, a CLI, and a daemon-hosted chat agent. Each pipeline run lives in its own git worktree under `~/.hive/worktrees/<run-id>/`; the daemon multiplexes concurrent runs against a configurable per-repo conflict guard.

## Why?

Claude Code's skill-based loops (build → review → test) get bypassed by the model — skills are prompts, and prompts are negotiable. Hive moves the control flow into Go state machines instead. Skills can suggest; FSMs decide.

## Quick install

Requires Go 1.21+, `git`, and `claude` on PATH. ANTHROPIC_API_KEY only if you want `chat.provider = "api"` (default is `"claude-code"` which uses your Pro/Team subscription).

```bash
git clone https://github.com/rohilrs/Hive.git
cd Hive
make install   # installs to ~/bin/hive
```

Add `~/bin` to your PATH if it isn't already.

## First run

```bash
hive init                           # creates ~/.hive/config.toml + runs env checks
hive daemon                         # start the daemon in the foreground (separate terminal)
hive project add hive ~/code/hive   # register your first project
hive task add hive "Add a smoke test for the new feature"
hive tui                            # open the TUI; press Ctrl/Alt + 1..9 to switch tabs
```

To dispatch a task immediately (auto_dispatch is on by default but you can also be explicit):

```bash
hive run <task-id>
```

## Daily flow (TUI keybinds)

| Key | Action |
|---|---|
| `Ctrl/Alt + 1..9` | Switch tabs |
| `Tab / Shift+Tab` | Cycle tabs forward/back |
| `Ctrl + K` | Open chat session picker |
| `Ctrl + N` | New chat session (chat tab) |
| `r` | Rename chat session / refresh (per tab) |
| `y / n` | Approve / deny a tool-use confirm card |
| `a` | Approve-all (session-wide) for the current tool |
| `e` | Edit a tool's args before running |
| `c` | Cancel a pending tool call |
| `t` | Open tool-results picker (chat tab) |
| `↑↓ / k j` | Scroll drill-in / list cursor |
| `Esc` | Close drill-in / modal |
| `q` / `Ctrl + C` | Quit |

CLI subcommands take `--help` for full flag listings.

## CLI overview

| Command | Purpose |
|---|---|
| `hive init` | Initialize ~/.hive with a default config.toml |
| `hive daemon` | Start the daemon in the foreground |
| `hive doctor` | Audit daemon + state across all subsystems |
| `hive tui` | Open the TUI |
| `hive status` | Print a one-shot status snapshot |
| `hive events` | Stream the daemon event log |
| `hive logs <run-id>` | Print captured logs for a run |
| `hive task add\|list` | Manage tasks |
| `hive project add\|list\|edit\|rm\|archive\|config` | Manage projects |
| `hive run <task-id>` | Dispatch a pending task immediately |
| `hive abandon <run-id>` | Cancel a running run (tears down worker subprocess) |
| `hive decompose <task-id>` | LLM-driven breakdown into sub-tasks |
| `hive predict` | Run the cost/iteration predictor against a task |
| `hive chat ["message"]` | One-shot chat or REPL |
| `hive chat history\|resume\|name\|delete` | Chat session management |
| `hive sources bind\|list\|unbind` | Bind a project to GitHub / Linear / inbox |
| `hive sync [--status]` | Manual sync trigger / status |
| `hive approvals list\|allow\|deny\|pending\|resolve` | Walk-away approval CLI |
| `hive document <run-id>` | Re-run the documenter stage for a finished run |

Every command accepts `--help`.

## Architecture

Daemon (`internal/daemon`) maintains the SQLite store at `~/.hive/db.sqlite` and dispatches pipeline runs against the `claudecode` adapter (`internal/adapter/claudecode`). Each run is a state machine (`internal/pipeline`) that drives Claude Code through Plan / Build / Debug / Finish-branch stages, gated by stall detection (Phase 3.2) and approvals (Phase 4). The TUI (`internal/tui`) is a Bubbletea app talking JSON-RPC over UDS. The chat agent (`internal/chat`) is hosted in-process and reaches Anthropic via `internal/anthropic.SDK`.

## Troubleshooting

- **`hive doctor --verbose`** — audits 7 subsystems (daemon, store, worktrees, sources, mcp, config, approvals) and shows what's wrong.
- **Daemon log** — when run via `hive daemon`, the daemon logs to stdout. Pipe to a file if running detached: `hive daemon >> ~/.hive/daemon.log 2>&1`.
- **Singleton lock** — `~/.hive/daemon.pid` is flock-held; the OS releases it on process exit. If `hive daemon` says "another instance is running" but no `hive` process exists, `rm ~/.hive/daemon.pid`.
- **Schema version mismatch** — daemon refuses to start if the DB is on a different schema than the binary expects. Rebuild (`make install`) and restart.
- **No claude on PATH** — every pipeline run will fail with "claude subprocess: exec: not found". Install Claude Code from your provider.
- **No ANTHROPIC_API_KEY** — only matters if `chat.provider = "api"` in `~/.hive/config.toml`. The default `"claude-code"` provider uses your subscription via `claude -p`.

## Status

Phase 7 = v1.0. Solo-dev project; not currently accepting external contributions.

## License

No license has been chosen yet. Until a LICENSE file is added, all rights are reserved.
