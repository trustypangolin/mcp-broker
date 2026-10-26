// Command mcp-broker-tray is the Windows notification-area app: it hosts the broker and
// offers its settings, approvals, firewall access and autostart from the tray icon.
//
// Build it as a GUI program so no console window opens:
//
//	go build -ldflags "-H windowsgui" ./cmd/mcp-broker-tray
package main

import (
	"flag"
	"os"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/proc"
	"github.com/trustypangolin/mcp-broker/internal/tray"
)

func main() {
	proc.RunHelper() // before anything else: it may be a CTRL_BREAK helper, not the tray
	cfgFlag := flag.String("config", "", "config file (default $"+config.EnvConfigPath+" or the user config dir)")
	flag.Parse()
	path, err := config.ResolvePath(*cfgFlag)
	if err == nil {
		err = tray.Run(path)
	}
	if err != nil {
		tray.Fatal(err)
		os.Exit(1)
	}
}
