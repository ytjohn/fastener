package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/ytjohn/fastener/internal/session"
)

func TestParseArgsDoubleDashEndsFlags(t *testing.T) {
	cases := []struct {
		args []string
		want []string
		cwd  string
		name string
	}{
		{[]string{"--", "a", "-f"}, []string{"a", "-f"}, "", ""},
		{[]string{"--cwd", "x", "--", "a", "--cwd", "y"}, []string{"a", "--cwd", "y"}, "x", ""},
		{[]string{"a", "--cwd", "x", "--", "-b", "--"}, []string{"a", "-b", "--"}, "x", ""},
		{[]string{"--cwd", "--", "a"}, []string{"a"}, "--", ""},
		{[]string{"--name", "--cwd", "--", "a", "-f"}, []string{"a", "-f"}, "", "--cwd"},
		{[]string{"--cwd", "--cwd", "--", "-x", "-y"}, []string{"-x", "-y"}, "--cwd", ""},
		{[]string{"--json", "--", "-x"}, []string{"-x"}, "", ""},
	}
	for _, c := range cases {
		fs := newFlagSet("t", io.Discard)
		cwd := fs.String("cwd", "", "")
		name := fs.String("name", "", "")
		fs.Bool("json", false, "")
		got, err := parseArgs(fs, c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if !reflect.DeepEqual(got, c.want) || *cwd != c.cwd || *name != c.name {
			t.Errorf("%v: pos=%v cwd=%q, want pos=%v cwd=%q", c.args, got, *cwd, c.want, c.cwd)
		}
	}
}

func TestInspectShowsUnknownLiveness(t *testing.T) {
	v := &session.View{
		Session:       session.Session{Name: "s", Backend: "fake"},
		Status:        session.Status{State: session.StateRunning},
		State:         session.StateRunning,
		LivenessError: "permission denied",
	}
	var b bytes.Buffer
	renderInspect(&b, v, inspectOf(v))
	if !strings.Contains(b.String(), "liveness    unknown (permission denied)") {
		t.Fatalf("human output:\n%s", b.String())
	}
	j, _ := json.Marshal(inspectOf(v))
	if !strings.Contains(string(j), `"liveness_error":"permission denied"`) {
		t.Fatalf("json: %s", j)
	}

	v.LivenessError = ""
	b.Reset()
	renderInspect(&b, v, inspectOf(v))
	j, _ = json.Marshal(inspectOf(v))
	if strings.Contains(b.String(), "liveness") || strings.Contains(string(j), "liveness_error") {
		t.Fatalf("liveness shown when known:\n%s\n%s", b.String(), j)
	}
}
