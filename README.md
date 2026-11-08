# Fastener

A daemonless command-line tool that launches, tracks and stops agent sessions:
headless runs of Claude Code, Codex, Pi and other agent CLIs. Each session is
supervised by its own small monitor process, so the UI, scheduler or shell that
started it can restart freely.

Status: v0.1. Design: `docs/superpowers/specs/`.

License: Apache-2.0.

## Quick start

```bash
go install github.com/ytjohn/fastener/cmd/fastener@latest
fastener check pi --model github-copilot/claude-haiku-4.5   # prove the backend works and served that model
fastener run claude --cwd ~/proj --prompt "Summarize README.md" --max-runtime 10m
fastener ps
fastener wait <name>; fastener inspect <name>
```

`run` prints the session name and returns once the agent has started; the
session keeps running if your shell exits. Then:

```bash
fastener logs <name> [-f] [--stderr]    # agent output (-f follows)
fastener stop <name> [--grace 10s]      # SIGTERM, then SIGKILL after the grace period; also cleans up leftover processes of an exited session
fastener rm <name> [--force]            # remove a finished session (refuses if it has leftover processes; --force kills them)
fastener check --all --static           # profile and binary checks for every backend, no model call
```

Add `--json` to `run`, `inspect`, `ps`, `backends`, `check` and `version` for
machine-readable output (see `docs/json-schema.md`). `wait` exits with the
agent's exit code (124 on `--timeout`, 125 if the session was lost or failed to
launch, 127 for an unknown session).

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

Linux is the tested platform. macOS builds, but its lifecycle tests have not run
yet (see `docs/followups.md`).
