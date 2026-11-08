# Fastener — Design

**Status:** v0.1 implemented; this document describes what was built, with later milestones marked
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

- **systemd** (v0.2; in v0.1 `--launcher systemd` is a clear error and `auto` means setsid):
  `systemd-run --user --unit=fastener-<name> --collect -p KillMode=control-group -p RuntimeMaxSec=<backstop> fastener _monitor <dir>`.
  The cgroup contains the whole process tree. `loginctl enable-linger` keeps it alive after logout.
  The binary is invoked by its absolute path (`os.Executable()`), because the user manager's PATH differs from the shell's.
- **setsid** (otherwise, including macOS): Go cannot fork without exec. The CLI therefore re-executes itself as `_monitor` with `SysProcAttr{Setsid: true}`, stdin from `/dev/null` and stdout/stderr to `monitor.log`, then releases the child without waiting for it. The CLI knows the monitor's pid immediately.

systemd mode is the more robust of the two. It guarantees the whole process tree is cleaned up (§4) and is preferred when available. The launcher used is recorded per generation (§3). Behavior is otherwise identical in both modes.

## 3. Session state

All state lives in plain files. There is no database.

The state directory is always an absolute path. `FASTENER_STATE_DIR` overrides it (made absolute if given relative); `XDG_STATE_HOME` is used only when it is absolute, and a relative value is ignored. With no usable `HOME` and no override there is no state directory, and commands say so. The config directory follows the same rules: `FASTENER_CONFIG_DIR`, then an absolute `XDG_CONFIG_HOME`, then `~/.config/fastener`; with no `HOME` it means "no user config".

```text
${FASTENER_STATE_DIR:-${XDG_STATE_HOME:-~/.local/state}/fastener}/
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
  .last-prune            # prune throttle stamp (v0.2)
```

- **Unique names:** `mkdir` of the session directory is the atomic name reservation; a duplicate name fails. `--name` is optional; when omitted, Fastener generates `<backend>-<6 random base32 chars>`. Names only need to be unique among sessions that still exist. Names match `^[a-z0-9][a-z0-9._-]{0,62}$`.
- **Writers:** all writes are atomic (a temporary file, then rename).
  - **`status.json`:** only the monitor writes it while it is alive. The CLI may finalize it only after confirming the monitor is dead (stop/rm of a `lost` or dead-`starting` session).
  - **Finalizing:** the CLI takes `flock(lock)`, re-reads `status.json`, and leaves it unchanged if it already says `exited`. This way an exit record the monitor wrote just before dying is never overwritten, and two CLIs never finalize at once. Under the lock, `Finalize` also re-checks the monitor recorded in `status.json` and in the generation's launcher record; if either is alive, or its liveness cannot be determined, it refuses with `ErrMonitorAlive` and writes nothing. Callers treat that as "the monitor is back" and retry through the normal stop path, once.
  - **Half-created sessions:** a missing `status.json` or `session.json` reads as a `starting` session (the CLI creates the directory before it writes the files). It becomes `lost` after twice the handshake timeout, so a CLI that died mid-create leaves something `rm` can remove.
  - **`session.json`, `generations/*.json` and `stop-request.json`:** written only by the CLI.
- **Versioning:** every JSON file carries `schema_version`. Newer CLIs read all older versions. Rule: fields are added, never repurposed.

### `session.json` (intent; immutable after creation)

- `schema_version`, `name`, `created_at`, `kind` (`headless` | `interactive`), `backend`, `cwd`
- `prompt_source`: `inline`, or the `--prompt-file` path. The text is always in `prompt.txt`, copied by the CLI at `run` time, so later edits to the file don't change what a session ran.
- `permissions`, `model`, `max_runtime` (absent = no limit), `grace`
- **Environment:** `env_base` (variables captured from the caller by the passthrough list), `env` (the `--env` values) and `env_file` (the path only, made absolute). They are stored separately because the env-file is read later, by the monitor, and precedence is base, then env-file, then `--env`.
- `labels`, `restart` (`no` | `on-reboot`), `remote` (bool)

### `generations/<n>.json` (launch plan; written by the CLI *before* starting the monitor)

- `schema_version`, `generation` (the first launch is 1)
- `argv`: the fully resolved argument list. `argv[0]` is resolved to an absolute path at plan time, against the session's PATH; the monitor never does a PATH lookup.
- `prompt_mode` (`stdin` | `arg`), `parser`: copied from the profile. (`url_pattern` and `conversation_finder` arrive with phase B.)
- **Profile provenance:** `profile_source` (`builtin` | the file path), `profile_sha256`
- `marker`: `<name>:<generation>:<random nonce>` (see "Process identity" in §4)
- `fastener_version`, `planned_at`

**The monitor reads only this plan.** It never opens the profile, so the recorded argv and hash are exactly what ran.

### `generations/<n>.launcher.json` (written by the CLI right after starting the monitor, before the handshake)

- `schema_version`, `launcher`: `setsid` with the monitor's `monitor` identity (`pid`, `start_time`), or (v0.2) `systemd` with `unit`
- `boot_id`, `launched_at`

This is the record that lets the CLI judge whether a monitor is alive before the monitor has written anything (§4).

### `status.json` (runtime facts; written by the monitor)

- `schema_version`, `state`, `generation`
- **Processes:**
  - monitor: `pid`, `start_time`
  - agent: `pid`, `pgid`, `start_time`
  - `boot_id`
- `started_at`, `ended_at`
- **Exit facts:** `ended_by`, `exit_code` (absent when a signal killed the agent), `signal` (the signal name that terminated the agent, if any), `error`
- **Parser fields** (each optional; omitted when unknown): `conversation_id`, `final_message` (capped at 64 KiB), `model` (the model the backend says it served; absent after an agent error), `model_source` (`server` or `client`), `agent_error`, `usage` {`input_tokens`, `output_tokens`, `cost_usd`} (present only when the backend reported usage), `parse_errors`; `remote_url` arrives with phase B

**The monitor writes its own identity first.** Right after it loads its plan, before it checks for a stop request, spawns anything or can fail in any other way, the monitor writes `status.json` once (state `starting`, with its `monitor` identity and `boot_id`). Each generation's status is built fresh; nothing is carried over from a previous generation. This shrinks the window in which no record identifies a live monitor.

### `stop-request.json` (written by the CLI)

- `schema_version`, `generation`, `requested_at`, `grace`, `by` (`stop` | `rm`)

A monitor ignores a request whose `generation` is not its own. `restore` deletes any leftover request before relaunching.

## 4. Session lifecycle

### States

| State | Meaning | Written by |
|---|---|---|
| `starting` | Session directory created, agent not yet spawned | CLI, then the monitor (which first records its own identity) |
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
- belong to the current boot, i.e. the `boot_id` matches the recorded one;
- still have the same start time when the identity is re-read *after* the environment was read, so a pid recycled while the environment was being read is dropped.

A process that cleared its environment is not counted, and is never signalled. The check fails safe: Fastener may miss one of its own orphans, but it never kills a stranger.

**Group liveness for `lost` sessions.** `inspect` reports `orphans: N`: the number of processes that pass the identity check above. `stop` and `rm` signal those pids individually (SIGTERM, wait for the grace period, then SIGKILL), not the pgid as a whole, so a reused pgid can never redirect a signal. Each signal re-verifies the pid's start time first. The orphan scan costs a pass over every process, so it runs only where it matters: `inspect`, `wait`, `stop`, `rm` and the launch handshake scan (`session.LoadFull`); `ps` loads sessions cheaply (`session.Load`) and reports no orphan counts. If an orphan survives SIGKILL, `stop` does not finalize the session and `rm` does not remove it.

### Launch handshake

1. The CLI creates the directory and writes `prompt.txt`, `session.json`, `generations/1.json` (the plan) and `status.json` with state `starting`.
2. The CLI starts the monitor through the chosen launcher, passing the session directory and the generation number.
3. The CLI writes `generations/1.launcher.json`. If that write fails it is a warning, not an error, because the monitor records its own identity in `status.json` first thing.
4. The CLI waits up to 5 s (configurable) for `running` or `exited`. On timeout it stops the session (a `stop-request.json` for generation 1 plus SIGTERM to the monitor if it is identified; the monitor honours the request at start and before the spawn), so a late-starting monitor never runs the prompt unattended. The error says the session was stopped.

Every handshake error names the session (`session <name>: ...`) so a caller can inspect or remove it. If the monitor dies during the handshake, the CLI finalizes the session as `launch-failed` only when nothing was ever spawned: no agent identity recorded, status still `starting`, and no marked process alive. A session that is `lost` after a spawned agent (or with surviving marked processes) is left `lost` with its orphans, so `stop` and `rm` can still find them. Should the handshake be unable to reload a view for the error return, it falls back to the cheap view it already has.

On start, if a `stop-request.json` for its generation already exists, the monitor records `ended_by=stop` without spawning the agent. It checks again just before the spawn, and also honours a SIGTERM/SIGINT/SIGHUP that arrived during setup (`ended_by=stop` with a request, else `signal`).

Before spawning, the monitor checks that the session's `cwd` exists and is a directory. If the spawn fails (missing binary, missing or non-directory `cwd`, unreadable env-file), the monitor records `exited` with `ended_by=launch-failed` and an error, and `run` exits non-zero with that error. If the handshake times out, `run` exits non-zero and reports the session name for inspection. `run` never reports success for a session that did not start.

**Before the monitor has written anything,** a `starting` session is judged using `generations/<n>.launcher.json`:
- **systemd** (v0.2): the unit is active. In v0.1 a launcher record with only a unit cannot be judged; this is a known gap (see `docs/followups.md`).
- **setsid:** the monitor's pid and start time are alive.

If that record is missing and the monitor has not yet written `status.json` (the CLI died between launching the monitor and writing it), the session is treated as `lost` once it is older than twice the handshake timeout. Before declaring any session `lost`, the CLI re-reads `status.json` and the launcher record once, because the monitor always writes before it exits.

`wait` on a `starting` session waits. `stop` and `rm` on a `starting` session use the normal stop path (a stop request, then SIGTERM to the monitor only), never `systemctl stop`. `systemctl stop` would signal the whole cgroup at once and bypass the monitor's TERM / grace / KILL ordering.

### Liveness

A process counts as alive only if all three hold:
- its pid exists (a zombie does not count);
- its start time matches the recorded `start_time`;
- the current `boot_id` matches the recorded one.

`proc.Check` returns `(alive, error)`; a non-nil error means liveness could not be determined. Two wrappers fix the policy for that case, and **unknown liveness never means dead**:
- `Alive` is true only when `Check` confirms the process is alive. It guards signalling.
- `MaybeAlive` is false only when `Check` confirms the process is gone. It guards declaring a session `lost` or finalizing it.

`proc.Signal` re-verifies identity and returns a non-`ErrNoProcess` error when liveness is unknown, so a caller never mistakes "could not tell" for "already gone".

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
  Grandchildren may get no grace period of their own: if the leader exits first, the grace wait ends early and the SIGKILL to the group follows immediately.
- **A pending exit beats a signal or limit.** If the agent has already exited by the time the monitor handles a stop signal or the `--max-runtime` timer, the exit stands (`ended_by=exit`); the monitor does not relabel a finished agent as stopped.
- **Zombie pinning (Linux).** After the agent dies, the monitor leaves it unreaped (`waitid` with `WNOWAIT`) until group signalling is finished, then reaps. While the zombie exists its pid and pgid cannot be reused, so the final SIGKILL of the group cannot reach a stranger. macOS has no such pin in v0.1 (`x/sys/unix` has no `Waitid`); the agent is reaped immediately and there is a small pgid-reuse window (see `docs/followups.md`).
- **Output drain.** After the agent exits the monitor waits up to 2 s for its stdout to close. A descendant that keeps the pipe open cannot hold the session open; output written after that is not recorded.
- **`--max-runtime`** follows the same path and records `ended_by=limit`. With no `--max-runtime` there is no limit, and `RuntimeMaxSec` is omitted.
- **systemd settings:**
  - `RuntimeMaxSec` = max_runtime + grace + 30 s, a backstop if the monitor hangs;
  - `TimeoutStopSec` = grace + 30 s, so systemd never cuts the grace period short.
- **`stop` on a `lost` session** with orphans signals each identified orphan pid (per the process-identity check above), using the same TERM / grace / KILL sequence. It then finalizes `ended_by=stop`, unless `Finalize` refuses with `ErrMonitorAlive` (the monitor came back), in which case it takes the normal stop path once.
- **`stop` on an `exited` session** that still has orphans (marker, pgid and boot verified, as above) cleans them up with TERM / grace / KILL and does not re-finalize, since `status.json` is already final. Descendants of a normally exited agent are tracked, not killed automatically.
- **`rm --force`** stops first, as above. For a session whose files are corrupt and cannot be loaded, `rm --force` reads whatever parses (`status.json` and the plan) to find and kill marked processes, then deletes the directory; a warning says when the status or plan could not be read, because orphans cannot then be found.

### Monitor death

| Mode | Outcome if the monitor is SIGKILLed |
|---|---|
| systemd | The cgroup is killed with the unit; no orphans |
| setsid, Linux | The agent itself is killed by `PR_SET_PDEATHSIG=SIGKILL`. Grandchildren in its process group can survive. They show as `lost` with `orphans: N`, and `stop` or `rm` cleans them up. |
| setsid, macOS | The agent and grandchildren may survive. Same handling: `lost` with `orphans`, cleaned up by `stop` or `rm`. |

A parser panic is recovered: it is logged, counted in `parse_errors`, and never ends the session.

**Go implementation constraint:** Linux delivers `PR_SET_PDEATHSIG` when the *thread* that spawned the child exits, not when the process exits (golang/go#27505). The monitor must spawn the agent from a goroutine locked to its OS thread (`runtime.LockOSThread`) that stays alive for the whole session.

### Reboot

- Headless sessions running at reboot become `lost`. They are **never** rerun automatically, because a headless prompt may have side effects.
- Interactive sessions with `--restart=on-reboot` are handled by `restore` (§8).

### Upgrades

- A running monitor is unaffected by replacing the binary on disk.
- The monitor reads its generation plan once, at start, and never reads the profile.
- New CLIs read old state through `schema_version`. Keeping and testing a `testdata` fixture directory from each released schema version is a follow-up; v0.1 does not keep them yet.

## 5. Environment

The agent's environment is explicit. It is never inherited wholesale: `systemd-run` does not pass the caller's environment through, and both modes must behave the same.

- **Base:** variables copied from the caller at launch, if set, according to the `env_passthrough` config list. The default list:
  - `HOME`, `USER`, `LOGNAME`, `PATH`
  - `LANG`, `LC_*`
  - `SSH_AUTH_SOCK` (so agents can `git push`)
  - `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`, `XDG_RUNTIME_DIR`
  - `TERM`, for interactive sessions only

  The resolved values are recorded in `session.json`.
- **`--env-file F`:** read by the monitor at spawn. Only the (absolute) path is recorded, so this is where secrets go. `KEY=VALUE` lines; whole-line `#` comments; one matching pair of outer quotes around a value is stripped. An unreadable file is a `launch-failed`.
- **`--env K=V`** (repeatable): recorded in `session.json`. The documentation says it is not for secrets.
- **Precedence:** base, then env-file, then `--env`.

Agent CLIs keep using their own credential stores under `HOME`.

## 6. Backend profiles

A profile is TOML describing how a backend's CLI is invoked.

```toml
schema_version = 1
name    = "claude"
command = "claude"
# default_permissions = "auto"   # optional; must be a level defined below

[headless]
args   = ["-p", "--output-format", "stream-json", "--verbose"]
prompt = "stdin"                 # "stdin" | "arg" (arg appends "--" and then the prompt)
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

# Phase B: the [interactive] tables below are rejected by v0.1
[interactive]
args = []
conversation_finder = "claude-transcripts"   # named finder (§8)

[interactive.remote]
args = ["--remote-control"]
url_pattern = 'https://claude\.ai/\S+'
```

### Rules

- **No shell.** Arguments are passed to `exec` directly, never through a shell. Placeholders (`{value}`, `{conversation_id}`) are replaced by plain string substitution within a single argument. Prompts and values can never add or split arguments, and tests cover this against injection attempts.
  - `prompt = "arg"` always appends `--` and then the prompt, so a prompt starting with `-` cannot become a flag. (`--` protects only the next positional argument; an agent CLI may still interpret the prompt itself, such as a leading `@`.)
  - `--model` values starting with `-` are rejected, as an injection guard.
  - `argv[0]` is resolved to an absolute path at plan time, against the session's PATH (the base environment plus env-file plus `--env`, not the caller's shell).
- **Command validation.** `command` is a bare name (`[A-Za-z0-9._+][A-Za-z0-9._+-]*`) or an absolute path, with no whitespace. A relative path such as `./x` is rejected so the plan never depends on the launch directory.
- **Template validation.** Every `[options]` template and `[resume].args` must be non-empty and contain its placeholder (`{value}`, `{conversation_id}`) exactly once; a placeholder anywhere else (headless args, permissions) is an error.
- **No silent fallback.**
  - A requested permission level, option, mode or `--remote` that the profile does not define is a hard error before anything is created.
  - The permission level comes from the `--permissions` flag if given, else the `default_permissions` config, else the profile's own optional `default_permissions`, else `auto`. This holds for both `run` and `check`, and the resolved level is recorded in `session.json`. A profile's `default_permissions` must name a level that profile defines (validated at load). An explicit `--permissions` must be a known level (`auto` or `skip`); anything else is a usage error (exit 2) for `run` and `check`.
  - Fastener never substitutes the backend's default permissions or `skip`.
- **Resolution:** `~/.config/fastener/backends/<name>.toml` (`$XDG_CONFIG_HOME`) **replaces** the embedded built-in of the same name entirely. There is no merging.
- **Validation:** an unknown key, a missing required section or an unknown parser or finder name is an error, both at load time and in `fastener check --static`.
- **Built-ins** (`profiles/*.toml`, embedded with `go:embed`): `claude`, `codex`, `pi`. Every flag in a built-in profile is verified against the real CLI by `fastener check` (the opt-in smoke tests, §12) before release; all three passed on the development machine.
  - **`claude`:** `-p --output-format stream-json --verbose`, prompt on stdin, parser `claude-stream-json`; `auto` is `--permission-mode auto`, `skip` is `--dangerously-skip-permissions`.
  - **`codex`:** `exec --json --skip-git-repo-check`, prompt on stdin, parser `none` until v0.2; `auto` is `--approve-for-me`, `skip` is `--dangerously-bypass-approvals-and-sandbox`. `--skip-git-repo-check` is there because Fastener sessions name their working directory explicitly and `check` runs in a temporary non-git directory.
  - **`pi`:** `-p --mode json`, prompt as an argument, parser `pi-json`. Pi has no permission system, so the profile defines only `skip` and sets `default_permissions = "skip"`: Pi sessions get `skip` without a flag (unless the config's `default_permissions` names another level). An explicit `--permissions auto` is still a hard error rather than a silent fallback. Pi exits 0 and only warns on stderr when a provider does not offer the requested model; a diagnose entry turns that warning into a hint.
- **Adding a backend** means writing a TOML file. Adding a *built-in* backend is a PR containing the profile plus a recorded-output test fixture. If the backend needs a new output format, the PR also adds a parser.

### 6.1 `fastener check`: proving a backend works

`fastener check <backend> [run options]` runs a real session through the normal path (same launcher, monitor, profile rendering and parser). It then verifies every step, so "does this backend work, with these options, right now?" has a mechanical answer. It is not a separate code path: if `check` passes, `run` with the same options works.

- **Prompt:** fixed: `Reply with exactly one word: pong`.
- **Defaults:**
  - `--max-runtime 2m`;
  - `--cwd` is a fresh temporary directory, removed afterwards;
  - label `fastener.check=true`;
  - permissions resolved exactly as for `run` (so `check pi` uses Pi's profile default, `skip`).
- **Options:** the same as `run` (`--model`, `--permissions`, `--env-file`, `--env`, `--cwd`, `--launcher`, `--max-runtime`). So `fastener check pi --model X` tests that exact combination.
- **Other flags:**
  - `--keep` keeps the session for inspection; by default it is removed after the check.
  - `--all` checks every resolvable backend. Under `--all`, a backend whose profile does not define the resolved permission level (the flag, else config, else the profile's `default_permissions`, else `auto`) is a skip, not a failure: one `profile` step with status `skip`, and `skipped: true` in JSON. `--all` exits 0 when every non-skipped backend passed. `--all` requires at least one checked backend: if every backend is skipped, or none exists, it exits 1. An explicit `check <backend>` for an undefined level still fails at the `profile` step, and an explicit `--permissions` outside `auto|skip` is a usage error (exit 2) for `run` and `check`.
  - `--static` skips the model call.
  - `--expect-model M` (check only) names the model the backend must report serving, when it differs from what `--model` passes (for example `--model haiku --expect-model claude-haiku`). Without it, the expectation is `--model`. With `--static` it has no effect, and `check` warns on stderr.
  - `--json` gives the step-by-step result.

**Steps** (stop at the first failure; each reports `pass` / `fail` / `skip` with detail):

| Step | Passes when |
|---|---|
| `profile` | The profile resolves and validates; the requested permission level and every given option are mapped |
| `binary` | `command` is found on the session's resolved PATH (not the caller's shell PATH); `version_args` exits 0 |
| `launch` | The handshake reaches `running`; `session.json`, `prompt.txt`, the plan, the launcher record, `status.json` and `output.log` all exist |
| `exit` | The session ends `ended_by=exit` with exit code 0 within the limit |
| `parse` | `conversation_id` and `final_message` were extracted; `agent_error` is not true (on failure the detail includes the agent's error text, `final_message`, cut to about 200 bytes); `parse_errors` is 0. With the `none` parser this step is `skip`. |
| `model` | The served `model` matches the expectation (`--expect-model`, else `--model`) under the prefix rule below. `skip` when the backend does not report its model (the detail says when an expectation could therefore not be verified); `pass` with `served <model>` when no model was requested. |
| `reply` | `final_message` contains `pong` (case-insensitive). With the `none` parser the text searched is `output.log` with every line containing the prompt removed, so an agent that merely echoes its input cannot pass. |
| `cleanup` | The session is stopped if still alive, and removed unless `--keep` |

**The model rule.** The `model` step catches a backend that served a different model than the one asked for: an org policy silently dropping a model, or Pi's pattern-based `--model` resolving to something else. Both names are lowercased; a provider prefix (the text before the last `/`) is split off; `.` becomes `-` in the model names. If both sides name a provider, the providers must be equal. The served model name must then equal the expected name, or start with it followed by `-`, `_` or `:`. So `claude-haiku-4.5` matches `claude-haiku-4-5-20251001` and `gpt-5` matches `gpt-5.6-luna`, but `gpt-5` does not match `gpt-50`, and a short alias such as `sonnet` does not match `claude-sonnet-5-5`. On a mismatch whose served name contains the expected name after its start (the alias case), the hint suggests naming the model from its start (`--model claude-sonnet`) or keeping `--model` and passing `--expect-model`; an expectation ending in a separator gets "drop the trailing separator"; a served provider that ends with `/<expected provider>` (for example `openrouter/anthropic` for `anthropic`) gets "the served provider is <full>; include it in --expect-model"; otherwise the hint is "the backend substituted a different model".

An expectation with no model name after the provider (`github-copilot/`, `/`, or an explicitly empty `--expect-model`) is a usage error (exit 2). When `model_source` is `client` (the agent CLI only recorded the id it requested, as with Pi's `openai-responses` adapter), the prefix rule still applies, since it catches the CLI resolving a pattern to another model, but a pass appends "(client-reported: the backend does not confirm the serving model, so a provider-side swap would not be detected)".

`--static` runs only `profile` and `binary`, with no model call and no cost. It suits CI and install checks, and replaces a separate `backends --check`.

**Diagnosis.** On failure, the profile's `[check].diagnose` patterns (regexes) are matched against a corpus that depends on the source, so a model's ordinary reply never triggers a hint: `stderr.log` and the status `error` always; `final_message` only when the agent reported an error or the parser is `none`; `output.log` only when the parser is `none`. A `limit` ending gets the timeout hint first, before any profile pattern (the corpus is not consulted). Otherwise the first match's `hint` is printed, and the last 20 lines of `stderr.log` are always included as detail when present. This turns known failures into actionable messages. Examples:
- Claude logged out: "auth: run `claude` interactively and log in".
- A provider no longer serves Pi's default model: "model unavailable: pass --model, or update the backend's default model".

Built-in fallbacks:
- A `limit` ending: "timed out: the agent may be waiting for interactive input (for example a login prompt)".
- Any other unmatched failure: the last 20 lines of `stderr.log` (no hint).

`diagnose` entries are data and ship in built-in profiles. Adding one for a newly seen failure is a profile PR.

**Exit code:** 0 if every requested check passed, 1 otherwise. Usage errors exit 2, and an unknown backend exits 127.

**Who uses it:**
- humans after installing or upgrading an agent CLI;
- the release smoke tests (§12);
- callers that want to check a backend before dispatching work to it (for example, catching a logged-out CLI before it wastes an attempt).

Phase B adds `check --interactive [--remote]`. It launches with a pty, confirms the conversation finder found an ID, and (for `--remote`) that `url_pattern` matched.

## 7. Parsers

Named Go functions selected by profile. v0.1 ships `claude-stream-json`, `pi-json` and `none`; `codex-jsonl` arrives in v0.2 (Codex uses `none` until then, so its `parse` step is `skip` and `reply` searches `output.log`).

- **`claude-stream-json`** (`claude -p --output-format stream-json --verbose`): `conversation_id` from any event's `session_id`; `model` from the last assistant event's `message.model` (a bare model id the Anthropic API reports; `model_source` `server`; Claude Code's `<synthetic>` placeholder is ignored), falling back to the `system/init` event's `model` (the CLI's own setting; `model_source` `client`) until an assistant event arrives; an error `result` clears `model`; `final_message`, `agent_error` (`is_error`) and `usage` from the `result` event.
- **`pi-json`** (`pi -p --mode json`): `conversation_id` from the `session` event's `id`. `model`, `model_source`, `final_message` and `agent_error` come from the last assistant `message_end`:
  - `agent_error` is `stopReason` `error` or `aborted` (as pi's print mode treats them);
  - `final_message` is the concatenated `text` content blocks (`thinking` blocks are skipped); on `error` it is `errorMessage`, and on `aborted` it is `errorMessage` or else `Request aborted`;
  - `model` is `provider/` plus `responseModel` when present, else `model`. Pi's adapters differ (pi 0.84.4): `anthropic-messages` overwrites `model` with the server's id; `openai-completions` keeps the requested id in `model` and sets `responseModel` only when the server reported a different one; `openai-responses` records only the requested id. So `model_source` is `client` for `api == "openai-responses"` and `server` otherwise. After an agent error `model` and `model_source` are cleared, since the attempted model never answered, and a last message without a model clears them too.

  `usage` is summed over every assistant `message_end` in the run (each reports one API call): `input + cacheRead + cacheWrite` input tokens, `output` output tokens and `cost.total`. Pi exits 0 even when the model call failed, so `agent_error` is what catches it.

- Run inside the monitor, streaming, one line at a time, while raw output is written to `output.log` unchanged.
- **Extracted fields** (all optional, omitted when unknown):
  - `conversation_id`
  - `final_message`
  - `model`: the model the backend reports it actually served, `provider/model` when the backend names the provider, else the bare model id. It can differ from the requested `--model` (an alias or pattern resolved to a full id, or a substitution); `check` compares the two (§6.1). Absent after an agent error.
  - `model_source`: `server` when the API response named the model, `client` when the agent CLI only recorded the id it requested (a provider-side substitution would not show).
  - `agent_error`: the agent's own report of failure, independent of the exit code
  - `usage` {`input_tokens`, `output_tokens`, `cost_usd`}: emitted only when the backend reported usage. `input_tokens` is all input tokens for the run, including cache reads and writes (Anthropic's own `usage.input_tokens` excludes cache tokens).
- Unknown event types are ignored. A malformed line increments `parse_errors`. Parser failure never affects the session: a parser panic is recovered, logged to `monitor.log` and counted in `parse_errors`. A line longer than 16 MB is written to `output.log` but not parsed (counted in `parse_errors`); the cap limits parsing, not memory. `final_message` is capped at 64 KiB.
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
fastener ps [--all] [--filter label=K=V | state=S | backend=B]... [--json]   # --filter: v0.2
fastener inspect <name> [--json]
fastener logs <name> [-f] [--stderr] [--generation N]   # --generation: phase B
fastener wait <name> [--timeout D]
fastener stop <name> [--grace D]
fastener rm <name> [--force]
fastener prune [--older-than D]   # v0.2
fastener backends [--json]
fastener check <backend> | --all [--static] [--keep] [--expect-model M] [run options] [--json]
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
  - `--prompt` and `--prompt-file` are mutually exclusive. One is required for headless runs, and `--cwd` is required.
  - Labels are stored in v0.1 (`check` uses `fastener.check=true`); filtering by them is v0.2.
  - `--launcher systemd` is a clear error in v0.1.
- **`ps`:**
  - Shows `running`, `starting` and `lost` sessions plus anything that ended in the last 24 h.
  - `--all` shows everything retained.
  - Filters (v0.2) are ANDed together.
  - It loads sessions without the orphan scan, so it reports no orphan counts; `inspect` does.
- **`wait`:**
  - Exits with the agent's exit code.
  - Exits 128+N when the agent was killed by signal N.
  - Exits 124 if `--timeout` elapses while the session is still running.
  - Exits 125 if the session did not complete normally (`launch-failed`, `lost`, `restore-failed`), and when the agent exited 0 but the parser's `agent_error` is true: Pi's json mode exits 0 after a failed model call, which would otherwise read as success. A non-zero exit code is returned unchanged.
  - Exits 127 for an unknown session name.

  A caller can therefore tell "timed out" from "lost" without a follow-up `inspect`. As with Podman, 124, 125 and 127 can collide with real agent exit codes. The documentation names `inspect --json` as the authoritative answer.
- **`logs`:** prints `output.log`; `--stderr` prints `stderr.log`; `-f` follows until the session ends.
- **`rm`:** refuses a session that is alive, or has orphans (including descendants an exited agent left behind), unless `--force` (which stops it first). `--force` also removes a corrupt session that cannot be loaded, after killing whatever marked processes it can still identify. Orphans that survive SIGKILL block removal.
- **Retention (v0.2; v0.1 never prunes):**
  - Ended, unpinned sessions older than the retention period (default 7 days; config `retention`) are pruned.
  - `lost` sessions with orphans are never pruned, because pruning would destroy the record needed to clean them up.
  - `run` and `ps` prune opportunistically, at most once per hour (the `.last-prune` stamp).
  - `prune` runs on demand.
  - There is no `--rm`-on-exit flag, because it would race callers reading results.
- **Config:** `~/.config/fastener/config.toml` holds:
  - `retention`
  - `default_permissions` (must be a level defined by each profile used; it overrides every profile's own `default_permissions`)
  - `handshake_timeout`
  - `launcher` (`auto` | `systemd` | `setsid`)
  - `env_passthrough`
  - `detach_key`
- **Environment overrides:** `FASTENER_STATE_DIR` and `FASTENER_CONFIG_DIR` override the state and config locations (§3).
- **`--json` output** of `run`, `ps`, `inspect`, `backends`, `check` and `version` is a versioned, documented contract (`docs/json-schema.md`). Human-readable output is not a contract.

## 11. Code layout

```text
cmd/fastener/          entry point; calls internal/cli (stdlib flag, no CLI framework)
internal/cli/          commands, flag parsing, views, check
internal/session/      state dir, schema types, read/write, liveness, prune, labels
internal/monitor/      _monitor: spawn, log, parser feed, limits, signals; (B) pty, attach
internal/launcher/     setsid launcher (systemd in v0.2), selection
internal/config/       config.toml and config/state locations
internal/runenv/       base environment, env-file parsing, PATH lookup
internal/profile/      TOML load, validate, render; embedded profiles/*.toml
internal/parser/       claude-stream-json, codex-jsonl, none; testdata/
internal/proc/         identity, Check/Alive/MaybeAlive, marked-process scan (linux, darwin)
internal/testbin/fakeagent/  test helper agent
internal/version/      version string (ldflags)
docs/                  json-schema.md, design specs
```

## 12. Testing

- **Unit:**
  - profile load and validation;
  - argument rendering, including injection attempts in prompts, models and placeholders;
  - no-silent-fallback errors;
  - parsers against recorded output;
  - schema readers against every older fixture version;
  - the liveness check with a mismatched start time.
- **Fake agent:** `internal/testbin/fakeagent`, built by the tests. It emits scripted stream-json or JSONL, sleeps, ignores SIGTERM, crashes, exits with a chosen code, forks a grandchild and reads stdin. A backend profile pointing at it (built by the tests; `internal/testbin/fakeagent`) lets the whole CLI-to-monitor path run in CI with no model calls.
- **Lifecycle** (v0.1 tests setsid mode only on Linux; the systemd-mode runs are deferred to v0.2 with the systemd launcher):
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
  - an agent that exits normally but leaves a grandchild: the session is `exited` with `orphans: N`, `rm` refuses, `stop` kills the grandchild without re-finalizing, then `rm` succeeds;
  - a monitor that dies before writing `status.json` (becomes `lost` via the generation record);
  - a reused pid, and a mismatched `boot_id`;
  - a process in a reused pgid without the session marker: it is never counted as an orphan and never signalled;
  - a stale `stop-request.json` from an earlier generation is ignored;
  - a profile edited between `run` and the monitor starting: the monitor uses the plan, and the recorded `sha256` matches what ran;
  - concurrent `stop` and `rm` finalizing a dead monitor (deferred to v0.2; `Finalize` has a unit test);
  - an old state fixture read by the new CLI (deferred to v0.2);
  - prune and retention (deferred to v0.2, with the feature);
  - `wait` exit codes;
  - label filtering (deferred to v0.2, with the feature).
- **Live smoke** (opt-in, `FASTENER_SMOKE=1`; costs model calls): runs `fastener check --all --json` and requires every built-in backend to pass. This is the release check that built-in flags are correct. There is no separate smoke-test logic.
- **`check` itself** is tested against the fake agent: a passing reply, a wrong reply, `agent_error`, a timeout (built-in hint), a `diagnose` pattern match in `stderr.log`, and a missing binary.
- **CI:** Linux and macOS runners; `go vet`, `go test -race`. The macOS job is `continue-on-error` in v0.1 because its lifecycle tests have never run there. `staticcheck` is not wired in yet.
- **Deferred to v0.2:** concurrent `stop`/`rm` through the CLI (`Finalize` has a unit test), `--grace` clamping in systemd mode, schema fixtures per release, and prune/retention and label filtering (those features are v0.2).

## 13. Milestones

1. **v0.1, headless core:**
   - setsid launcher;
   - `run`, `ps`, `inspect`, `logs`, `wait`, `stop`, `rm`;
   - profiles (built-in `claude`, `codex`, `pi`) plus the `claude-stream-json` and `none` parsers (`claude-stream-json` moved into v0.1 because `check` is of little use for Claude without it);
   - labels (stored);
   - `check`, both full and `--static`, with stderr capture and diagnose patterns;
   - `FASTENER_STATE_DIR` and `FASTENER_CONFIG_DIR`;
   - fake agent and lifecycle tests.
2. **v0.2:**
   - systemd launcher;
   - `codex-jsonl` parser and a Pi JSON parser (after capturing real output);
   - `ps --filter`, prune and retention;
   - more `diagnose` patterns;
   - macOS hardening (see `docs/followups.md`).
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
- On macOS the exited agent is reaped immediately, so its pgid is not pinned while the group is signalled, and the TERM/KILL sequence has small race windows. Linux pins the zombie with `waitid(WNOWAIT)`.
- Unknown liveness (an unreadable `/proc` or failed sysctl) is treated as maybe-alive, so a session can stay `running` or block finalization until liveness can be read.
- A normally exited agent can leave descendants running. They are tracked (`inspect` shows `orphans: N` for the `exited` session; `rm` refuses without `--force`), never killed implicitly; `stop` or `rm --force` cleans them up.
- In setsid mode an agent's descendants can outlive a killed monitor (on Linux: grandchildren only; on macOS: the agent too). This is handled by group-level orphan cleanup, not prevented. systemd mode prevents it.
- Orphan identity fails safe, so some orphans can be missed:
  - a descendant that moves to its own process group, or clears its environment, is never counted or signalled;
  - the check-then-signal sequence leaves a small pid-reuse window. On Linux, `pidfd_open` plus `pidfd_send_signal` can close it.
