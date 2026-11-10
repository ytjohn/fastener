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
//	                           (also in bridge mode)
//	FAKE_GRANDCHILD_PIDFILE=p  write that child's pid to p
//	FAKE_GRANDCHILD_WRITES=d,… the child inherits stdout (holding the pipe open)
//	                           and prints "grandchild-<i>" at each delay d after it starts
//	FAKE_SETSID_CHILD=pidfile  first start a long-lived child in a new session (out of the
//	                           process group, as a daemonizing agent would) and write its pid
//	FAKE_CRASH=1               kill itself with SIGKILL after init
//	FAKE_REPLY_RLIMIT=1        reply with the soft RLIMIT_NOFILE the agent was started with
//	FAKE_MODEL=id              report id as the init event's model and in an assistant
//	                           event's message.model (both absent when unset)
//
// Remote bridge mode (a stand-in for `claude remote-control`; stdin is not read):
//
//	FAKE_UNTRUSTED=1           print Claude's workspace-trust error to stderr and exit 1
//	FAKE_BRIDGE=1              redraw a status screen like Claude's until SIGTERM
//	FAKE_BRIDGE_INTERVAL=dur   time between frames (default 50ms)
//	FAKE_BRIDGE_URL_AFTER=dur  show the bridge URL only after this long (default 0)
//	FAKE_BRIDGE_SESSION_AFTER=dur  add an OSC 8 session link after this long (default 300ms)
//	FAKE_BRIDGE_FRAMES=n       print n frames as fast as possible, then keep redrawing
//	                           at the interval
//	FAKE_BRIDGE_EXIT_AFTER=dur exit 0 on its own after this long
//
// Pty mode (an interactive agent; stdin is the terminal):
//
//	FAKE_PTY=1                 print "tty=<bool> ctty=<bool> size=<rows>x<cols>", then
//	                           read lines: "exit N" exits N, "flood N" prints N bytes of
//	                           "x" in 100-byte lines then "flood-done", "size" prints
//	                           "size=<rows>x<cols>", anything else is echoed as "echo:<line>";
//	                           every SIGWINCH prints "winch=<rows>x<cols>"; EOF exits 0
//	FAKE_PTY_INIT=1            also print a claude stream-json init event at start
//	FAKE_PTY_FLOOD=n           print n bytes at start, as "flood n" would
//	FAKE_PTY_AUTOEXIT=1        exit with FAKE_EXIT right after the start output
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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
		if w := os.Getenv("FAKE_GRANDCHILD_WRITES"); w != "" {
			start := time.Now()
			for i, d := range strings.Split(w, ",") {
				dur, _ := time.ParseDuration(d)
				time.Sleep(time.Until(start.Add(dur)))
				fmt.Printf("grandchild-%d\n", i)
			}
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if p := os.Getenv("FAKE_SETSID_CHILD"); p != "" {
		detachedChild(p)
	}
	if os.Getenv("FAKE_UNTRUSTED") == "1" {
		dir, _ := os.Getwd()
		fmt.Fprintf(os.Stderr, "Error: Workspace not trusted. Please run `claude` in %s first to review and accept the workspace trust dialog.\n", dir)
		os.Exit(1)
	}
	if os.Getenv("FAKE_BRIDGE") == "1" {
		grandchild()
		bridge()
		return
	}
	if os.Getenv("FAKE_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	if os.Getenv("FAKE_PTY") == "1" {
		ptyMode()
		return
	}
	_, _ = io.ReadAll(os.Stdin)
	if s := os.Getenv("FAKE_STDERR"); s != "" {
		fmt.Fprintln(os.Stderr, s)
	}
	grandchild()
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
	if m := os.Getenv("FAKE_MODEL"); m != "" {
		emit(map[string]any{"type": "assistant", "session_id": sid,
			"message": map[string]any{"role": "assistant", "model": m, "content": []any{}}})
	}
	reply := "pong"
	if v, ok := os.LookupEnv("FAKE_REPLY"); ok {
		reply = v
	}
	if os.Getenv("FAKE_REPLY_RLIMIT") == "1" {
		// Go raised this process's own soft limit at startup and restores the
		// original in its children, so a child shows what the agent was given.
		out, err := exec.Command("/bin/sh", "-c", "ulimit -Sn").Output()
		reply = strings.TrimSpace(string(out))
		if err != nil {
			reply = "error: " + err.Error()
		}
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

// grandchild starts a long-lived child in the same process group when
// FAKE_GRANDCHILD=1 (headless and bridge modes alike).
func grandchild() {
	if os.Getenv("FAKE_GRANDCHILD") != "1" {
		return
	}
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "FAKE_ROLE=grandchild")
	if os.Getenv("FAKE_GRANDCHILD_WRITES") != "" {
		c.Stdout = os.Stdout
	}
	if err := c.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "grandchild:", err)
		os.Exit(90)
	}
	if p := os.Getenv("FAKE_GRANDCHILD_PIDFILE"); p != "" {
		_ = os.WriteFile(p, []byte(strconv.Itoa(c.Process.Pid)), 0o644)
	}
}

// detachedChild starts a grandchild-role child with setsid (its own session
// and process group), writes its pid to pidfile and leaves it running.
func detachedChild(pidfile string) {
	c := exec.Command(os.Args[0])
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FAKE_SETSID_CHILD=") {
			c.Env = append(c.Env, kv)
		}
	}
	c.Env = append(c.Env, "FAKE_ROLE=grandchild")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "setsid child:", err)
		os.Exit(90)
	}
	_ = os.WriteFile(pidfile, []byte(strconv.Itoa(c.Process.Pid)), 0o644)
	_ = c.Process.Release()
}

func envDur(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// bridge mimics `claude remote-control`'s status screen (Claude Code
// 2.1.319): every frame moves the cursor up over the previous one, erases,
// and redraws; a session line is an OSC 8 hyperlink.
func bridge() {
	const env = "env_01FAKEBRIDGEFAKEBRIDGE01"
	const sess = "session_01FAKEBRIDGESESSION01"
	interval := envDur("FAKE_BRIDGE_INTERVAL", 50*time.Millisecond)
	urlAfter := envDur("FAKE_BRIDGE_URL_AFTER", 0)
	sessAfter := envDur("FAKE_BRIDGE_SESSION_AFTER", 300*time.Millisecond)
	fast, _ := strconv.Atoi(os.Getenv("FAKE_BRIDGE_FRAMES"))
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	w := bufio.NewWriter(os.Stdout)
	start := time.Now()
	prev := 0
	frame := func() {
		lines := []string{"\u00b7\u2714\ufe0e\u00b7 Connected \u00b7 example \u00b7 main"}
		since := time.Since(start)
		capacity := "0/32"
		var link string
		if since >= sessAfter {
			capacity = "1/32"
			link = "    \x1b]8;;https://claude.ai/code/" + sess + "?from=cli\x07Attached\x1b]8;;\x07"
		}
		lines = append(lines, "    Capacity: "+capacity+" \u00b7 New sessions will be created in the current directory")
		if link != "" {
			lines = append(lines, link)
		}
		lines = append(lines, "")
		if since >= urlAfter {
			lines = append(lines, "Continue coding in the Claude mobile app or https://claude.ai/code?environment="+env)
		} else {
			lines = append(lines, "Connecting\u2026")
		}
		lines = append(lines, "space to show QR code \u00b7 w to toggle spawn mode")
		if prev > 0 {
			fmt.Fprintf(w, "\x1b[%dA\x1b[J", prev)
		}
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
		prev = len(lines)
		w.Flush()
	}
	for i := 0; i < fast; i++ {
		frame()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var exitAfter <-chan time.Time
	if d := envDur("FAKE_BRIDGE_EXIT_AFTER", 0); d > 0 {
		exitAfter = time.After(d)
	}
	for {
		frame()
		select {
		case <-exitAfter:
			return
		case <-term:
			fmt.Fprintln(w, "Environment preserved. Restart to reconnect existing sessions.")
			w.Flush()
			return
		case <-t.C:
		}
	}
}

// ptyMode is an interactive agent on a terminal (see the package comment).
func ptyMode() {
	var mu sync.Mutex
	say := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Printf(format+"\n", args...)
	}
	size := func() string {
		ws, err := unix.IoctlGetWinsize(0, unix.TIOCGWINSZ)
		if err != nil {
			return "none"
		}
		return fmt.Sprintf("%dx%d", ws.Row, ws.Col)
	}
	_, ttyErr := unix.IoctlGetTermios(0, ioctlGetTermios)
	ctty := false
	if f, err := os.Open("/dev/tty"); err == nil {
		ctty = true
		f.Close()
	}
	winch := make(chan os.Signal, 8)
	signal.Notify(winch, syscall.SIGWINCH)
	if os.Getenv("FAKE_PTY_INIT") == "1" {
		b, _ := json.Marshal(map[string]any{"type": "system", "subtype": "init", "session_id": "fake-pty-0001"})
		say("%s", b)
	}
	say("tty=%v ctty=%v size=%s", ttyErr == nil, ctty, size())
	flood := func(n int) {
		line := strings.Repeat("x", 99)
		mu.Lock()
		w := bufio.NewWriter(os.Stdout)
		for ; n >= 100; n -= 100 {
			fmt.Fprintln(w, line)
		}
		if n > 0 {
			fmt.Fprintln(w, strings.Repeat("x", n-1))
		}
		w.Flush()
		mu.Unlock()
		say("flood-done")
	}
	if n, _ := strconv.Atoi(os.Getenv("FAKE_PTY_FLOOD")); n > 0 {
		flood(n)
	}
	if os.Getenv("FAKE_PTY_AUTOEXIT") == "1" {
		code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
		os.Exit(code)
	}
	go func() {
		for range winch {
			say("winch=%s", size())
		}
	}()
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		f := strings.Fields(line)
		switch {
		case len(f) == 2 && f[0] == "exit":
			code, _ := strconv.Atoi(f[1])
			os.Exit(code)
		case len(f) == 2 && f[0] == "flood":
			n, _ := strconv.Atoi(f[1])
			flood(n)
		case line == "size":
			say("size=%s", size())
		default:
			say("echo:%s", line)
		}
	}
}
