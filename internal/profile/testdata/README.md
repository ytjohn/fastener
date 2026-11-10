# Profile fixtures

- `claude-2.1.322-trust-dialog.raw`: the raw terminal output of
  `claude --remote-control <name>` (Claude Code 2.1.322) under a 40x120 pty in
  a folder whose trust dialog had not been accepted, captured on 2026-11-10
  and cut off when the process was stopped. The folder path was replaced by
  `/work/project`; nothing else was changed. The built-in claude profile's
  `[interactive.remote] untrusted_pattern` must match it.
