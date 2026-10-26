package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

func cmdInit(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file to create")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	path, err := config.ResolvePath(*cfgFlag)
	if err != nil {
		return err
	}
	if err := config.WriteStarter(path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists", path)
		}
		return err
	}
	fmt.Fprintf(stdout, `created %s

next:
  mcp-broker token create claude-code --save
  mcp-broker serve            (or start mcp-broker-tray.exe)
  mcp-broker dashboard        to add connectors
`, path)
	return nil
}
