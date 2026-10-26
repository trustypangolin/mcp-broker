//go:build !windows

// Command mcp-broker-tray is the Windows notification-area app. Elsewhere, run
// `mcp-broker serve` and use `mcp-broker dashboard`.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "mcp-broker-tray is Windows-only; run `mcp-broker serve` and `mcp-broker dashboard` instead")
	os.Exit(1)
}
