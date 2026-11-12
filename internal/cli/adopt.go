package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/ytjohn/fastener/internal/finder"
	"github.com/ytjohn/fastener/internal/proc"
	"github.com/ytjohn/fastener/internal/runenv"
	"github.com/ytjohn/fastener/internal/session"
)

// adoptRequest is `fastener adopt`'s conversation: its ID (a UUID) and
// whether --force skips the in-use guard.
type adoptRequest struct {
	ID    string
	Force bool
}

// adoptActiveWindow is how recently a conversation's file may have been
// written for adopt to take it as in use elsewhere (an agent writes its
// conversation at every message, so an open session someone is using has
// written within minutes; an idle one has not, which the process check
// covers where the agent was started with the ID).
const adoptActiveWindow = 3 * time.Minute

const adoptUsage = `usage: fastener adopt <backend> <conversation-id> [flags]

Resume a conversation started outside Fastener as an interactive session
you can attach to, drive remotely and restore after a reboot. The ID is
the conversation's UUID (claude --resume, codex resume, pi --session-id).

Folder:
  --cwd DIR                the conversation's folder. claude and pi read it
                           from the conversation (another DIR is refused);
                           codex needs it.
Session:
  --name N                 session name (generated if omitted)
  --remote                 also drive it from claude.ai/code; prints the URL
  --size ROWSxCOLS         terminal size (default: this terminal's, else 24x80)
  --attach                 attach this terminal right after the launch
  --restart no|on-reboot   on-reboot: fastener restore resumes it after a reboot
Agent:
  --permissions auto|skip  default: config, else the profile's, else auto
  --model M                model passed to the backend
  --env KEY=VALUE          for the agent (repeatable; recorded, not for secrets)
  --env-file F             KEY=VALUE file read by the monitor (for secrets)
  --label KEY=VALUE        label (repeatable)
  --launcher auto|setsid|systemd   default: config
  --grace D                time between SIGTERM and SIGKILL on stop (default 10s)
Other:
  --force                  adopt even if the conversation looks in use
  --json                   print the session as JSON

A conversation looks in use when its file was written in the last 3
minutes, a running process has its ID in its arguments, or a Fastener
session holds it. Close the other session first, or pass --force: two
agents writing one conversation interleave it.

Exit status: 0 adopted; 1 refused or failed; 2 usage; 127 no such
conversation or backend.
`

// adoptNotFound is a conversation the backend's store does not have (exit 127).
type adoptNotFound struct{ err error }

func (e *adoptNotFound) Error() string { return e.err.Error() }
func (e *adoptNotFound) Unwrap() error { return e.err }

func cmdAdopt(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("adopt <backend> <conversation-id>", stderr)
	fs.Usage = func() { fmt.Fprint(stderr, adoptUsage) }
	var o launchOptions
	var env, labels multiFlag
	var asJSON, attachAfter, force bool
	var sizeFlag string
	fs.StringVar(&o.Cwd, "cwd", "", "")
	fs.StringVar(&o.Name, "name", "", "")
	fs.BoolVar(&o.Remote, "remote", false, "")
	fs.StringVar(&sizeFlag, "size", "", "")
	fs.BoolVar(&attachAfter, "attach", false, "")
	fs.StringVar(&o.Restart, "restart", restartNo, "")
	fs.StringVar(&o.Permissions, "permissions", "", "")
	fs.StringVar(&o.Model, "model", "", "")
	fs.Var(&env, "env", "")
	fs.StringVar(&o.EnvFile, "env-file", "", "")
	fs.Var(&labels, "label", "")
	fs.StringVar(&o.Launcher, "launcher", "", "")
	fs.DurationVar(&o.Grace, "grace", 10*time.Second, "")
	fs.BoolVar(&force, "force", false, "")
	fs.BoolVar(&asJSON, "json", false, "")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "usage: fastener adopt <backend> <conversation-id> [--cwd DIR] [flags] (fastener adopt -h for the flags)")
		return exitUsage
	}
	o.Backend, o.Env, o.Interactive = pos[0], env, true
	if !finder.ValidID(pos[1]) {
		return fail(stderr, usagef("%q is not a conversation ID: want a UUID, as claude, codex and pi name conversations", pos[1]))
	}
	o.Adopt = &adoptRequest{ID: pos[1], Force: force}
	if err := validatePermissions(o.Permissions); err != nil {
		return fail(stderr, err)
	}
	if o.Labels, err = parseLabels(labels); err != nil {
		return fail(stderr, err)
	}
	if err := validateRestart(o.Restart, o.Remote, true); err != nil {
		return fail(stderr, err)
	}
	switch {
	case attachAfter && asJSON:
		fmt.Fprintln(stderr, "fastener: --json and --attach cannot be combined")
		return exitUsage
	case attachAfter && !term.IsTerminal(int(os.Stdin.Fd())):
		// Checked before launching, so nothing starts that cannot be attached.
		fmt.Fprintln(stderr, "fastener: --attach needs a terminal on stdin")
		return exitUsage
	}
	if o.PTY, err = interactiveSize(sizeFlag); err != nil {
		return fail(stderr, err)
	}
	autoPrune()
	v, p, err := launchPrepared(o, stderr)
	if err != nil {
		if v != nil && asJSON {
			writeJSON(stdout, inspectOf(v))
		}
		var nf *adoptNotFound
		if errors.As(err, &nf) {
			fmt.Fprintf(stderr, "fastener: %v\n", err)
			return exitNotFound
		}
		return fail(stderr, err)
	}
	if o.Remote {
		wait := p.cfg.RemoteWaitDuration()
		v, err = waitRemoteURL(v, p, wait)
		out := inspectOf(v)
		var rf *remoteFailure
		if errors.As(err, &rf) {
			out.Reason, out.Folder = rf.reason, rf.folder
		}
		if asJSON {
			writeJSON(stdout, out)
		}
		if err != nil {
			return fail(stderr, err)
		}
		if !asJSON {
			if u := v.Status.RemoteURL; u != "" {
				fmt.Fprintf(stdout, "%s  %s\n", v.Session.Name, u)
			} else {
				fmt.Fprintln(stdout, v.Session.Name)
			}
		}
		if v.Status.RemoteURL == "" {
			fmt.Fprintf(stderr, "fastener: %s: no URL yet after %s; the %s is still running (fastener inspect %s shows the URL once reported)\n",
				v.Session.Name, wait, remoteNoun(p), v.Session.Name)
		}
	} else if asJSON {
		writeJSON(stdout, inspectOf(v))
	} else if !attachAfter {
		fmt.Fprintln(stdout, v.Session.Name)
	}
	if attachAfter {
		return cmdAttach([]string{v.Session.Name}, stdout, stderr)
	}
	return exitOK
}

// adoptConversation finds the adopted conversation in the backend's store
// (the profile's finder, with the agent's environment), settles the folder
// the session runs in, and refuses a conversation that looks in use
// elsewhere unless --force. cwd is the absolute --cwd, or "". It returns
// the folder.
func adoptConversation(o launchOptions, p *prepared, cwd string) (string, error) {
	a := o.Adopt
	// The agent's environment, layered as the monitor layers it, for the
	// variables that move a store (CLAUDE_CONFIG_DIR, CODEX_HOME, ...).
	env := runenv.Merge(p.envBase, p.rendered.Env, p.fileEnv, o.Env)
	locs, err := finder.Locate(p.rendered.Finder, finder.Query{Cwd: cwd, Env: env, SessionID: a.ID})
	if errors.Is(err, finder.ErrNotFound) {
		return "", &adoptNotFound{fmt.Errorf("backend %s has no conversation %s (%v)", o.Backend, a.ID, err)}
	}
	if err != nil {
		return "", err
	}
	loc, cwd, err := adoptFolder(o.Backend, a.ID, locs, cwd)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("conversation %s belongs to %s, which is not a directory here", a.ID, cwd)
	}
	if !a.Force {
		if err := adoptInUse(a.ID, loc, time.Now()); err != nil {
			return "", err
		}
	}
	return cwd, nil
}

// adoptFolder picks the folder the adopted session runs in. With --cwd it
// must be the conversation's own (claude and pi store a conversation under
// its folder, and a restore looks for it there). Without it, it is the
// folder the conversation records; a backend whose store records none
// (codex) needs --cwd.
func adoptFolder(backend, id string, locs []finder.Located, cwd string) (finder.Located, string, error) {
	if cwd != "" {
		var match []finder.Located
		for _, l := range locs {
			if l.BelongsTo(cwd) && (l.Cwd == "" || samePath(l.Cwd, cwd)) {
				match = append(match, l)
			}
		}
		if len(match) == 0 {
			return finder.Located{}, "", fmt.Errorf("conversation %s belongs to %s, not %s; run adopt from its folder (--cwd %s), or leave --cwd out",
				id, locFolders(locs), cwd, firstFolder(locs))
		}
		return newest(match), cwd, nil
	}
	known := ""
	for _, l := range locs {
		switch {
		case l.AnyFolder():
			return finder.Located{}, "", usagef("--cwd is required to adopt a %s conversation: %s resumes one in whatever folder it is started in", backend, backend)
		case l.Cwd == "" && len(locs) == 1:
			return finder.Located{}, "", usagef("--cwd is required: the %s store does not record the folder of conversation %s (%s)", backend, id, l.Path)
		case l.Cwd == "" || (known != "" && !samePath(known, l.Cwd)):
			return finder.Located{}, "", usagef("--cwd is required: conversation %s is stored for more than one folder (%s)", id, locFolders(locs))
		}
		known = l.Cwd
	}
	return newest(locs), known, nil
}

// adoptInUse refuses a conversation that looks in use elsewhere: its file
// was written within adoptActiveWindow, a live process has its ID in its
// arguments (claude --resume <id>, codex resume <id>, pi --session-id
// <id>; Linux /proc, macOS sysctl), or a Fastener session that is not over
// holds it (one that is lost or restore-failed could be restored next to
// it). A conversation open in an agent started without its ID on the
// command line and idle for longer than the window is not detected.
func adoptInUse(id string, loc finder.Located, now time.Time) error {
	const how = "close the other session, or pass --force"
	if age := now.Sub(loc.ModTime); age < adoptActiveWindow {
		// An agent also writes as it exits, so a session just closed reads
		// as in use for a while; the message says --force is then fine.
		return fmt.Errorf("conversation %s looks in use: %s was written %s ago (within %s); %s (safe once the other session is closed)",
			id, loc.Path, age.Truncate(time.Second), adoptActiveWindow, how)
	}
	procs, err := proc.FindArgv(id)
	if err != nil {
		return fmt.Errorf("cannot check whether a process is using conversation %s: %v; to adopt it anyway, pass --force", id, err)
	}
	if len(procs) > 0 {
		cmd := strings.Join(procs[0].Argv, " ")
		if len(cmd) > 120 {
			cmd = cmd[:117] + "..."
		}
		return fmt.Errorf("conversation %s looks in use: process %d (%s) has it in its arguments; %s", id, procs[0].PID, cmd, how)
	}
	names, err := session.Names()
	if err != nil {
		return err
	}
	hs := handshakeTimeout()
	for _, n := range names {
		v, err := session.Load(n, hs)
		if err != nil || v.Session.Kind != session.KindInteractive || v.State == session.StateExited {
			continue
		}
		if v.Status.ConversationID == id || (v.Plan != nil && v.Plan.SessionID == id) {
			return fmt.Errorf("conversation %s is already Fastener session %s's (state %s); stop or rm that session, or pass --force", id, n, v.State)
		}
	}
	return nil
}

func newest(locs []finder.Located) finder.Located {
	best := locs[0]
	for _, l := range locs[1:] {
		if l.ModTime.After(best.ModTime) {
			best = l
		}
	}
	return best
}

// locFolders names where the conversation is: its recorded folders, else
// its files.
func locFolders(locs []finder.Located) string {
	var out []string
	for _, l := range locs {
		if l.Cwd != "" {
			out = append(out, l.Cwd)
		} else {
			out = append(out, "the folder of "+l.Path)
		}
	}
	return strings.Join(out, ", ")
}

func firstFolder(locs []finder.Located) string {
	for _, l := range locs {
		if l.Cwd != "" {
			return l.Cwd
		}
	}
	return "DIR"
}

// samePath reports whether a and b name the same folder, as written or
// with symlinks resolved.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}
