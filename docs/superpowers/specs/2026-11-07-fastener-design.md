# Fastener — Design

**Status:** Approved design, pre-implementation
**Date:** 2026-11-07
**Repository:** `github.com/ytjohn/fastener` · **License:** Apache-2.0 · **Language:** Go
**Platforms:** Linux and macOS (Windows out of scope)

## 1. What Fastener is

Fastener is a daemonless command-line tool that launches, tracks and stops **agent sessions**: runs of existing agent CLIs such as Claude Code, Codex and Pi. Each session runs independently of whatever launched or is watching it. A UI, orchestrator or shell can restart freely without affecting running sessions.

It is closer to Podman's CLI experience than to an agent framework. Fastener does not:
- run a model;
- schedule or prioritize work;
- decide whether work succeeded.

### Goals

- Launch headless agent runs and, later, interactive and remote-controlled sessions, from any directory, with explicit permissions and time limits.
- `ps`, `inspect`, `logs`, `wait`, `stop`, `rm`: table stakes for every session kind.
- Report mechanical facts about how a session ended, plus fields reliably extracted from the agent's output.
- Survive the launching shell, Fastener upgrades and (for opted-in sessions) host reboots.
- Support new agent CLIs by adding a TOML profile, without code changes.

### Non-goals

- **A daemon or server of any kind.** See §2.
- **A library.** Go packages are `internal/`. The stable interface is the CLI and its `--json` output.
- **Managing arbitrary processes.** There is no raw `run -- <cmd>` mode; every session goes through a backend profile.
- **Judging outcomes.** "Did the task succeed?" belongs to callers (a dispatcher, a human). Fastener reports facts.
- **Task queues, retries, dependencies.**
- **Network listeners in the core.** See §9.

## 2. Architecture

One binary, two roles:

| Role | Lifetime | Responsibility |
|---|---|---|
| `fastener <command>` (CLI) | Seconds | Resolve the profile, create the session directory, start the monitor, read state, send signals, prune |
| `fastener _monitor <session-dir>` (hidden) | As long as the session | Spawn the agent, write logs, run the parser, enforce limits, write `status.json`; (phase B) own the pty and attach socket |

**Daemonless** means there is no long-running Fastener process except the one monitor per session, which exits with its session. This is Podman's `conmon` model.

**Launchers:** how a monitor is started.

- **systemd** (when a user systemd instance is available):
  `systemd-run --user --unit=fastener-<name> --collect -p KillMode=control-group -p RuntimeMaxSec=<backstop> fastener _monitor <dir>`.
  The cgroup contains the whole process tree. `loginctl enable-linger` keeps it alive after logout.
- **setsid** (otherwise, including macOS): a double fork and `setsid` detaches the monitor from the CLI's session and terminal.

The launcher used is recorded in `session.json`. Behavior is otherwise identical in both modes.

## 3. Session state

All state lives in plain files. There is no database.

```text
${XDG_STATE_HOME:-~/.local/state}/fastener/
  sessions/<name>/
    session.json     # written once by the CLI at creation
    status.json      # written by the monitor (see "single writer")
    output.log       # raw agent output for the current generation
    output.<n>.log   # previous generations (restored sessions)
    monitor.log      # monitor diagnostics
    attach.sock      # phase B
  .last-prune        # prune throttle stamp
```

- **Unique names:** `mkdir` of the session directory is the atomic name reservation; a duplicate name fails. `--name` is optional; when omitted, Fastener generates `<backend>-<6 random base32 chars>`. Names only need to be unique among sessions that still exist. Names match `^[a-z0-9][a-z0-9._-]{0,62}$`.
- **Single writer:** only the monitor writes `status.json` while it is alive, by writing a temporary file and renaming it. After the CLI has confirmed the monitor is dead, it may finalize `status.json` (stop/rm of a `lost` session). The two never write concurrently.
- **Versioning:** every JSON file carries `schema_version`. Newer CLIs read all older versions. Rule: fields are added, never repurposed.

### `session.json` (immutable after creation)

- `schema_version`, `name`, `created_at`, `kind` (`headless` | `interactive`), `backend`
- **Profile provenance:** `source` (`builtin` | the file path), `sha256`
- `argv`: the fully resolved argument list, `cwd`
- `prompt`: for `--prompt`, the text is stored in `prompt.txt`; for `--prompt-file`, the path only
- `permissions`, `model`
- `max_runtime`, `grace`
- **Environment:** `env` (the base list plus `--env` values), `env_file` (the path only)
- `labels`, `restart` (`no` | `on-reboot`), `remote` (bool)
- `launcher` (`systemd` | `setsid`), `fastener_version`

### `status.json` (written by the monitor)

- `schema_version`, `state`, `generation`, `restarts`
- **Monitor process:** `pid`, `start_time`
- **Agent process:** `pid`, `pgid`, `start_time`
- `started_at`, `ended_at`
- **Exit facts:** `ended_by`, `exit_code`, `signal`, `error`
- **Parser fields** (each optional; omitted when unknown): `conversation_id`, `final_message`, `agent_error`, `usage` {`input_tokens`, `output_tokens`, `cost_usd`}, `parse_errors`, `remote_url`

## 4. Session lifecycle

### States

| State | Meaning | Written by |
|---|---|---|
| `starting` | Session directory created, agent not yet spawned | CLI, then the monitor |
| `running` | Agent spawned and alive | Monitor |
| `exited` | Ended; `ended_by` ∈ `exit`, `limit`, `stop`, `signal`, `launch-failed` | Monitor (or the CLI finalizing a dead monitor) |
| `lost` | Monitor not alive and no `exited` recorded | **Inferred** by the CLI on read; never written by the monitor |
| `restore-failed` | `restore` could not resume (phase B) | CLI |

`inspect` adds `agent_alive: true` to a `lost` session whose agent still runs (an orphan, possible on macOS).

### Launch handshake

1. The CLI creates the directory and writes `session.json` and `status.json` with state `starting`.
2. The CLI starts the monitor through the chosen launcher.
3. The CLI waits up to 5 s (configurable) for `running` or `exited`.

If the spawn fails (missing binary, bad working directory), the monitor records `exited` with `ended_by=launch-failed` and an error, and `run` exits non-zero with that error. If the handshake times out, `run` exits non-zero and reports the session name for inspection. `run` never reports success for a session that did not start.

### Liveness

A process counts as alive only if its pid exists **and** its start time matches the recorded `start_time`. Start time comes from `/proc/<pid>/stat` on Linux and `sysctl kern.proc` on macOS. This rules out a reused pid being mistaken for the session. In systemd mode the unit's active state is also checked.

### Stop and time limits

- `fastener stop <name> [--grace D]` sends SIGTERM to the monitor (default grace 10 s). The monitor:
  1. sends SIGTERM to the agent's process group;
  2. waits for the grace period;
  3. sends SIGKILL to the group;
  4. records `ended_by=stop`.
- `--max-runtime` follows the same path and records `ended_by=limit`. In systemd mode, `RuntimeMaxSec` = max_runtime + grace + 30 s acts as a backstop if the monitor hangs.
- `stop` on a `lost` session with a live agent kills the agent's process group directly (after the pid and start-time check) and finalizes `ended_by=stop`.

### Monitor death

| Mode | Agent outcome |
|---|---|
| systemd | The cgroup is killed with the unit, so no orphan is left |
| setsid, Linux | The agent is spawned with `PR_SET_PDEATHSIG=SIGKILL`, so no orphan is left |
| setsid, macOS | The agent may survive; it becomes `lost` with `agent_alive`, and `stop` cleans it up |

### Reboot

- Headless sessions running at reboot become `lost`. They are **never** rerun automatically, because a headless prompt may have side effects.
- Interactive sessions with `--restart=on-reboot` are handled by `restore` (§8).

### Upgrades

- A running monitor is unaffected by replacing the binary on disk.
- The monitor reads its profile and settings once, at start.
- New CLIs read old state through `schema_version`. A `testdata` fixture directory from each released schema version is kept and tested.

## 5. Environment

The agent's environment is explicit. It is never inherited wholesale: `systemd-run` does not pass the caller's environment through, and both modes must behave the same.

- **Base:** `HOME`, `USER`, `LOGNAME`, `LANG`/`LC_*` if set, and the caller's `PATH` captured at launch.
- **`--env-file F`:** read by the monitor at spawn. Only the path is recorded, so this is where secrets go. `KEY=VALUE` lines; `#` comments.
- **`--env K=V`** (repeatable): recorded in `session.json`. The documentation says it is not for secrets.
- **Precedence:** base, then env-file, then `--env`.

Agent CLIs keep using their own credential stores under `HOME`.

## 6. Backend profiles

A profile is TOML describing how a backend's CLI is invoked.

```toml
schema_version = 1
name    = "claude"
command = "claude"

[headless]
args   = ["-p", "--output-format", "stream-json", "--verbose"]
prompt = "stdin"                 # "stdin" | "arg" (arg appends the prompt as the final argument)
parser = "claude-stream-json"    # one of the named parsers in §7

[options]
model = ["--model", "{value}"]   # added only when --model is given

[permissions]                    # one entry per Fastener level
auto = ["--permission-mode", "auto"]
skip = ["--dangerously-skip-permissions"]

[resume]
args = ["--resume", "{conversation_id}"]

# Phase B
[interactive]
args = []
conversation_finder = "claude-transcripts"   # named finder (§8)

[interactive.remote]
args = ["--remote-control"]
url_pattern = 'https://claude\.ai/\S+'
```

### Rules

- **No shell.** Arguments are passed to `exec` directly, never through a shell. Placeholders (`{value}`, `{conversation_id}`) are replaced by plain string substitution within a single argument. Prompts and values can never add or split arguments, and tests cover this against injection attempts.
- **No silent fallback.**
  - A requested permission level, option, mode or `--remote` that the profile does not define is a hard error before anything is created.
  - `--permissions` defaults to `auto`.
  - Fastener never substitutes the backend's default permissions or `skip`.
- **Resolution:** `~/.config/fastener/backends/<name>.toml` (`$XDG_CONFIG_HOME`) **replaces** the embedded built-in of the same name entirely. There is no merging.
- **Validation:** an unknown key, a missing required section or an unknown parser or finder name is an error, both at load time and in `fastener backends --check`.
- **Built-ins** (`profiles/*.toml`, embedded with `go:embed`): `claude`, `codex`, `pi`. Every flag in a built-in profile must be verified against the real CLI by the opt-in smoke tests before release. The values above are illustrative until then.
- **Adding a backend** means writing a TOML file. Adding a *built-in* backend is a PR containing the profile plus a recorded-output test fixture. If the backend needs a new output format, the PR also adds a parser.

## 7. Parsers

Named Go functions selected by profile: `claude-stream-json`, `codex-jsonl`, `none`.

- Run inside the monitor, streaming, one line at a time, while raw output is written to `output.log` unchanged.
- **Extracted fields** (all optional, omitted when unknown):
  - `conversation_id`
  - `final_message`
  - `agent_error`: the agent's own report of failure, independent of the exit code
  - `usage` {`input_tokens`, `output_tokens`, `cost_usd`}
- Unknown event types are ignored. A malformed line increments `parse_errors`. Parser failure never affects the session.
- `status.json` is updated whenever a field changes (debounced to about 1 per second) and always at exit.
- Each parser is tested against recorded real output in `internal/parser/testdata/`. A backend output change is fixed by updating the fixture and the parser.

## 8. Interactive and remote sessions (phase B)

Designed now so that v1's state layout and monitor need no rework; built after v1.

- **The monitor owns the pty** (the `dtach`/`abduco` model, using `github.com/creack/pty`). Not tmux: one tmux server holds every session for a user, which is the shared-daemon failure mode Fastener avoids.
- **Attach:** the monitor listens on `attach.sock`. `fastener attach <name>` connects the user's terminal. The detach key defaults to `ctrl-]` and is configurable. Several clients may attach at once. On attach, the monitor resizes the pty and sends SIGWINCH to force a redraw; Fastener does not emulate the screen. Terminal output is also written to `output.log`.
- **Attach protocol:** versioned framed messages (`hello{version}`, `data`, `resize{rows,cols}`, `detach`), independent of the transport. The Unix socket is the only transport in scope.
- **Remote:** `run --interactive --remote` uses `[interactive.remote]` args. If `url_pattern` matches terminal output, `remote_url` is set. A detached remote session is an interactive session nobody is attached to.
- **Conversation ID:** found by a named **finder** (for example `claude-transcripts`: the newest transcript for the session's `cwd` created after `started_at`). Codex and Pi finders still need investigation and are a known risk.
- **Restart policy:**
  - `--restart=no|on-reboot`, default `no`. `on-reboot` is refused for headless sessions.
  - A session with a restart policy is pinned and never pruned.
  - `fastener restore` relaunches every `lost` session with `on-reboot`:
    - same name;
    - the next `generation` number; the previous `output.log` is rotated to `output.<n>.log`;
    - `[resume]` args with the recorded `conversation_id`;
    - remote control re-enabled.
  - If there is no `conversation_id`, the session becomes `restore-failed` with a reason. **Never a fresh conversation.**
  - `fastener install-restore` installs the trigger: a one-shot `fastener-restore.service` user unit on systemd, a launchd agent on macOS, or prints a cron `@reboot` line otherwise. Without it nothing restarts.

## 9. Future, explicitly out of scope

- **Remote attach.**
  - Works with no extra code as `ssh host fastener attach <name>` (including `tailscale ssh`).
  - Native network transports (tsnet, WebSocket) would be a separate opt-in command (for example `fastener expose <name>`) that bridges `attach.sock` and runs only while wanted.
  - Never inside `_monitor`: per-session tailnet nodes, network listeners in front of shell-capable agents, binary size and vendor coupling all argue against it.
- **Restarting a monitor or agent that crashes** (as opposed to restoring after a reboot).
- **Per-session limits on log size.**
- **Model-assisted anything.** That belongs to callers.

## 10. CLI reference (v1 = headless)

```text
fastener run <backend> [--name N] --cwd DIR (--prompt TEXT | --prompt-file F)
             [--permissions auto|skip] [--model M] [--max-runtime D] [--grace D]
             [--env-file F] [--env K=V]... [--label K=V]... [--attach] [--json]
fastener ps [--all] [--filter label=K=V | state=S | backend=B]... [--json]
fastener inspect <name> [--json]
fastener logs <name> [-f] [--generation N]
fastener wait <name> [--timeout D]
fastener stop <name> [--grace D]
fastener rm <name> [--force]
fastener prune [--older-than D]
fastener backends [--check] [--json]
fastener version [--json]
# phase B
fastener run <backend> --interactive [--remote] [--restart no|on-reboot] ...
fastener attach <name>
fastener restore
fastener install-restore
```

- **`run`:**
  - Detached by default; prints the session name, or the full status with `--json`.
  - `--attach` follows the logs after launch.
  - `--prompt` and `--prompt-file` are mutually exclusive. One is required for headless runs.
- **`ps`:**
  - Shows `running`, `starting` and `lost` sessions plus anything that ended in the last 24 h.
  - `--all` shows everything retained.
  - Filters are ANDed together.
- **`wait`:**
  - Exits with the agent's exit code.
  - Exits 125 for `launch-failed`, `lost` or a `wait` timeout.
  - Exits 128+N when the agent was killed by signal N.
- **`rm`:** refuses a session that is alive unless `--force` (which stops it first).
- **Retention:**
  - Ended, unpinned sessions older than the retention period (default 7 days; config `retention`) are pruned.
  - `run` and `ps` prune opportunistically, at most once per hour (the `.last-prune` stamp).
  - `prune` runs on demand.
  - There is no `--rm`-on-exit flag, because it would race callers reading results.
- **Config:** `~/.config/fastener/config.toml` holds `retention`, `default_permissions` (must be a level defined by each profile used), `handshake_timeout`, `launcher` (`auto` | `systemd` | `setsid`) and `detach_key`.
- **`--json` output** of `run`, `ps`, `inspect`, `backends` and `version` is a versioned, documented contract (`docs/json-schema.md`). Human-readable output is not a contract.

## 11. Code layout

```text
cmd/fastener/          main, CLI wiring (cobra or stdlib flag; decided in the plan)
internal/session/      state dir, schema types, read/write, liveness, prune, labels
internal/monitor/      _monitor: spawn, log, parser feed, limits, signals; (B) pty, attach
internal/launcher/     systemd-run and setsid launchers, detection
internal/profile/      TOML load, validate, render; embedded profiles/*.toml
internal/parser/       claude-stream-json, codex-jsonl, none; testdata/
internal/proc/         pid + start-time lookup (linux, darwin), process-group signals
internal/fakeagent/    test helper agent
docs/                  json-schema.md, profiles.md, design specs
```

## 12. Testing

- **Unit:**
  - profile load and validation;
  - argument rendering, including injection attempts in prompts, models and placeholders;
  - no-silent-fallback errors;
  - parsers against recorded output;
  - schema readers against every older fixture version;
  - the liveness check with a mismatched start time.
- **Fake agent:** `internal/fakeagent`, built by the tests. It emits scripted stream-json or JSONL, sleeps, ignores SIGTERM, crashes, exits with a chosen code, forks a grandchild and reads stdin. A backend profile pointing at it lets the whole CLI-to-monitor path run in CI with no model calls.
- **Lifecycle** (each in setsid mode, and in systemd mode where a user systemd instance exists, otherwise skipped):
  - the session survives the CLI exiting;
  - duplicate names;
  - launch failure;
  - handshake timeout;
  - `stop` with an agent that ignores SIGTERM, including the grandchild being cleaned up;
  - `--max-runtime`;
  - SIGKILL of the monitor (becomes `lost`, and the agent is cleaned up on Linux);
  - a reused pid;
  - an old state fixture read by the new CLI;
  - prune and retention;
  - `wait` exit codes;
  - label filtering.
- **Live smoke** (opt-in, `FASTENER_SMOKE=1`; costs model calls): each built-in profile runs a trivial prompt with `auto` permissions. The test checks for `exited` with exit 0, a non-empty `conversation_id` and a non-empty `final_message`. This is also the release check that built-in flags are correct.
- **CI:** Linux and macOS runners; `go vet`, `staticcheck`, `go test -race`.

## 13. Milestones

1. **v0.1, headless core:**
   - setsid launcher;
   - `run`, `ps`, `inspect`, `logs`, `wait`, `stop`, `rm`;
   - profiles plus the `none` parser;
   - fake agent and lifecycle tests.
2. **v0.2:**
   - systemd launcher;
   - `claude-stream-json` and `codex-jsonl` parsers;
   - built-in `claude`, `codex` and `pi` profiles verified by smoke tests;
   - labels, prune, `backends --check`.
3. **v0.3, phase B:** pty monitor, `attach`, `--remote`, conversation finders, `restore`, `install-restore`.

**Acceptance for v0.3:**
1. Start a detached remote-control Claude session.
2. Disconnect.
3. Upgrade the Fastener binary.
4. `ps` and `inspect` the session, and open `remote_url`.
5. Reboot; `restore` resumes the same conversation.
6. `stop` and `rm` it.

No Fastener daemon is involved at any point.

## 14. Known risks

- The exact flag names for headless runs, permissions and remote control for each backend are unverified (they are gated by smoke tests).
- Finding conversation IDs for interactive Codex and Pi sessions is uninvestigated.
- How Claude auto-permission mode and Codex's equivalent behave in a headless run when an action is denied: whether the denial is reported to the model or the run blocks.
- macOS lacks a parent-death signal, so the orphaned-agent case is handled by `stop` and not prevented.
