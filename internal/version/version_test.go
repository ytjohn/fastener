package version

import "testing"

func TestLdflagsVersionWins(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "v9.9.9"
	if got := String(); got != "v9.9.9" {
		t.Fatalf("String() = %q, want v9.9.9", got)
	}
}

func TestFallbackIsNeverEmpty(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = ""
	// Under `go test` the main module is "(devel)", so the fallback is "dev".
	if got := String(); got != "dev" {
		t.Fatalf("String() = %q, want dev", got)
	}
}
