//go:build !windows

// tuipty has no ConPTY to lean on outside Windows: the demo drives the
// real TUI there. On other platforms build this stub so ./... stays
// green and the failure message points at the supported path.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "tuipty: only supported on Windows — run demo/demo-60s.sh there, or --live from a real console")
	os.Exit(2)
}
