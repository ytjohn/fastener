# Fastener — Design

**Status:** v0.1 implemented, plus the v0.2 systemd launcher; this document describes what was built, with later milestones marked
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

- **systemd** (v0.2):
  `systemd-run --user --unit=fastener-<name>-g<gen>-<rand>.service --collect --quiet -p Type=exec -p KillMode=mixed -p TimeoutStopSec=<grace+30s> [-p RuntimeMaxSec=<max_runtime+grace+30s>] -p LimitNOFILE=<soft>:<hard> -p WorkingDirectory=/ -p StandardInput=null -p StandardOutput=append:<monitor.log> -p StandardError=append:<monitor.log> -- <abs fastener> _monitor <abs dir> <gen>`.
  - The cgroup contains the whole process tree. Without lingering (`loginctl enable-linger`) the user manager, and every unit in it, stops when the user's last login session ends.
  - **`KillMode=mixed`.** When systemd stops the unit (the `RuntimeMaxSec` backstop, logout without linger, shutdown), only the main process (the monitor) gets SIGTERM, so the monitor runs its own TERM / grace / KILL of the agent (§4). Once the main process is gone, whether it exited normally or was killed, systemd sends SIGKILL to whatever is left in the cgroup at once. `control-group` would instead SIGTERM every process at once and then wait `TimeoutStopSec` before the SIGKILL.
  - **`LimitNOFILE`** carries the caller's open-file limit (`RLIMIT_NOFILE`, soft and hard), because the user manager's default soft limit (`DefaultLimitNOFILESoft`, usually 1024) is far below a typical shell's, and setsid would pass the caller's limit through. Go raises its own soft limit to hard-1 at startup and restores the original in its children, so when `getrlimit` shows that raise the CLI reads the original soft limit from a child (`/bin/sh -c 'ulimit -Sn'`).
  - The binary is invoked by its absolute path (`os.Executable()`), because the user manager's PATH differs from the shell's. `$` in the command is written as `$$`, because the manager expands both `$VAR` and `${VAR}` in it.
  - **A failed `systemd-run`.** The unit name is chosen before the call. If `systemd-run` fails or times out, the manager may still have accepted the job, so the CLI checks the unit: if it is alive (or its state is unknown) it is killed outright (`systemctl --user kill --signal=SIGKILL`, the whole cgroup, since nothing should be running yet) and `reset-failed`, and the launch fails naming the unit. If that kill fails, the launcher returns a unit-only record instead of an error, so the handshake and `stop` can still manage the unit.
  - **Unit name.** Session names already match `[a-z0-9._-]`, all valid in unit names, so no escaping is needed. The generation keeps a restored generation (§8.4) distinct, and a random suffix keeps two state directories (or a name reused while its old unit is still stopping) from colliding.
  - `Type=exec` makes `systemd-run` return only once the monitor has been executed. The CLI then reads the unit's `MainPID`, takes its identity (`proc.Of`) and reads `MainPID` again to rule out a reused pid; the launcher record holds both the unit and that monitor identity, so everything that works by monitor identity works the same in both modes. If the identity cannot be had, the record holds only the unit, which is judged by the unit's state (§4).
- **setsid** (otherwise, including macOS): Go cannot fork without exec. The CLI therefore re-executes itself as `_monitor` with `SysProcAttr{Setsid: true}`, stdin from `/dev/null` and stdout/stderr to `monitor.log`, then releases the child without waiting for it. The CLI knows the monitor's pid immediately.

**Selection** (`--launcher`, else config `launcher`, default `auto`):
- `auto` uses systemd when `systemctl --user is-system-running` prints `running` or `degraded`, `systemd-run` is on PATH, **and** the user has lingering enabled (`loginctl show-user <uid> -p Linger --value` is `yes`); each probe is one call with a 1 s timeout. Otherwise it uses setsid. So macOS, containers, a shell with no user bus (no `XDG_RUNTIME_DIR`) and a user without linger get setsid: `auto` never picks a launcher whose sessions would stop at logout.
- `systemd` is an error, naming the probe's failure, when no user manager is usable. Without linger it works, but warns once on stderr that sessions will stop at logout.
- `setsid` always uses setsid.
systemd mode is the more robust of the two. It guarantees the whole process tree is cleaned up (§4) and is preferred when available. The launcher used is recorded per generation (§3).

**Leftover processes: the one deliberate difference between the modes.** In systemd mode, whatever is still in the cgroup when the monitor is gone gets SIGKILL from systemd. In setsid mode it keeps running, and is tracked as `orphans` for `stop` or `rm` to clean up. What counts as an orphan depends on the session (§4, "Group liveness"): for headless sessions, and any session without `restart = "on-reboot"`, only marked processes still in the agent's process group, so a process the agent deliberately moved out of it (a daemonized tmux server, an ssh-agent, a dev server, and shells later opened inside them) is never counted or killed; for a `restart = "on-reboot"` bridge that is lost (or that `stop` finalized from lost) and for a `restore-failed` session, every marked process in any group, because `restore` must not relaunch beside them.

| Case | systemd | setsid |
|---|---|---|
| The agent exits normally, leaving descendants | SIGKILLed as soon as the monitor exits; `exited`, `orphans: 0` | Keep running; `exited` with `orphans: N`; `rm` refuses until `stop` |
| `fastener stop` / `rm --force` | The monitor's TERM / grace / KILL of the agent's group; then anything outside the group is SIGKILLed with the cgroup | The monitor's TERM / grace / KILL of the agent's group; anything that left the group keeps running |
| The monitor dies (SIGKILL, crash) | The agent dies (`PR_SET_PDEATHSIG`); the rest of the cgroup is SIGKILLed at once; `lost`, `orphans: 0` | The agent dies (Linux); grandchildren keep running; `lost` with `orphans: N` |
| Logout without linger, or the `RuntimeMaxSec` backstop | SIGTERM to the monitor only: TERM / grace / KILL, `ended_by=signal`, then the cgroup is SIGKILLed | Logout does not touch the session (unless logind's `KillUserProcesses=yes`); there is no backstop |

Use `--launcher setsid` to keep leftover processes running and tracked.

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
    restore.lock         # flock target that serialises `restore` of this session (§8.4)
    attach.sock          # interactive generations: the monitor's attach socket (§8.2), while it runs
  .last-prune            # prune throttle stamp: empty until a prune completes, then its time; mtime is the throttle; also the prune flock target
```

- **Unique names:** `mkdir` of the session directory is the atomic name reservation; a duplicate name fails. `--name` is optional; when omitted, Fastener generates `<backend>-<6 random base32 chars>`. Names only need to be unique among sessions that still exist. Names match `^[a-z0-9][a-z0-9._-]{0,62}$`.
- **Writers:** all writes are atomic (a temporary file, then rename).
  - **`status.json`:** only the monitor writes it while it is alive. The CLI may finalize it only after confirming the monitor is dead (stop/rm of a `lost` or dead-`starting` session).
  - **Finalizing:** the CLI takes `flock(lock)`, re-reads `status.json`, and leaves it unchanged if it already says `exited`. This way an exit record the monitor wrote just before dying is never overwritten, and two CLIs never finalize at once. Under the lock, `Finalize` also re-checks the monitor recorded in `status.json` and in the generation's launcher record; if either is alive, or its liveness cannot be determined, it refuses with `ErrMonitorAlive` and writes nothing. Callers treat that as "the monitor is back" and retry through the normal stop path, once.
  - **Half-created sessions:** a missing `status.json` or `session.json` reads as a `starting` session (the CLI creates the directory before it writes the files). It becomes `lost` after twice the handshake timeout, so a CLI that died mid-create leaves something `rm` can remove.
  - **`session.json`, `generations/*.json` and `stop-request.json`:** written only by the CLI.
- **Versioning:** every JSON file carries `schema_version`. Newer CLIs read all older versions. Rule: fields are added, never repurposed.

### `session.json` (intent; immutable after creation)

- `schema_version`, `name`, `created_at`, `kind` (`headless` | `remote` | `interactive`; an interactive remote session is `interactive` with `remote: true`), `backend`, `cwd`
- `prompt_source`: `inline`, `stdin` (for `--prompt-file -`), or the `--prompt-file` path. The text is always in `prompt.txt`, copied by the CLI at `run` time, so later edits to the file don't change what a session ran.
- `permissions`, `model`, `max_runtime` (absent = no limit), `grace`
- **Environment:** `env_base` (variables captured from the caller by the passthrough list), `env` (the `--env` values) and `env_file` (the path only, made absolute). They are stored separately because the env-file is read later, by the monitor, and precedence is base, then env-file, then `--env`.
- `labels`, `restart` (`no` | `on-reboot`), `remote` (bool)

### `generations/<n>.json` (launch plan; written by the CLI *before* starting the monitor)

- `schema_version`, `generation` (the first launch is 1)
- `argv`: the fully resolved argument list. `argv[0]` is resolved to an absolute path at plan time, against the session's PATH; the monitor never does a PATH lookup.
- `prompt_mode` (`stdin` | `arg` | `none` for a remote bridge, or an interactive session started without a prompt), `parser`, `dedupe_frames` (remote bridges, §8.1): copied from the profile. (`conversation_finder` arrives with interactive restore.)
- `url_pattern` (v0.4, `run --interactive --remote`): the profile's `[interactive.remote] url_pattern`, which the monitor matches against the terminal output to set `remote_url` (§8.2). `env` (v0.4): the profile's `[interactive].env`. `session_id` and `finder` (v0.4): the conversation ID passed at launch and the finder that locates the conversation (§8.2).
- `pty` (v0.4, interactive generations only): `{rows, cols}`, the initial window size (0 or absent means 24 × 80). Its presence is what makes the monitor run the agent on a pty (§8.2); a plan without it, including every plan written before v0.4, runs exactly as before.
- **Profile provenance:** `profile_source` (`builtin` | the file path), `profile_sha256`
- `marker`: `<name>:<generation>:<random nonce>` (see "Process identity" in §4)
- `fastener_version`, `planned_at`

**The monitor reads only this plan.** It never opens the profile, so the recorded argv and hash are exactly what ran.

### `generations/<n>.launcher.json` (written by the CLI right after starting the monitor, before the handshake)

- `schema_version`, `launcher`: `setsid` with the monitor's `monitor` identity (`pid`, `start_time`), or `systemd` with `unit` and, when it could be read from the unit's `MainPID`, `monitor`
- `boot_id`, `launched_at`

This is the record that lets the CLI judge whether a monitor is alive before the monitor has written anything (§4).

### `status.json` (runtime facts; written by the monitor)

- `schema_version`, `state`, `generation`
- **Processes:**
  - monitor: `pid`, `start_time`
  - agent: `pid`, `pgid`, `start_time`
  - `boot_id`
- `started_at`, `ended_at`
- **Exit facts:** `ended_by`, `exit_code` (absent when a signal killed the agent), `signal` (the signal name that terminated the agent, if any), `error`, `output_truncated` (true when the stdout drain timed out; see "Output drain" in §4)
- `restore_reason` (with state `restore-failed`, §8.4): why `restore` could not relaunch the session; `error` has the message
- **Attach** (interactive generations, while the monitor runs; cleared in the exit record): `attach_socket`, the path the monitor listens on, and `attached_clients`, how many clients are attached (flushed with the parser fields, about once a second)
- **Parser fields** (each optional; omitted when unknown): `conversation_id`, `final_message` (capped at 64 KiB), `model` (the model the backend says it served; absent after an agent error), `model_source` (`server` or `client`), `agent_error`, `usage` {`input_tokens`, `output_tokens`, `cost_usd`} (present only when the backend reported usage), `parse_errors`; for remote bridges `remote_url` and `remote_sessions` (§8.1)

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
| `restore-failed` | `restore` could not relaunch the session (§8.4); `restore_reason` says why | CLI |

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

**Group liveness for `lost` sessions.** `inspect` reports `orphans: N`: the number of processes that pass the identity check above. Which processes are scanned depends on the session:
- **Headless sessions, and any session without `restart = "on-reboot"`** (`lost`, or `exited` after spawning an agent): the marker **in the recorded process group only**, as in v0.1. A process the agent moved out of its group on purpose (`setsid`, daemonizing: a tmux server, an ssh-agent, a dev server, and anything the user later runs inside them) is never counted, and `stop` and `rm --force` never signal it, even after a monitor crash.
- **`restart = "on-reboot"` sessions that are `lost`, or `exited` because `stop` finalized them from `lost`, and every `restore-failed` session:** the marker **in any process group**, the same scan `restore` uses (§8.4), so `stop` clears exactly what blocks a restore and a retry never dead-ends. A recorded boot other than the current one needs no scan. For a process found outside the recorded group the process-group condition above is replaced by the marker and boot checks alone. `stop` and `rm` signal those pids individually (SIGTERM, wait for the grace period, then SIGKILL), not the pgid as a whole, so a reused pgid can never redirect a signal. Each pid's identity is verified before it is signalled; on Linux it is pinned with a pidfd before the check and both signals go through that pidfd (see Liveness). The orphan scan costs a pass over every process, so it runs only where it matters: `inspect`, `wait`, `stop`, `rm` and the launch handshake scan (`session.LoadFull`); `ps` loads sessions cheaply (`session.Load`) and reports no orphan counts. If an orphan survives SIGKILL, `stop` does not finalize the session and `rm` does not remove it.

### Launch handshake

1. The CLI creates the directory and writes `prompt.txt`, `session.json`, `generations/1.json` (the plan) and `status.json` with state `starting`.
2. The CLI starts the monitor through the chosen launcher, passing the session directory and the generation number.
3. The CLI writes `generations/1.launcher.json`. If that write fails it is a warning, not an error, because the monitor records its own identity in `status.json` first thing.
4. The CLI waits up to 5 s (configurable) for `running` or `exited`. On timeout it stops the session (a `stop-request.json` for generation 1 plus SIGTERM to the monitor if it is identified; the monitor honours the request at start and before the spawn), so a late-starting monitor never runs the prompt unattended. The error says the session was stopped.

Every handshake error names the session (`session <name>: ...`) so a caller can inspect or remove it. If the monitor dies during the handshake, the CLI finalizes the session as `launch-failed` only when nothing was ever spawned: no agent identity recorded, status still `starting`, and no marked process alive. A session that is `lost` after a spawned agent (or with surviving marked processes) is left `lost` with its orphans, so `stop` and `rm` can still find them. Should the handshake be unable to reload a view for the error return, it falls back to the cheap view it already has.

On start, if a `stop-request.json` for its generation already exists, the monitor records `ended_by=stop` without spawning the agent. It checks again just before the spawn, and also honours a SIGTERM/SIGINT/SIGHUP that arrived during setup (`ended_by=stop` with a request, else `signal`).

Before spawning, the monitor checks that the session's `cwd` exists and is a directory. If the spawn fails (missing binary, missing or non-directory `cwd`, unreadable env-file), the monitor records `exited` with `ended_by=launch-failed` and an error, and `run` exits non-zero with that error. If the handshake times out, `run` exits non-zero and reports the session name for inspection. `run` never reports success for a session that did not start.

**Before the monitor has written anything,** a `starting` session is judged using `generations/<n>.launcher.json`:
- **A monitor identity** (setsid, and systemd when `MainPID` was read): the monitor's pid and start time are alive.
- **Only a unit** (systemd): `systemctl --user show <unit> -p ActiveState -p LoadState`. `active`, `activating`, `deactivating`, `reloading` and `refreshing` are alive; `inactive`, `failed`, `maintenance`, and a unit that is no longer loaded (collected) are dead. Any error running `systemctl` means unknown, which counts as maybe-alive (and fills `liveness_error`), exactly as for a process. `Finalize`'s re-check under the lock judges such a record the same way.

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

**Signalling without a reuse window (Linux).** A check followed by `kill(pid)` leaves a window: the process can exit and its pid be reused between the two. On Linux, `proc.Open` closes it by pinning first: it calls `pidfd_open(pid)`, *then* verifies pid, start time and boot ID, and returns a `proc.Handle` whose signals go through `pidfd_send_signal`. The pidfd names that exact process instance:
- if the pid was reused before the open, the start-time check fails and nothing is signalled;
- if the process dies after the open, the check fails (gone, or a zombie), and once it is reaped the pidfd gives `ESRCH` (`ErrNoProcess`) even if the pid now belongs to a stranger.

`proc.Signal` is `Open`, one signal, `Close`, so the monitor SIGTERM from `stop` is covered too. Orphan cleanup holds one handle per orphan across the whole TERM / grace / KILL sequence, so a pid that exits and is reused during the grace period cannot receive the SIGKILL. Where `pidfd_open` is refused (`ENOSYS` before Linux 5.3, `EPERM` under seccomp, or any error other than `ESRCH`), and on macOS, which has no pidfd, a handle holds only the identity and each signal re-verifies it before `kill(2)`, as before; the fallback is silent. The monitor's own `kill(-pgid)` of the agent's group does not use pidfds; the zombie pin protects it (below).

Sources:
- **Linux:** start time from `/proc/<pid>/stat` field 22 (clock ticks since boot, stored as-is); boot ID from `/proc/sys/kernel/random/boot_id`.
- **macOS:** start time from `sysctl kern.proc.pid` (`p_starttime`); boot ID from `kern.bootsessionuuid` (`kern.boottime` can shift when the clock is adjusted).

This rules out a reused pid, including one reused after a reboot, being mistaken for the session. In systemd mode the unit's active state is checked when no monitor identity is recorded.

### Stop and time limits

- `fastener stop <name> [--grace D]` (default grace from `session.json`, 10 s if unset). In systemd mode, for a live session with a `--max-runtime`, a `--grace` longer than the session's grace is clamped to it with a warning, because the unit's `RuntimeMaxSec` backstop was sized from the session's grace and could otherwise fire mid-grace (recorded as `signal`, not `stop`). With no max runtime there is no `RuntimeMaxSec`, and `TimeoutStopSec` does not bound a stop the monitor runs itself, so nothing is clamped.
  1. writes `stop-request.json`;
  2. sends SIGTERM to the monitor by its identity; in systemd mode with only a unit recorded, `systemctl --user kill --kill-whom=main --signal=SIGTERM <unit>`, retried as `--kill-who=main` if that option is unrecognized (older systemd only has `--kill-who`; newer systemd renamed it and accepts `--kill-who` only as an unambiguous prefix of `--kill-whom`). Never `systemctl stop`.
- When it receives SIGTERM, the monitor reads `stop-request.json`, if present, for the grace period. It then:
  1. sends SIGTERM to the agent's process group;
  2. waits for the grace period;
  3. sends SIGKILL to the group;
  4. records `ended_by=stop` (with a request) or `signal` (without one).
  Grandchildren may get no grace period of their own: if the leader exits first, the grace wait ends early and the SIGKILL to the group follows immediately.
- **A later stop request can shorten the grace.** While the monitor waits out the grace period, a further SIGTERM (or SIGINT/SIGHUP) makes it re-read `stop-request.json`. A request whose `requested_at` is newer than the last one seen moves the SIGKILL to `requested_at + grace` if that is sooner; it never extends the wait. So a later `stop --grace <shorter>` shortens the remaining grace of a stop already in progress. (`rm --force` asks for the session's normal grace, so it does not shorten one.) A signal with no newer request is logged and ignored.
  - **Clocks.** The first stop's deadline is measured from when the monitor received the signal, on the monotonic clock. A later request's deadline is `requested_at + grace`, a wall-clock time written by the CLI, so a step of the system clock can move it earlier or later (never past the deadline already in force).
  - **`ended_by`.** It is `stop` if any stop request was seen, including a newer one that did not shorten the wait. A `--max-runtime` termination stays `limit` even if a stop request shortened its grace.
- **A pending exit beats a signal or limit.** If the agent has already exited by the time the monitor handles a stop signal or the `--max-runtime` timer, the exit stands (`ended_by=exit`); the monitor does not relabel a finished agent as stopped.
- **Zombie pinning.** After the agent dies, the monitor leaves it unreaped until group signalling is finished, then reaps it with `cmd.Wait`, the only reaper. While the zombie exists its pid and pgid cannot be reused, so the final SIGKILL of the group cannot reach a stranger. How the exit is seen without reaping:
  - **Linux:** `waitid` with `WNOWAIT`.
  - **macOS** (`x/sys/unix` has no `Waitid` there): a kqueue `EVFILT_PROC` / `NOTE_EXIT` knote, registered by the spawn goroutine right after `cmd.Start`; `kevent` blocks until the exit. If the agent exited before the registration, `kevent` fails with `ESRCH` and the monitor waits for the zombie instead (`sysctl kern.proc.pid` shows `p_stat == SZOMB`). The non-blocking check used when a signal or the limit races the exit is that same zombie check. The kqueue is close-on-exec and closed once the exit is seen.
- **Output drain.** After the agent exits the monitor waits up to 2 s for its stdout to close. A descendant that keeps the pipe open cannot hold the session open, so this is deliberate: anything written to stdout after those 2 s is lost and never reaches `output.log`. When the drain times out the monitor says so in `monitor.log` and records `output_truncated: true` in `status.json`, so callers can tell the log may be incomplete. (stderr goes straight to `stderr.log` through the descendant's own file descriptor and is not affected.)
- **`--max-runtime`** follows the same path and records `ended_by=limit`. With no `--max-runtime` there is no limit, and `RuntimeMaxSec` is omitted.
- **systemd settings:**
  - `RuntimeMaxSec` = max_runtime + grace + 30 s, a backstop if the monitor hangs;
  - `TimeoutStopSec` = grace + 30 s, so when systemd stops the unit it never cuts the monitor's grace period short;
  - `KillMode=mixed`: systemd's own stop sends SIGTERM to the monitor only; leftovers in the cgroup get SIGKILL once the monitor is gone (§2).
- **`stop` on a `lost` session** with orphans signals each identified orphan pid (per the process-identity check above), using the same TERM / grace / KILL sequence. It then finalizes `ended_by=stop`, unless `Finalize` refuses with `ErrMonitorAlive` (the monitor came back), in which case it takes the normal stop path once.
- **`stop` on an `exited` session** that still has orphans (marker, pgid and boot verified, as above) cleans them up with TERM / grace / KILL and does not re-finalize, since `status.json` is already final. In setsid mode descendants of a normally exited agent are tracked, not killed automatically; in systemd mode the cgroup kill ends them as soon as the monitor exits (§2).
- **`rm --force`** stops first, as above. For a session whose files are corrupt and cannot be loaded, `rm --force` reads whatever parses (`status.json` and the plan) to find and kill marked processes, then deletes the directory; a warning says when the status or plan could not be read, because orphans cannot then be found.

### Monitor death

| Mode | Outcome if the monitor is SIGKILLed |
|---|---|
| systemd | The monitor is the unit's main process, so its death stops the unit and, with `KillMode=mixed`, systemd SIGKILLs everything left in the cgroup at once, including a grandchild that ignores SIGTERM (the agent also dies by `PR_SET_PDEATHSIG`). The session reads `lost` with no orphans. |
| setsid, Linux | The agent itself is killed by `PR_SET_PDEATHSIG=SIGKILL`. Grandchildren in its process group can survive. They show as `lost` with `orphans: N`, and `stop` or `rm` cleans them up. |
| setsid, macOS | macOS has no parent-death signal, so the agent itself survives, along with any grandchildren. Same handling: `lost` with `orphans` (the agent counts as one), cleaned up by `stop` or `rm`. |

A parser panic is recovered: it is logged, counted in `parse_errors`, and never ends the session.

**Go implementation constraint:** Linux delivers `PR_SET_PDEATHSIG` when the *thread* that spawned the child exits, not when the process exits (golang/go#27505). The monitor must spawn the agent from a goroutine locked to its OS thread (`runtime.LockOSThread`) that stays alive for the whole session. macOS uses the same locked-thread spawn and wait, though it has no `PDEATHSIG`.

### Reboot

- Headless sessions running at reboot become `lost`. They are **never** rerun automatically, because a headless prompt may have side effects.
- Remote bridges started with `--restart on-reboot` are relaunched by `restore` (§8.4); interactive sessions follow in v0.4.

### Upgrades

- A running monitor is unaffected by replacing the binary on disk.
- The monitor reads its generation plan once, at start, and never reads the profile.
- New CLIs read old state through `schema_version`. `internal/session/testdata/state/<release>/` keeps real session directories as each release wrote them (v0.1.0 so far), and a test loads every one with the current code. A release that changes a schema adds a directory; existing ones are never edited.

## 5. Environment

The agent's environment is explicit. It is never inherited wholesale: `systemd-run` does not pass the caller's environment through, and both modes must behave the same.

- **Base:** variables copied from the caller at launch, if set, according to the `env_passthrough` config list. The default list:
  - `HOME`, `USER`, `LOGNAME`, `PATH`
  - `LANG`, `LC_*`
  - `SSH_AUTH_SOCK` (so agents can `git push`)
  - `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`, `XDG_RUNTIME_DIR`
  - `TERM`, for interactive sessions only: the caller's `TERM`, or `xterm-256color` when the caller has none (or `dumb`)

  The resolved values are recorded in `session.json`.
- **Profile env** (interactive sessions): `[interactive].env` pairs, recorded in the plan (`env`), come after the base and before the env-file.
- **`--env-file F`:** read by the monitor at spawn. Only the (absolute) path is recorded, so this is where secrets go. `KEY=VALUE` lines; whole-line `#` comments; one matching pair of outer quotes around a value is stripped. An unreadable file is a `launch-failed`.
- **`--env K=V`** (repeatable): recorded in `session.json`. The documentation says it is not for secrets.
- **Precedence:** base, then (interactive) the profile's `[interactive].env`, then env-file, then `--env`.

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

[remote]                         # run --remote (§8.1); no prompt
args              = ["remote-control", "--name", "{name}"]
parser            = "claude-remote"
untrusted_pattern = '(?i)workspace not trusted'
untrusted_hint    = "fastener authorize claude-folder-trust {dir}"
dedupe_frames     = true

[remote.permissions]             # per-level override of [permissions]
skip = ["--permission-mode", "bypassPermissions"]

# v0.4: an interactive session (`run --interactive`), the TUI on a pty
[interactive]
args   = []
prompt = "arg"            # "arg": an optional initial prompt goes after "--"; "none": none taken
# parser = "none"         # default
# env = ["KEY=VALUE"]     # set for the agent (after env_base, before --env-file and --env)
# [interactive.permissions] overrides [permissions] per level, as [remote.permissions] does
session_id = ["--session-id", "{session_id}"]   # optional: pass a fresh UUID as the conversation ID
finder     = "claude-transcripts"                # optional: a built-in finder (§8.2)

[interactive.remote]      # run --interactive --remote
args              = ["--remote-control", "{name}"]
url_pattern       = 'https://claude\.ai/code/session_[A-Za-z0-9_-]+'
untrusted_pattern = '(?is)quick\s*safety\s*check\b.{0,1000}?\byes,\s*i\s*trust\s*this\s*folder'
untrusted_hint    = "fastener authorize claude-folder-trust {dir}"
```

### Rules

- **No shell.** Arguments are passed to `exec` directly, never through a shell. Placeholders (`{value}`, `{conversation_id}`) are replaced by plain string substitution within a single argument. Prompts and values can never add or split arguments, and tests cover this against injection attempts.
  - `prompt = "arg"` always appends `--` and then the prompt, so a prompt starting with `-` cannot become a flag. An agent CLI may still interpret the prompt argument itself (Pi reads a leading `@` as a file to attach, even after `--`), so prefer `prompt = "stdin"` whenever the backend reads piped stdin; every built-in profile does.
  - `--model` values starting with `-` are rejected, as an injection guard.
  - `argv[0]` is resolved to an absolute path at plan time, against the session's PATH (the base environment plus env-file plus `--env`, not the caller's shell).
- **Command validation.** `command` is a bare name (`[A-Za-z0-9._+][A-Za-z0-9._+-]*`) or an absolute path, with no whitespace. A relative path such as `./x` is rejected so the plan never depends on the launch directory.
- **Template validation.** Every `[options]` template and `[resume].args` must be non-empty and contain its placeholder (`{value}`, `{conversation_id}`) exactly once; a placeholder anywhere else (headless args, permissions) is an error. `[remote].args` and `[interactive.remote].args` must be non-empty and may contain `{name}` at most once; `[interactive.remote].url_pattern` is required and, like the untrusted pattern, must compile; `[interactive].prompt` must be `arg` or `none`, and `[interactive].env` entries `KEY=VALUE`. `{name}` is reserved: anywhere else (headless args, options, resume, permissions, `[remote.permissions]`, version args) it is rejected at load. This is a small compatibility change in v0.3: a user profile from v0.1 or v0.2 that passes a literal `{name}` there no longer loads, and `check --static` reports why.
- **No silent fallback.**
  - A requested permission level, option, mode or `--remote` that the profile does not define is a hard error before anything is created.
  - The permission level comes from the `--permissions` flag if given, else the `default_permissions` config, else the profile's own optional `default_permissions`, else `auto`. This holds for both `run` and `check`, and the resolved level is recorded in `session.json`. A profile's `default_permissions` must name a level that profile defines (validated at load). An explicit `--permissions` must be a known level (`auto` or `skip`); anything else is a usage error (exit 2) for `run` and `check`.
  - Fastener never substitutes a level that the flag, config or profile did not name.
- **Resolution:** `~/.config/fastener/backends/<name>.toml` (`$XDG_CONFIG_HOME`) **replaces** the embedded built-in of the same name entirely. There is no merging.
- **Validation:** an unknown key, a missing required section or an unknown parser or finder name is an error, both at load time and in `fastener check --static`.
- **Built-ins** (`profiles/*.toml`, embedded with `go:embed`): `claude`, `codex`, `pi`. Every flag in a built-in profile is verified against the real CLI by `fastener check` (the opt-in smoke tests, §12) before release; all three passed on the development machine.
  - **`claude`:** `-p --output-format stream-json --verbose`, prompt on stdin, parser `claude-stream-json`; `auto` is `--permission-mode auto`, `skip` is `--dangerously-skip-permissions`. `[remote]` runs `remote-control --name {name}` with parser `claude-remote`; there `skip` is `--permission-mode bypassPermissions` (§8.1).
  - **`codex`:** `exec --json --skip-git-repo-check`, prompt on stdin, parser `codex-jsonl`; `auto` is `--approve-for-me`, `skip` is `--dangerously-bypass-approvals-and-sandbox`. `--skip-git-repo-check` is there because Fastener sessions name their working directory explicitly and `check` runs in a temporary non-git directory.
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

An expectation with no model name after the provider (`github-copilot/`, `/`, or an explicitly empty `--expect-model`) is a usage error (exit 2). When `model_source` is `client` (the agent CLI only recorded the id it requested, as with most of Pi's adapters), the prefix rule still applies, since it catches the CLI resolving a pattern to another model, but a pass appends "(client-reported: the backend does not confirm the serving model, so a provider-side swap would not be detected)".

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

A later version adds `check --interactive [--remote]`: launch with a pty, confirm the TUI started (and the conversation finder found an ID), and for `--remote` that `url_pattern` matched.

## 7. Parsers

Named Go functions selected by profile. v0.1 ships `claude-stream-json`, `pi-json`, `codex-jsonl` and `none`; v0.3 adds `claude-remote`.

- **`claude-stream-json`** (`claude -p --output-format stream-json --verbose`): `conversation_id` from any event's `session_id`; `model` from the last top-level assistant event's `message.model` (events with a `parent_tool_use_id` are subagent turns, possibly on another model, and are skipped) (a bare model id the Anthropic API reports; `model_source` `server`; Claude Code's `<synthetic>` placeholder is ignored), falling back to the `system/init` event's `model` (the CLI's own setting; `model_source` `client`) until an assistant event arrives; an error `result` clears `model`; `final_message`, `agent_error` (`is_error`) and `usage` from the `result` event.
- **`pi-json`** (`pi -p --mode json`): `conversation_id` from the `session` event's `id`. `model`, `model_source`, `final_message` and `agent_error` come from the last assistant `message_end`:
  - `agent_error` is `stopReason` `error` or `aborted` (as pi's print mode treats them);
  - `final_message` is the concatenated `text` content blocks (`thinking` blocks are skipped); on `error` it is `errorMessage`, and on `aborted` it is `errorMessage` or else `Request aborted`;
  - `model` is `provider/` plus `responseModel` when present, else `model`. Pi's adapters differ (pi 0.84.4): `anthropic-messages` overwrites `model` with the server's id; `openai-completions` keeps the requested id in `model` and sets `responseModel` only when the server reported a different one; every other adapter (`openai-responses`, `azure-openai-responses`, `openai-codex-responses`, `bedrock-converse-stream`, `google-generative-ai`, `google-vertex`, `mistral-conversations`, `pi-messages`, `cloudflare`) only echoes the requested id. So `model_source` is `server` when `responseModel` is set or `api` is `anthropic-messages` or `openai-completions`, and `client` otherwise, including for an unknown adapter. After an agent error `model` and `model_source` are cleared, since the attempted model never answered, and a last message without a model clears them too.

  `usage` is summed over every assistant `message_end` in the run (each reports one API call): `input + cacheRead + cacheWrite` input tokens, `output` output tokens and `cost.total`. Pi exits 0 even when the model call failed, so `agent_error` is what catches it.

- **`codex-jsonl`** (`codex exec --json`, codex-cli 0.167.0): `conversation_id` from `thread.started`'s `thread_id`; `final_message` is the text of the last `item.completed` whose item is an `agent_message`.
  - `agent_error` is set by a `turn.failed` or a top-level `error` event; `final_message` is then the message of the last such event (`turn.failed`'s `error.message` or the top-level `message`). An `item.completed` `error` item alone records nothing: it is a warning (for example "Model metadata not found") that precedes the real failure.
  - `usage` is summed over every `turn.completed`: `input_tokens` is Codex's own reported total and `output_tokens` its reported output. `cached_input_tokens` and `reasoning_output_tokens` are subsets in OpenAI semantics and are not added; `cache_write_input_tokens` is not added either because the captures cannot confirm it is excluded from `input_tokens`. Codex reports no cost, so `cost_usd` is omitted (not 0).
  - Codex does not report the served model in these events, so `model` and `model_source` are absent and `check`'s model step reports `skip` ("backend does not report its model"), the honest result.

- **`claude-remote`** (`claude remote-control`, Claude Code 2.1.319): the bridge's stdout is a status screen redrawn every few seconds, with ANSI cursor and erase sequences, and each session line is an OSC 8 hyperlink (`ESC]8;;URL BEL label ESC]8;; BEL`; ST is accepted as the terminator too). The parser strips terminal escapes (CSI, OSC, nF such as `ESC ( B`, two-byte escapes) and turns other C0 controls except tab into spaces, keeping an OSC 8 link's URL as text. It then records `remote_url`, the latest `https://claude.ai/code?environment=env_…` seen, and `remote_sessions`, every `https://claude.ai/code/session_…` seen, without its query, de-duplicated in the order first seen. IDs may contain letters, digits, `_` and `-`. The list keeps at most 64 entries: the oldest is evicted first, and a URL seen again after its eviction is added again at the end. Nothing on the screen is an error, so it never counts parse errors.

- Run inside the monitor, streaming, one line at a time, while raw output is written to `output.log` unchanged. The one exception is a plan with `dedupe_frames` (§8.1): the monitor then also ends lines at a lone `\r`, holds each line until it ends, and drops it from `output.log` when it repeats the row at the same position in the previous redrawn frame (or, before any frame marker, completes a repeated block of lines); every line is still parsed, a written line keeps its raw bytes minus leading cursor-movement sequences, and a line longer than the 16 MB cap is written in full, never dropped.
- **Extracted fields** (all optional, omitted when unknown):
  - `conversation_id`
  - `final_message`
  - `model`: the model the backend reports it actually served, `provider/model` when the backend names the provider, else the bare model id. It can differ from the requested `--model` (an alias or pattern resolved to a full id, or a substitution); `check` compares the two (§6.1). Absent after an agent error.
  - `model_source`: `server` when the API response named the model, `client` when the agent CLI only recorded the id it requested (a provider-side substitution would not show).
  - `agent_error`: the agent's own report of failure, independent of the exit code
  - `usage` {`input_tokens`, `output_tokens`, `cost_usd`}: emitted only when the backend reported usage. `input_tokens` is all input tokens for the run, including cache reads and writes (Anthropic's own `usage.input_tokens` excludes cache tokens).
- Unknown event types are ignored. A malformed line increments `parse_errors`. Parser failure never affects the session: a parser panic is recovered, logged to `monitor.log` and counted in `parse_errors`. A line longer than 16 MB is written to `output.log` but not parsed (counted once in `parse_errors`); parsing resumes at the next line. The monitor holds at most 16 MB of a line: the rest of an overlong line is streamed to `output.log` as it is read, so the cap bounds memory as well as parsing. `final_message` is capped at 64 KiB.
- `status.json` is updated whenever a field changes (debounced to about 1 per second) and always at exit. The monitor snapshots its status under a lock and writes the snapshot (marshal, fsync, rename) outside it, so a slow disk never stalls the parser or backs up the agent's stdout. Writes are serialized and carry a sequence number: an older snapshot never overwrites a newer one, and nothing is written after the `exited` record.
- Each parser is tested against recorded real output in `internal/parser/testdata/`. A backend output change is fixed by updating the fixture and the parser.

## 8. Remote and interactive sessions

Phase B is split by how each backend offers remote control. Probes on 2026-11-09 (Claude Code 2.1.319) found two different Claude modes:

| Mode | Command | Needs a terminal | What it is |
|---|---|---|---|
| Remote bridge | `claude remote-control [--name N]` | **No** | A long-running bridge for one folder. Sessions are added to it from claude.ai/code or the mobile app; each runs as a child `claude --print --sdk-url …`. |
| Interactive remote session | `claude --remote-control [name]` | **Yes** (without a tty it falls into `--print` and fails) | One interactive TUI session, driven locally or remotely. |

Codex has its own server and remote-control model; supporting it is a separate later phase.

### 8.1 Route A: remote bridges (v0.3)

A remote bridge is a long-running headless session, so it reuses the v0.2 machinery (monitor, launchers, liveness, stop, prune) with no pty.

- **Profile:** a `[remote]` section. It takes no prompt, and an unknown key is an error as everywhere else.
  - `args`: the arguments after `command`. The placeholder `{name}` (at most once, and nowhere else in the profile) is replaced by the session name within its argument. Claude: `["remote-control", "--name", "{name}"]`.
  - `parser`: default `claude-remote` (§7).
  - **Permissions** map through `[permissions]` as for headless runs, appended after `args`. `[remote.permissions]` overrides individual levels when the bridge command rejects a headless flag; a level listed there must also be defined in `[permissions]`. `claude remote-control --help` (2.1.319) accepts `--permission-mode` with `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk` and `plan`, but not `--dangerously-skip-permissions`, so Claude's `auto` stays `--permission-mode auto` and `skip` is overridden to `--permission-mode bypassPermissions`.
  - `untrusted_pattern`: a regex matched against `stderr.log` of a bridge that ends before reporting its URL (Claude: `(?i)workspace not trusted`).
  - `untrusted_hint` (optional; requires `untrusted_pattern`): how to trust the folder, appended to the untrusted error; `{dir}` is replaced by the folder. Claude: `fastener authorize claude-folder-trust {dir}`. Without it the error names no command.
  - `dedupe_frames`: the monitor drops redrawn status lines from `output.log` (below). Claude sets it.
- **CLI:** `fastener run <backend> --remote --cwd DIR [--name N] [--permissions auto|skip]`. `--prompt`, `--prompt-file`, `--model` and `--follow` are usage errors (exit 2) with `--remote` (a bridge runs until stopped, so `--follow` would never return; use `logs -f`), and so is `--remote` against a profile without `[remote]` (no silent fallback; the message names the backend). There is no default time limit (`--max-runtime` still works). `session.json` records `kind = "remote"` and `remote = true`, no `prompt.txt` is written, and the plan's `prompt_mode` is `none` (stdin is `/dev/null`).
- **Waiting for the URL:** after the handshake, `run --remote` re-reads the session every 100 ms for up to `remote_wait` (config, default 15 s; the monitor flushes parser fields about once a second):
  - once `remote_url` is recorded it prints `<name>  <remote_url>` (two spaces), or the inspect view with `--json`, and exits 0;
  - if the session ends first, it exits 1 with the reason: `untrusted_workspace` (below), or `exited` with how it ended and the last line of `stderr.log`; the message says `ended before reporting its URL`, or `ended after reporting <url>` when the bridge printed one before going away. `--json` prints the inspect view plus `reason` (and `folder` for `untrusted_workspace`);
  - if the wait elapses while the bridge still runs, it prints the name, notes on stderr that there is no URL yet, and exits 0; the bridge keeps running and `inspect` shows the URL once reported.
- **What Claude prints** (observed, 2.1.319): a status screen redrawn every few seconds. Each redraw moves the cursor up over the previous frame and erases it (`ESC[nA ESC[J`), then prints the folder and branch, the capacity, one OSC 8 hyperlink per session (`https://claude.ai/code/session_…?from=cli`), and the bridge URL `https://claude.ai/code?environment=env_…`. The `claude-remote` parser (§7) records `remote_url` and `remote_sessions` in `status.json`; `inspect` shows both and `ps --json` shows `remote_url`. stderr can carry harmless `[bridge] …` notices.
- **Log growth:** with `dedupe_frames`, the monitor compares each redrawn frame with the previous one, position by position:
  - **Line ends.** A line ends at `\n` or at a `\r` not followed by `\n` (a carriage-return redraw), and every line is parsed. For dedupe a *row* ends at `\n`; the lines ending in a lone `\r` before it are earlier drawings of the same row (a spinner, a progress line), so a row's text is its last drawing (an empty drawing, as in `\r\r\n`, overwrites nothing).
  - **Frames.** A frame starts at a line whose leading cursor sequences move the cursor up or home: cursor-up `ESC[<n>A` (Claude 2.1.319 follows it with `ESC[J`), cursor-previous-line `ESC[<n>F`, cursor-position `ESC[H` / `ESC[<r>;<c>H` / `f` (a full-screen `ESC[H ESC[2J` redraw), and Ink's log-update `ESC[2K ESC[1A … ESC[2K ESC[G`. Erase or column moves alone (`ESC[2K`, `ESC[G`) redraw the current row and start no frame. The rows before the first marker form the first frame.
  - **Repeats with frames.** A row is not written when its text, with terminal escapes stripped, equals the row at the same position in the previous frame; a `\r` drawing is not written when it equals that row too, or the drawing before it. A frame is compared with the frame right before it and nothing older, so a screen that changes and changes back (Connected, Disconnected, Connected) logs every change, every row that differs from the previous frame is written, and a steady screen logs nothing after its first frame.
  - **Repeats without frames.** Until a marker is seen there are no frames to compare, so a line (row or `\r` drawing) is dropped only when it completes a repeated block: for some period p ≤ 64, the p lines ending with it equal the p lines before them. p = 1 is a line equal to the one before it; a 2-line status printed over and over, or a spinner cycling through its glyphs, is written for one round and all but the last line of the next, and then dropped until a line breaks the cycle, which is always written. Recent lines are not kept in a set (an LRU): a line seen a while ago is written again, so a return to an earlier state is never lost. The cost is that a block printed again only after something else (A, B, A) is written each time, and that output which legitimately repeats a block line for line is logged once.
  - **Rows stay true.** Once a `\r` drawing of a row is written, a repeat that the log's row does not show yet is written anyway (the row's final text always; with frames, any drawing), so the text `logs` shows last for a row is the screen's; when the log shows it already, the row is finished with a lone `\n`. A frame marker after a row left open by `\r` writes `\n` first, so the next row never overwrites it.
  - **What is written.** Lines are written with their raw bytes (colours and OSC 8 links included), minus leading cursor-movement, erase and cursor show/hide sequences (`ESC[<n>A`–`H`, `f`, `J`, `K`, `ESC[?<n>h`/`l`), so `logs` on a terminal does not erase earlier, unrelated lines; the only bytes added are the `\n`s that finish rows. Lines over 64 KiB are always written and never compared, so the stripped copy hashed for each line stays small.
  - **Memory.** Two frames of SHA-256 hashes (at most 1024 rows each) and the 64 most recent line hashes. A line is held only as far as the parser already holds it, so the 16 MB line cap and its memory bound are unchanged; an overlong line is written in full.
  - **Measured** (fixtures in `internal/monitor`, bytes in → `output.log`): 10,000 identical Claude frames (1.75 MB) give 350 bytes; the sanitized Claude 2.1.319 capture 4,351 → 1,684 (pinned by its test); a full-screen `ESC[H ESC[2J` screen 23,781 → 103, Ink log-update 33,261 → 103, a markerless 2-line block 17,500 → 91, a 4-step `\r` spinner 49,600 → 465. `logs` shows the changes, not a faithful replay of the screen.
  - **Not recognised:** a frame start in the middle of a line (a redraw that moves up from the end of an unterminated last row; Ink's log-update ends every frame with `\n`, so its markers start lines), and a spinner drawn by redrawing whole frames, which changes every frame and so is logged every frame.
- **Shutdown:** SIGTERM stops the bridge in about 3 s and its session children exit with it, so the existing stop path applies unchanged (`ended_by=stop`).
- **Workspace trust:** `claude remote-control` refuses a folder whose trust dialog has not been accepted (`Workspace not trusted. Please run claude in <dir> first …`). Trust is stored in Claude's global config (`projects[<key>].hasTrustDialogAccepted`), keyed by the folder's git repository root (or its real path outside a repo), so a trusted parent folder does not cover a separate repository; the home directory is never persisted. There is no flag, setting or environment variable to trust a folder non-interactively; Claude's own messages point to setting that field instead (§8.3). In an untrusted folder Claude exits at once with ``Error: Workspace not trusted. Please run `claude` in <dir> first …`` on stderr. When `stderr.log` matches `untrusted_pattern`, `run --remote` exits 1 with `fastener: <dir> is not a trusted workspace for claude remote-control; authorize it with: fastener authorize claude-folder-trust <dir>` (the part after the semicolon is Claude's `untrusted_hint`) and, in `--json`, `"reason": "untrusted_workspace"` and `"folder": "<dir>"`, so a caller such as a dispatcher or assistant can ask the user to authorize it (§8.3). Claude's `[check].diagnose` has the same pattern.
- **Restore after reboot:** `--restart=on-reboot`, `fastener restore` and `fastener install-restore` as in §8.4. Probed on 2026-11-09 (Claude Code 2.1.319): re-running `claude remote-control --name N` in the same folder gets the **same** `env_…` environment ID and re-spawns that environment's existing sessions on its own, so a bridge is restored by relaunching its original arguments, with no saved ID. `--session-id` takes a session ID (`cse_X` and `session_X` name the same session) and attaches that one session in single-session mode ("exits when complete"), so it is not used to restore a bridge; it is the reattach path for one listed session. `--session-id env_…` is rejected, and `--continue` only resumes the most recent session within about 4 hours.

### 8.2 Route B: interactive sessions with a pty (v0.4)

Needed for `claude --remote-control`, for local attach to any backend, and for Pi.

- **The monitor owns the pty** (the `dtach`/`abduco` model, using `github.com/creack/pty`). Not tmux: one tmux server holds every session for a user, which is the shared-daemon failure mode Fastener avoids.
- **What makes a generation interactive** is the plan's `pty` field (§3), which `run --interactive` sets. The monitor then opens a pty at the plan's size and starts the agent in a new session (`setsid`) with the pty slave as its controlling terminal and as stdin, stdout and stderr (so `stderr.log` stays empty). As session leader the agent also leads its own process group (pgid = pid), so everything in §4 applies unchanged: the marker, identity, `stop` with its TERM / grace / KILL of the group, zombie pinning, `PR_SET_PDEATHSIG` on Linux, orphans, exit facts, and both launchers. `prompt_mode` `stdin` cannot be combined with a pty (the terminal is stdin): such a plan is `launch-failed`. A process that the agent puts in another process group of its terminal (a job-control shell) is outside the group like any process that leaves it (§4); it gets SIGHUP from the kernel when the agent, the session leader, exits.
- **Output.** The monitor reads the pty master continuously, whether or not anyone is attached, so the agent never blocks writing to its terminal. Each chunk goes to the attach clients (never waiting for them) and to the same pump as headless stdout: raw bytes to `output.log`, with the same 16 MB line bound and the same 2 s drain after the agent exits (`output_truncated` when a descendant still holds the terminal open). `output.log` holds what the terminal showed, including the line discipline's echo of input and `\r\n` line ends.
- **Parsers on a pty.** The plan's parser runs over that same stream, line by line (a line ends at `\n`; the `\r` before it is trimmed), exactly as on headless stdout, and `none` is always available. A TUI's output is mostly escape sequences, so interactive profiles will usually set `none` or a parser written for that screen (such as `url_pattern` below); no parser strips escapes for them yet.
- **Attach:** the monitor listens on `attach.sock` (§3; a fallback path when the session directory is too long for a socket address, recorded in `status.json`'s `attach_socket`). Only the owning user can connect: the socket is 0600 inside a 0700 directory, and the peer's uid is checked. `fastener attach <name>` connects the user's terminal. The detach key defaults to `ctrl-]` and is configurable. Several clients may attach at once; all receive the output, input from any reaches the pty, and the last client to attach or resize sets the size. On attach, the monitor resizes the pty and sends SIGWINCH to force a redraw (also when the size did not change); Fastener does not emulate the screen. A client too slow to keep up is disconnected rather than allowed to stall the agent or the others. Detaching leaves the agent running. When the session ends, every client receives its exit facts and the socket is removed. `status.json` records `attached_clients` while the generation runs, so `ps` and `inspect` show it.
- **Attach protocol:** versioned framed messages (`hello{version}`, `welcome`, `data`, `resize{rows,cols}`, `detach`, `exit`, `error`), independent of the transport, with bounded frames; a malformed frame drops that client only. The Unix socket is the only transport in scope. The wire format is in [attach-protocol.md](attach-protocol.md).
- **Starting one:** `run <backend> --interactive` renders the profile's `[interactive]` section: `args`, the permission level (`[interactive.permissions]`, else `[permissions]`), `--model` through `[options]`, and, when a prompt is given and `prompt = "arg"`, `--` and the prompt; a profile with `prompt = "none"` refuses a prompt (exit 2). The prompt is optional; it is passed as the backend's argument and never typed into the pty. A backend without `[interactive]` refuses `--interactive` (exit 2). The pty starts at `--size ROWSxCOLS`, else the caller's terminal size when stdin is one, else 24 × 80. `session.json` records `kind = "interactive"`. Detached is the default; `--attach` attaches right after the launch and then behaves exactly like `fastener attach` (exit codes included); it needs a terminal on stdin, checked before anything starts. `--follow` is refused (use `--attach`, or `logs -f`). `--restart on-reboot` is refused until interactive restore (#55).
- **Built-in profiles** (probed on 2026-11-10 under a pty): **claude** (2.1.322) runs its TUI with no first-run dialog in a trusted folder and takes an initial prompt as `-- <prompt>`. **codex** (0.167.0) starts its TUI cleanly; the prompt is its positional `[PROMPT]`. **pi** (0.84.4) starts its TUI with no dialog; it reads messages as positional arguments but treats one starting with `@` as a file, so its profile takes no initial prompt (`prompt = "none"`). Only claude has `[interactive.remote]`; Codex remote is #56.
- **Remote:** `run --interactive --remote` uses `[interactive.remote]` args after `[interactive].args` (for Claude, `["--remote-control", "{name}"]`). The plan carries `url_pattern`; the monitor matches it against the terminal stream and sets `remote_url` to the latest match, and `run` waits for it (up to `remote_wait`) and prints `<name>  <url>` as `run --remote` does.
  - **Matching.** Claude draws the URL as `/remote-control is active · Continue here, on your phone, or at https://claude.ai/code/session_…`, an OSC 8 link (with `?from=cli`) redrawn several times, and draws spaces with cursor-forward escapes, so plain escape stripping glues words together. The monitor therefore strips escapes with cursor movements turned into spaces (attribute changes inside a word just vanish, and an OSC 8 link's URI is kept), and works incrementally: each byte is stripped once, and the pattern runs over the new text plus the 512 bytes before it, so a URL split across reads or escapes still matches. A read that ends inside an escape sequence, or inside a string (OSC, DCS, SOS, PM, APC, which run to BEL or ST and may contain other ESC bytes), holds it (up to 4 KiB) until it ends, so the stripped text is exactly what stripping the whole stream would give. Matches are taken in stream order, an OSC 8 link's URI counting where the link is, with two rules, because on a narrow terminal (a phone attaching at under about 60 columns) a link's visible text wraps across rows while its URI stays whole: a match that is a strict prefix of the last link URI is a fragment of it and is ignored; and a visible match that is a strict prefix of the last URL recorded is ignored, while a link URI always replaces it. A genuinely new visible URL after a link is still taken. A match that reaches the end of the output seen so far is held back until more output shows where it ends, or until the output has been quiet for 500 ms. `remote_url` is the latest match taken. Claude's pattern names the ID's characters (`session_[A-Za-z0-9_-]+`), since with spaces drawn as escapes `\S+` would run into the next word.
  - **Trust.** In a folder whose trust dialog has not been accepted, Claude's TUI does not exit: it shows "Quick safety check: Is this a project you created or one you trust?" and waits. While `run` waits for the URL it also matches the terminal output against `untrusted_pattern`; on a match it stops the session and reports `untrusted_workspace` with the hint (`fastener authorize claude-folder-trust <dir>`), exactly as for a bridge. The TUI echoes the initial prompt, so a prompt could quote the dialog: Claude's pattern therefore matches the dialog itself, its heading ("Quick safety check: …") followed within 1000 characters by its option "Yes, I trust this folder" (checked against the dialog Claude Code 2.1.322 really draws, `internal/profile/testdata/`), and before matching `run` removes the first echo of the prompt's own text from the output (case-insensitively, whitespace collapsed), only for a prompt of at least 40 characters, so a short prompt made of the dialog's words ("trust", "Quick safety check") cannot cut them out of the real dialog. A bridge (`[remote]`) takes no prompt and is matched on `stderr.log` after it exits, so it has no such echo. A session that ends before its URL is explained from its terminal output (a pty merges stderr into it).
  - A detached remote session is an interactive session nobody is attached to.
- **Conversation ID** (v0.4, for interactive restore, §8.4): the ID of the conversation the agent writes in its own store, recorded as `status.conversation_id`.
  - **Assigned up front where the backend allows it (exact).** `[interactive] session_id = ["--session-id", "{session_id}"]` makes `run` pass a fresh random UUID, recorded in the plan as `session_id`. Claude Code 2.1.322 accepts `--session-id <uuid>` in the TUI, also with `--remote-control` (probed 2026-11-10: the transcript was `<uuid>.jsonl`); pi 0.84.4's `--session-id <id>` uses the ID for a new session and opens the session if it exists. Codex 0.167.0 has no such option.
  - **Found by a named finder** (`[interactive] finder`, Go code in `internal/finder`, no scripting). A finder gets the session's `cwd`, `started_at`, the agent's environment (for the variables that move a store) and the assigned ID, and returns the ID, or *not found* (nothing written yet) or *ambiguous* (several candidates: it never guesses). It reads only file names, modification times and, for Codex, a few metadata fields of a rollout's first line; never a conversation's content. All stores are named after the agent's physical cwd (`getcwd`), so symlinks in `cwd` are resolved (the path as given is tried too).
    - **`claude-transcripts`:** `<config home>/projects/<key>/<uuid>.jsonl`; the config home is `$CLAUDE_CONFIG_DIR`, else `~/.claude`; the key is the cwd with every character other than an ASCII letter or digit replaced by `-` (a worktree keys to its own path), and a key longer than 200 characters is cut to 200 and followed by `-` and the base-36 absolute value of the cwd's Java-style string hash (over UTF-16 code units), as Claude Code 2.1.322 computes it. With an assigned ID it checks that file exists (exact). Without one, it takes the single `<uuid>.jsonl` there modified since the session started (heuristic; ambiguous with two Claude sessions in the same folder).
    - **`pi-sessions`:** `<agent dir>/sessions/--<cwd without its leading "/", with "/", "\" and ":" as "-">--/<ISO time with ":" and "." as "-">_<id>.jsonl`; the agent dir is `$PI_CODING_AGENT_DIR`, else `~/.pi/agent`. With an assigned ID it checks for `*_<id>.jsonl` (exact); without one it takes the single file whose name's time is at or after the start (heuristic).
    - **`codex-rollouts`:** `<codex home>/sessions/YYYY/MM/DD/rollout-<local time>-<id>.jsonl`; the codex home is `$CODEX_HOME`, else `~/.codex`; the day directories are local calendar dates (walked by date, so a DST day is not skipped). The first line is a `session_meta` record with the ID, cwd, start time and source (`exec` for `codex exec`; the TUI has written `cli`, and in 0.167.0 alongside the desktop app `vscode`, so only `exec` is excluded); at most 1 MiB of it is read. The finder takes the single rollout whose cwd is the session's, whose source is not `exec` and whose start is within 10 minutes after the session's spawn. The window narrows the case of a Codex the user starts by hand in the same folder later (scenario A), but cannot tell apart two Codex sessions started in it (scenario B: the later one may write first).
    - A relative `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `PI_CODING_AGENT_DIR` (or `HOME`) in the agent's environment is taken relative to the session's `cwd`, as the agent reads it. Modification times are compared with `started_at` less 1 s, for filesystems that store them coarsely.
  - **Exact or candidate.** Only an exact answer, the assigned ID once the agent has written that conversation, becomes `conversation_id` (`conversation_source: session_id`). A heuristic answer (codex, or claude and pi with a profile that assigns no ID) is only `conversation_candidate` (`conversation_source`: the finder's name), and it stays one only while it is the single candidate ever seen: when a second, different one appears (at the same lookup or later), the candidate is cleared for good and `conversation_error` lists them; at exit it is kept only if the finder still names it alone. **The contract for interactive restore (#55):** it resumes `conversation_id` automatically, and a `conversation_candidate` only when the user confirms it explicitly (for example `--accept-candidate`).
  - **What `conversation_id` means.** It is the conversation Fastener started the agent with, not necessarily the one the TUI shows now: `/clear`, `/resume` or `/new` inside Claude's or pi's TUI moves to another conversation while the assigned one still exists. Fastener does not follow that.
  - **When.** The agents write their conversation at the first message, not at start (Claude, Codex; pi likewise), which may be hours later. The monitor asks the finder 2 s, 10 s, 30 s, 1, 2, 5, 10, 20 and 30 min after the spawn, then hourly, until it has an exact answer (a candidate keeps the lookups going, to catch a second one), and once more as the agent exits, bounded at 2 s; each lookup is a stat or a directory listing. While there is neither an ID nor a candidate, `conversation_error` says why (not written yet, or the ambiguous candidates). An assigned ID is in the plan from the start, so a restore after the monitor died (a reboot) can look the conversation up itself.
  - **One source.** In an interactive session the finder owns `conversation_id`: a profile naming both a finder and an `[interactive]` parser that sets `conversation_id` (`claude-stream-json`, `codex-jsonl`, `pi-json`) is rejected at load.

### 8.3 Authorizing folder trust (v0.3)

Starting a bridge in a new folder should not require someone to open a terminal there, but trust is a security decision and is never granted implicitly.

- **`fastener authorize claude-folder-trust <dir> [--json]`** is an explicit, one-off command the user runs (or approves). Running it is the act that puts the folder on Fastener's trust allowlist; it then records Claude's trust for it. Nothing else in Fastener writes Claude's config (`run --remote` only reports `untrusted_workspace`, §8.1); a caller that hits `untrusted_workspace` asks the user to confirm and then runs the command, never on its own. `fastener authorize -h` says that the command edits Claude Code's own config file, the documented alternative to the interactive trust dialog.
- **The allowlist and records** live in `<config dir>/authorizations.json` (`$FASTENER_CONFIG_DIR`, else the XDG default), a machine-written file kept apart from the hand-edited `config.toml`, so Fastener never rewrites the user's TOML (and its comments). It is `{"schema_version": 1, "authorizations": [{folder, key, claude_config, authorized_at, verified_at?, claude_version?}]}`, written atomically (temp file and rename, mode 0600) under an exclusive `flock` on `authorizations.json.lock`, so concurrent commands keep each other's records. `config.toml` may also hold `trust_dirs`, an array of absolute folders: a user-maintained allowlist, read but never written. The allowlist records what the user authorized: running `authorize` is the explicit act, and the command's internal "is it allowlisted" check before writing is a consistency check, not a separate control.
- **How trust is recorded.** Claude Code stores it in its global config (`$CLAUDE_CONFIG_DIR/.claude.json` when `CLAUDE_CONFIG_DIR` is set, else `~/.claude.json`) as `projects[<key>].hasTrustDialogAccepted = true`, and its own messages name setting that field as the alternative to the interactive dialog. Fastener writes it the way Claude does (verified against Claude Code 2.1.319, which saves with a `proper-lockfile` lock and re-reads the file under the lock before applying a change, so a running Claude does not overwrite an edit made under the same lock):
  1. **Folder.** `<dir>` is made absolute with symlinks resolved; it must exist and be a directory. The home directory (Claude never persists its trust) and `/` are refused.
  2. **Key.** Computed as Claude Code 2.1.319 does, from the filesystem (Claude runs no git command for it, so `core.worktree`, `safe.directory` and the `GIT_*` discovery variables play no part):
     - walk up from the folder (inclusive) to the nearest directory holding a `.git` that is a directory or a regular file (a symlink counts if it leads to one); that directory is the repository root;
     - if that `.git` is a file naming a linked worktree (`gitdir: <adm>`, where `<adm>/commondir` is a regular file pointing at the main repository's git directory `<common>`, `<adm>` is in `<common>/worktrees`, and `<adm>/gitdir` points back, after resolving symlinks, at this `.git`), the key is the **main** repository's root: the parent of `<common>`, or `<common>` itself when it is not named `.git` and holds no `.git`. If any check fails, the worktree keeps its own root. A submodule's `.git` file names `<super>/.git/modules/<name>`, which has no `commondir`, so a submodule keys to its own root;
     - outside a repository, the key is the folder's real path; forward slashes.

     The key can therefore be a parent repository root, which trusts the whole repository; the output says which key was used. A key that is the home directory (a dotfiles repository) or `/` is refused. Not ported: Claude's path-safety screens on the `gitdir`/`commondir` contents (UNC, WSL and similar paths), under which Claude would keep the worktree's own root.
  3. **Config file.** `$CLAUDE_CONFIG_DIR/.claude.json` when set, else `~/.claude.json`. Claude picks a different file in two cases, and then the command refuses and names the file Claude would use rather than writing the wrong one: `CLAUDE_CODE_CUSTOM_OAUTH_URL` is set (Claude uses `.claude-custom-oauth.json`), or a non-empty legacy `.config.json` exists in Claude's config home (`$CLAUDE_CONFIG_DIR`, else `~/.claude`). If the file does not exist the command fails ("run claude once first") and creates nothing. A symlinked config is written through to its target, so the link stays a link.
  4. **Lock.** Claude 2.1.319 calls proper-lockfile with an explicit `lockfilePath` of `<config path>.lock`, on the path **as computed, not resolved through symlinks**, so Fastener takes exactly that lock: the **directory** `<config path>.lock`, created with `mkdir` (success means it is held) and released with `rmdir`. When the config file itself is a symlink, Fastener then also takes `<real path>.lock` (proper-lockfile's default, for writers that lock there), always in that order, waiting at most 2 s for it (so a Claude blocked on the first lock is not starved past its own retry budget), and releases them in reverse. When only a parent directory is a symlink (`/home` → `/var/home`, `/tmp` → `/private/tmp`, a symlinked `CLAUDE_CONFIG_DIR`), both spellings name the same lock directory (same base name, `os.SameFile` parent), so it is taken once. On `EEXIST` the command retries with backoff (20 ms doubling to 500 ms) for up to 10 s, then fails cleanly without touching the file. A lock directory whose mtime is older than proper-lockfile's default stale time (10 s; a live holder refreshes it every 5 s) is removed with `rmdir` and the `mkdir` retried; a fresh lock is never removed, and a lock path that is not a directory is an error (it is never unlinked). While held, Fastener refreshes each lock's mtime every 5 s and remembers the mtime it set; a refresh failure is recorded. Right before the rename, and again before releasing, it checks that each lock directory still exists with the mtime it last set and that no refresh failed; otherwise the lock is compromised (as proper-lockfile would say), nothing is written, and the lock is left alone. If the lock is found compromised only on release, after the rename, the command still succeeds (the field is set and verified) and prints a warning on stderr (`warning` in `--json`). The lock is released by a `defer`, so also on a panic.
  5. **Write, under the lock.** Re-read and parse the file, keeping everything: object key order, and every string, number, boolean and null as its exact source bytes (so `1794150081977`, long floats and string escapes are untouched). Set `projects[<key>].hasTrustDialogAccepted = true`, creating `projects` or the entry if missing (appended last, as JavaScript does) and touching no other field. If it is already `true`, nothing is written ("already trusted") and no backup is made. Otherwise the document is serialised as Claude does (`JSON.stringify(config, null, 2)`; object keys and leaves are written back as their source bytes, so a key with a lone-surrogate escape or invalid UTF-8, or a duplicate key, is kept exactly: two-space indent, no trailing newline unless the file had one), so an unchanged file re-encodes byte for byte and the diff is the one field. Two independent checks then run, and if either fails nothing is written: a parse with `encoding/json` (`UseNumber`) must show the original values with only that field set, and a byte-level scan of every object key must find the original keys, byte for byte, plus exactly the keys the edit added (which catches keys that `encoding/json` decodes lossily and would merge). The original bytes are backed up to `<fastener config dir>/backups/claude.json.<UTC timestamp>` (mode 0600; the newest 5 are kept), then the new content goes to a temp file in the same directory, is fsynced and renamed over the file with its mode preserved. The temp file is removed on every error path. The new file is owned by the user who ran the command (its mode is preserved); run it as the user whose config it is. Then the lock is released.
  6. **Verify.** Re-read the file and confirm the field is `true`; print the folder, the key (and whether it is a repository root), the config path and the backup. A failure here (for example after Claude moves trust out of this file) is reported, not retried. The command does not start a bridge; `run --remote` in the folder is the end-to-end check.
  7. **Record.** The folder is recorded (with its key, config path and `authorized_at`) before Claude's config is written, since it is the allowlist entry; after the verify the record gains `verified_at` and `claude_version` (`claude --version`, if it runs). If the write fails, the folder stays on the allowlist without `verified_at`, and running the command again retries.
- **`fastener authorize --list [--json]`** prints the records and `trust_dirs` (JSON in `docs/json-schema.md`). **`fastener authorize --revoke <dir> [--json]`** removes the folder's record (matching both its resolved and its plain absolute path, so a deleted folder can be revoked). It does **not** change Claude's config, and says so (naming any other recorded folders that share the same key, since removing Claude's entry would untrust them too): Claude keeps trusting the folder until its `projects[<key>]` entry is removed there. A folder still listed in `trust_dirs` stays allowlisted, with a warning; revoking a folder that has no record exits 1.
- **Exit codes:** 0 on success (including "already trusted"), 1 on a failure, 2 on a usage error.

### 8.4 Restart policy and restore (v0.3 for bridges, v0.4 for interactive)

- **`--restart no|on-reboot`**, default `no`, recorded in `session.json`. `on-reboot` needs `--remote` or `--interactive` (v0.4); with a headless run it is a usage error (exit 2), because a headless prompt may have side effects and is never rerun. With `--interactive` the profile must be able to resume a conversation (`[resume]`, or `[interactive] session_id`), else it is a usage error.
- **Pinned.** A session with `restart = "on-reboot"` is never pruned, whatever its state (`prune --json` reports it as kept, `pinned`; a `lost` one is kept as `lost` anyway).
- **`fastener restore [--dry-run] [--json] [name...]`.** With no names it considers every session whose effective state is `lost` and whose `restart` is `on-reboot`, and lists only those. Named sessions are always reported, and may also be `restore-failed` (so a failure can be retried once its cause is fixed). For each candidate:
  1. **Eligibility.** It must be a remote bridge (`kind = "remote"`) or an interactive session (`kind = "interactive"`, remote or not) with `restart = "on-reboot"`, and `lost` or `restore-failed`; a **named** session may also be one that `stop` finalized from `lost` (`exited`, `ended_by=stop`, error `monitor was lost; finalized by stop`), which is where the `orphans` skip below leads. Otherwise it is skipped: `not_on_reboot`, `not_remote` (a headless session is never restored, even one whose `session.json` claims `on-reboot`), `running` (or starting), `not_lost` (it ended under its monitor, for example a normal `stop`).
  2. **Lock.** An exclusive `flock` on `restore.lock` in the session directory, polled for up to 2×handshake + `remote_wait` + 30 s (then skipped, `restore_in_progress`). `rm` (with or without `--force`) tries the same lock without waiting and refuses with `a restore is in progress` while it is held. It is a separate file from `lock`, which `Finalize` takes, because the relaunch itself finalizes (a launch failure, a `restore-failed` record). Under the lock the session is read again, so a second, concurrent `restore` finds the first one's relaunch `running` and skips it: two restores relaunch exactly once.
  3. **Nothing of the old generation alive.** The view must not be alive (a `lost` monitor is confirmed dead: unknown liveness is never dead, §4). Then every process carrying the current generation's marker is looked for **in any process group**, the scan `stop` also uses for a lost on-reboot or `restore-failed` session (§4); a recorded boot ID other than the current one needs no scan, because nothing survives a reboot. Any such process skips the session with `orphans` and the message `stop it first (fastener stop <name>), then fastener restore <name>`: `stop` kills them and finalizes the session, and the named restore then relaunches it; a scan that cannot be done (no readable plan for a generation that spawned something, a failed process scan) skips it with `orphans_unknown`.
  4. **Re-render.** The argv is rendered from the profile as it is **now**, using the intent in `session.json`: backend, permission level, name (for `{name}`), environment (`env_base`, the env-file re-read, `--env`) and so the session PATH for the binary. A bridge gets its original `[remote]` arguments: the environment ID is stable per folder and the bridge re-spawns its sessions itself (§8.1), so no ID is saved or passed. No fallback: a profile that no longer loads (`profile`), has no `[remote]` (`no_remote`), no longer defines the session's level (`no_permission_level`), an env-file that cannot be read (`env_file`), or a binary that cannot be found on the session PATH (`binary_not_found`) makes the session `restore-failed` with that reason, and nothing else is changed (no plan, no rotation).
  5. **Next generation.** `output.log` and `stderr.log` are renamed to `output.<old>.log` and `stderr.<old>.log` (appended to, if one somehow exists); a leftover `stop-request.json` is deleted; `generations/<n+1>.json` is written with the new argv, profile hash, a new marker and the Fastener version, then a `starting` `status.json` for generation n+1. The monitor is started with the **launcher the previous generation used** (the kind in `generations/<old>.launcher.json`), so a bridge keeps its supervision whatever the config says now; only when there is no record does it use `launcher` from config, else `auto` (§2), with a note on stderr. At boot `fastener-restore.service` runs while `systemctl --user is-system-running` still prints `starting`; restore's probe accepts that state as usable (the manager accepts transient units while starting, and `systemd-run` queues the start job), so a systemd bridge is not refused and `auto` does not fall back to setsid. `run` keeps requiring `running` or `degraded`. `generations/<n+1>.launcher.json` is written and the normal handshake runs. A starting session with no monitor record yet is given 2×handshake from the plan's `planned_at`, not from `created_at`.
  6. **URL.** Like `run --remote`, it then waits up to `remote_wait` for `remote_url`. A bridge still running without one is `restored` (with a note on stderr).
  7. **`session.json` is never written.**
- **When the relaunch does not come up.** If the new generation ends at once, it is recorded `restore-failed` (the generation's exit facts are kept; `restore_reason` and `error` are added): `untrusted_workspace` when `stderr.log` matches the profile's `untrusted_pattern` (the folder is no longer trusted; the message carries the profile's `untrusted_hint`, e.g. `fastener authorize claude-folder-trust <dir>`), `exited` for any other end before the URL, `launch_failed` when the monitor could not start or the agent could not be spawned. A new generation that ended with `ended_by=stop` (someone ran `stop` while restore waited for the URL) is left `exited` and reported `skipped`, reason `stopped`. If the new monitor died instead (`lost`), the session is left `lost` (reason `lost` in the output), so `stop` and `rm` can still find what it left. `restore-failed` is written like `Finalize`: under the session `lock`, refusing while a recorded monitor may be alive; unlike `Finalize` it also relabels an `exited` record, and it clears `remote_url` and `remote_sessions`, which would otherwise describe a bridge that is not running (a new generation's status starts empty anyway). `ps` and `inspect` show the state, and `inspect` the reason. A `restore-failed` generation that left marked processes behind shows them as `orphans`: plain `rm` refuses it, `stop` and `rm --force` kill them as for an exited session with orphans.
- **Output.** One line per reported session: `<name>  <result>[  generation N][  <url>][: <message>]`. `--json` is an array of `{name, result, reason?, message?, generation?, remote_url?}` (docs/json-schema.md). `result` is `restored`, `skipped` or `failed`; with `--dry-run`, which changes nothing and takes no lock (it does the eligibility, orphan and re-render checks), `would-restore` or `would-fail` (and `skipped`).
- **Exit codes.** 0 when every candidate was restored or skipped (or there was nothing to restore); 1 when any failed (`would-fail` under `--dry-run`, and an unknown name, `not_found`); 2 for usage errors.
- **`logs <name> --generation N`** reads `output.<N>.log` (with `--stderr`, `stderr.<N>.log`), or the current log when N is the current generation; N beyond the current one is an error, and `-f` follows only the current generation.
- **Interactive sessions (v0.4).** Restore resumes the session's conversation; it **never starts a fresh one**.
  - **Which conversation.** From the last generation: `status.conversation_id` (exact, §8.2); else the ID assigned at launch (`plan.session_id`), if the profile's finder now finds that the agent wrote it (the monitor may have died before it looked); else a `conversation_candidate` (recorded, or the finder's heuristic answer now) **only with `fastener restore --accept-candidate <name>...`**, which needs the sessions named, since it confirms a guess. A candidate without it fails with `unconfirmed_candidate`, and the message names the command; a recorded ambiguity, or no conversation at all (nothing was ever sent to the agent), fails with `no_conversation`. Both are recorded `restore-failed`, so they show in `ps` and can be retried by name.
  - **Re-render.** `[interactive].args`, then the resume: the profile's `[resume].args` with `{conversation_id}` (with `[resume] subcommand = true` right after the command, before `[interactive].args`, as `codex resume <id>` needs), else, without `[resume]`, `[interactive] session_id` with the conversation's ID; then `[interactive.remote].args` (the same `{name}`) for a remote session, the permission level and `--model`. No prompt, and no new session ID: the new plan's `session_id` is the resumed ID, so its finder confirms it at once. The plan keeps the previous generation's pty size, `url_pattern`, profile env and finder.
  - **Backends** (probed 2026-11-10; none forks on resume, so a restored generation's work stays in the same conversation, and the next restore resumes it again): **Claude** 2.1.322 `--resume <id>` reopens the transcript under the same ID and appends to the same `<id>.jsonl` (no `--session-id` with it; `--fork-session` would make a new one), and with `--remote-control <name>` it reattaches the same claude.ai remote session (the same `session_…` URL). **pi** 0.84.4 reopens an existing session given the same `--session-id` and appends to the same `<time>_<id>.jsonl`. **Codex** 0.167.0 resumes with the `codex resume <id>` subcommand (`[resume] subcommand = true`) and appends to the same rollout file; its conversation is only ever a candidate.
  - **IDs are checked at every layer**, since they go into the agent's argv and come from files: every finder returns only UUIDs (the shape all three agents use; others are skipped and logged), restore refuses a recorded ID that is not one (`bad_conversation_id`, recorded `restore-failed`), and rendering refuses a resume ID that starts with `-` or holds anything but ID characters.
  - **Remote.** A remote interactive session's restore waits up to `remote_wait` for the new generation's `remote_url`, like `run --interactive --remote` (the trust dialog is reported `untrusted_workspace`, stopping the session); one without remote control is restored once its handshake is done.
  - **Safety** is the bridges': the any-group marker scan must find nothing of the old generation alive, under the restore lock.
- **`fastener install-restore [--uninstall] [--print]`** installs the trigger. Without it nothing restarts. The binary is `os.Executable()` with symlinks resolved; a binary in the temp directory or a Go build cache (`go run`) gets a warning, since the trigger runs that path at boot. The resolved, absolute state root and config directory are always written into the trigger as `FASTENER_STATE_DIR` and `FASTENER_CONFIG_DIR` (whether or not the caller set them), so the boot-time restore looks exactly where the installing shell did, whatever environment the user manager, launchd or cron provides. `--print` shows what would be installed (or removed) and writes nothing; re-running updates the file; `--uninstall` removes it.
  - **systemd user manager available** (Linux; the same probe as the launcher, §2): writes `fastener-restore.service` to `$XDG_CONFIG_HOME/systemd/user/` (else `~/.config/systemd/user/`) with `Type=oneshot`, `ExecStart=<fastener> restore`, `Wants=` and `After=network-online.target` (in a user manager that target is usually absent, so this orders nothing there but does no harm), `Environment=` lines, `KillMode=process` (so monitors restore started with the setsid launcher outlive the one-shot unit; systemd-launched ones are their own units anyway) and `WantedBy=default.target`, then runs `systemctl --user daemon-reload` and `enable` (never `start`). Without lingering it warns: the user manager, and so the unit, starts only at login. `--uninstall` runs `disable`, deletes the file and reloads.
  - **macOS:** writes the LaunchAgent `~/Library/LaunchAgents/net.ytjohn.fastener.restore.plist` (label `net.ytjohn.fastener.restore`): `ProgramArguments` `[<fastener>, restore]`, `RunAtLoad`, `AbandonProcessGroup` (launchd must not kill what restore started), `EnvironmentVariables` and `StandardOutPath`/`StandardErrorPath` `<state dir>/restore.log`. It does **not** run `launchctl`: an agent in that folder is loaded at the next login by itself, and a CLI run over ssh may have no GUI domain. It prints `launchctl bootstrap gui/<uid> <plist>` to load it now, and `--uninstall` prints the `launchctl bootout` command. A LaunchAgent runs at login, not at boot.
  - **Neither:** prints a crontab `@reboot FASTENER_CONFIG_DIR=… FASTENER_STATE_DIR=… <fastener> restore` line (shell-quoted, `%` written as `\%` since cron turns a bare `%` into a newline; with a note on stderr), changes nothing and exits 0.

### 8.5 Codex remote (later phase)

Codex's server and remote-control model is different enough to be planned separately once routes A and B exist.

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
fastener run <backend> --remote [--name N] --cwd DIR [--permissions auto|skip] [--restart no|on-reboot] [--json] ...   # v0.3, §8.1
fastener restore [--dry-run] [--json] [--accept-candidate] [name...]   # v0.3, §8.4; interactive v0.4
fastener install-restore [--uninstall] [--print]         # v0.3, §8.4
fastener ps [--all] [--filter label=K[=V] | state=S | backend=B | name=GLOB]... [--json]
fastener inspect <name> [--json]
fastener logs <name> [-f] [--stderr] [--generation N]
fastener wait <name> [--timeout D]
fastener stop <name> [--grace D]      # clamped to the session's grace in systemd mode with --max-runtime
fastener rm <name> [--force]
fastener prune [--older-than D] [--dry-run] [--json]
fastener backends [--json]
fastener check <backend> | --all [--static] [--keep] [--expect-model M] [run options] [--json]
fastener version [--json]
fastener authorize claude-folder-trust <dir> [--json]    # v0.3, §8.3
fastener authorize --list [--json] | --revoke <dir> [--json]
fastener attach <name> [--detach-key KEY] [--quiet]          # v0.4
fastener run <backend> --interactive [--remote] [--size RxC] [--attach] [--prompt P] ...   # v0.4
```

- **`run`:**
  - Flags and positionals may interleave. A bare `--` ends flag parsing: every later argument is positional, even one starting with `-`. (A `--` that is itself a value flag's value, as in `--cwd --`, is just that value.)
  - Detached by default; prints the session name, or the full status with `--json`.
  - `--follow` follows the logs after launch, then exits with the code `wait` would report for the session (the agent's exit code, 128+N for a signal, 125 for an abnormal end, including a session removed while following). (This is not `attach`, which is the phase B pty command.)
  - `--prompt` and `--prompt-file` are mutually exclusive. One is required for headless runs, and `--cwd` is required.
  - `--remote` starts the profile's remote bridge (§8.1): no prompt, no `--model`, and it waits up to `remote_wait` for the bridge URL.
  - `--prompt-file -` reads the prompt from stdin. A prompt (from a file or stdin) larger than 4 MiB is a usage error (exit 2) naming the limit, and `--prompt-file` accepts regular files and pipes (a FIFO, or process substitution such as `<(cmd)`; reading blocks until a writer appears) but rejects devices such as `/dev/zero`, directories and sockets before opening them.
  - Labels are stored (`check` uses `fastener.check=true`) and can be filtered on with `ps --filter label=K=V`.
  - `--launcher auto|systemd|setsid` (default: config `launcher`, else `auto`; see §2).
- **`ps`:**
  - Shows `running`, `starting` and `lost` sessions plus anything that ended in the last 24 h.
  - `--all` shows everything retained.
  - `--filter KEY=VALUE` is repeatable; filters are ANDed. Keys:
    - `label=K=V`: the session has label K with value V. `label=K` (no `=V`) matches any session that has label K, whatever its value.
    - `state=S`: the effective state; S is one of `starting`, `running`, `exited`, `lost`, `restore-failed`.
    - `backend=B`: the backend name.
    - `name=GLOB`: a shell glob (Go `path.Match`) on the session name, e.g. `name=bd-12-a*`.
  - An unknown key, a malformed filter or an unknown state is a usage error (exit 2) that lists the valid keys or states.
  - Filters apply on top of the time window, never instead of it. `ps --filter state=exited` still shows only sessions that ended in the last 24 h; add `--all` to search everything retained.
  - `--json` is unchanged in shape and holds only the matching rows (`[]` when none match; exit 0).
  - It loads sessions without the orphan scan, so it reports no orphan counts; `inspect` does.
- **`wait`:**
  - Exits with the agent's exit code.
  - Exits 128+N when the agent was killed by signal N.
  - Exits 124 if `--timeout` elapses while the session is still running.
  - Exits 125 if the session did not complete normally (`launch-failed`, `lost`, `restore-failed`), if it was removed (`rm`) while `wait` was running (the message is `session <name> was removed while waiting`), and when the agent exited 0 but the parser's `agent_error` is true: Pi's json mode exits 0 after a failed model call, which would otherwise read as success. A non-zero exit code is returned unchanged.
  - Exits 127 for a session name that never existed.

  A caller can therefore tell "timed out" from "lost" without a follow-up `inspect`. As with Podman, 124, 125 and 127 can collide with real agent exit codes. The documentation names `inspect --json` as the authoritative answer.
- **`attach`** (v0.4): connects the terminal to an interactive session's pty through `status.attach_socket` (§8.2, docs/attach-protocol.md). Stdin must be a terminal; it is put in raw mode, so every key (ctrl-c included) goes to the agent, and SIGWINCH is forwarded as a resize. The detach key (`--detach-key`, else config `detach_key`, default `ctrl-]`; `ctrl-a`…`ctrl-z`, `ctrl-@`, `ctrl-\`, `ctrl-]`, `ctrl-^`, `ctrl-_`, or `none`; `ctrl-[` is ESC and refused) leaves the session running; it is matched anywhere in the input, so pasting it detaches too, bytes after it in the same read are dropped, and there is no way to send it literally. A one-line hint naming the key goes to stderr when it is a terminal, unless `--quiet`.
  - **Input never blocks the terminal.** One goroutine always reads the terminal and looks for the detach key; it hands input to a sender through a queue of at most 1 MiB. While the session is not reading its terminal (the pty and the monitor's per-client queue full, §8.2), input beyond that is discarded and counted, and the count is reported on exit (`[fastener: N bytes of input were discarded; the session was not reading its terminal]`). Conversely, a very fast multi-megabyte paste into an agent that echoes it can make the monitor drop `attach` as too slow (exit 125).
  - **Leaving.** On the detach key, a signal or an error, `attach` first stops the agent's output reaching the terminal (bounded at 500 ms), then restores the terminal, then sends `detach` with a 500 ms write deadline and closes the connection, so no way out waits on the network without a bound, and no agent output follows the restore. What is restored: the termios settings `attach` changed, and, when stdout is a terminal, a reset is written (leave the alternate screen, show the cursor, turn off mouse reporting and bracketed paste, reset text attributes); screen contents are not restored. This happens on every way out after attaching: the detach key, the session ending, an error frame from the monitor, a lost connection, SIGINT/SIGTERM/SIGHUP to `attach` itself, a write to a closed stdout pipe (SIGPIPE is caught, so the write fails instead of killing `attach`), and a panic. Then one line on stderr says what happened (`[fastener: detached from <name>]`, `[fastener: session <name> exited N]`, …). The exit frame carries `agent_error`, so the session's code is mapped exactly as `wait` maps it.
  - Exit codes:
    - 0: detached with the detach key;
    - the session's code when it ended while attached, mapped as `wait` maps it (128+N for a signal, 125 for `launch-failed`);
    - 1: could not attach: the session is not interactive (its plan has no `pty`), not `running`, has no attach socket, or the monitor refused the connection (`busy`, `version`);
    - 2: usage error, a bad detach key, or stdin not a terminal;
    - 125: the attachment was lost after it was made (the connection closed without an exit, or the monitor dropped this client, e.g. as `slow`);
    - 127: no such session;
    - 128+N: `attach` itself got signal N; the session keeps running. The terminal closing (stdin ends) is always reported as SIGHUP, 129, whether or not the SIGHUP arrived first.
- **`logs`:** prints `output.log`; `--stderr` prints `stderr.log`; `-f` follows until the session ends; `--generation N` prints a restored session's earlier generation (`output.<N>.log`, §8.4).
- **`restore`** and **`install-restore`:** §8.4. `--restart on-reboot` needs `--remote` in v0.3.
- **`rm`:** refuses a session that is alive, or has orphans (including descendants an exited agent or a failed restore left behind), or that a `restore` is relaunching (even with `--force`), unless `--force` (which stops it first). `--force` also removes a corrupt session that cannot be loaded, after killing whatever marked processes it can still identify. Orphans that survive SIGKILL block removal.
- **Retention:**
  - A session is pruned when all of these hold:
    - its effective state is `exited` or `restore-failed` (never `starting`, `running` or `lost`);
    - the later of its `ended_at` and `created_at` (zero values ignored; a session with neither is never pruned) is older than the retention period (default 168h; config `retention`, or `prune --older-than`, which must be positive). Prune never acts on less than 1h from config: a smaller `retention` still loads (v0.1 accepted any positive value) but is clamped to 1h, with a stderr warning ("retention 30m is below the 1h minimum; using 1h") from `prune` and, once per actual run, from the opportunistic prune. `--older-than` is explicit and has no floor, but warns below 1h;
    - it has no orphans (the same scan `rm` uses; a grandchild left by a normal exit blocks pruning until it is stopped), and the scan was possible: an exited session that recorded a process group but has no readable plan, or whose process scan failed, is kept with reason `unknown` (fail closed; `rm` refuses it without `--force`, and `rm --force` warns and removes it);
    - it is not pinned (`restart=on-reboot`, which only remote bridges can set; §8.4).
  - `lost` sessions are never pruned, because they may hold the only record of live processes. `prune` reports old ones as kept.
  - `prune [--older-than D] [--dry-run] [--json]` runs on demand and prints the names removed (or, with `--dry-run`, that would be). It exits 0, or 1 if any removal failed. `--json` is `{"removed": [...], "kept": [{"name", "reason"}]}`, where `kept` lists only sessions old enough to prune that were kept (`lost`, `orphans`, `pinned` or `unknown`). If a removal fails, a `failed` array of `{name, error}` is added and the exit code is 1.
  - `run` and `ps` prune opportunistically, at most once per hour:
    - the stamp is `.last-prune` in the state root; if it is non-empty and under an hour old, the command does one `stat` and moves on;
    - otherwise the command takes a non-blocking `flock` on the stamp, so only one of several concurrent CLIs prunes, and the rest skip;
    - the stamp is written when the prune completes;
    - all errors are ignored, so a prune never fails or delays the command beyond the work itself;
    - `ps` prunes before it lists, so it never shows sessions it is about to remove;
    - nothing happens before the first session exists (no stamp is created without a `sessions/` directory);
    - config `auto_prune = false` turns it off (default `true`); `fastener prune` still works. The opportunistic prune obeys `retention`.
  - Deletion is safe against the name being reused: prune and `rm` first rename the directory to a hidden tombstone (`.prune-<name>-<rand>`, which no command lists), re-read it from there, check it is the same session (same `created_at`, generation and process marker) and still prunable, and only then delete it. If the check fails it is renamed back with no-replace semantics (`renameat2` `RENAME_NOREPLACE` on Linux, `RENAME_EXCL` on macOS, falling back to stat-then-rename on ENOSYS/EINVAL); if the name was taken meanwhile the tombstone is left and a warning names it. A prune or `rm` killed between rename and delete leaves a tombstone; at the start of every prune, tombstones untouched for 5 minutes are renamed back (if the name is free) so the normal rules judge them again, and otherwise left with a warning.
  - `fastener prune` takes the same lock but polls it for up to 10 s; if another prune is still running it exits 1 with "another prune is running". If the filesystem does not support `flock` (ENOLCK, EOPNOTSUPP) it proceeds without the lock and warns. `--dry-run` takes no lock. Concurrent `prune` calls otherwise serialize and do not error.
  - A stamp is fresh only if it is under an hour old and not in the future.
  - Callers should know that `run --name foo` may succeed after an old exited `foo` has been auto-pruned, and that a forward clock jump can make sessions look old and prune them early.
  - There is no `--rm`-on-exit flag, because it would race callers reading results.
- **Config:** `~/.config/fastener/config.toml` holds:
  - `retention` (positive duration, default `168h`)
  - `auto_prune` (bool, default `true`: opportunistic pruning by `run` and `ps`)
  - `default_permissions` (must be a level defined by each profile used; it overrides every profile's own `default_permissions`)
  - `handshake_timeout`
  - `launcher` (`auto` | `systemd` | `setsid`)
  - `env_passthrough`
  - `detach_key` (default `ctrl-]`; see `attach`)
  - `remote_wait` (positive duration, default `15s`: how long `run --remote` waits for a bridge's URL)
  - `trust_dirs` (array of absolute folders: a user-maintained allowlist for `authorize claude-folder-trust`, §8.3; Fastener never writes this file, and keeps its own records in `authorizations.json`)
- **Environment overrides:** `FASTENER_STATE_DIR` and `FASTENER_CONFIG_DIR` override the state and config locations (§3).
- **`--json` output** of `run`, `ps`, `inspect`, `prune`, `restore`, `backends`, `check`, `authorize` and `version` is a versioned, documented contract (`docs/json-schema.md`). Human-readable output is not a contract.

## 11. Code layout

```text
cmd/fastener/          entry point; calls internal/cli (stdlib flag, no CLI framework)
internal/cli/          commands, flag parsing, views, check
internal/session/      state dir, schema types, read/write, liveness, prune, labels
internal/monitor/      _monitor: spawn, log, parser feed, limits, signals; the pty of interactive generations
internal/attach/       attach protocol (docs/attach-protocol.md): frames, server, client, socket path and peer credentials
internal/launcher/     setsid and systemd launchers, selection
internal/systemd/      systemctl/loginctl calls: availability, linger, unit state, MainPID, kill, reset-failed
internal/config/       config.toml, config/state locations, authorizations.json
internal/claudetrust/  Claude's trust key, proper-lockfile lock, order- and byte-preserving JSON edit (§8.3)
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
- **Lifecycle** (setsid mode on Linux; the shared harness pins `launcher = "setsid"` so the suite behaves the same with or without a user systemd. `internal/cli/systemd_test.go` repeats run/wait, stop of a TERM-ignoring agent, `--max-runtime`, an external SIGTERM, a SIGKILLed monitor and a normal exit with a leftover grandchild (both: SIGKILLed at once, no orphans), the `--grace` clamp, the agent's open-file limit matching setsid, unit collection and `auto` selection (with linger) under the systemd launcher. Those tests skip when no user manager is usable (`systemctl --user is-system-running` is not `running` or `degraded`), so **CI, whose runners have no user systemd session, does not exercise the systemd launcher**; run them locally before a release):
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
  - signalling through a pidfd (Linux): a handle held across the target's reap refuses to signal, a process that dies between `pidfd_open` and the identity check is not signalled, and an `ENOSYS`/`EPERM` from `pidfd_open` falls back to check-then-kill;
  - a process in a reused pgid without the session marker: it is never counted as an orphan and never signalled;
  - a stale `stop-request.json` from an earlier generation is ignored;
  - a profile edited between `run` and the monitor starting: the monitor uses the plan, and the recorded `sha256` matches what ran;
  - concurrent `stop` and `rm` finalizing a dead monitor (deferred to v0.2; `Finalize` has a unit test);
  - an old state fixture read by the new CLI (deferred to v0.2);
  - `wait` exit codes;
  - label filtering (shipped as `ps --filter`).
- **Live smoke** (opt-in, `FASTENER_SMOKE=1`; costs model calls): runs `fastener check --all --json` and requires every built-in backend to pass. This is the release check that built-in flags are correct. There is no separate smoke-test logic.
- **`check` itself** is tested against the fake agent: a passing reply, a wrong reply, `agent_error`, a timeout (built-in hint), a `diagnose` pattern match in `stderr.log`, and a missing binary.
- **CI:** Linux and macOS runners; `go vet`, `go test -race -v`. Both jobs are blocking. The pinned-zombie tests (`internal/monitor/pinned_test.go`: the no-reap wait before and after the exit, exit beating a signal, stop killing a grandchild), the stop tests and the orphan-scan lifecycle tests run on both. The tests that SIGKILL the monitor and expect only grandchildren to survive need `PDEATHSIG` and skip on macOS. `staticcheck` is not wired in yet.
- **Deferred to v0.2:** concurrent `stop`/`rm` through the CLI (`Finalize` has a unit test) and schema fixtures per release.

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
   - (done: systemd launcher);
   - (done: `codex-jsonl` and `pi-json` parsers, from captured real output);
   - prune and retention;
   - more `diagnose` patterns;
   - macOS hardening (done: zombie pinning with kqueue, blocking macOS CI; see `docs/followups.md`).
3. **v0.3, remote bridges (route A, §8.1):** `[remote]` profiles, `run --remote`, the `claude-remote` parser (`remote_url`, `remote_sessions`), frame de-duplication in `output.log`, `untrusted_workspace` failures, `fastener authorize claude-folder-trust` (§8.3), `--restart on-reboot`, `restore` and `install-restore` for bridges.
4. **v0.4, interactive sessions (route B, §8.2):** pty monitor, `attach`, `run --interactive [--remote]`, conversation finders and interactive restore. Pi's interactive and remote support depends on this.
5. **Later:** Codex remote (§8.5).

**Acceptance for v0.3:**
1. Start a detached remote bridge for a trusted folder; `run` prints its URL.
2. Add a session to it from claude.ai/code or the mobile app; `inspect` lists it.
3. Disconnect, upgrade the Fastener binary, then `ps` and `inspect` the bridge.
4. Reboot; `restore` brings the bridge back and it reattaches.
5. `stop` and `rm` it.
6. An untrusted folder fails with `untrusted_workspace` and the authorize hint.

No Fastener daemon is involved at any point.

## 14. Known risks

- The exact flag names for headless runs, permissions and remote control for each backend are unverified (they are gated by smoke tests).
- Codex cannot be given a conversation ID, so its interactive sessions rely on a heuristic finder (§8.2).
- How Claude auto-permission mode and Codex's equivalent behave in a headless run when an action is denied: whether the denial is reported to the model or the run blocks.
- macOS has no parent-death signal, so in setsid mode (the only mode there) an agent can outlive a killed monitor. It is handled as an orphan of the `lost` session (`stop` or `rm --force` cleans it up), not prevented. The zombie is pinned on both platforms (§4).
- Unknown liveness (an unreadable `/proc` or failed sysctl) is treated as maybe-alive, so a session can stay `running` or block finalization until liveness can be read.
- A normally exited agent can leave descendants running. In setsid mode they are tracked (`inspect` shows `orphans: N` for the `exited` session; `rm` refuses without `--force`), never killed implicitly; `stop` or `rm --force` cleans them up. In systemd mode they are SIGKILLed with the unit's cgroup as soon as the monitor exits, with no grace of their own: a caller that wants an agent's background processes to outlive it must use `--launcher setsid`.
- Without linger, `auto` uses setsid and an explicit `--launcher systemd` warns, because the user manager (and every session unit) stops at logout.
- In setsid mode an agent's descendants can outlive a killed monitor (on Linux: grandchildren only; on macOS: the agent too). This is handled by group-level orphan cleanup, not prevented. systemd mode prevents it.
- Orphan identity fails safe, so some orphans can be missed:
  - a descendant that moves to its own process group, or clears its environment, is never counted or signalled;
  - on macOS, and on Linux where `pidfd_open` is unavailable (before 5.3, or refused by seccomp), the check-then-signal sequence leaves a small pid-reuse window, including across an orphan's grace period. Linux with pidfds closes it (§4, Liveness).
