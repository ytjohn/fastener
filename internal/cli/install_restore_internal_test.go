package cli

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// fakeHost installs a host whose systemctl calls are recorded, never run.
type fakeHost struct {
	calls  [][]string
	linger bool
	fail   string // a systemctl verb that fails
}

func (f *fakeHost) install(t *testing.T, goos string, systemdOK bool, exe string) {
	t.Helper()
	old := newInstallHost
	t.Cleanup(func() { newInstallHost = old })
	newInstallHost = func() installHost {
		return installHost{
			goos:      goos,
			available: func() (bool, error) { return systemdOK, nil },
			linger:    func() (bool, error) { return f.linger, nil },
			systemctl: func(args ...string) error {
				if goos == "darwin" || !systemdOK {
					t.Errorf("systemctl %v called on a host without systemd", args)
				}
				f.calls = append(f.calls, args)
				if len(args) > 0 && args[0] == f.fail {
					return errors.New("simulated systemctl failure")
				}
				return nil
			},
			executable: func() (string, error) { return exe, nil },
			uid:        501,
		}
	}
}

// installEnvDirs points HOME, XDG_CONFIG_HOME and the Fastener dirs at temp
// dirs, so nothing outside the test is touched.
func installEnvDirs(t *testing.T) (home, xdg, state, cfg string) {
	t.Helper()
	home, xdg, state, cfg = t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("FASTENER_STATE_DIR", state)
	t.Setenv("FASTENER_CONFIG_DIR", cfg)
	return
}

func runInstall(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	code = cmdInstallRestore(args, &o, &e)
	return o.String(), e.String(), code
}

// golden compares got (with the temp dirs replaced by $NAME) to
// testdata/install-restore/<name>.
func golden(t *testing.T, name, got string, dirs map[string]string) {
	t.Helper()
	for k, v := range dirs {
		got = strings.ReplaceAll(got, v, "$"+k)
	}
	path := filepath.Join("testdata", "install-restore", name)
	if *updateGolden {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

const testExe = "/opt/fastener/bin/fastener"

func TestInstallRestoreSystemd(t *testing.T) {
	_, xdg, state, cfg := installEnvDirs(t)
	dirs := map[string]string{"STATE": state, "CONFIG": cfg, "XDG_CONFIG_HOME": xdg}
	f := &fakeHost{}
	f.install(t, "linux", true, testExe)
	unitPath := filepath.Join(xdg, "systemd", "user", "fastener-restore.service")

	// --print writes nothing and calls nothing.
	out, _, code := runInstall(t, "--print")
	if code != 0 || len(f.calls) != 0 || exists(unitPath) {
		t.Fatalf("--print: exit %d, calls %v, unit written %v", code, f.calls, exists(unitPath))
	}
	golden(t, "print-systemd.txt", out, dirs)

	out, errOut, code := runInstall(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	b, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "fastener-restore.service", string(b), dirs)
	if want := [][]string{{"daemon-reload"}, {"enable", "fastener-restore.service"}}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	if !strings.Contains(errOut, "lingering is off") || !strings.Contains(out, "installed "+unitPath) {
		t.Errorf("stdout %q stderr %q", out, errOut)
	}

	// Idempotent: a re-run rewrites the unit (here, for a moved binary) and
	// enables it again; with linger on there is no warning.
	f.calls, f.linger = nil, true
	f.install(t, "linux", true, "/usr/local/bin/fastener")
	if _, errOut, code = runInstall(t); code != 0 || errOut != "" {
		t.Fatalf("re-run: exit %d, stderr %q", code, errOut)
	}
	if b, _ := os.ReadFile(unitPath); !strings.Contains(string(b), "ExecStart=/usr/local/bin/fastener restore\n") || strings.Contains(string(b), testExe) {
		t.Errorf("unit not updated:\n%s", b)
	}
	if len(f.calls) != 2 {
		t.Errorf("re-run calls = %v", f.calls)
	}

	// --uninstall --print, then --uninstall (twice).
	f.calls = nil
	if out, _, code = runInstall(t, "--uninstall", "--print"); code != 0 || len(f.calls) != 0 || !exists(unitPath) || !strings.Contains(out, "would remove: "+unitPath) {
		t.Errorf("--uninstall --print: exit %d, calls %v, out %q", code, f.calls, out)
	}
	if out, _, code = runInstall(t, "--uninstall"); code != 0 || exists(unitPath) || !strings.Contains(out, "removed "+unitPath) {
		t.Errorf("--uninstall: exit %d, out %q, unit still there %v", code, out, exists(unitPath))
	}
	if want := [][]string{{"disable", "fastener-restore.service"}, {"daemon-reload"}}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("uninstall calls = %v, want %v", f.calls, want)
	}
	f.calls = nil
	if out, _, code = runInstall(t, "--uninstall"); code != 0 || len(f.calls) != 0 || !strings.Contains(out, "not installed") {
		t.Errorf("second --uninstall: exit %d, calls %v, out %q", code, f.calls, out)
	}

	// A failing systemctl is an error.
	f.fail = "enable"
	if _, errOut, code = runInstall(t); code != 1 || !strings.Contains(errOut, "simulated systemctl failure") {
		t.Errorf("failing enable: exit %d, %q", code, errOut)
	}
}

// TestInstallRestoreNoFastenerEnv: without FASTENER_* set, the unit carries
// no Environment= lines, and the unit dir falls back to ~/.config.
func TestInstallRestoreNoFastenerEnv(t *testing.T) {
	home, _, _, _ := installEnvDirs(t)
	t.Setenv("FASTENER_STATE_DIR", "")
	t.Setenv("FASTENER_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "relative/ignored")
	f := &fakeHost{linger: true}
	f.install(t, "linux", true, testExe)
	if _, errOut, code := runInstall(t); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "fastener-restore.service"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Environment=") {
		t.Errorf("unexpected Environment= lines:\n%s", b)
	}
}

func TestInstallRestoreLaunchd(t *testing.T) {
	home, _, state, cfg := installEnvDirs(t)
	dirs := map[string]string{"STATE": state, "CONFIG": cfg, "HOME": home}
	f := &fakeHost{}
	f.install(t, "darwin", false, testExe)
	plist := filepath.Join(home, "Library", "LaunchAgents", "net.ytjohn.fastener.restore.plist")

	out, _, code := runInstall(t, "--print")
	if code != 0 || exists(plist) {
		t.Fatalf("--print: exit %d, plist written %v", code, exists(plist))
	}
	golden(t, "print-launchd.txt", out, dirs)

	out, errOut, code := runInstall(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	b, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "net.ytjohn.fastener.restore.plist", string(b), dirs)
	if !strings.Contains(out, "launchctl bootstrap gui/501 "+plist) {
		t.Errorf("stdout lacks the bootstrap command: %q", out)
	}
	// Idempotent.
	f.install(t, "darwin", false, "/usr/local/bin/fastener")
	if _, _, code = runInstall(t); code != 0 {
		t.Fatal("re-run failed")
	}
	if b, _ := os.ReadFile(plist); !strings.Contains(string(b), "<string>/usr/local/bin/fastener</string>") {
		t.Errorf("plist not updated:\n%s", b)
	}
	if out, _, code = runInstall(t, "--uninstall"); code != 0 || exists(plist) || !strings.Contains(out, "launchctl bootout gui/501/net.ytjohn.fastener.restore") {
		t.Errorf("--uninstall: exit %d, out %q", code, out)
	}
	if out, _, code = runInstall(t, "--uninstall"); code != 0 || !strings.Contains(out, "not installed") {
		t.Errorf("second --uninstall: exit %d, out %q", code, out)
	}
	if len(f.calls) != 0 {
		t.Errorf("systemctl called on macOS: %v", f.calls)
	}
}

func TestInstallRestoreCron(t *testing.T) {
	home, xdg, state, cfg := installEnvDirs(t)
	f := &fakeHost{}
	f.install(t, "linux", false, "/opt/my fastener/fastener")
	for _, args := range [][]string{nil, {"--print"}, {"--uninstall"}} {
		out, errOut, code := runInstall(t, args...)
		want := "@reboot FASTENER_CONFIG_DIR=" + cfg + " FASTENER_STATE_DIR=" + state + " '/opt/my fastener/fastener' restore\n"
		if code != 0 || out != want || !strings.Contains(errOut, "crontab -e") {
			t.Errorf("%v: exit %d, stdout %q (want %q), stderr %q", args, code, out, want, errOut)
		}
	}
	for _, d := range []string{home, xdg} {
		if ents, _ := os.ReadDir(d); len(ents) != 0 {
			t.Errorf("cron fallback wrote into %s: %v", d, ents)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("systemctl called: %v", f.calls)
	}
}

func TestInstallRestoreWarnsTransientBinary(t *testing.T) {
	installEnvDirs(t)
	f := &fakeHost{linger: true}
	f.install(t, "linux", true, filepath.Join(os.TempDir(), "go-build123", "b001", "exe", "fastener"))
	_, errOut, code := runInstall(t, "--print")
	if code != 0 || !strings.Contains(errOut, "install fastener somewhere permanent") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if _, _, code := runInstall(t, "extra"); code != 2 {
		t.Errorf("positional arg: exit %d, want 2", code)
	}
}

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/fastener": "/usr/bin/fastener",
		"/opt/my apps/f":    `"/opt/my apps/f"`,
		"/opt/100%/f":       "/opt/100%%/f",
		"/opt/$HOME/f":      "/opt/$$HOME/f",
		`/opt/a"b/f`:        `"/opt/a\"b/f"`,
	} {
		if got := systemdQuote(in, true); got != want {
			t.Errorf("systemdQuote(%q) = %q, want %q", in, got, want)
		}
	}
	if got := systemdQuote("A=$x", false); got != "A=$x" {
		t.Errorf("Environment value: %q", got)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
