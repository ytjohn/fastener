# Fastener

A daemonless command-line tool that launches, tracks and stops agent sessions:
headless runs of Claude Code, Codex, Pi and other agent CLIs. Each session is
supervised by its own small monitor process, so the UI, scheduler or shell that
started it can restart freely.

Status: pre-release (v0.1 in progress). Design: `docs/superpowers/specs/`.

License: Apache-2.0.
