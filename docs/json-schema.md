# Fastener `--json` output (schema_version 1)

The `--json` output of `run`, `inspect`, `ps`, `prune`, `restore`, `backends`, `check`, `authorize` and `version` is
a stable contract. Human-readable output is not. Within a schema version, fields are
only added, never removed or repurposed. Absent optional fields are omitted, not null.
Timestamps are RFC 3339 in UTC. Durations are Go duration strings (`"10s"`).

## `inspect <name> --json` and `run --json`

`run --json` prints the same object as `inspect --json`, taken right after the launch handshake. `run --remote --json` takes it once the bridge has reported its URL (`status.remote_url`), once `remote_wait` (config, default 15 s) has elapsed while the bridge still runs (exit 0; `remote_url` is then absent), or once the bridge has ended before reporting its URL (exit 1, with `reason`).

| Field | Meaning |
|---|---|
| `schema_version` | 1 |
| `name`, `backend` | Session name and backend |
| `state` | Effective state: `starting`, `running`, `exited`, `lost` (inferred: not exited and the monitor is not alive), `restore-failed` |
| `orphans` | Number of live processes still carrying this session's marker. Always present; non-zero only for `lost` sessions, `exited` sessions whose agent left descendants running, and `restore-failed` sessions whose relaunch left some. Only processes in the agent's process group are counted, except for `restore-failed` sessions and `restart = "on-reboot"` sessions that are `lost` (or `exited` after `stop` finalized them from `lost`), where a marked process in any group counts (design §4). |
| `session` | Contents of `session.json` (intent; see below) |
| `status` | Contents of `status.json` (runtime facts; see below) |
| `plan` | The current generation's launch plan. Omitted if unreadable. |
| `launcher` | The launcher record. Omitted if not written. |
| `paths` | `dir`, `output_log`, `stderr_log`, `monitor_log` (absolute paths) |
| `liveness_error` | String, omitted when empty. Present only while `state` is `starting` or `running` and the check on the monitor process failed (for example a permission error reading `/proc`). Fastener then assumes the monitor is alive rather than declaring the session `lost`, so a session can look `running` for as long as the error persists. |
| `orphan_error` | String, omitted when empty. Why `orphans` cannot be trusted: no readable plan for the current generation, a failed process scan, or (after a failed `run`) a reload that failed twice, so the true count may be higher than reported. |
| `reason` | String, `run --remote` (and `run --interactive --remote`) only, omitted otherwise. Why the bridge ended before reporting its URL: `untrusted_workspace` (its `stderr.log` matched the profile's `[remote] untrusted_pattern`, or, for an interactive session, its terminal output matched `[interactive.remote] untrusted_pattern`, in which case `run` stopped the session: the backend refuses the folder until its trust is accepted, see `folder`) or `exited` (any other end before `run` saw the URL; the error on stderr names how it ended and the last line of `stderr.log`, and says `ended after reporting <url>` instead of `ended before reporting its URL` when the bridge had printed one, which is then in `status.remote_url`). |
| `folder` | String, with `reason: "untrusted_workspace"` only: the absolute folder to authorize (the session's `cwd`). |
| `warnings` | Array of strings, omitted when empty. One entry per state file whose `schema_version` is newer than this fastener understands, e.g. `status.json has schema_version 2; this fastener understands up to 1, so some fields may be ignored`. The file is still read; unknown fields are ignored. |

`session` fields:
- `schema_version`, `name`, `created_at`, `kind` (`headless` | `remote` | `interactive`: `run --interactive`, with `remote: true` when also `--remote`), `backend`, `cwd` (absolute)
- `prompt_source`: `inline`, `stdin` (for `--prompt-file -`), or the absolute `--prompt-file` path; `""` for a remote bridge, which takes no prompt, and for an interactive session started without one. The text itself is not in the JSON.
- `permissions`, `model`, `max_runtime`, `grace`
- `env_base`: environment values captured from the caller (the passthrough list); `env`: the `--env` values; `env_file`: the path only. Precedence is base, then env-file, then `--env`.
- `labels`, `restart` (`no` | `on-reboot`; `on-reboot` only with `remote`, and not for interactive sessions), `remote` (true for `run --remote`, including `run --interactive --remote`)

`plan` fields:
- `schema_version`, `generation`
- `argv`: the fully resolved argument list; `argv[0]` is an absolute path. For `prompt = "arg"` backends it contains the prompt text itself
- `prompt_mode` (`stdin` | `arg` | `none`: a remote bridge, whose stdin is `/dev/null`, or an interactive session started without a prompt), `parser`
- `dedupe_frames`: `true` when the monitor drops redrawn status lines from `output.log` (a line equal to the same position in the previous frame) and strips leading cursor-movement escapes from the lines it writes (from the profile's `[remote] dedupe_frames`; every line is still parsed). Omitted otherwise.
- `url_pattern`: for `run --interactive --remote`, the profile's `[interactive.remote] url_pattern`; the monitor matches it against the terminal output (escapes stripped) and records the latest match as `status.remote_url`. Omitted otherwise.
- `env`: array of `KEY=VALUE` from the profile's `[interactive].env`, set for the agent after `session.env_base` and before the env-file and `session.env`. Omitted when empty.
- `pty`: `{rows, cols}` for an interactive generation, which the monitor runs on a pty it owns (design §8.2); `rows` and `cols` are the initial window size, each omitted when 0 (meaning 24 × 80). Omitted for every other generation.
- `profile_source` (`builtin` or the file path), `profile_sha256`
- `marker`: the value of `FASTENER_SESSION` in the agent's environment
- `fastener_version`, `planned_at`

`launcher` fields: `schema_version`, `launcher` (`setsid` | `systemd`), `unit` (systemd only: the transient unit, e.g. `fastener-<name>-g1-<rand>.service`), `monitor` (`{pid, start_time}`; for systemd, omitted when the unit's main pid could not be read, and the session is then judged by the unit's state), `boot_id`, `launched_at`.

`status` fields:
- `schema_version`, `state`, `generation`
- `monitor`, `agent`: `{pid, start_time}`. `start_time` is clock ticks since boot on Linux and microseconds since the epoch on macOS; treat it as opaque.
- `pgid`, `boot_id`
- `started_at`, `ended_at`
- `ended_by`: `exit` | `limit` | `stop` | `signal` | `launch-failed`
- `exit_code` (absent when the agent was killed by a signal), `signal` (a name, e.g. `SIGKILL`), `error`
- `restore_reason`: with state `restore-failed`, why `restore` could not relaunch the session (see `restore --json` below for the values); `error` then holds the message, and `remote_url` and `remote_sessions` are cleared (they would describe a bridge that is not running). Omitted otherwise.
- `attach_socket`: interactive generations, while the monitor runs: the absolute path of the attach socket (`<dir>/attach.sock`, or a fallback under `/tmp/fastener-<uid>/` when that path is too long for a socket address; see docs/attach-protocol.md). Omitted otherwise and in the exit record.
- `attached_clients`: interactive generations, while the monitor runs: how many clients are attached (an integer, `0` included), updated about once a second. Omitted otherwise and in the exit record. A `lost` session may still show the last value its monitor wrote.
- `output_truncated`: `true` when a descendant still held the agent's stdout open 2 s after the agent exited. The monitor stopped reading, so anything written to stdout after that is not in `output.log`. Omitted otherwise.
- parser fields, each present only when known:
  - `conversation_id`
  - `final_message`, capped at 64 KiB (cut on a UTF-8 boundary)
  - `model`: the model the backend reports it actually served, as `provider/model` when the backend names the provider (Pi), else the bare model id (Claude). It can differ from the requested `--model`: a short alias or pattern resolves to a full id, and a policy can substitute a different model. It is absent when the agent reported an error (`agent_error`), because the attempted model never answered.
  - `model_source`: who reported `model`: `server` (the API response named it) or `client` (the agent CLI recorded the id it requested, so a provider-side substitution would not show; Pi's adapters other than `anthropic-messages` and `openai-completions`, unless they report a `responseModel`). Present exactly when `model` is.
  - `agent_error`
  - `usage`: `{input_tokens, output_tokens, cost_usd}`, present only when the backend reported usage or a cost. `cost_usd` is omitted when the backend reported no cost (Pi without pricing, Codex), never emitted as 0. `input_tokens` and `output_tokens` are omitted when the backend reported no token counts (for example a Claude result with `total_cost_usd` but no `usage` block, which yields `{"cost_usd": x}`)
  - `parse_errors`
  - `remote_url`: for a remote bridge (parser `claude-remote`), the bridge's URL, `https://claude.ai/code?environment=env_…`, the latest one the bridge printed; for an interactive remote session, the latest match of `plan.url_pattern` in its terminal output (Claude: `https://claude.ai/code/session_…`)
  - `remote_sessions` (remote bridges): array of the session URLs the bridge has shown, `https://claude.ai/code/session_…` without their query string, de-duplicated, in the order first seen; at most 64 entries, the oldest evicted first (a URL seen again after its eviction is added again at the end)
  - `usage.input_tokens` counts *all* input tokens for the run, including cache reads and writes. This differs from Anthropic's own `usage.input_tokens`, which excludes cache tokens.
- `updated_at`

While a session runs, parser fields are flushed about once a second and always at exit.

## `ps --json`

An array (never null) of `{name, backend, kind?, state, created_at, ended_at?, ended_by?, exit_code?, labels?, remote_url?, interactive?, attached_clients?}`, newest first. `kind` is `session.kind` (`headless`, `remote` or `interactive`; omitted for a session whose `session.json` is missing). `remote_url` is a remote bridge's or interactive remote session's URL (`status.remote_url`), once it has reported one. `interactive` is `true` when the current generation runs on a pty (`plan.pty`), omitted otherwise; `attached_clients` is `status.attached_clients`, present only for an interactive session whose state is `running`. Without `--all`, `ps` lists running, starting and lost sessions plus those that ended in the last 24 hours. `ps` does not scan for orphans (too costly across many sessions) and has no orphans field; use `inspect` for that. `--filter KEY=VALUE` (repeatable, ANDed; see design §10) keeps only matching rows after the window is applied; the shape is unchanged and no match gives `[]`. A session that cannot be read is skipped with a warning on stderr. A session whose files have a newer `schema_version` than this fastener understands is listed, with one `fastener: warning: <name>: …` line on stderr (the same text as `inspect`'s `warnings`).

## `prune --json`

`{removed, kept}`: `removed` is an array (never null) of session names deleted, or, with `--dry-run`, that would be; `kept` is an array (never null) of `{name, reason}` for sessions old enough to prune that were kept, where `reason` is `lost`, `orphans`, `pinned` or `unknown` (the leftover-process scan was not possible). Sessions that are not old enough, or are alive, appear in neither. If a removal fails, a `failed` array of `{name, error}` is added and the exit code is 1. Opportunistic pruning by `run` and `ps` prints nothing.

## `restore --json`

An array (never null) with one entry per session reported (in name order, or in argument order for named sessions): `{name, result, reason?, message?, generation?, remote_url?}`. Without names only lost `on-reboot` sessions are reported (`[]` when there are none); named sessions are always reported.
- `result`: `restored`, `skipped` or `failed`; with `--dry-run`, `would-restore`, `would-fail` or `skipped`.
- `reason`, present unless restored:
  - skipped: `not_on_reboot`, `not_remote` (headless sessions are never restored), `running`, `not_lost` (it ended rather than being lost; a named session that `stop` finalized from lost is accepted), `orphans` (processes of the old generation are alive; `message` says to run `fastener stop <name>`, then `fastener restore <name>`), `orphans_unknown`, `restore_in_progress`, `not_found` (only when the session was removed while restore waited for its lock), `stopped` (the relaunched generation was stopped with `fastener stop` before it reported its URL; it is left `exited`);
  - failed (recorded as state `restore-failed` with `status.restore_reason`): `config`, `profile`, `no_remote`, `no_permission_level`, `env_file`, `binary_not_found`, `launch_failed`, `untrusted_workspace` (the folder is no longer trusted; `message` names the command that trusts it), `exited` (the relaunched bridge ended before reporting its URL), `error` (an I/O error while relaunching);
  - failed, not recorded: `not_found` (a named session that does not exist), `lost` (the relaunched monitor died; the session is left `lost` for `stop`/`rm`).
- `message`: a human-readable explanation, present unless restored.
- `generation`: the new generation's number. Present for `restored` and `would-restore` (the generation that would be launched), and for any result reached after restore began writing the new generation (it rotates the logs first): a `failed` result with reason `launch_failed`, `untrusted_workspace`, `exited`, `lost` or an `error` while rotating or writing, and `skipped` with reason `stopped`. Absent otherwise, including failures found before anything was written (`config`, `profile`, `no_remote`, `no_permission_level`, `env_file`, `binary_not_found`) and a `launch_failed` from choosing the launcher.
- `remote_url`: the bridge's URL, once the relaunched bridge reported it within `remote_wait` (the same `env_…` URL as before for Claude).

Exit code 0 when nothing failed, 1 when any entry is `failed` or `would-fail`, 2 for usage errors.

## `check --json`

An array, one entry per backend checked: `{backend, passed, session?, skipped?, steps: [{name, status, detail?, hint?}]}`.
- `name` is one of `profile`, `binary`, `launch`, `exit`, `parse`, `model`, `reply`, `cleanup`, in that order. Checking stops at the first failed step, so later steps are absent.
- `model` compares status.json's `model` field with `--expect-model`, else `--model`, using the prefix rule in the design spec (§6.1). It is `skip` when the backend does not report its model, `pass` when the served model matches (or nothing was requested), and `fail` on a mismatch. When status.json's `model_source` is `client`, a pass's detail ends with a `(client-reported: …)` note.
- `status` is `pass`, `fail` or `skip`. With the `none` parser the `parse` step is `skip`.
- `session` is the check session's name (absent for `--static`).
- `skipped: true` appears only under `--all`, for a backend whose profile does not define the resolved permission level (`--permissions`, else the config's `default_permissions`, else the profile's `default_permissions`, else `auto`). Such an entry has `passed: false` and a single `profile` step with status `skip`. A skip is not a failure.

## `backends --json`

An array of `{name, source, sha256, command, parser, permissions, default_permissions?}`, or `{name, error}` for a profile that fails to load. `source` is `builtin` or the file path; `permissions` lists the defined levels; `default_permissions` is the profile's own default level, present only when the profile sets one (the config's `default_permissions` and `--permissions` still take precedence).

## `authorize --json`

- **`authorize claude-folder-trust <dir> --json`:** `{folder, key, key_is_repo_root, claude_config, already_trusted, backup?, warning?, claude_version?}`. `folder` is the resolved folder; `key` is the `projects` key set in Claude's config (the git repository root when `key_is_repo_root`, else the folder); `claude_config` is the file written; `already_trusted` is true when the field was already `true` and nothing was written; `backup` is the backup of the previous file, present only when it was changed; `warning` is present when the change succeeded but releasing Claude's lock found it taken over after the rename; `claude_version` is `claude --version`, when it ran.
- **`authorize --list --json`:** `{schema_version, authorizations, trust_dirs}`. `authorizations` is an array (never null) of `{folder, key, claude_config, authorized_at, verified_at?, claude_version?}`, the contents of `authorizations.json` in the config directory, in the order added; a record without `verified_at` was added to the allowlist but Claude's config was not confirmed (re-run the command). `trust_dirs` is an array (never null) of the folders listed in `config.toml`'s `trust_dirs`, as written there.
- **`authorize --revoke <dir> --json`:** `{folder, revoked, in_trust_dirs, key?, claude_config?, same_key?}`. `revoked` is whether a record was removed (exit 0; exit 1 when there was none); `in_trust_dirs` is whether `config.toml` still lists the folder, which keeps it on the allowlist. `key` and `claude_config` name the Claude entry that is left unchanged; `same_key` lists other recorded folders with the same key, present only when there are any.

## `version --json`

`{schema_version, version}`.

## Exit codes

- **`wait`:**
  - the agent's exit code;
  - 128+N if the agent was killed by signal N;
  - 124 if `--timeout` elapsed;
  - 125 for `launch-failed`, `lost` or `restore-failed`, for a session removed while `wait` was running (it exists when `wait` starts, then disappears), for an exited session with neither an exit code nor a known signal, and for an agent that exited 0 while status `agent_error` is true (Pi's json mode exits 0 on a model error); a non-zero exit code is returned as is;
  - 127 for a session that never existed (unknown name at the time `wait` starts).
- **`run --follow`:** same as `wait` (except 124, as there is no timeout). If the session is removed while following, it prints `session <name> was removed while following` and exits 125.
- **Other commands:**
  - 0 for success;
  - 1 for failure (for `check`, any non-skipped backend failed, or `--all` checked nothing);
  - 2 for usage errors, including an unknown `--permissions` level;
  - 127 for an unknown session or backend.

As with Podman, `wait`'s 124, 125 and 127 can collide with real agent exit codes. `inspect --json` is authoritative.
