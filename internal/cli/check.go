package cli

import (
	"fmt"
	"io"
)

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "fastener: check not implemented yet")
	return exitUsage
}
