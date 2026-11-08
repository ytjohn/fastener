//go:build linux

package proc

import (
	"os/exec"
	"testing"
	"time"
)

func TestZombieNotAlive(t *testing.T) {
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
	boot, err := BootID()
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	var state byte
	for time.Now().Before(deadline) {
		if state, _, _, err = readStat(pid); err == nil && state == 'Z' {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state != 'Z' {
		t.Fatalf("process never became a zombie (state %q, err %v)", state, err)
	}
	if Alive(id, boot) {
		t.Fatal("zombie reported alive")
	}
	_ = cmd.Wait()
	if Alive(id, boot) {
		t.Fatal("reaped process reported alive")
	}
}

func TestParseStat(t *testing.T) {
	tail := " S 1 777 1 0 -1 4194304 1 0 0 0 0 0 0 0 20 0 1 0 4242 1 1"
	for _, comm := range []string{"sleep", "a) b (c", "x y)"} {
		state, pgid, start, err := parseStat(5, "5 ("+comm+")"+tail)
		if err != nil {
			t.Fatalf("comm %q: %v", comm, err)
		}
		if state != 'S' || pgid != 777 || start != 4242 {
			t.Fatalf("comm %q: got %c %d %d", comm, state, pgid, start)
		}
	}
	if _, _, _, err := parseStat(5, "5 (sleep) S 1 2 3"); err == nil {
		t.Fatal("short stat did not error")
	}
	if _, _, _, err := parseStat(5, "garbage"); err == nil {
		t.Fatal("malformed stat did not error")
	}
}
