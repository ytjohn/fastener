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
  The binary is invoked by its absolute path (`os.Executable()`), because the user manager's PATH differs from the shell's.
- **setsid** (otherwise, including macOS): Go cannot fork without exec. The CLI therefore re-executes itself as `_monitor` with `SysProcAttr{Setsid: true}`, stdin from `/dev/null` and stdout/stderr to `monitor.log`, then releases the child without waiting for it. The CLI knows the monitor's pid immediately.

systemd mode is the more robust of the two. It guarantees the whole process tree is cleaned up (§4) and is preferred when available. The launcher used is recorded per generation (§3). Behavior is otherwise identical in both modes.

## 3. Session state

All state lives in plain files. There is no database.

```text
${XDG_STATE_HOME:-~/.local/state}/fastener/
  sessions/<name>/
    session.json         # intent; written once by the CLI at creation
    prompt.txt           # the prompt (from --prompt, or copied from --prompt-file at run time)
    generations/<n>.json          # launch plan per generation; written by the CLI before the monitor starts
    generations/<n>.launcher.json # launcher identity; written by the CLI right after the monitor starts
    status.json          # runtime facts; written by the monitor (see "single writer")
    stop-request.json    # written by the CLI before signalling a stop
    output.log           # agent stdout (raw stream) for the current generation
    stderr.log           # agent stderr for the current generation (headless; a pty merges both)
    output.<n>.log       # previous generations (restored sessions); likewise stderr.<n>.log
    monitor.log          # monitor diagnostics
    lock                 # flock target for CLI-side finalization
    attach.sock          # phase B
  .last-prune            # prune throttle stamp
```

- **Unique names:** `mkdir` of the session directory is the atomic name reservation; a duplicate name fails. `--name` is optional; when omitted, Fastener generates `<backend>-<6 random base32 chars>`. Names only need to be unique among sessions that still exist. Names match `^[a-z0-9][a-z0-9._-]{0,62}$`.
- **Writers:** all writes are atomic (a temporary file, then rename).
  - **`status.json`:** only the monitor writes it while it is alive. The CLI may finalize it only after confirming the monitor is dead (stop/rm of a `lost` or dead-`starting` session).
  - **Finalizing:** the CLI takes `flock(lock)`, re-reads `status.json`, and leaves it unchanged if it already says `exited`. This way an exit record the monitor wrote just before dying is never overwritten, and two CLIs never finalize at once.
  - **`session.json`, `generations/*.json` and `stop-request.json`:** written only by the CLI.
- **Versioning:** every JSON file carries `schema_version`. Newer CLIs read all older versions. Rule: fields are added, never repurposed.

### `session.json` (intent; immutable after creation)

- `schema_version`, `name`, `created_at`, `kind` (`headless` | `interactive`), `backend`, `cwd`
- `prompt_source`: `inline`, or the `--prompt-file` path. The text is always in `prompt.txt`, copied by the CLI at `run` time, so later edits to the file don't change what a session ran.
- `permissions`, `model`, `max_runtime` (absent = no limit), `grace`
- **Environment:** `env` (resolved base plus `--env` values), `env_file` (the path only)
- `labels`, `restart` (`no` | `on-reboot`), `remote` (bool)

### `generations/<n>.json` (launch plan; written by the CLI *before* starting the monitor)

- `schema_version`, `generation` (the first launch is 1)
- `argv`: the fully resolved argument list
- `parser`, `url_pattern`, `conversation_finder`: copied from the profile
- **Profile provenance:** `source` (`builtin` | the file path), `sha256`
- `marker`: `<name>:<generation>:<random nonce>` (see "Process identity" in §4)
- `fastener_version`, `planned_at`

**The monitor reads only this plan.** It never opens the profile, so the recorded argv and hash are exactly what ran.

### `generations/<n>.launcher.json` (written by the CLI right after starting the monitor, before the handshake)

- `schema_version`, `launcher`: `systemd` with `unit`, or `setsid` with the monitor's `pid` and `start_time`
- `boot_id`, `launched_at`

This is the record that lets the CLI judge whether a monitor is alive before the monitor has written anything (§4).

### `status.json` (runtime facts; written by the monitor)

- `schema_version`, `state`, `generation`
- **Processes:**
  - monitor: `pid`, `start_time`
  - agent: `pid`, `pgid`, `start_time`
  - `boot_id`
- `started_at`, `ended_at`
- **Exit facts:** `ended_by`, `exit_code`, `signal` (the signal that terminated the agent, if any), `error`
- **Parser fields** (each optional; omitted when unknown): `conversation_id`, `final_message`, `agent_error`, `usage` {`input_tokens`, `output_tokens`, `cost_usd`}, `parse_errors`, `remote_url`

### `stop-request.json` (written by the CLI)

- `schema_version`, `generation`, `requested_at`, `grace`, `by` (`stop` | `rm`)

A monitor ignores a request whose `generation` is not its own. `restore` deletes any leftover request before relaunching.

## 4. Session lifecycle

### States

| State | Meaning | Written by |
|---|---|---|
| `starting` | Session directory created, agent not yet spawned | CLI, then the monitor |
| `running` | Agent spawned and alive | Monitor |
| `exited` | Ended; see `ended_by` below | Monitor (or the CLI finalizing a dead monitor) |
| `lost` | Not `exited`, and the monitor is not alive | **Inferred** by the CLI on read; never written by the monitor |
| `restore-failed` | `restore` could not resume (phase B) | CLI |

**What `ended_by` means:**

| Value | Meaning |
|---|---|
| `exit` | The agent ended on its own: exited, or was killed by a signal the monitor did not send. `exit_code` or `signal` says which. |
| `limit` | The monitor's `max_runtime` timer fired. |
| `stop` | The monitor received SIGTERM with a `stop-request.json` present (`fastener stop` or `rm`). |
| `signal` | The monitor received SIGTERM, SIGINT or SIGHUP with no stop request. Something outside Fastener stopped it: logout without linger, `kill`, or systemd's `RuntimeMaxSec` backstop. |
| `launch-failed` | The agent could not be spawned. |

**Process identity.** The monitor puts `FASTENER_SESSION=<marker>` into the agent's environment, and every descendant inherits it. Before Fastener counts or signals any process outside the monitor's own direct supervision, that process must:
- be in the recorded `pgid`;
- carry the current generation's marker in its environment (read from `/proc/<pid>/environ` on Linux, or `sysctl KERN_PROCARGS2` on macOS; both are readable for the user's own processes);
- belong to the current boot, i.e. the `boot_id` matches the recorded one.

A process that cleared its environment is not counted, and is never signalled. The check fails safe: Fastener may miss one of its own orphans, but it never kills a stranger.

**Group liveness for `lost` sessions.** `inspect` reports `orphans: N`: the number of processes that pass the identity check above. `stop` and `rm` signal those pids individually, not the pgid as a whole, so a reused pgid can never redirect a signal.

### Launch handshake

1. The CLI creates the directory and writes `session.json`, `prompt.txt`, `generations/1.json` (the plan) and `status.json` with state `starting`.
2. The CLI starts the monitor through the chosen launcher, passing the session directory and the generation number.
3. The CLI writes `generations/1.launcher.json`.
4. The CLI waits up to 5 s (configurable) for `running` or `exited`.

On start, if a `stop-request.json` for its generation already exists, the monitor records `ended_by=stop` without spawning the agent.

If the spawn fails (missing binary, bad working directory), the monitor records `exited` with `ended_by=launch-failed` and an error, and `run` exits non-zero with that error. If the handshake times out, `run` exits non-zero and reports the session name for inspection. `run` never reports success for a session that did not start.

**Before the monitor has written anything,** a `starting` session is judged using `generations/<n>.launcher.json`:
- **systemd:** the unit is active.
- **setsid:** the monitor's pid and start time are alive.

If that record is missing (the CLI died between launching the monitor and writing it), the session is treated as `lost` once it is older than twice the handshake timeout.

`wait` on a `starting` session waits. `stop` and `rm` on a `starting` session use the normal stop path (a stop request, then SIGTERM to the monitor only), never `systemctl stop`. `systemctl stop` would signal the whole cgroup at once and bypass the monitor's TERM / grace / KILL ordering.

### Liveness

A process counts as alive only if all three hold:
- its pid exists;
- its start time matches the recorded `start_time`;
- the current `boot_id` matches the recorded one.

Sources:
- **Linux:** start time from `/proc/<pid>/stat` field 22 (clock ticks since boot, stored as-is); boot ID from `/proc/sys/kernel/random/boot_id`.
- **macOS:** start time from `sysctl kern.proc.pid` (`p_starttime`); boot ID from `kern.bootsessionuuid` (`kern.boottime` can shift when the clock is adjusted).

This rules out a reused pid, including one reused after a reboot, being mistaken for the session. In systemd mode the unit's active state is also checked.

### Stop and time limits

- `fastener stop <name> [--grace D]` (default grace from `session.json`, 10 s if unset). In systemd mode, a `--grace` longer than the session's grace is clamped to it with a warning, because the unit's timers were sized from the session's grace.
  1. writes `stop-request.json`;
  2. sends SIGTERM to the monitor (or, in systemd mode, `systemctl --user kill --signal=SIGTERM --kill-whom=main` on the unit).
- When it receives SIGTERM, the monitor reads `stop-request.json`, if present, for the grace period. It then:
  1. sends SIGTERM to the agent's process group;
  2. waits for the grace period;
  3. sends SIGKILL to the group;
  4. records `ended_by=stop` (with a request) or `signal` (without one).
- **`--max-runtime`** follows the same path and records `ended_by=limit`. With no `--max-runtime` there is no limit, and `RuntimeMaxSec` is omitted.
- **systemd settings:**
  - `RuntimeMaxSec` = max_runtime + grace + 30 s, a backstop if the monitor hangs;
  - `TimeoutStopSec` = grace + 30 s, so systemd never cuts the grace period short.
- **`stop` on a `lost` session** with orphans signals each identified orphan pid (per the process-identity check above), using the same TERM / grace / KILL sequence. It then finalizes `ended_by=stop`.

### Monitor death

| Mode | Outcome if the monitor is SIGKILLed |
|---|---|
| systemd | The cgroup is killed with the unit; no orphans |
| setsid, Linux | The agent itself is killed by `PR_SET_PDEATHSIG=SIGKILL`. Grandchildren in its process group can survive. They show as `lost` with `orphans: N`, and `stop` or `rm` cleans them up. |
| setsid, macOS | The agent and grandchildren may survive. Same handling: `lost` with `orphans`, cleaned up by `stop` or `rm`. |

**Go implementation constraint:** Linux delivers `PR_SET_PDEATHSIG` when the *thread* that spawned the child exits, not when the process exits (golang/go#27505). The monitor must spawn the agent from a goroutine locked to its OS thread (`runtime.LockOSThread`) that stays alive for the whole session.

### Reboot

- Headless sessions running at reboot become `lost`. They are **never** rerun automatically, because a headless prompt may have side effects.
- Interactive sessions with `--restart=on-reboot` are handled by `restore` (§8).

### Upgrades

- A running monitor is unaffected by replacing the binary on disk.
- The monitor reads its generation plan once, at start, and never reads the profile.
- New CLIs read old state through `schema_version`. A `testdata` fixture directory from each released schema version is kept and tested.

## 5. Environment

The agent's environment is explicit. It is never inherited wholesale: `systemd-run` does not pass the caller's environment through, and both modes must behave the same.

- **Base:** variables copied from the caller at launch, if set, according to the `env_passthrough` config list. The default list:
  - `HOME`, `USER`, `LOGNAME`, `PATH`
  - `LANG`, `LC_*`
  - `SSH_AUTH_SOCK` (so agents can `git push`)
  - `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`, `XDG_RUNTIME_DIR`
  - `TERM`, for interactive sessions only

  The resolved values are recorded in `session.json`.
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

[check]                          # used by `fastener check` (§6.1)
version_args = ["--version"]     # for --static
diagnose = [
  { match = '(?i)not logged in|please run /login|invalid api key', hint = "auth: run `claude` interactively and log in" },
]

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
  - The permission level comes from the `--permissions` flag if given, else the `default_permissions` config, else `auto`.
  - Fastener never substitutes the backend's default permissions or `skip`.
- **Resolution:** `~/.config/fastener/backends/<name>.toml` (`$XDG_CONFIG_HOME`) **replaces** the embedded built-in of the same name entirely. There is no merging.
- **Validation:** an unknown key, a missing required section or an unknown parser or finder name is an error, both at load time and in `fastener check --static`.
- **Built-ins** (`profiles/*.toml`, embedded with `go:embed`): `claude`, `codex`, `pi`. Every flag in a built-in profile must be verified against the real CLI by `fastener check` (the opt-in smoke tests, §12) before release. The values above are illustrative until then.
- **Adding a backend** means writing a TOML file. Adding a *built-in* backend is a PR containing the profile plus a recorded-output test fixture. If the backend needs a new output format, the PR also adds a parser.

### 6.1 `fastener check`: proving a backend works

`fastener check <backend> [run options]` runs a real session through the normal path (same launcher, monitor, profile rendering and parser). It then verifies every step, so "does this backend work, with these options, right now?" has a mechanical answer. It is not a separate code path: if `check` passes, `run` with the same options works.

- **Prompt:** fixed: `Reply with exactly one word: pong`.
- **Defaults:**
  - `--max-runtime 2m`;
  - `--cwd` is a fresh temporary directory, removed afterwards;
  - label `fastener.check=true`;
  - permissions resolved exactly as for `run`.
- **Options:** the same as `run` (`--model`, `--permissions`, `--env-file`, `--env`, `--cwd`, `--launcher`, `--max-runtime`). So `fastener check pi --model X` tests that exact combination.
- **Other flags:**
  - `--keep` keeps the session for inspection; by default it is removed after the check.
  - `--all` checks every resolvable backend.
  - `--static` skips the model call.
  - `--json` gives the step-by-step result.

**Steps** (stop at the first failure; each reports `pass` / `fail` / `skip` with detail):

| Step | Passes when |
|---|---|
| `profile` | The profile resolves and validates; the requested permission level and every given option are mapped |
| `binary` | `command` is found on the session's resolved PATH (not the caller's shell PATH); `version_args` exits 0 |
| `launch` | The handshake reaches `running`; `session.json`, `prompt.txt`, the plan, the launcher record, `status.json` and `output.log` all exist |
| `exit` | The session ends `ended_by=exit` with exit code 0 within the limit |
| `parse` | `conversation_id` and `final_message` were extracted; `agent_error` is not true; `parse_errors` is 0 |
| `reply` | `final_message` contains `pong` (case-insensitive) |
| `cleanup` | The session is stopped if still alive, and removed unless `--keep` |

`--static` runs only `profile` and `binary`, with no model call and no cost. It suits CI and install checks, and replaces a separate `backends --check`.

**Diagnosis.** On failure, the profile's `[check].diagnose` patterns (regexes) are matched against `stderr.log`, `output.log` and the parser's `final_message`. The first match's `hint` is printed. This turns known failures into actionable messages. Examples:
- Claude logged out: "auth: run `claude` interactively and log in".
- A provider no longer serves Pi's default model: "model unavailable: pass --model, or update the backend's default model".

Built-in fallbacks:
- A `limit` ending: "timed out: the agent may be waiting for interactive input (for example a login prompt)".
- Any other unmatched failure: the last 20 lines of `stderr.log`.

`diagnose` entries are data and ship in built-in profiles. Adding one for a newly seen failure is a profile PR.

**Exit code:** 0 if every requested check passed, 1 otherwise. Usage errors exit 2, and an unknown backend exits 127.

**Who uses it:**
- humans after installing or upgrading an agent CLI;
- the release smoke tests (§12);
- callers that want to check a backend before dispatching work to it (for example, catching a logged-out CLI before it wastes an attempt).

Phase B adds `check --interactive [--remote]`. It launches with a pty, confirms the conversation finder found an ID, and (for `--remote`) that `url_pattern` matched.

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
    - the next `generation` number, with a new `generations/<n>.json`; the previous `output.log` is rotated to `output.<n>.log`;
    - `[resume]` args with the recorded `conversation_id`;
    - remote control re-enabled.

    The argument list is **re-rendered from the profile current at restore time** (so a fixed flag takes effect) using the intent in `session.json`. The new generation's plan and launcher files record the new argv, profile hash, marker, launcher and Fastener version. `session.json` stays unchanged.

    If the current profile no longer defines the session's permission level, `[resume]` or `[interactive.remote]`, the session becomes `restore-failed` with that reason. This is the same no-silent-fallback rule as for `run`.
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
             [--env-file F] [--env K=V]... [--label K=V]... [--follow] [--json]
fastener ps [--all] [--filter label=K=V | state=S | backend=B]... [--json]
fastener inspect <name> [--json]
fastener logs <name> [-f] [--generation N]
fastener wait <name> [--timeout D]
fastener stop <name> [--grace D]
fastener rm <name> [--force]
fastener prune [--older-than D]
fastener backends [--json]
fastener check <backend> | --all [--static] [--keep] [run options] [--json]
fastener version [--json]
# phase B
fastener run <backend> --interactive [--remote] [--restart no|on-reboot] ...
fastener attach <name>
fastener restore
fastener install-restore
```

- **`run`:**
  - Detached by default; prints the session name, or the full status with `--json`.
  - `--follow` follows the logs after launch. (This is not `attach`, which is the phase B pty command.)
  - `--prompt` and `--prompt-file` are mutually exclusive. One is required for headless runs.
- **`ps`:**
  - Shows `running`, `starting` and `lost` sessions plus anything that ended in the last 24 h.
  - `--all` shows everything retained.
  - Filters are ANDed together.
- **`wait`:**
  - Exits with the agent's exit code.
  - Exits 128+N when the agent was killed by signal N.
  - Exits 124 if `--timeout` elapses while the session is still running.
  - Exits 125 if the session did not complete normally (`launch-failed`, `lost`, `restore-failed`).
  - Exits 127 for an unknown session name.

  A caller can therefore tell "timed out" from "lost" without a follow-up `inspect`. As with Podman, 124, 125 and 127 can collide with real agent exit codes. The documentation names `inspect --json` as the authoritative answer.
- **`rm`:** refuses a session that is alive, or has orphans, unless `--force` (which stops it first).
- **Retention:**
  - Ended, unpinned sessions older than the retention period (default 7 days; config `retention`) are pruned.
  - `lost` sessions with orphans are never pruned, because pruning would destroy the record needed to clean them up.
  - `run` and `ps` prune opportunistically, at most once per hour (the `.last-prune` stamp).
  - `prune` runs on demand.
  - There is no `--rm`-on-exit flag, because it would race callers reading results.
- **Config:** `~/.config/fastener/config.toml` holds:
  - `retention`
  - `default_permissions` (must be a level defined by each profile used)
  - `handshake_timeout`
  - `launcher` (`auto` | `systemd` | `setsid`)
  - `env_passthrough`
  - `detach_key`
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
  - `stop` versus an external SIGTERM (`ended_by=stop` vs `signal`), and the per-call `--grace`;
  - `--max-runtime`;
  - SIGKILL of the monitor:
    - the session becomes `lost`;
    - on Linux the agent dies;
    - a grandchild shows in `orphans`;
    - `stop` cleans the grandchild up;
  - a monitor that dies before writing `status.json` (becomes `lost` via the generation record);
  - a reused pid, and a mismatched `boot_id`;
  - a process in a reused pgid without the session marker: it is never counted as an orphan and never signalled;
  - a stale `stop-request.json` from an earlier generation is ignored;
  - a profile edited between `run` and the monitor starting: the monitor uses the plan, and the recorded `sha256` matches what ran;
  - concurrent `stop` and `rm` finalizing a dead monitor;
  - an old state fixture read by the new CLI;
  - prune and retention;
  - `wait` exit codes;
  - label filtering.
- **Live smoke** (opt-in, `FASTENER_SMOKE=1`; costs model calls): runs `fastener check --all --json` and requires every built-in backend to pass. This is the release check that built-in flags are correct. There is no separate smoke-test logic.
- **`check` itself** is tested against the fake agent: a passing reply, a wrong reply, `agent_error`, a timeout (built-in hint), a `diagnose` pattern match in `stderr.log`, and a missing binary.
- **CI:** Linux and macOS runners; `go vet`, `staticcheck`, `go test -race`.

## 13. Milestones

1. **v0.1, headless core:**
   - setsid launcher;
   - `run`, `ps`, `inspect`, `logs`, `wait`, `stop`, `rm`;
   - profiles plus the `none` parser;
   - `check`, both full and `--static`, with stderr capture;
   - fake agent and lifecycle tests.
2. **v0.2:**
   - systemd launcher;
   - `claude-stream-json` and `codex-jsonl` parsers;
   - built-in `claude`, `codex` and `pi` profiles verified by smoke tests;
   - labels, prune;
   - `diagnose` patterns for the known Claude auth and Pi model failures.
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
- In setsid mode an agent's descendants can outlive a killed monitor (on Linux: grandchildren only; on macOS: the agent too). This is handled by group-level orphan cleanup, not prevented. systemd mode prevents it.
- Orphan identity fails safe, so some orphans can be missed:
  - a descendant that moves to its own process group, or clears its environment, is never counted or signalled;
  - the check-then-signal sequence leaves a small pid-reuse window. On Linux, `pidfd_open` plus `pidfd_send_signal` can close it.
