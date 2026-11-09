# Fastener

A daemonless command-line tool that launches, tracks and stops agent sessions:
headless runs of Claude Code, Codex, Pi and other agent CLIs. Each session is
supervised by its own small monitor process, so the UI, scheduler or shell that
started it can restart freely.

Status: v0.1. Design: [`docs/design.md`](docs/design.md); `--json` contract: [`docs/json-schema.md`](docs/json-schema.md); planned work: [`docs/followups.md`](docs/followups.md).

License: Apache-2.0.

## Quick start

```bash
go install github.com/ytjohn/fastener/cmd/fastener@latest
fastener check pi --model github-copilot/claude-haiku-4.5   # prove the backend works and served that model
fastener run claude --cwd ~/proj --prompt "Summarize README.md" --max-runtime 10m
fastener ps
fastener ps --filter label=bead=factotum-abc --json   # filters: label=K[=V], state=S, backend=B, name=GLOB; repeatable, ANDed
fastener wait <name>; fastener inspect <name>
```

`run` prints the session name and returns once the agent has started; the
session keeps running if your shell exits. Then:

```bash
fastener logs <name> [-f] [--stderr] [--generation N]   # agent output (-f follows; --generation: a restored session's earlier run)
fastener stop <name> [--grace 10s]      # SIGTERM, then SIGKILL after the grace period; also cleans up leftover processes of an exited session
fastener rm <name> [--force]            # remove a finished session (refuses if it has leftover processes; --force kills them)
fastener prune [--older-than D] [--dry-run]   # remove old ended sessions (see Retention)
fastener check --all --static           # profile and binary checks for every backend, no model call
```

### Retention

Ended sessions are deleted after `retention` (config; default `168h`, 7 days),
measured from when they ended. `run` and `ps` do this opportunistically, at most
once an hour (`.last-prune` in the state directory); `ps` prunes before it lists.
`fastener prune [--older-than D] [--dry-run] [--json]` does it on demand. Only
`exited` and `restore-failed` sessions are removed. Sessions that are running,
`lost`, pinned, that still have leftover processes, or whose leftover-process
check could not be done (reason `unknown`) are never removed (`prune` reports
the old ones it kept). Prune never acts on less than 1h from config: a smaller `retention` is clamped to 1h with a warning. After an old session is
auto-pruned, its name can be reused by `run --name`. Set `auto_prune = false` in the config to turn off the
opportunistic pruning.

`logs` shows the agent's stdout as recorded in `output.log`. After the agent
exits, the monitor reads its stdout for at most 2 s more: if a process the agent
started keeps the pipe open, later output is not recorded, and `inspect --json`
shows `"output_truncated": true` in `status`.

Add `--json` to `run`, `inspect`, `ps`, `prune`, `backends`, `check`, `authorize` and `version` for
machine-readable output (see `docs/json-schema.md`). `wait` exits with the
agent's exit code: 124 on `--timeout`; 125 if the session was lost, failed to
launch or was removed while waiting, or if the agent exited 0 but reported an
error (as Pi does on a model error); 127 for a session that never existed.
`run --follow` exits with the same codes as `wait` (minus 124).

Backends are TOML profiles: `fastener backends` lists them. Add your own in
`~/.config/fastener/backends/<name>.toml`; a file there replaces the built-in of
the same name.

`check` also compares the model the backend reports serving with `--model`. The served
name must start with the requested one (`claude-haiku-4.5` matches
`claude-haiku-4-5-20251001`), so a short alias such as `--model haiku` fails; pass
`--expect-model claude-haiku` alongside it. A backend that does not report its model
gets a `skip`.

The permission level is `--permissions`, else the config's `default_permissions`, else
the profile's own `default_permissions`, else `auto`. Pi has no permission system, so its
profile defines only `skip` and makes it the default; `--permissions auto` on Pi is an
error. `check --all` skips backends that do not define the resolved level; at least one
backend must actually be checked.

State lives under `~/.local/state/fastener` and config under `~/.config/fastener`;
`FASTENER_STATE_DIR` and `FASTENER_CONFIG_DIR` override them.

Sessions run under a transient user systemd unit when one is available and
you have lingering enabled (`--launcher auto`, the default: `systemctl --user
is-system-running` says `running` or `degraded`, and `loginctl show-user $UID -p
Linger` is `yes`); otherwise, and on macOS, the monitor is started with
`setsid`. Pick one with `--launcher systemd|setsid` or `launcher = "..."` in
`config.toml`. `--launcher systemd` without linger works but warns: the user
manager, and every session in it, stops when you log out (`loginctl
enable-linger` fixes that). In systemd mode, for a session with `--max-runtime`,
`stop --grace` cannot exceed the session's own `--grace` (it is clamped with a
warning), because the unit's runtime backstop was sized from it. CI has no user
systemd, so the systemd launcher is tested only on machines that have one.

### Remote bridges

`fastener run claude --remote --cwd DIR [--name N]` starts `claude remote-control`
in DIR as a long-running session you drive from claude.ai/code or the Claude app.
It takes no prompt and has no default time limit. `run` waits up to `remote_wait`
(config, default 15s) for the bridge URL and prints `<name>  <url>`; `inspect`
shows `remote_url` and the `remote_sessions` opened so far, and `stop` ends the
bridge and its sessions. Claude refuses a folder whose trust dialog was never
accepted: `run --remote` then exits 1 with `<dir> is not a trusted workspace`
(`"reason": "untrusted_workspace"` with `--json`) and names the command that trusts
it (below). Redrawn status lines are dropped from `output.log`, so a
bridge left running for days does not fill the disk. `--permissions skip` maps to
`--permission-mode bypassPermissions`. Other backends opt in with a `[remote]`
profile section (design §8.1).

### Keeping a bridge across reboots

```bash
fastener run claude --remote --restart on-reboot --cwd ~/proj --name proj
fastener install-restore            # once: runs `fastener restore` at boot (or login)
fastener install-restore --print    # show what it would install
```

A bridge started with `--restart on-reboot` is pinned (never pruned). After a reboot
it reads `lost`; `fastener restore` relaunches every lost on-reboot bridge as the next
generation, in the same folder with the same arguments (re-rendered from the current
profile), so Claude reattaches the same `env_…` environment and its sessions. The
previous generation's logs stay readable with `fastener logs <name> --generation 1`.
If the profile no longer has `[remote]` or the session's permission level, the binary
is gone, or the folder is no longer trusted, the session becomes `restore-failed` with a
reason (`inspect` shows it; nothing falls back to something else); fix the cause and run
`fastener restore <name>`. A bridge is relaunched with the launcher it last ran under. If
processes of the old run are still alive, restore skips it: `fastener stop <name>`, then
`fastener restore <name>`. `restore --dry-run` shows what it would do.

`install-restore` installs a one-shot `fastener-restore.service` systemd user unit
(warning if lingering is off, since the unit then runs only at login), or on macOS a
LaunchAgent `~/Library/LaunchAgents/net.ytjohn.fastener.restore.plist` (it loads at
login; the command prints the `launchctl bootstrap` line to load it now), or, with
neither, prints a crontab `@reboot` line. The trigger records the state and config
directories in use when you install it. `--uninstall` removes it. Headless sessions
are never rerun.

### Trusting a folder for remote bridges

```bash
fastener authorize claude-folder-trust ~/proj   # explicit, one-off; edits Claude's config
fastener authorize --list [--json]               # what Fastener has authorized
fastener authorize --revoke ~/proj               # forget it (Claude's config is left as is)
```

`claude remote-control` only runs in a folder whose trust dialog was accepted.
`authorize claude-folder-trust` records that acceptance without the dialog: it sets
`projects[<key>].hasTrustDialogAccepted = true` in Claude Code's own config
(`$CLAUDE_CONFIG_DIR/.claude.json`, else `~/.claude.json`), which is what the dialog
does. `<key>` is computed as Claude computes it: the git repository root when the folder is
inside a repository (the main repository's root for a linked worktree; a submodule's own
root), so the whole repository is trusted (the output shows the key), else the folder. The home
directory and `/` are refused, and Claude must have run once so that its config exists.
If Claude would use a different file (a non-empty legacy `~/.claude/.config.json`, or
`CLAUDE_CODE_CUSTOM_OAUTH_URL` set), the command refuses and names it.

The edit is made the way Claude makes its own: under Claude's lock (`.claude.json.lock`;
it waits up to 10 s for a running Claude), re-reading the file, changing only that field
and keeping every other value byte for byte, then an atomic rename (the mode is kept;
the file ends up owned by whoever ran the command). A copy of the
previous file goes to `~/.config/fastener/backups/` (the last 5 are kept). A folder that
is already trusted is left alone.

Running the command is the explicit decision; Fastener's allowlist just records it
(`authorizations.json` in the config directory; `trust_dirs = [...]` in
`config.toml` is an extra, hand-maintained list). Nothing else in Fastener writes
Claude's config, and `run --remote` never authorizes a folder by itself. `--revoke`
only removes Fastener's record; to make Claude ask again, delete the folder's entry
under `projects` in Claude's config.

### Leftover processes

An agent can start processes that outlive it. **systemd mode kills them;
setsid mode keeps them running and tracks them** (`inspect` shows `orphans: N`,
`rm` refuses until `stop` or `rm --force` cleans them up).

- **The agent exits normally:** systemd SIGKILLs its leftovers as soon as the
  session ends; setsid leaves them running, tracked.
- **`fastener stop`:** both send the agent's process group SIGTERM, wait the
  grace period, then SIGKILL; systemd also SIGKILLs anything that left the group.
- **The monitor dies:** both kill the agent (Linux; on macOS the agent keeps
  running, tracked); systemd SIGKILLs the rest at once, setsid leaves
  grandchildren running, tracked.
- **Logout without linger, or the runtime backstop:** systemd stops the session
  like an outside SIGTERM (`ended_by=signal`, with the normal grace) and then
  kills the leftovers; under setsid, logout does not touch the session.

Use `--launcher setsid` if an agent's background processes must keep running.

Linux and macOS both run the full test suite in CI. macOS has no parent-death
signal, so there a killed monitor leaves the agent itself running too (setsid is
the only launcher on macOS); it is tracked as an orphan of the `lost` session and
`stop` or `rm --force` cleans it up. The tests of that path run on Linux only.
