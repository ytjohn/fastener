package cli

import "testing"

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
