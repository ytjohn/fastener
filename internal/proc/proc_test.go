package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestSelfAlive(t *testing.T) {
	id, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	boot, err := BootID()
	if err != nil || boot == "" {
		t.Fatalf("BootID = %q, %v", boot, err)
	}
	if !Alive(id, boot) {
		t.Fatal("own process not alive")
	}
	if Alive(Identity{PID: id.PID, StartTime: id.StartTime + 1}, boot) {
		t.Fatal("mismatched start time reported alive")
	}
	if Alive(id, "some-other-boot") {
		t.Fatal("mismatched boot id reported alive")
	}
}

func startInGroup(t *testing.T, env []string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func mustBoot(t *testing.T) string {
	t.Helper()
	boot, err := BootID()
	if err != nil {
		t.Fatal(err)
	}
	return boot
}

func TestCheckEmptyBoot(t *testing.T) {
	id, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := Check(id, ""); ok || err != nil {
		t.Fatalf("Check(empty boot) = %v, %v", ok, err)
	}
}

func TestFindMarkedSameGroup(t *testing.T) {
	boot := mustBoot(t)
	marked := startInGroup(t, append(os.Environ(), MarkerEnv+"=s1:1:abc"))
	pgid := marked.Process.Pid
	other := exec.Command("sleep", "30")
	other.Env = os.Environ()
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })

	got, err := FindMarked(pgid, "s1:1:abc", boot)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PID != marked.Process.Pid {
		t.Fatalf("FindMarked = %+v, want only pid %d", got, marked.Process.Pid)
	}
	for _, m := range []string{"s1:1:xyz", "s1:1:ab"} {
		got, err := FindMarked(pgid, m, boot)
		if err != nil || len(got) != 0 {
			t.Fatalf("marker %q: %+v, %v", m, got, err)
		}
	}
	if got, _ := FindMarked(pgid, "s1:1:abc", "other-boot"); len(got) != 0 {
		t.Fatalf("wrong boot counted: %+v", got)
	}
}

func TestSignalMismatchedIdentity(t *testing.T) {
	boot := mustBoot(t)
	cmd := startInGroup(t, os.Environ())
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	bad := Identity{PID: id.PID, StartTime: id.StartTime + 1}
	if err := Signal(bad, boot, syscall.SIGKILL); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("Signal = %v, want ErrNoProcess", err)
	}
	time.Sleep(50 * time.Millisecond)
	if !Alive(id, boot) {
		t.Fatal("target was signalled despite identity mismatch")
	}
}

func TestWaitGone(t *testing.T) {
	boot := mustBoot(t)
	cmd := startInGroup(t, os.Environ())
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if left := WaitGone([]Identity{id}, boot, 100*time.Millisecond); len(left) != 1 {
		t.Fatalf("expected survivor, got %v", left)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	defer func() { <-done }() // cleanup's Wait must not race the goroutine's
	start := time.Now()
	if left := WaitGone([]Identity{id}, boot, 5*time.Second); len(left) != 0 {
		t.Fatalf("expected none left, got %v", left)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("WaitGone took %v", el)
	}
}

func TestHandleClose(t *testing.T) {
	boot := mustBoot(t)
	cmd := startInGroup(t, os.Environ())
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Open(id, boot)
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	h.Close() // idempotent
	if err := h.Signal(syscall.SIGKILL); err == nil || errors.Is(err, ErrNoProcess) {
		t.Fatalf("Signal on closed handle = %v, want a closed error", err)
	}
	time.Sleep(50 * time.Millisecond)
	if !Alive(id, boot) {
		t.Fatal("closed handle signalled its target")
	}
}

func TestOpenRejectsMismatch(t *testing.T) {
	boot := mustBoot(t)
	id, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   Identity
		boot string
	}{
		{Identity{PID: id.PID, StartTime: id.StartTime + 1}, boot},
		{id, "other-boot"},
		{id, ""},
		{Identity{}, boot},
	} {
		if h, err := Open(c.id, c.boot); !errors.Is(err, ErrNoProcess) {
			if h != nil {
				h.Close()
			}
			t.Fatalf("Open(%+v, %q) = %v, want ErrNoProcess", c.id, c.boot, err)
		}
	}
}

func TestIsZombie(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	id, err := Of(pid)
	if err != nil {
		t.Fatal(err)
	}
	if z, err := IsZombie(pid); z || err != nil {
		t.Fatalf("running child: IsZombie = %v, %v", z, err)
	}
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if z, err := IsZombie(pid); z && err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("killed child never showed as a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if Alive(id, mustBoot(t)) {
		t.Fatal("zombie reported alive")
	}
	_ = cmd.Wait()
	if _, err := IsZombie(pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("reaped child: IsZombie err = %v, want ErrNoProcess", err)
	}
}
