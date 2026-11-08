# Follow-ups

Deferred work after v0.1, grouped by area. Each item is one line plus the reason it was deferred.

## macOS

- Pin the exited agent's zombie with kqueue `EVFILT_PROC` + `NOTE_EXIT`: `x/sys/unix` has no `Waitid`, so the pgid is not pinned while the group is signalled.
- Close the darwin TERM/KILL race windows (pgid reuse between the agent's exit and the group kill): follows from the missing zombie pin.
- Harden macOS CI: the lifecycle tests have never run there, so the job is `continue-on-error`; remove that once it is green.

## systemd launcher (v0.2)

- Implement the systemd launcher (`systemd-run --user`, cgroup kill, `RuntimeMaxSec`/`TimeoutStopSec` sizing): v0.1 ships setsid only and `--launcher systemd` is an error.
- Session resolve must judge a unit-only launcher record (`unit` set, no `monitor` identity) by the unit's active state: today it is a `TODO` in `View.resolve`, and such a record cannot be judged.
- Clamp `stop --grace` to the session's grace in systemd mode, with a warning, and test it: the unit's timers are sized from the session's grace.

## Monitor

- Write `status.json` outside the mutex: the write holds `m.mu`, so a slow disk blocks the pump's parser updates.
- Re-read the stop request on a second signal: a stop with a longer or shorter grace arriving mid-termination is ignored.
- Output written after the 2 s drain timeout is dropped: a descendant holding the pipe open must not keep the session open, but its late output is lost.
- The 16 MB line cap limits parsing but not memory: `ReadBytes` still allocates the whole line.

## CLI

- Add a handshake-timeout test: it needs a launcher hook that delays the monitor.
- `wait` returns 127 after `rm` removes the session mid-wait: indistinguishable from "never existed", and it collides with agent exit codes.
- Support `--prompt-file -` (stdin) and put a size cap on prompts: the prompt is read whole into memory and copied to `prompt.txt`.
- Define the exit code of `run --follow`: it follows the log but does not report the agent's outcome.
- `cleanCorrupt` cannot find orphans when `status.json` is unreadable: it has no pgid or boot ID to search with, so it only warns.
- Add integration tests for the monitor-start-failure view and the `cleanCorrupt` plan-read warning: both are covered by unit logic only.
- A handshake that cannot reload a view falls back to the cheap view without the orphan scan: the error report may under-count orphans.
- `--` only protects the next positional argument: a profile with `prompt = "arg"` still lets the agent CLI interpret the prompt (see Pi's `@` below).
- Concurrent `stop` and `rm` through the CLI: only `Finalize` is tested for it.

## Parsers

- Add `codex-jsonl`. Event shape captured 2026-11-07: `thread.started` carries `thread_id`; an `item.completed` event whose item is an `agent_message` carries the text; `turn.completed` carries usage. The built-in profile uses `parser = "none"` until then.
- Pi's output is plain text (`--mode text`): consider `--mode json` and a Pi parser to get a conversation ID and usage.
- Pi may treat a prompt starting with `@` as a file include: unverified, so check whether the prompt needs escaping.
- Add more `diagnose` patterns as new failures are seen: they are data, one profile PR each.

## Session

- Keep schema `testdata` fixtures per release and test the new CLI against each: there is only schema 1 so far.
- Warn when a file carries a newer `schema_version` than the CLI knows: today it is read silently.
- Surface persistent unknown liveness in `inspect`: unknown is treated as maybe-alive, so a session can look `running` indefinitely with no explanation.

## Profiles

- `command = "."` is accepted: it matches the bare-name pattern. It fails later at lookup, but validation should reject it.
- A usage line with a cost but no tokens is dropped by the `claude-stream-json` parser: `usage` is emitted only when token counts are reported.

## Planned for v0.2

- `prune` and retention (default 7 days, `.last-prune` throttle): v0.1 never deletes sessions except through `rm`.
- `ps --filter` (label, state, backend): labels are stored but not yet filterable.
- pidfd-based signalling on Linux (`pidfd_open` + `pidfd_send_signal`): closes the pid-reuse window between check and signal for orphans.
