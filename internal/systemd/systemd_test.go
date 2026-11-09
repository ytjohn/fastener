package systemd

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeTools puts a systemctl that prints state (exiting 1 unless running, as
// the real one does) and a systemd-run on a PATH of their own.
func fakeTools(t *testing.T, state string) {
	t.Helper()
	dir := t.TempDir()
	code := "1"
	if state == "running" {
		code = "0"
	}
	for name, body := range map[string]string{
		"systemctl":   "#!/bin/sh\necho " + state + "\nexit " + code + "\n",
		"systemd-run": "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestAvailableStates(t *testing.T) {
	for _, c := range []struct {
		state              string
		available, booting bool
	}{
		{"running", true, true},
		{"degraded", true, true},
		{"starting", false, true},
		{"initializing", false, false},
		{"stopping", false, false},
		{"offline", false, false},
	} {
		fakeTools(t, c.state)
		if ok, err := Available(); ok != c.available {
			t.Errorf("%s: Available = %v (%v), want %v", c.state, ok, err, c.available)
		}
		if ok, err := AvailableOrStarting(); ok != c.booting {
			t.Errorf("%s: AvailableOrStarting = %v (%v), want %v", c.state, ok, err, c.booting)
		}
	}
}
