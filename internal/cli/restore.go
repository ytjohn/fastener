package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/ytjohn/fastener/internal/config"
	"github.com/ytjohn/fastener/internal/finder"
	"github.com/ytjohn/fastener/internal/launcher"
	"github.com/ytjohn/fastener/internal/profile"
	"github.com/ytjohn/fastener/internal/runenv"
	"github.com/ytjohn/fastener/internal/session"
	"github.com/ytjohn/fastener/internal/version"
)

// Results of `fastener restore` (`--json` "result").
const (
	restoreRestored     = "restored"
	restoreSkipped      = "skipped"
	restoreFailed       = "failed"
	restoreWouldRestore = "would-restore" // --dry-run
	restoreWouldFail    = "would-fail"    // --dry-run
)

// Reasons `fastener restore` reports. A failed restore records its reason in
// status.json (restore_reason) with state restore-failed, except not_found
// and lost, where there is nothing (or nothing safe) to record.
const (
	// skipped
	rsNotOnReboot    = "not_on_reboot"   // restart is not on-reboot
	rsNotRemote      = "not_remote"      // a headless session is never rerun (only bridges and interactive sessions are)
	rsRunning        = "running"         // still supervised (or another restore just relaunched it)
	rsNotLost        = "not_lost"        // exited: it was stopped or ended, not lost to a reboot
	rsOrphans        = "orphans"         // marked processes of the old generation are alive
	rsOrphansUnknown = "orphans_unknown" // the leftover-process scan was not possible
	rsBusy           = "restore_in_progress"
	rsStopped        = "stopped" // the relaunched generation was stopped (fastener stop) while restore waited
	// failed
	rsNotFound     = "not_found"
	rsConfig       = "config"
	rsProfile      = "profile"
	rsNoRemote     = "no_remote"
	rsNoPermission = "no_permission_level"
	rsBinary       = "binary_not_found"
	rsEnvFile      = "env_file"
	rsLaunch       = "launch_failed"
	rsUntrusted    = reasonUntrusted // untrusted_workspace
	rsExited       = reasonExited    // the bridge ended before reporting its URL
	rsLost         = "lost"          // the relaunched monitor died; left lost for stop/rm
	rsError        = "error"         // an I/O error while relaunching
	// interactive sessions
	rsNoResume       = "no_resume"             // the profile cannot resume an interactive session
	rsNoConversation = "no_conversation"       // nothing to resume: the agent never wrote a conversation (or it is ambiguous)
	rsCandidate      = "unconfirmed_candidate" // only a heuristic candidate; needs --accept-candidate
)

type restoreOut struct {
	Name       string `json:"name"`
	Result     string `json:"result"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	Generation int    `json:"generation,omitempty"`
	RemoteURL  string `json:"remote_url,omitempty"`
}

func cmdRestore(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("restore [name...]", stderr)
	dry := fs.Bool("dry-run", false, "show what would be restored; change nothing")
	asJSON := fs.Bool("json", false, "print JSON")
	accept := fs.Bool("accept-candidate", false, "for the named interactive sessions: resume a conversation_candidate (a heuristic finder's guess) as their conversation")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	if *accept && len(pos) == 0 {
		return fail(stderr, usagef("--accept-candidate needs the sessions named: it confirms a guess, which is never done for every session at once"))
	}
	acceptCandidate = *accept
	for _, n := range pos {
		if !session.ValidName(n) {
			return fail(stderr, usagef("invalid session name %q", n))
		}
	}
	if session.SessionsDir() == "" {
		return fail(stderr, session.ErrNoStateDir)
	}
	explicit := len(pos) > 0
	names := pos
	if !explicit {
		if names, err = session.Names(); err != nil {
			return fail(stderr, err)
		}
	}
	results := []restoreOut{}
	code := exitOK
	for _, n := range names {
		r, ok := restoreOne(n, explicit, *dry, stderr)
		if !ok {
			continue // not a candidate
		}
		if r.Result == restoreFailed || r.Result == restoreWouldFail {
			code = exitFail
		}
		results = append(results, r)
	}
	if *asJSON {
		writeJSON(stdout, results)
		return code
	}
	for _, r := range results {
		line := r.Name + "  " + r.Result
		if r.Generation > 0 {
			line += fmt.Sprintf("  generation %d", r.Generation)
		}
		if r.RemoteURL != "" {
			line += "  " + r.RemoteURL
		}
		if r.Message != "" {
			line += ": " + r.Message
		}
		fmt.Fprintln(stdout, line)
	}
	return code
}

// acceptCandidate is restore's --accept-candidate: resume an interactive
// session's conversation_candidate. Only ever set for named sessions.
var acceptCandidate bool

// restoreLockWait bounds how long restore waits for another restore of the
// same session: that one's handshake plus its wait for the URL, with margin.
func restoreLockWait(cfg config.Config) time.Duration {
	return 2*cfg.Handshake() + cfg.RemoteWaitDuration() + 30*time.Second
}

// restoreOne restores one session. ok is false when the session is not a
// candidate and was not named, so it is left out of the output.
func restoreOne(name string, explicit, dry bool, stderr io.Writer) (r restoreOut, ok bool) {
	r = restoreOut{Name: name}
	skip := func(reason, msg string) (restoreOut, bool) {
		r.Result, r.Reason, r.Message = restoreSkipped, reason, msg
		return r, true
	}
	hs := handshakeTimeout()
	v, err := session.Load(name, hs)
	if err != nil {
		if !explicit {
			fmt.Fprintf(stderr, "fastener: skipping %s: %v\n", name, err)
			return r, false
		}
		r.Result, r.Message = restoreFailed, err.Error()
		if errors.Is(err, session.ErrNotFound) {
			r.Reason = rsNotFound
		} else {
			r.Reason = rsError
		}
		return r, true
	}
	if !explicit && (v.Session.Restart != restartOnReboot || v.State != session.StateLost) {
		return r, false // only lost on-reboot sessions are candidates
	}
	if reason, msg := notRestorable(v, explicit); reason != "" {
		return skip(reason, msg)
	}
	if dry {
		return dryRestore(v)
	}
	cfg, _ := config.Load() // only for the lock wait; prepareRestore reports a bad config
	unlock, err := session.LockRestore(v.Dir, restoreLockWait(cfg))
	if errors.Is(err, session.ErrRestoreBusy) {
		return skip(rsBusy, err.Error())
	}
	if errors.Is(err, session.ErrNotFound) {
		return skip(rsNotFound, "the session was removed while restore was waiting for it")
	}
	if err != nil {
		r.Result, r.Reason, r.Message = restoreFailed, rsError, err.Error()
		return r, true
	}
	defer unlock()
	// Under the lock, look again: another restore may have relaunched it.
	if v, err = session.LoadFull(name, hs); errors.Is(err, session.ErrNotFound) {
		// Removed (rm) while this restore waited for the lock: nothing to do.
		return skip(rsNotFound, "the session was removed while restore was waiting for it")
	} else if err != nil {
		r.Result, r.Reason, r.Message = restoreFailed, rsError, err.Error()
		return r, true
	}
	if reason, msg := notRestorable(v, explicit); reason != "" {
		return skip(reason, msg)
	}
	if reason, msg := restoreOrphans(v); reason != "" {
		return skip(reason, msg)
	}
	return relaunch(v, stderr), true
}

// notRestorable is why v cannot be restored at all, "" if it can be tried.
// Lost and restore-failed sessions with restart=on-reboot that are remote
// bridges or interactive sessions can be, and, when named (explicit), one
// that `stop` finalized from lost (after cleaning up the orphans that made
// restore skip it).
func notRestorable(v *session.View, explicit bool) (reason, msg string) {
	s := v.Session
	switch {
	case s.Restart != restartOnReboot:
		return rsNotOnReboot, fmt.Sprintf("restart is %q, not on-reboot", s.Restart)
	case !(s.Kind == session.KindRemote && s.Remote) && s.Kind != session.KindInteractive:
		return rsNotRemote, fmt.Sprintf("a %s session is never restored (only remote bridges and interactive sessions are)", kindOf(s))
	case v.Alive():
		return rsRunning, "the session is " + v.State
	case v.State == session.StateExited && explicit && v.FinalizedFromLost():
		return "", "" // `stop` cleaned up a lost session's orphans; named, it may be restored
	case v.State == session.StateExited:
		return rsNotLost, "the session ended (ended_by=" + v.Status.EndedBy + "); restore only relaunches lost sessions"
	case v.State != session.StateLost && v.State != session.StateRestoreFailed:
		return rsNotLost, "the session is " + v.State
	}
	return "", ""
}

func kindOf(s session.Session) string {
	if s.Kind == "" {
		return session.KindHeadless
	}
	return s.Kind
}

// restoreOrphans checks that nothing of the old generation is still alive:
// every process carrying its marker, in any process group
// (session.View.MarkedAnyGroup: a previous boot's processes cannot have
// survived, so that case needs no scan). stop uses the same scan for a lost
// session, so the way out the message names clears exactly what blocks.
func restoreOrphans(v *session.View) (reason, msg string) {
	ids, err := v.MarkedAnyGroup()
	if err != nil {
		return rsOrphansUnknown, err.Error() + ", so leftover processes cannot be ruled out"
	}
	if len(ids) > 0 {
		n := v.Session.Name
		return rsOrphans, fmt.Sprintf("%d process(es) of generation %d are still alive (%s); stop it first (fastener stop %s), then fastener restore %s",
			len(ids), v.Generation(), pidList(ids), n, n)
	}
	return "", ""
}

// restoreSelect picks the launcher for a relaunch (launcher.SelectRestore,
// which accepts a user manager that is still starting at boot). A variable so
// tests in this package can substitute one.
var restoreSelect = launcher.SelectRestore

// restoreLauncherPref is the launcher a relaunch uses: the kind the previous
// generation's launcher record names, so a bridge keeps the supervision it
// was started with whatever the config says now. Without a record (or one
// naming an unknown kind) it is the config's launcher, else auto, with a note.
func restoreLauncherPref(v *session.View, cfg config.Config, stderr io.Writer) string {
	if l := v.Launcher; l != nil && (l.Launcher == "setsid" || l.Launcher == "systemd") {
		return l.Launcher
	}
	pref := cfg.Launcher
	if pref == "" {
		pref = "auto"
	}
	fmt.Fprintf(stderr, "fastener: note: %s: generation %d has no launcher record; using launcher %s (config, else auto)\n",
		v.Session.Name, v.Generation(), pref)
	return pref
}

// restoreFailure is a reason and message for a restore that could not be done.
type restoreFailure struct{ reason, msg string }

func (e *restoreFailure) Error() string { return e.msg }

// prepareRestore re-renders the bridge's argv from the profile as it is now,
// using the intent in session.json (backend, permissions, name, environment).
// Nothing falls back: a profile without [remote], a level it no longer
// defines, or a binary that is gone is a failure with its reason.
//
// An interactive session is rendered from [interactive] (and
// [interactive.remote] when it is remote) resuming its conversation (see
// restoreConversation) through the profile's [resume] or [interactive]
// session_id; v is needed for that, and may be nil for a bridge.
func prepareRestore(v *session.View) (*prepared, *restoreFailure) {
	s := v.Session
	cfg, err := config.Load()
	if err != nil {
		return nil, &restoreFailure{rsConfig, err.Error()}
	}
	prof, err := profile.Load(s.Backend, config.BackendsDir())
	if err != nil {
		return nil, &restoreFailure{rsProfile, err.Error()}
	}
	interactive := s.Kind == session.KindInteractive
	var r profile.Rendered
	var fileEnv []string
	if s.EnvFile != "" {
		if fileEnv, err = runenv.ReadFile(s.EnvFile); err != nil {
			return nil, &restoreFailure{rsEnvFile, "env file: " + err.Error()}
		}
	}
	if interactive {
		if !prof.CanResumeInteractive() {
			return nil, &restoreFailure{rsNoResume, fmt.Sprintf("backend %s cannot resume an interactive session (its profile has neither [resume] nor [interactive] session_id)", s.Backend)}
		}
		id, f := restoreConversation(v, runenv.Merge(s.EnvBase, planEnv(v), fileEnv, s.Env))
		if f != nil {
			return nil, f
		}
		r, err = prof.RenderInteractive(profile.Request{Permissions: s.Permissions, Model: s.Model, Name: s.Name, ResumeID: id}, s.Remote)
	} else {
		r, err = prof.RenderRemote(profile.Request{Permissions: s.Permissions, Name: s.Name})
	}
	switch {
	case errors.Is(err, profile.ErrNoRemote), errors.Is(err, profile.ErrNoInteractive):
		return nil, &restoreFailure{rsNoRemote, err.Error()}
	case errors.Is(err, profile.ErrNoResume):
		return nil, &restoreFailure{rsNoResume, err.Error()}
	case errors.Is(err, profile.ErrNoPermissionLevel):
		return nil, &restoreFailure{rsNoPermission, err.Error()}
	case err != nil:
		return nil, &restoreFailure{rsProfile, err.Error()}
	}
	p := &prepared{cfg: cfg, prof: prof, perms: s.Permissions, rendered: r, envBase: s.EnvBase, envFile: s.EnvFile,
		fileEnv: fileEnv, interactive: interactive}
	p.argv0, err = runenv.LookPath(prof.Command, runenv.Get(p.mergedEnv(s.Env), "PATH"))
	if err != nil {
		return nil, &restoreFailure{rsBinary, err.Error()}
	}
	return p, nil
}

// dryRestore reports what restore would do, changing nothing.
func dryRestore(v *session.View) (restoreOut, bool) {
	r := restoreOut{Name: v.Session.Name}
	if reason, msg := restoreOrphans(v); reason != "" {
		r.Result, r.Reason, r.Message = restoreSkipped, reason, msg
		return r, true
	}
	if _, f := prepareRestore(v); f != nil {
		r.Result, r.Reason, r.Message = restoreWouldFail, f.reason, f.msg
		return r, true
	}
	r.Result, r.Generation = restoreWouldRestore, v.Generation()+1
	return r, true
}

// relaunch starts the next generation of v, which is lost (or restore-failed)
// with nothing of it alive; the caller holds the restore lock. session.json
// is never written.
func relaunch(v *session.View, stderr io.Writer) restoreOut {
	name, d := v.Session.Name, v.Dir
	r := restoreOut{Name: name}
	failed := func(reason, msg string, record bool) restoreOut {
		r.Result, r.Reason, r.Message = restoreFailed, reason, msg
		if record {
			if err := session.MarkRestoreFailed(d, reason, msg); err != nil {
				fmt.Fprintf(stderr, "fastener: warning: %s: cannot record restore-failed: %v\n", name, err)
			}
		}
		return r
	}
	p, f := prepareRestore(v)
	if f != nil {
		return failed(f.reason, f.msg, true)
	}
	l, err := restoreSelect(restoreLauncherPref(v, p.cfg, stderr), stderr)
	if err != nil {
		return failed(rsLaunch, err.Error(), true)
	}
	old := v.Generation()
	gen := old + 1
	r.Generation = gen
	now := time.Now().UTC()
	plan := session.Plan{
		SchemaVersion: session.SchemaVersion, Generation: gen,
		Argv: append([]string{p.argv0}, p.rendered.Args...), PromptMode: p.rendered.PromptMode,
		Parser: p.rendered.Parser, DedupeFrames: p.rendered.DedupeFrames, ProfileSource: p.prof.Source, ProfileSHA256: p.prof.SHA256,
		Marker: session.NewMarker(name, gen), FastenerVersion: version.String(), PlannedAt: now,
		URLPattern: p.rendered.URLPattern, Env: p.rendered.Env,
		SessionID: p.rendered.SessionID, Finder: p.rendered.Finder,
	}
	if p.interactive {
		// The same terminal size as before; attaching resizes it anyway.
		size := session.PTYPlan{Rows: session.DefaultPTYRows, Cols: session.DefaultPTYCols}
		if v.Plan != nil && v.Plan.PTY != nil {
			size = *v.Plan.PTY
		}
		plan.PTY = &size
	}
	// Rotate the old generation's logs, drop a leftover stop request (the
	// monitor would ignore it, being for another generation, but a later
	// stop must not find it), then the new plan and a starting status.
	if err := session.RotateLogs(d, old); err != nil {
		return failed(rsError, "rotate logs: "+err.Error(), true)
	}
	if err := os.Remove(d.StopPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return failed(rsError, err.Error(), true)
	}
	if err := session.WriteJSON(d.PlanPath(gen), plan); err != nil {
		return failed(rsError, err.Error(), true)
	}
	if err := session.WriteJSON(d.StatusPath(), session.Status{
		SchemaVersion: session.SchemaVersion, State: session.StateStarting, Generation: gen, UpdatedAt: now,
	}); err != nil {
		return failed(rsError, err.Error(), true)
	}
	nv, err := startGeneration(d, name, gen, l, p.cfg.Handshake(), stderr)
	if err != nil {
		reason, record := earlyOutcome(nv, rsLaunch)
		return failed(reason, err.Error(), record)
	}
	if !v.Session.Remote {
		// An interactive session without remote control: up is up.
		r.Result = restoreRestored
		if nv != nil && nv.State == session.StateExited {
			reason, record := earlyOutcome(nv, rsExited)
			return failed(reason, fmt.Sprintf("generation %d ended at once (ended_by=%s)", gen, nv.Status.EndedBy), record)
		}
		return r
	}
	nv, err = waitRemoteURL(nv, p, p.cfg.RemoteWaitDuration())
	if err != nil {
		var rf *remoteFailure
		if !errors.As(err, &rf) {
			return failed(rsError, err.Error(), false) // a read error: the bridge may be running
		}
		if nv != nil && nv.State == session.StateExited && nv.Status.EndedBy == session.EndedByStop {
			// Someone stopped the new generation: that is their decision, not
			// a failed restore, so it is left exited.
			r.Result, r.Reason = restoreSkipped, rsStopped
			r.Message = fmt.Sprintf("generation %d was stopped before it reported its URL", gen)
			return r
		}
		reason, record := earlyOutcome(nv, rf.reason)
		return failed(reason, err.Error(), record)
	}
	r.Result, r.RemoteURL = restoreRestored, nv.Status.RemoteURL
	if r.RemoteURL == "" {
		fmt.Fprintf(stderr, "fastener: %s: no URL yet after %s; the %s is still running (fastener inspect %s shows the URL once reported)\n",
			name, p.cfg.RemoteWaitDuration(), remoteNoun(p), name)
	}
	return r
}

// earlyOutcome is the reason, and whether to record restore-failed, for a
// relaunch that did not come up. An exited generation is recorded with the
// failure's reason. A lost one (its monitor died, perhaps leaving processes)
// is left lost, so stop and rm can find what survived. A view still alive (a
// handshake that timed out and could not stop it), or none, is not recorded.
func earlyOutcome(nv *session.View, reason string) (string, bool) {
	switch {
	case nv != nil && nv.State == session.StateExited:
		return reason, true
	case nv != nil && nv.State == session.StateLost:
		return rsLost, false
	}
	return reason, false
}

// planEnv is the previous plan's profile env ([interactive].env), if any.
func planEnv(v *session.View) []string {
	if v.Plan == nil {
		return nil
	}
	return v.Plan.Env
}

// restoreConversation picks the conversation an interactive session resumes
// (design §8.4), from its last generation:
//   - status conversation_id: exact, resumed;
//   - else, the ID assigned at launch (plan session_id), if the finder now
//     finds the agent wrote it (the monitor may have died before it looked):
//     exact, resumed;
//   - else a candidate (status conversation_candidate, or the finder's
//     heuristic answer now) only with --accept-candidate;
//   - else there is nothing to resume, and the restore fails: it never
//     starts a fresh conversation in its place.
func restoreConversation(v *session.View, env []string) (string, *restoreFailure) {
	st, plan := v.Status, v.Plan
	if st.ConversationID != "" {
		return st.ConversationID, nil
	}
	var find finder.Finder
	if plan != nil && plan.Finder != "" {
		find, _ = finder.Get(plan.Finder)
	}
	q := finder.Query{Cwd: v.Session.Cwd, Env: env}
	if st.StartedAt != nil {
		q.StartedAt = *st.StartedAt
	}
	if plan != nil && plan.SessionID != "" {
		q.SessionID = plan.SessionID
		if find != nil {
			if id, err := find(q); err == nil && id != "" {
				return id, nil
			}
		}
		return "", &restoreFailure{rsNoConversation, fmt.Sprintf(
			"session %s has no conversation to resume: the agent never wrote conversation %s (nothing was sent to it?); restore never starts a fresh one",
			v.Session.Name, plan.SessionID)}
	}
	if strings.HasPrefix(st.ConversationError, finder.ErrAmbiguous.Error()) {
		// The monitor saw more than one candidate: that stays so.
		return "", &restoreFailure{rsNoConversation, fmt.Sprintf("session %s: %s; restore cannot tell which to resume", v.Session.Name, st.ConversationError)}
	}
	cand := st.ConversationCandidate
	if cand == "" && find != nil {
		// Nothing recorded (the monitor may have died before it looked): ask now.
		if id, err := find(q); err == nil {
			cand = id
		} else if errors.Is(err, finder.ErrAmbiguous) {
			return "", &restoreFailure{rsNoConversation, fmt.Sprintf("session %s: %v; restore cannot tell which to resume", v.Session.Name, err)}
		}
	}
	switch {
	case cand != "" && acceptCandidate:
		return cand, nil
	case cand != "":
		n := v.Session.Name
		return "", &restoreFailure{rsCandidate, fmt.Sprintf(
			"session %s's conversation is only a guess (%s, from finder %s): another conversation in the same folder could be it; to resume it anyway: fastener restore --accept-candidate %s",
			n, cand, plan.Finder, n)}
	}
	return "", &restoreFailure{rsNoConversation, fmt.Sprintf("session %s has no conversation to resume; restore never starts a fresh one", v.Session.Name)}
}
