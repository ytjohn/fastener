// Package cli implements the fastener command line.
package cli

import (
	"fmt"
	"io"
)

func Main(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "fastener: not implemented yet")
	return 2
}
