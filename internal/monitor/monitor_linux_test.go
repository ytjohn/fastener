//go:build linux

package monitor

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ytjohn/fastener/internal/session"
)

func TestStopKillsGrandchild(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	d := setupRun(t, []string{buildFake(t)}, t.TempDir(), []string{"FAKE_GRANDCHILD=1", "FAKE_GRANDCHILD_PIDFILE=" + pidfile, "FAKE_SLEEP=30s"}, 0)
	sigs := make(chan os.Signal, 4)
	st := runWith(t, d, sigs, func() {
		waitFor(t, "grandchild pidfile", func() bool { b, err := os.ReadFile(pidfile); return err == nil && len(b) > 0 })
		requestStop(t, d)
		sigs <- syscall.SIGTERM
	})
	if st.EndedBy != session.EndedByStop {
		t.Fatalf("ended_by=%s", st.EndedBy)
	}
	b, _ := os.ReadFile(pidfile)
	pid := strings.TrimSpace(string(b))
	waitFor(t, "grandchild dead", func() bool {
		stat, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			return true
		}
		i := strings.LastIndex(string(stat), ") ")
		return i >= 0 && strings.HasPrefix(string(stat)[i+2:], "Z")
	})
}

// TestExitBeatsSignal makes the agent exit (and sit pinned as a zombie) and a
// signal ready at the same moment; the real exit must win.
func TestExitBeatsSignal(t *testing.T) {
	for i := 0; i < 20; i++ {
		d := setupRun(t, []string{buildFake(t)}, t.TempDir(), []string{"FAKE_EXIT=3"}, 0)
		sigs := make(chan os.Signal, 4)
		m, err := load(d, 1, log.New(os.Stderr, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		m.beforeWait = func(pid int) {
			waitFor(t, "agent zombie", func() bool {
				stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				if err != nil {
					return false
				}
				i := strings.LastIndex(string(stat), ") ")
				return i >= 0 && strings.HasPrefix(string(stat)[i+2:], "Z")
			})
			sigs <- syscall.SIGTERM
		}
		done := make(chan int, 1)
		go func() { done <- m.run(sigs) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("run did not finish")
		}
		var st session.Status
		if err := session.ReadJSON(d.StatusPath(), &st); err != nil {
			t.Fatal(err)
		}
		if st.EndedBy != session.EndedByExit || st.ExitCode == nil || *st.ExitCode != 3 || st.Signal != "" {
			t.Fatalf("iter %d: ended_by=%s code=%v signal=%s", i, st.EndedBy, st.ExitCode, st.Signal)
		}
	}
}
