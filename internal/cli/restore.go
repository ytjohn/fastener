package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/ytjohn/fastener/internal/config"
	"github.com/ytjohn/fastener/internal/proc"
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
	rsNotRemote      = "not_remote"      // a headless session is never rerun; interactive arrives in v0.4
	rsRunning        = "running"         // still supervised (or another restore just relaunched it)
	rsNotLost        = "not_lost"        // exited: it was stopped or ended, not lost to a reboot
	rsOrphans        = "orphans"         // marked processes of the old generation are alive
	rsOrphansUnknown = "orphans_unknown" // the leftover-process scan was not possible
	rsBusy           = "restore_in_progress"
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
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
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
	if reason, msg := notRestorable(v); reason != "" {
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
	if err != nil {
		r.Result, r.Reason, r.Message = restoreFailed, rsError, err.Error()
		return r, true
	}
	defer unlock()
	// Under the lock, look again: another restore may have relaunched it.
	if v, err = session.LoadFull(name, hs); err != nil {
		r.Result, r.Reason, r.Message = restoreFailed, rsError, err.Error()
		return r, true
	}
	if reason, msg := notRestorable(v); reason != "" {
		return skip(reason, msg)
	}
	if reason, msg := restoreOrphans(v); reason != "" {
		return skip(reason, msg)
	}
	return relaunch(v, stderr), true
}

// notRestorable is why v cannot be restored at all, "" if it can be tried.
// Lost and restore-failed sessions with restart=on-reboot that are remote
// bridges can be.
func notRestorable(v *session.View) (reason, msg string) {
	s := v.Session
	switch {
	case s.Restart != restartOnReboot:
		return rsNotOnReboot, fmt.Sprintf("restart is %q, not on-reboot", s.Restart)
	case s.Kind != session.KindRemote || !s.Remote:
		return rsNotRemote, fmt.Sprintf("a %s session is never restored (only remote bridges are, in this version)", kindOf(s))
	case v.Alive():
		return rsRunning, "the session is " + v.State
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
// every process carrying its marker, in any process group (stricter than the
// pgid scan stop and rm use, since a relaunch would run beside it). A previous
// boot's processes cannot have survived, so that case needs no scan.
func restoreOrphans(v *session.View) (reason, msg string) {
	if v.Plan == nil || v.Plan.Marker == "" {
		if v.Status.PGID > 0 || v.Status.Agent != nil {
			return rsOrphansUnknown, "no readable plan for the current generation, so leftover processes cannot be ruled out"
		}
		return "", "" // nothing was ever spawned
	}
	boot := v.Status.BootID
	if boot == "" && v.Launcher != nil {
		boot = v.Launcher.BootID
	}
	cur, err := proc.BootID()
	if err != nil {
		return rsOrphansUnknown, "cannot read the boot ID: " + err.Error()
	}
	if boot != "" && boot != cur {
		return "", ""
	}
	ids, err := proc.FindMarkedAnyGroup(v.Plan.Marker)
	if err != nil {
		return rsOrphansUnknown, "process scan failed: " + err.Error()
	}
	if len(ids) > 0 {
		return rsOrphans, fmt.Sprintf("%d process(es) of generation %d are still alive (%s); stop it first (fastener stop %s)",
			len(ids), v.Generation(), pidList(ids), v.Session.Name)
	}
	return "", ""
}

// restoreFailure is a reason and message for a restore that could not be done.
type restoreFailure struct{ reason, msg string }

func (e *restoreFailure) Error() string { return e.msg }

// prepareRestore re-renders the bridge's argv from the profile as it is now,
// using the intent in session.json (backend, permissions, name, environment).
// Nothing falls back: a profile without [remote], a level it no longer
// defines, or a binary that is gone is a failure with its reason.
func prepareRestore(s session.Session) (*prepared, *restoreFailure) {
	cfg, err := config.Load()
	if err != nil {
		return nil, &restoreFailure{rsConfig, err.Error()}
	}
	prof, err := profile.Load(s.Backend, config.BackendsDir())
	if err != nil {
		return nil, &restoreFailure{rsProfile, err.Error()}
	}
	r, err := prof.RenderRemote(profile.Request{Permissions: s.Permissions, Name: s.Name})
	switch {
	case errors.Is(err, profile.ErrNoRemote):
		return nil, &restoreFailure{rsNoRemote, err.Error()}
	case errors.Is(err, profile.ErrNoPermissionLevel):
		return nil, &restoreFailure{rsNoPermission, err.Error()}
	case err != nil:
		return nil, &restoreFailure{rsProfile, err.Error()}
	}
	p := &prepared{cfg: cfg, prof: prof, perms: s.Permissions, rendered: r, envBase: s.EnvBase, envFile: s.EnvFile}
	if s.EnvFile != "" {
		if p.fileEnv, err = runenv.ReadFile(s.EnvFile); err != nil {
			return nil, &restoreFailure{rsEnvFile, "env file: " + err.Error()}
		}
	}
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
	if _, f := prepareRestore(v.Session); f != nil {
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
	p, f := prepareRestore(v.Session)
	if f != nil {
		return failed(f.reason, f.msg, true)
	}
	l, err := selectFunc(p.cfg.Launcher, stderr)
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
	nv, err = waitRemoteURL(nv, p, p.cfg.RemoteWaitDuration())
	if err != nil {
		var rf *remoteFailure
		if !errors.As(err, &rf) {
			return failed(rsError, err.Error(), false) // a read error: the bridge may be running
		}
		reason, record := earlyOutcome(nv, rf.reason)
		return failed(reason, err.Error(), record)
	}
	r.Result, r.RemoteURL = restoreRestored, nv.Status.RemoteURL
	if r.RemoteURL == "" {
		fmt.Fprintf(stderr, "fastener: %s: no URL yet after %s; the bridge is still running (fastener inspect %s shows the URL once reported)\n",
			name, p.cfg.RemoteWaitDuration(), name)
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
