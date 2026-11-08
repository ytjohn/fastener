# Fastener `--json` output (schema_version 1)

The `--json` output of `run`, `inspect`, `ps`, `backends`, `check` and `version` is
a stable contract. Human-readable output is not. Within a schema version, fields are
only added, never removed or repurposed. Absent optional fields are omitted, not null.
Timestamps are RFC 3339 in UTC. Durations are Go duration strings (`"10s"`).

## `inspect <name> --json` and `run --json`

`run --json` prints the same object as `inspect --json`, taken right after the launch handshake.

| Field | Meaning |
|---|---|
| `schema_version` | 1 |
| `name`, `backend` | Session name and backend |
| `state` | Effective state: `starting`, `running`, `exited`, `lost` (inferred: not exited and the monitor is not alive), `restore-failed` |
| `orphans` | Number of live processes still carrying this session's marker. Always present; non-zero only for `lost` sessions and for `exited` sessions whose agent left descendants running. |
| `session` | Contents of `session.json` (intent; see below) |
| `status` | Contents of `status.json` (runtime facts; see below) |
| `plan` | The current generation's launch plan. Omitted if unreadable. |
| `launcher` | The launcher record. Omitted if not written. |
| `paths` | `dir`, `output_log`, `stderr_log`, `monitor_log` (absolute paths) |

`session` fields:
- `schema_version`, `name`, `created_at`, `kind` (`headless`), `backend`, `cwd` (absolute)
- `prompt_source`: `inline`, or the absolute `--prompt-file` path. The text itself is not in the JSON.
- `permissions`, `model`, `max_runtime`, `grace`
- `env_base`: environment values captured from the caller (the passthrough list); `env`: the `--env` values; `env_file`: the path only. Precedence is base, then env-file, then `--env`.
- `labels`, `restart` (`no`), `remote` (false)

`plan` fields:
- `schema_version`, `generation`
- `argv`: the fully resolved argument list; `argv[0]` is an absolute path. For `prompt = "arg"` backends it contains the prompt text itself
- `prompt_mode` (`stdin` | `arg`), `parser`
- `profile_source` (`builtin` or the file path), `profile_sha256`
- `marker`: the value of `FASTENER_SESSION` in the agent's environment
- `fastener_version`, `planned_at`

`launcher` fields: `schema_version`, `launcher` (`setsid`), `unit` (systemd only, v0.2), `monitor` (`{pid, start_time}`), `boot_id`, `launched_at`.

`status` fields:
- `schema_version`, `state`, `generation`
- `monitor`, `agent`: `{pid, start_time}`. `start_time` is clock ticks since boot on Linux and microseconds since the epoch on macOS; treat it as opaque.
- `pgid`, `boot_id`
- `started_at`, `ended_at`
- `ended_by`: `exit` | `limit` | `stop` | `signal` | `launch-failed`
- `exit_code` (absent when the agent was killed by a signal), `signal` (a name, e.g. `SIGKILL`), `error`
- parser fields, each present only when known:
  - `conversation_id`
  - `final_message`, capped at 64 KiB (cut on a UTF-8 boundary)
  - `model`: the model the backend reports it actually served, as `provider/model` when the backend names the provider (Pi), else the bare model id (Claude). It can differ from the requested `--model`: a short alias or pattern resolves to a full id, and a policy can substitute a different model. It is absent when the agent reported an error (`agent_error`), because the attempted model never answered.
  - `model_source`: who reported `model`: `server` (the API response named it) or `client` (the agent CLI recorded the id it requested, so a provider-side substitution would not show; Pi's adapters other than `anthropic-messages` and `openai-completions`, unless they report a `responseModel`). Present exactly when `model` is.
  - `agent_error`
  - `usage`: `{input_tokens, output_tokens, cost_usd}`, present only when the backend reported usage
  - `parse_errors`
  - `usage.input_tokens` counts *all* input tokens for the run, including cache reads and writes. This differs from Anthropic's own `usage.input_tokens`, which excludes cache tokens.
- `updated_at`

While a session runs, parser fields are flushed about once a second and always at exit.

## `ps --json`

An array (never null) of `{name, backend, state, created_at, ended_at?, ended_by?, exit_code?, labels?}`, newest first. Without `--all`, `ps` lists running, starting and lost sessions plus those that ended in the last 24 hours. `ps` does not scan for orphans (too costly across many sessions) and has no orphans field; use `inspect` for that. A session that cannot be read is skipped with a warning on stderr.

## `check --json`

An array, one entry per backend checked: `{backend, passed, session?, skipped?, steps: [{name, status, detail?, hint?}]}`.
- `name` is one of `profile`, `binary`, `launch`, `exit`, `parse`, `model`, `reply`, `cleanup`, in that order. Checking stops at the first failed step, so later steps are absent.
- `model` compares status.json's `model` field with `--expect-model`, else `--model`, using the prefix rule in the design spec (§6.1). It is `skip` when the backend does not report its model, `pass` when the served model matches (or nothing was requested), and `fail` on a mismatch. When status.json's `model_source` is `client`, a pass's detail ends with a `(client-reported: …)` note.
- `status` is `pass`, `fail` or `skip`. With the `none` parser the `parse` step is `skip`.
- `session` is the check session's name (absent for `--static`).
- `skipped: true` appears only under `--all`, for a backend whose profile does not define the resolved permission level (`--permissions`, else the config's `default_permissions`, else the profile's `default_permissions`, else `auto`). Such an entry has `passed: false` and a single `profile` step with status `skip`. A skip is not a failure.

## `backends --json`

An array of `{name, source, sha256, command, parser, permissions, default_permissions?}`, or `{name, error}` for a profile that fails to load. `source` is `builtin` or the file path; `permissions` lists the defined levels; `default_permissions` is the profile's own default level, present only when the profile sets one (the config's `default_permissions` and `--permissions` still take precedence).

## `version --json`

`{schema_version, version}`.

## Exit codes

- **`wait`:**
  - the agent's exit code;
  - 128+N if the agent was killed by signal N;
  - 124 if `--timeout` elapsed;
  - 125 for `launch-failed`, `lost` or `restore-failed`, for an exited session with neither an exit code nor a known signal, and for an agent that exited 0 while status `agent_error` is true (Pi's json mode exits 0 on a model error); a non-zero exit code is returned as is;
  - 127 for an unknown session, including one removed while `wait` was running.
- **Other commands:**
  - 0 for success;
  - 1 for failure (for `check`, any non-skipped backend failed, or `--all` checked nothing);
  - 2 for usage errors, including an unknown `--permissions` level;
  - 127 for an unknown session or backend.

As with Podman, `wait`'s 124, 125 and 127 can collide with real agent exit codes. `inspect --json` is authoritative.
