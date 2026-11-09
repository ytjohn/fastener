package claudetrust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// isolate points CLAUDE_CONFIG_DIR, FASTENER_CONFIG_DIR and HOME at temp
// dirs and asserts that Claude's config path is inside them, so no test can
// touch the real ~/.claude.json. It returns the Claude config path (not
// created) and the Fastener config dir.
func isolate(t *testing.T) (claudeCfg, fastenerDir string) {
	t.Helper()
	realHome, _ := os.UserHomeDir()
	base := realDir(t, t.TempDir())
	for _, d := range []string{"claude", "fastener", "home"} {
		if err := os.Mkdir(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(base, "claude"))
	t.Setenv("FASTENER_CONFIG_DIR", filepath.Join(base, "fastener"))
	t.Setenv("HOME", filepath.Join(base, "home"))
	p, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, base+string(filepath.Separator)) {
		t.Fatalf("Claude config %s is outside the test dir %s", p, base)
	}
	if realHome != "" && p == filepath.Join(realHome, ".claude.json") {
		t.Fatalf("Claude config is the real %s", p)
	}
	return p, filepath.Join(base, "fastener")
}

func realDir(t *testing.T, d string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func opts(cfg, fdir string) Options {
	return Options{ConfigPath: cfg, BackupDir: filepath.Join(fdir, "backups"), LockTimeout: 2 * time.Second}
}

// fixture is a Claude config in Claude's own style (JSON.stringify(v, null, 2)).
const fixture = `{
  "numStartups": 11847,
  "installMethod": "native",
  "autoUpdates": false,
  "firstStartTime": "2025-06-01T12:00:00.000Z",
  "userID": "abc\"def\\ghi\u0001",
  "lastSeen": 1794150081977,
  "hugeInt": 123456789012345678901234567890,
  "ratio": 0.30000000000000004,
  "expo": 1e+21,
  "neg": -0.0,
  "unicode": "café – 日本  ",
  "nothing": null,
  "emptyObj": {},
  "emptyArr": [],
  "tipsHistory": {
    "a": 1,
    "b": [
      1,
      2.5,
      {
        "deep": true
      }
    ]
  },
  "projects": {
    "/work/existing": {
      "allowedTools": [],
      "mcpServers": {},
      "hasTrustDialogAccepted": false,
      "lastCost": 0.123456789,
      "lastSessionId": "9f3c",
      "exampleFiles": [
        "a.go",
        "b.go"
      ]
    },
    "/work/trusted": {
      "hasTrustDialogAccepted": true
    }
  },
  "zLast": "end"
}`

func writeFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func loose(t *testing.T, b []byte) map[string]any {
	t.Helper()
	v, err := decodeLoose(b)
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func TestRoundTripIsByteIdentical(t *testing.T) {
	v, err := decodeOrdered([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if got := encodeStringify(v); string(got) != fixture {
		t.Fatalf("round trip changed the bytes:\n%s", got)
	}
}

func TestDecodeRejectsTrailingData(t *testing.T) {
	if _, err := decodeOrdered([]byte(`{"a":1} {}`)); err == nil {
		t.Fatal("trailing data accepted")
	}
	if _, err := decodeOrdered([]byte(`{"a":`)); err == nil {
		t.Fatal("truncated JSON accepted")
	}
}

func TestTrustPreservesEverythingElse(t *testing.T) {
	for _, key := range []string{"/work/existing", "/work/new"} {
		t.Run(key, func(t *testing.T) {
			cfg, fdir := isolate(t)
			writeFixture(t, cfg, fixture, 0o640)
			res, err := Trust(key, opts(cfg, fdir))
			if err != nil {
				t.Fatal(err)
			}
			if res.AlreadyTrusted || res.Backup == "" {
				t.Fatalf("result %+v", res)
			}
			got, _ := os.ReadFile(cfg)
			want := loose(t, []byte(fixture))
			projects := want["projects"].(map[string]any)
			if e, ok := projects[key].(map[string]any); ok {
				e[TrustField] = true
			} else {
				projects[key] = map[string]any{TrustField: true}
			}
			if g := loose(t, got); !reflect.DeepEqual(g, want) {
				t.Fatalf("content differs beyond the trust field:\n%s", got)
			}
			// Exact numbers and escapes survive textually, too.
			for _, s := range []string{`1794150081977`, `123456789012345678901234567890`, `0.30000000000000004`, `1e+21`, `-0.0`, `"abc\"def\\ghi\u0001"`, `"café – 日本  "`, `0.123456789`} {
				if !bytes.Contains(got, []byte(s)) {
					t.Errorf("lost %s", s)
				}
			}
			// Only the trust line differs from the original text.
			if key == "/work/existing" {
				exp := strings.Replace(fixture, `"hasTrustDialogAccepted": false,`, `"hasTrustDialogAccepted": true,`, 1)
				if string(got) != exp {
					t.Fatalf("not a one-field change:\n%s", got)
				}
			} else {
				exp := strings.Replace(fixture, "      \"hasTrustDialogAccepted\": true\n    }\n  },",
					"      \"hasTrustDialogAccepted\": true\n    },\n    \"/work/new\": {\n      \"hasTrustDialogAccepted\": true\n    }\n  },", 1)
				if string(got) != exp {
					t.Fatalf("unexpected layout:\n%s", got)
				}
			}
			fi, _ := os.Stat(cfg)
			if fi.Mode().Perm() != 0o640 {
				t.Fatalf("mode %v, want 0640", fi.Mode().Perm())
			}
			assertNoTemps(t, filepath.Dir(cfg))
			if _, err := os.Stat(cfg + ".lock"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("lock left behind")
			}
			b, _ := os.ReadFile(res.Backup)
			if string(b) != fixture {
				t.Fatal("backup is not the original")
			}
			if !strings.HasPrefix(res.Backup, filepath.Join(fdir, "backups")+"/") {
				t.Fatalf("backup %s not in the fastener config dir", res.Backup)
			}
		})
	}
}

func TestTrustCreatesProjects(t *testing.T) {
	cfg, fdir := isolate(t)
	writeFixture(t, cfg, "{\n  \"numStartups\": 1\n}\n", 0o600)
	if _, err := Trust("/w", opts(cfg, fdir)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cfg)
	want := "{\n  \"numStartups\": 1,\n  \"projects\": {\n    \"/w\": {\n      \"hasTrustDialogAccepted\": true\n    }\n  }\n}\n"
	if string(got) != want {
		t.Fatalf("got %q", got)
	}
}

func TestTrustRefusesOddShapes(t *testing.T) {
	for _, c := range []string{`[]`, `{"projects": []}`, `{"projects": {"/w": 3}}`, `{not json`} {
		cfg, fdir := isolate(t)
		writeFixture(t, cfg, c, 0o600)
		if _, err := Trust("/w", opts(cfg, fdir)); err == nil {
			t.Errorf("%s: accepted", c)
		}
		if b, _ := os.ReadFile(cfg); string(b) != c {
			t.Errorf("%s: file changed", c)
		}
	}
}

func TestAlreadyTrustedWritesNothing(t *testing.T) {
	cfg, fdir := isolate(t)
	writeFixture(t, cfg, fixture, 0o600)
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(cfg, old, old); err != nil {
		t.Fatal(err)
	}
	res, err := Trust("/work/trusted", opts(cfg, fdir))
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyTrusted || res.Backup != "" {
		t.Fatalf("result %+v", res)
	}
	fi, _ := os.Stat(cfg)
	if !fi.ModTime().Equal(old) {
		t.Fatalf("mtime changed: %v", fi.ModTime())
	}
	if _, err := os.Stat(filepath.Join(fdir, "backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("backup made without a write")
	}
}

func TestMissingClaudeConfig(t *testing.T) {
	cfg, fdir := isolate(t)
	_, err := Trust("/w", opts(cfg, fdir))
	if !errors.Is(err, ErrNoClaudeConfig) || !strings.Contains(err.Error(), "run claude once first") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("config created")
	}
}

func TestSymlinkedConfigReplacesTarget(t *testing.T) {
	cfg, fdir := isolate(t)
	target := filepath.Join(filepath.Dir(cfg), "real.json")
	writeFixture(t, target, fixture, 0o600)
	if err := os.Symlink(target, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := Trust("/w", opts(cfg, fdir)); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(cfg); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
	if err := Verify(target, "/w"); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRetention(t *testing.T) {
	cfg, fdir := isolate(t)
	writeFixture(t, cfg, fixture, 0o600)
	for i := 0; i < 8; i++ {
		if _, err := Trust(fmt.Sprintf("/w/%d", i), opts(cfg, fdir)); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(fdir, "backups"))
	if len(ents) != KeepBackups {
		t.Fatalf("%d backups, want %d", len(ents), KeepBackups)
	}
	// The newest backup holds the state before the last write: /w/6 but not /w/7.
	last := filepath.Join(fdir, "backups", ents[len(ents)-1].Name())
	if Verify(last, "/w/6") != nil || Verify(last, "/w/7") == nil {
		t.Fatal("newest backup is not the latest pre-write state")
	}
	// The oldest kept is from write 3 (writes 0-2 were pruned).
	first := filepath.Join(fdir, "backups", ents[0].Name())
	if Verify(first, "/w/2") != nil || Verify(first, "/w/3") == nil {
		t.Fatal("oldest kept backup is wrong")
	}
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

// --- locking ---

func TestWaitsForFreshLock(t *testing.T) {
	cfg, fdir := isolate(t)
	writeFixture(t, cfg, fixture, 0o600)
	lock := cfg + ".lock"
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	released := make(chan time.Time, 1)
	go func() {
		time.Sleep(400 * time.Millisecond)
		released <- time.Now()
		_ = os.Remove(lock)
	}()
	start := time.Now()
	if _, err := Trust("/w", opts(cfg, fdir)); err != nil {
		t.Fatal(err)
	}
	if rel := <-released; time.Since(start) < 400*time.Millisecond || time.Now().Before(rel) {
		t.Fatal("did not wait for the lock")
	}
	if err := Verify(cfg, "/w"); err != nil {
		t.Fatal(err)
	}
}

func TestBreaksStaleLock(t *testing.T) {
	cfg, fdir := isolate(t)
	writeFixture(t, cfg, fixture, 0o600)
	lock := cfg + ".lock"
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := Trust("/w", opts(cfg, fdir)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("waited on a stale lock")
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lock left")
	}
}

func TestFreshLockTimesOut(t *testing.T) {
	cfg, fdir := isolate(t)
	writeFixture(t, cfg, fixture, 0o600)
	lock := cfg + ".lock"
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	// A holder that keeps refreshing the lock, as Claude does.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				now := time.Now()
				_ = os.Chtimes(lock, now, now)
			}
		}
	}()
	o := opts(cfg, fdir)
	o.LockTimeout, o.LockStale = 700*time.Millisecond, 500*time.Millisecond
	_, err := Trust("/w", o)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err %v", err)
	}
	if b, _ := os.ReadFile(cfg); string(b) != fixture {
		t.Fatal("file changed")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("removed a fresh lock")
	}
}

func TestLockRefreshedWhileHeld(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "x.lock")
	l, err := acquireLock(lock, time.Second, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(lock, old, old)
	time.Sleep(300 * time.Millisecond)
	fi, _ := os.Stat(lock)
	if time.Since(fi.ModTime()) > 250*time.Millisecond {
		t.Fatal("lock mtime not refreshed")
	}
	if err := l.release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("not released")
	}
}

// fakeClaudeSave mimics Claude's saveConfigWithLock: take the mkdir lock,
// re-read the file, add a key, write it back, release.
func fakeClaudeSave(path, key string) error {
	lock := path + ".lock"
	for os.Mkdir(lock, 0o755) != nil {
		time.Sleep(time.Millisecond)
	}
	defer os.Remove(lock)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return err
	}
	m[key] = true
	time.Sleep(200 * time.Microsecond) // widen the read-modify-write window
	out, _ := json.MarshalIndent(m, "", "  ")
	tmp := path + ".claude-tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func TestConcurrentWithClaude(t *testing.T) {
	cfg, fdir := isolate(t)
	writeFixture(t, cfg, fixture, 0o600)
	const n = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if err := fakeClaudeSave(cfg, fmt.Sprintf("claude-%d", i)); err != nil {
				errs <- err
			}
		}
	}()
	o := opts(cfg, fdir)
	o.LockTimeout = 10 * time.Second
	for i := 0; i < n; i++ {
		if _, err := Trust(fmt.Sprintf("/w/%d", i), o); err != nil {
			errs <- err
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	m := loose(t, mustRead(t, cfg))
	for i := 0; i < n; i++ {
		if m[fmt.Sprintf("claude-%d", i)] != true {
			t.Errorf("Claude's key %d lost", i)
		}
		if Verify(cfg, fmt.Sprintf("/w/%d", i)) != nil {
			t.Errorf("trust key %d lost", i)
		}
	}
	if m["lastSeen"].(json.Number).String() != "1794150081977" {
		t.Error("number changed")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// --- keys ---

func TestKeys(t *testing.T) {
	isolate(t)
	base := realDir(t, t.TempDir())

	plain := filepath.Join(base, "plain", "sub")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := ResolveFolder(plain)
	if err != nil {
		t.Fatal(err)
	}
	if k, inRepo, err := Key(f); err != nil || inRepo || k != plain {
		t.Fatalf("plain: %q %v %v", k, inRepo, err)
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(plain, link); err != nil {
		t.Fatal(err)
	}
	if f, err := ResolveFolder(link); err != nil || f != plain {
		t.Fatalf("symlink: %q %v", f, err)
	}
	if f, err := ResolveFolder(filepath.Join(base, "plain", "..", "plain", "sub", ".")); err != nil || f != plain {
		t.Fatalf("unclean path: %q %v", f, err)
	}

	if _, err := ResolveFolder(filepath.Join(base, "missing")); err == nil {
		t.Fatal("missing folder accepted")
	}
	file := filepath.Join(base, "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := ResolveFolder(file); err == nil {
		t.Fatal("file accepted")
	}
	if _, err := ResolveFolder("/"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("/ : %v", err)
	}
	home := os.Getenv("HOME")
	if _, err := ResolveFolder(home); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("home: %v", err)
	}
	homeLink := filepath.Join(base, "homelink")
	_ = os.Symlink(home, homeLink)
	if _, err := ResolveFolder(homeLink); err == nil {
		t.Fatal("home via symlink accepted")
	}
}

func TestKeyGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	isolate(t)
	base := realDir(t, t.TempDir())
	repo := filepath.Join(base, "repo")
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Setenv("GIT_DIR", "/nonexistent") // must not redirect the lookup
	for _, dir := range []string{repo, sub} {
		k, inRepo, err := Key(dir)
		if err != nil || !inRepo || k != repo {
			t.Fatalf("%s: %q %v %v", dir, k, inRepo, err)
		}
	}
	// Reached through a symlink, the key is still the real repo root.
	link := filepath.Join(base, "repolink")
	_ = os.Symlink(repo, link)
	f, _ := ResolveFolder(filepath.Join(link, "a"))
	if k, _, _ := Key(f); k != repo {
		t.Fatalf("via symlink: %q", k)
	}

	// A home directory that is a git repository (dotfiles): a folder
	// inside it keys to home, which is refused.
	t.Setenv("HOME", repo)
	if _, _, err := Key(sub); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("home repo key: %v", err)
	}
}
