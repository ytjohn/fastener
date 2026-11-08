package runenv

import (
	"os"
	"path/filepath"
	"reflect"
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
