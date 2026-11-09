package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ytjohn/fastener/internal/config"
	"github.com/ytjohn/fastener/internal/session"
	"github.com/ytjohn/fastener/internal/systemd"
)

// The boot-time trigger `fastener install-restore` installs.
const (
	restoreUnit  = "fastener-restore.service"
	launchdLabel = "net.ytjohn.fastener.restore"
)

// installHost is everything install-restore asks of the machine. Tests
// replace it, so they never touch the real user manager or ~/Library.
type installHost struct {
	goos       string
	available  func() (bool, error) // a usable systemd user manager
	linger     func() (bool, error)
	systemctl  func(args ...string) error // systemctl --user <args>
	executable func() (string, error)     // this binary, symlinks resolved
	uid        int
}

var newInstallHost = func() installHost {
	return installHost{
		goos: runtime.GOOS, available: systemd.Available, linger: systemd.Linger,
		systemctl: userSystemctl, executable: resolvedExecutable, uid: os.Getuid(),
	}
}

func userSystemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func resolvedExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func cmdInstallRestore(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("install-restore", stderr)
	uninstall := fs.Bool("uninstall", false, "remove the trigger (disable and delete)")
	printOnly := fs.Bool("print", false, "print what would be installed; write nothing")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseFail(err)
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: fastener install-restore [--uninstall] [--print]")
		return exitUsage
	}
	h := newInstallHost()
	exe, err := h.executable()
	if err != nil {
		return fail(stderr, fmt.Errorf("cannot find this binary: %w", err))
	}
	if !*uninstall {
		if why := transientBinary(exe); why != "" {
			fmt.Fprintf(stderr, "fastener: warning: %s is %s; the trigger runs this path at boot, so install fastener somewhere permanent first\n", exe, why)
		}
	}
	env := restoreEnv()
	switch {
	case h.goos == "darwin":
		err = installLaunchd(h, exe, env, *uninstall, *printOnly, stdout)
	case availableOK(h):
		err = installSystemd(h, exe, env, *uninstall, *printOnly, stdout, stderr)
	default:
		installCron(exe, env, *uninstall, stdout, stderr)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func availableOK(h installHost) bool {
	ok, _ := h.available()
	return ok
}

// transientBinary says why exe looks like a build that will not be there at
// boot (a temp dir, a go build cache), "" otherwise.
func transientBinary(exe string) string {
	tmp := filepath.Clean(os.TempDir())
	switch {
	case strings.Contains(exe, string(filepath.Separator)+"go-build"):
		return "in a Go build cache (go run)"
	case strings.HasPrefix(exe, tmp+string(filepath.Separator)):
		return "in the temp directory " + tmp
	}
	if c := os.Getenv("GOCACHE"); c != "" && strings.HasPrefix(exe, filepath.Clean(c)+string(filepath.Separator)) {
		return "in the Go build cache"
	}
	return ""
}

// restoreEnv is the environment the trigger passes to `fastener restore`:
// the resolved, absolute state root and config directory, always, so the
// boot-time restore looks exactly where this command does whatever the
// trigger's own environment (a user manager, launchd, cron) would resolve.
func restoreEnv() [][2]string {
	var env [][2]string
	if d := config.Dir(); d != "" {
		env = append(env, [2]string{"FASTENER_CONFIG_DIR", d})
	}
	if d := session.Root(); d != "" {
		env = append(env, [2]string{"FASTENER_STATE_DIR", d})
	}
	return env
}

// systemdUnitDir is $XDG_CONFIG_HOME/systemd/user (XDG_CONFIG_HOME only when
// absolute), else ~/.config/systemd/user.
func systemdUnitDir() (string, error) {
	if v := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(v) {
		return filepath.Join(v, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("no HOME: cannot locate the systemd user unit directory")
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// systemdUnit renders fastener-restore.service.
func systemdUnit(exe string, env [][2]string) string {
	var b strings.Builder
	b.WriteString(`# Installed by "fastener install-restore"; "fastener install-restore --uninstall" removes it.
[Unit]
Description=Fastener: restore remote bridges after a reboot
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
`)
	fmt.Fprintf(&b, "ExecStart=%s restore\n", systemdQuote(exe, true))
	for _, kv := range env {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(kv[0]+"="+kv[1], false))
	}
	b.WriteString(`# Monitors started with the setsid launcher must outlive this one-shot unit.
KillMode=process

[Install]
WantedBy=default.target
`)
	return b.String()
}

// systemdQuote quotes s for a unit file: % is a specifier (%%), and in a
// command line $ is expanded ($$); a value with whitespace, quotes or
// backslashes is double-quoted with C-style escapes.
func systemdQuote(s string, command bool) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if command {
		s = strings.ReplaceAll(s, "$", "$$")
	}
	if !strings.ContainsAny(s, " \t\n\"'\\;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

func installSystemd(h installHost, exe string, env [][2]string, uninstall, printOnly bool, stdout, stderr io.Writer) error {
	dir, err := systemdUnitDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, restoreUnit)
	if uninstall {
		if printOnly {
			fmt.Fprintf(stdout, "would run: systemctl --user disable %s\nwould remove: %s\nwould run: systemctl --user daemon-reload\n", restoreUnit, path)
			return nil
		}
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stdout, "%s is not installed (%s does not exist)\n", restoreUnit, path)
			return nil
		}
		if err := h.systemctl("disable", restoreUnit); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := h.systemctl("daemon-reload"); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\n", path)
		return nil
	}
	unit := systemdUnit(exe, env)
	if printOnly {
		fmt.Fprintf(stdout, "# %s\n%s", path, unit)
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := session.WriteFileAtomic(path, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := h.systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := h.systemctl("enable", restoreUnit); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "installed %s\nenabled %s: runs %s restore when your user manager starts\n", path, restoreUnit, exe)
	if lingers, lerr := h.linger(); !lingers {
		why := "lingering is off"
		if lerr != nil {
			why = "lingering could not be checked (" + lerr.Error() + ")"
		}
		fmt.Fprintf(stderr, "fastener: warning: %s, so the user manager, and this unit, start only when you log in, not at boot (loginctl enable-linger fixes that)\n", why)
	}
	return nil
}

// launchdPlistPath is ~/Library/LaunchAgents/<label>.plist.
func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("no HOME: cannot locate ~/Library/LaunchAgents")
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// launchdPlist renders the LaunchAgent. AbandonProcessGroup keeps launchd
// from killing what restore started (setsid monitors are in their own
// groups anyway); its output goes to restore.log in the state directory.
func launchdPlist(exe string, env [][2]string, logPath string) string {
	x := func(s string) string {
		var b strings.Builder
		_ = xmlEscape(&b, s)
		return b.String()
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Installed by "fastener install-restore"; "fastener install-restore --uninstall" removes it. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + x(exe) + `</string>
		<string>restore</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>AbandonProcessGroup</key>
	<true/>
`)
	if len(env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, kv := range env {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", x(kv[0]), x(kv[1]))
		}
		b.WriteString("\t</dict>\n")
	}
	if logPath != "" {
		fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", x(logPath), x(logPath))
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlEscape(w io.StringWriter, s string) error {
	_, err := w.WriteString(strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s))
	return err
}

// installLaunchd writes or removes the LaunchAgent. It never runs launchctl:
// an agent in ~/Library/LaunchAgents is loaded at the next login by itself,
// and a CLI run over ssh may have no GUI domain to bootstrap into. It prints
// the command that loads (or unloads) it now.
func installLaunchd(h installHost, exe string, env [][2]string, uninstall, printOnly bool, stdout io.Writer) error {
	path, err := launchdPlistPath()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", h.uid)
	if uninstall {
		if printOnly {
			fmt.Fprintf(stdout, "would remove: %s\n", path)
			return nil
		}
		if err := os.Remove(path); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stdout, "%s is not installed (%s does not exist)\n", launchdLabel, path)
			return nil
		} else if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\nif it is loaded, unload it now with: launchctl bootout %s/%s\n", path, domain, launchdLabel)
		return nil
	}
	logPath := ""
	if r := session.Root(); r != "" {
		logPath = filepath.Join(r, "restore.log")
	}
	plist := launchdPlist(exe, env, logPath)
	if printOnly {
		fmt.Fprintf(stdout, "<!-- %s -->\n%s", path, plist)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if logPath != "" {
		_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
	}
	if err := session.WriteFileAtomic(path, []byte(plist), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "installed %s: launchd runs %s restore at each login\n", path, exe)
	fmt.Fprintf(stdout, "to load it now (it then runs restore once): launchctl bootstrap %s %s\n", domain, path)
	fmt.Fprintf(stdout, "(after changing an already loaded agent: launchctl bootout %s/%s first)\n", domain, launchdLabel)
	return nil
}

// cronLine is the @reboot line for crontab.
func cronLine(exe string, env [][2]string) string {
	var parts []string
	for _, kv := range env {
		parts = append(parts, kv[0]+"="+shellQuote(kv[1]))
	}
	parts = append(parts, shellQuote(exe), "restore")
	// cron turns an unescaped % in the command into a newline.
	return "@reboot " + strings.ReplaceAll(strings.Join(parts, " "), "%", `\%`)
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`;&|<>()*?[]#~%") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// installCron changes nothing: without systemd or launchd it prints the
// crontab line to add (or, with --uninstall, to remove).
func installCron(exe string, env [][2]string, uninstall bool, stdout, stderr io.Writer) {
	line := cronLine(exe, env)
	if uninstall {
		fmt.Fprintf(stderr, "fastener: no systemd user manager or launchd here; remove this line with crontab -e if you added it:\n")
	} else {
		fmt.Fprintf(stderr, "fastener: no systemd user manager or launchd here; add this line with crontab -e:\n")
	}
	fmt.Fprintln(stdout, line)
}
