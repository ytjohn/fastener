// Command fakeagent stands in for an agent CLI in Fastener's tests. It reads
// its prompt from stdin and prints claude stream-json. Behaviour:
//
//	--version                  print a version line and exit 0
//	FAKE_REPLY=text            reply text (default "pong")
//	FAKE_REPLY_ENV=NAME        reply with the value of env var NAME
//	FAKE_IS_ERROR=1            result.is_error = true
//	FAKE_EXIT=n                exit code (default 0)
//	FAKE_SLEEP=dur             sleep between init and result
//	FAKE_IGNORE_TERM=1         ignore SIGTERM
//	FAKE_STDERR=text           print text to stderr first
//	FAKE_GRANDCHILD=1          start a long-lived child in the same process group
//	FAKE_GRANDCHILD_PIDFILE=p  write that child's pid to p
//	FAKE_CRASH=1               kill itself with SIGKILL after init
//	FAKE_MODEL=id              report id as the init event's model (absent when unset)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func emit(v map[string]any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("fakeagent 1.0")
		return
	}
	if os.Getenv("FAKE_ROLE") == "grandchild" {
		signal.Ignore(syscall.SIGTERM, syscall.SIGHUP)
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("FAKE_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	_, _ = io.ReadAll(os.Stdin)
	if s := os.Getenv("FAKE_STDERR"); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}
	if os.Getenv("FAKE_GRANDCHILD") == "1" {
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "FAKE_ROLE=grandchild")
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "grandchild:", err)
			os.Exit(90)
		}
		if p := os.Getenv("FAKE_GRANDCHILD_PIDFILE"); p != "" {
			_ = os.WriteFile(p, []byte(strconv.Itoa(c.Process.Pid)), 0o644)
		}
	}
	const sid = "fake-session-0001"
	initEv := map[string]any{"type": "system", "subtype": "init", "session_id": sid}
	if m := os.Getenv("FAKE_MODEL"); m != "" {
		initEv["model"] = m
	}
	emit(initEv)
	if os.Getenv("FAKE_CRASH") == "1" {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	if d := os.Getenv("FAKE_SLEEP"); d != "" {
		dur, _ := time.ParseDuration(d)
		time.Sleep(dur)
	}
	reply := "pong"
	if v, ok := os.LookupEnv("FAKE_REPLY"); ok {
		reply = v
	}
	if name := os.Getenv("FAKE_REPLY_ENV"); name != "" {
		reply = os.Getenv(name)
	}
	emit(map[string]any{
		"type": "result", "subtype": "success", "session_id": sid,
		"result": reply, "is_error": os.Getenv("FAKE_IS_ERROR") == "1",
		"total_cost_usd": 0.001, "usage": map[string]any{"input_tokens": 3, "output_tokens": 1},
	})
	code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
	os.Exit(code)
}
