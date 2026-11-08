package proc

import (
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

func TestDeadAndZombieNotAlive(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := BootID()
	_ = cmd.Process.Kill()
	time.Sleep(100 * time.Millisecond) // now a zombie: killed but not yet reaped
	if Alive(id, boot) {
		t.Fatal("zombie reported alive")
	}
	_ = cmd.Wait()
	if Alive(id, boot) {
		t.Fatal("reaped process reported alive")
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

func TestFindMarkedRequiresMarker(t *testing.T) {
	boot, _ := BootID()
	marked := startInGroup(t, append(os.Environ(), MarkerEnv+"=s1:1:abc"))
	unmarked := startInGroup(t, os.Environ())

	got, err := FindMarked(marked.Process.Pid, "s1:1:abc", boot)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PID != marked.Process.Pid {
		t.Fatalf("FindMarked(marked) = %+v", got)
	}
	// Same pgid lookup, but the process lacks the marker: never counted.
	got, _ = FindMarked(unmarked.Process.Pid, "s1:1:abc", boot)
	if len(got) != 0 {
		t.Fatalf("unmarked process counted: %+v", got)
	}
	// Right marker, wrong boot: nothing.
	got, _ = FindMarked(marked.Process.Pid, "s1:1:abc", "other-boot")
	if len(got) != 0 {
		t.Fatalf("wrong boot counted: %+v", got)
	}
}

func TestWaitGone(t *testing.T) {
	boot, _ := BootID()
	cmd := startInGroup(t, os.Environ())
	id, _ := Of(cmd.Process.Pid)
	if left := WaitGone([]Identity{id}, boot, 100*time.Millisecond); len(left) != 1 {
		t.Fatalf("expected survivor, got %v", left)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if left := WaitGone([]Identity{id}, boot, 2*time.Second); len(left) != 0 {
		t.Fatalf("expected none left, got %v", left)
	}
}
