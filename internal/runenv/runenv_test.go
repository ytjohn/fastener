package runenv

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBasePassthrough(t *testing.T) {
	environ := []string{"HOME=/h", "PATH=/bin", "LC_ALL=C", "LC_TIME=x", "SECRET=s", "LCX=no"}
	got := Base([]string{"HOME", "PATH", "LC_*"}, environ)
	want := []string{"HOME=/h", "PATH=/bin", "LC_ALL=C", "LC_TIME=x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Base = %v, want %v", got, want)
	}
}

func TestMergeLaterWins(t *testing.T) {
	got := Merge([]string{"A=1", "B=1"}, []string{"B=2", "C=2"}, []string{"A=3"})
	want := []string{"A=3", "B=2", "C=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge = %v, want %v", got, want)
	}
	if Get(got, "B") != "2" || Get(got, "Z") != "" {
		t.Fatal("Get wrong")
	}
}

func TestReadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	os.WriteFile(p, []byte("# comment\n\nA=1\nB = two words \nexport C=3\n"), 0o600)
	got, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A=1", "B=two words", "C=3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadFile = %v, want %v", got, want)
	}
	os.WriteFile(p, []byte("NOEQUALS\n"), 0o600)
	if _, err := ReadFile(p); err == nil {
		t.Fatal("expected error for line without '='")
	}
}

func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")
	os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "notexec"), []byte(""), 0o644)
	got, err := LookPath("agent", "relative/dir:"+dir)
	if err != nil || got != exe {
		t.Fatalf("LookPath = %q, %v", got, err)
	}
	if _, err := LookPath("notexec", dir); err == nil {
		t.Fatal("non-executable found")
	}
	if _, err := LookPath("agent", "relative/dir"); err == nil {
		t.Fatal("relative PATH entries must be ignored")
	}
	if got, err := LookPath(exe, ""); err != nil || got != exe {
		t.Fatalf("absolute LookPath = %q, %v", got, err)
	}
}

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadFileQuotesHashBOM(t *testing.T) {
	p := writeEnv(t, "\uFEFFA=\"x y\"\nB='z'\nC=\"un\nD=a # b\nE=\"a\"b\"\n")
	got, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A=x y", "B=z", `C="un`, "D=a # b", `E=a"b`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadFile = %q, want %q", got, want)
	}
}

func TestReadFileInvalidKey(t *testing.T) {
	got, err := ReadFile(writeEnv(t, "A=1\n1BAD=2\n"))
	if err == nil || got != nil {
		t.Fatalf("got %v, %v; want nil, error", got, err)
	}
	if !strings.Contains(err.Error(), `:2: invalid key "1BAD"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestReadFileLongValue(t *testing.T) {
	val := strings.Repeat("x", 100*1024)
	got, err := ReadFile(writeEnv(t, "BIG="+val+"\n"))
	if err != nil || len(got) != 1 || got[0] != "BIG="+val {
		t.Fatalf("long value failed: err=%v", err)
	}
}

func TestMergeDuplicateWithinLayer(t *testing.T) {
	got := Merge([]string{"A=1", "B=1", "A=2"})
	want := []string{"A=2", "B=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge = %v, want %v", got, want)
	}
}

func TestLookPathEdgeCases(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LookPath("adir", dir); err == nil {
		t.Fatal("directory with exec bit accepted")
	}
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got, err := LookPath("link", dir); err != nil || got != link {
		t.Fatalf("symlink LookPath = %q, %v", got, err)
	}
	if _, err := LookPath("real", ":"+dir); err != nil {
		t.Fatalf("empty PATH entry should be skipped: %v", err)
	}
	if _, err := LookPath("real", ""); err == nil {
		t.Fatal("empty PATH found something")
	}
}

func TestMergeEmptyIsNonNil(t *testing.T) {
	if got := Merge(nil, nil); got == nil || len(got) != 0 {
		t.Fatalf("Merge(nil, nil) = %#v, want non-nil empty", got)
	}
}
