package cli

import (
	"io"
	"reflect"
	"testing"
)

func TestParseArgsDoubleDashEndsFlags(t *testing.T) {
	cases := []struct {
		args []string
		want []string
		cwd  string
	}{
		{[]string{"--", "a", "-f"}, []string{"a", "-f"}, ""},
		{[]string{"--cwd", "x", "--", "a", "--cwd", "y"}, []string{"a", "--cwd", "y"}, "x"},
		{[]string{"a", "--cwd", "x", "--", "-b", "--"}, []string{"a", "-b", "--"}, "x"},
		{[]string{"--cwd", "--", "a"}, []string{"a"}, "--"}, // "--" as a flag's value is not a terminator
	}
	for _, c := range cases {
		fs := newFlagSet("t", io.Discard)
		cwd := fs.String("cwd", "", "")
		fs.Bool("json", false, "")
		got, err := parseArgs(fs, c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if !reflect.DeepEqual(got, c.want) || *cwd != c.cwd {
			t.Errorf("%v: pos=%v cwd=%q, want pos=%v cwd=%q", c.args, got, *cwd, c.want, c.cwd)
		}
	}
}
