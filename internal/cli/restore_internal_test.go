package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ytjohn/fastener/internal/config"
	"github.com/ytjohn/fastener/internal/session"
)

func TestRestoreLauncherPref(t *testing.T) {
	view := func(kind string) *session.View {
		v := &session.View{Session: session.Session{Name: "br"}}
		if kind != "" {
			v.Launcher = &session.LauncherRecord{Launcher: kind}
		}
		return v
	}
	for _, c := range []struct {
		record, config, want string
		note                 bool
	}{
		{"setsid", "systemd", "setsid", false}, // the record wins over the config
		{"systemd", "setsid", "systemd", false},
		{"", "setsid", "setsid", true}, // no record: config, with a note
		{"", "", "auto", true},
		{"bogus", "systemd", "systemd", true},
	} {
		var errb bytes.Buffer
		got := restoreLauncherPref(view(c.record), config.Config{Launcher: c.config}, &errb)
		if got != c.want || (errb.Len() > 0) != c.note {
			t.Errorf("record %q, config %q: %q (note %q), want %q (note %v)", c.record, c.config, got, errb.String(), c.want, c.note)
		}
		if c.note && !strings.Contains(errb.String(), "no launcher record") {
			t.Errorf("note = %q", errb.String())
		}
	}
}
