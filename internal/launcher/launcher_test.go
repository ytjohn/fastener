package launcher

import (
	"path/filepath"
	"testing"

	"github.com/ytjohn/fastener/internal/session"
)

func TestMonitorArgsAbsolute(t *testing.T) {
	t.Setenv("FASTENER_STATE_DIR", "rel/state")
	d, args, err := monitorArgs(session.DirFor("x"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(string(d)) || args[0] != "_monitor" || args[1] != string(d) || args[2] != "2" {
		t.Fatalf("dir=%q args=%v", d, args)
	}
}
