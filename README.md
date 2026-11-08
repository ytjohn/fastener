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
fastener check claude --model haiku          # prove the backend works
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

Pi has no permission system, so Pi sessions need `--permissions skip`. `check --all --static`
skips backends that do not define the current permission level (Pi, unless you pass
`--permissions skip`); at least one backend must actually be checked.

State lives under `~/.local/state/fastener` and config under `~/.config/fastener`;
`FASTENER_STATE_DIR` and `FASTENER_CONFIG_DIR` override them.

Linux is the tested platform. macOS builds, but its lifecycle tests have not run
yet (see `docs/followups.md`).
