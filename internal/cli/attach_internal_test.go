package cli

import (
	"bytes"
	"flag"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseDetachKey(t *testing.T) {
	for in, want := range map[string]int{
		"ctrl-]": 0x1d, "CTRL-]": 0x1d, "ctrl-a": 1, "ctrl-z": 26, "ctrl-Z": 26,
		`ctrl-\`: 0x1c, "ctrl-@": 0, "ctrl-^": 0x1e, "ctrl-_": 0x1f, "none": noDetachKey, " None ": noDetachKey,
	} {
		got, err := parseDetachKey(in)
		if err != nil || got != want {
			t.Errorf("parseDetachKey(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "ctrl-[", "ctrl-", "ctrl-ab", "ctrl-1", "alt-x", "]", "^]"} {
		if _, err := parseDetachKey(in); err == nil {
			t.Errorf("parseDetachKey(%q) accepted", in)
		}
	}
}

// TestSettle: settle returns once input has been quiet for settleQuiet,
// waits while replies keep arriving, and never past settleMax.
func TestSettle(t *testing.T) {
	var last atomic.Int64
	start := time.Now()
	settle(&last)
	if d := time.Since(start); d < settleQuiet || d > settleQuiet+100*time.Millisecond {
		t.Fatalf("quiet terminal: settled after %v", d)
	}

	start = time.Now()
	go func() { // a reply arrives 30ms in
		time.Sleep(30 * time.Millisecond)
		last.Store(time.Now().UnixNano())
	}()
	settle(&last)
	if d := time.Since(start); d < 30*time.Millisecond+settleQuiet {
		t.Fatalf("late reply: settled after %v", d)
	}

	stop := make(chan struct{})
	go func() { // a terminal that never goes quiet
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				last.Store(time.Now().UnixNano())
			}
		}
	}()
	start = time.Now()
	settle(&last)
	close(stop)
	if d := time.Since(start); d < settleMax || d > settleMax+100*time.Millisecond {
		t.Fatalf("busy terminal: settled after %v", d)
	}
}

// TestRunHelpListsEveryFlag: the hand-written run help names every flag the
// flag set defines, and the flag package's own descriptions stay out of it.
func TestRunHelpListsEveryFlag(t *testing.T) {
	var out bytes.Buffer
	if code := cmdRun([]string{"-h"}, &out, &out); code != exitUsage && code != exitOK {
		t.Fatalf("run -h exited %d", code)
	}
	if out.String() != runHelp {
		t.Fatalf("run -h printed something other than runHelp:\n%s", out.String())
	}
	fs := newFlagSet("run", io.Discard)
	var names []string
	var r runFlags
	r.define(fs)
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	if len(names) == 0 {
		t.Fatal("no flags")
	}
	for _, n := range names {
		if !strings.Contains(runHelp, "--"+n+" ") && !strings.Contains(runHelp, "--"+n+"\n") {
			t.Errorf("run help does not mention --%s", n)
		}
	}
}
