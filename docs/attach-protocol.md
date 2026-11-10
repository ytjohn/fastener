# Attach protocol (version 1)

The monitor of an interactive session owns the agent's pty (design §8.2) and
serves it to attached terminals over this protocol. It is a stream of framed
messages and does not depend on the transport; a Unix stream socket is the
only transport in use. The implementation, both ends, is `internal/attach`.

## Transport: the attach socket

- **Where.** `<session dir>/attach.sock`. A Unix socket path must fit in
  `sun_path` (108 bytes on Linux, 104 on macOS, each including the
  terminating NUL). When `<session dir>/attach.sock` is longer than that
  (107 or 103 bytes), the socket goes to
  `/tmp/fastener-<uid>/<h>.sock` instead, where `<h>` is the first 16 hex
  digits of the SHA-256 of the session directory's path. The directory is
  created with mode 0700; an existing one must be a real directory (not a
  symlink), owned by the user, with no group or other permission bits, or
  the monitor refuses it and the generation is `launch-failed` with an error
  naming the path and the limit. Clients do not need to compute any of this:
  while the generation runs, `status.json` has the bound path in
  `attach_socket` (`inspect --json` shows it).
- **Creation.** The socket is bound inside a directory only the user can
  enter (the session directory is 0700) and then chmodded to 0600. A socket
  already at the path is replaced only when it is stale: the monitor
  connects to it first, and only `ECONNREFUSED` (nobody listening, as after a
  killed monitor) lets it remove the file. A successful connect means another
  monitor is serving there, and the generation is `launch-failed`; so is any
  other connect error, or any other kind of file at the path.
- **Who may connect.** Besides the file mode, the monitor checks the
  kernel's peer credentials on every connection (`SO_PEERCRED` on Linux,
  `LOCAL_PEERCRED` on macOS) and closes, without a word, any connection
  whose uid is not its own.
- **Lifetime.** The monitor listens from just before the agent is spawned
  until the generation's outcome is recorded; then it removes the socket,
  sends `exit` to every client and closes them. It removes the file only if
  the path still names the socket it bound (same inode), so a monitor that
  is still shutting down never removes a newer monitor's socket. A monitor killed with
  SIGKILL leaves the socket file behind; connecting to it fails
  (`ECONNREFUSED`), and the next monitor for the session replaces it.

## Frames

Every frame is a 5-byte header followed by the payload:

| Offset | Size | Field |
|---|---|---|
| 0 | 1 | type |
| 1 | 4 | payload length, big-endian unsigned |
| 5 | length | payload |

The payload is at most 65536 bytes (`MaxPayload`). A header announcing more
is a protocol error. Integers in payloads are big-endian unsigned 16-bit
(`u16`).

| Type | Name | Direction | Payload |
|---|---|---|---|
| 1 | `hello` | client → server | `version u16`, then optionally `rows u16`, `cols u16`; further bytes are ignored |
| 2 | `welcome` | server → client | `version u16`, `rows u16`, `cols u16`: the version in use and the pty's size in effect after the attach (the pty's current size if applying the hello's size failed) |
| 3 | `data` | both | terminal bytes: pty output (server → client) or input (client → server) |
| 4 | `resize` | client → server | `rows u16`, `cols u16`, both non-zero; exactly 4 bytes |
| 5 | `detach` | client → server | empty: the client leaves; the session keeps running |
| 6 | `exit` | server → client | JSON `{"ended_by", "exit_code"?, "signal"?, "error"?}`, as in `status.json`; the server then closes |
| 7 | `error` | server → client | JSON `{"code", "message", "versions"?}`; the server then closes |

Error codes: `version` (the hello's version is not one the server speaks;
`versions` lists those it does), `protocol` (a malformed or unexpected
frame), `busy` (16 clients are already attached), `slow` (the client fell
too far behind; see below).

## Session

1. The client connects and sends `hello` within 5 s. Anything else first, or
   nothing, is a `protocol` error.
2. If the version is not 1, the server answers `error` with code `version`
   and closes. A later version of the protocol is negotiated the same way:
   the client sends the highest version it speaks, and a server that cannot
   speak it says which versions it can.
3. The server answers `welcome`. From then on it sends `data` frames with
   everything the agent writes to the terminal. Nothing written before the
   attach is replayed: Fastener does not emulate the screen.
4. If the hello carried a size (rows and cols both non-zero), the server
   applies it to the pty and the agent redraws (below). A hello without a
   size attaches without resizing.
5. The client sends `data` (keystrokes, written to the pty as they are,
   each frame's bytes kept together), `resize` when its terminal changes,
   and `detach` (or simply closes the connection) to leave.
6. When the session ends the server sends `exit` and closes.

A malformed frame (an oversized length, an unknown type, a `resize` of the
wrong length or a zero size, a second `hello`, a frame cut short) drops that
client only, with an `error` frame where possible. Other clients and the
agent are unaffected.

## Several clients

- Every attached client receives all output from the time it attached.
- Input from any client is written to the pty.
- **Size: the last client to attach with a size, or to resize, wins.**
  Detaching does not restore an earlier size (as in dtach and abduco).
- **Redraw.** Applying a size is `TIOCSWINSZ` on the pty master. When the
  size changes, the kernel itself sends SIGWINCH to the terminal's
  foreground process group (Linux and macOS alike). When it does not change,
  the monitor sends SIGWINCH itself, so an attach always forces a redraw: to
  the foreground process group from `TIOCGPGRP` on the master on Linux, and
  to the agent's process group on macOS, where `TIOCGPGRP` works only on a
  process's own controlling terminal. A signal is only sent between the
  agent's spawn and its reap, while its pgid cannot be reused.
- **Slow clients.** Output for each client is queued in memory, at most
  1 MiB of frames (counted in bytes, however small the frames). A client
  whose queue would grow past that is disconnected: its waiting output is
  discarded and an `error` frame with code `slow` is sent in its place,
  within a second if the connection takes it, before the connection is
  closed. The pty is read by the monitor whether or not anyone is attached,
  and the fan-out never waits for a client, so a stuck client can stall
  neither the agent nor the other clients.
- **Input.** One writer writes input to the pty, a whole `data` frame at a
  time, so frames from different clients never interleave. Up to 64 frames
  wait for it while the agent is not reading its terminal; once that queue
  is full, a client sending more input waits (its later frames are read
  after its input is queued). Resizes and detaches never wait for input,
  whichever client sends them.
- At most 16 clients are attached at once; one more gets `error` code `busy`.
