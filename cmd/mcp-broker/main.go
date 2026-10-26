// Command mcp-broker is one MCP endpoint for every agent on the host.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/proc"
	"github.com/trustypangolin/mcp-broker/internal/version"
)

const usage = `usage: mcp-broker <command> [flags]

commands:
  serve       run the broker (Streamable HTTP; hosted by the tray app)
  connect     stdio shim: forward a stdio MCP client to the running broker
  dashboard   open the dashboard in a browser, signed in
  init        create a starter config
  validate    check config, paths and commands
  token       create, list and revoke agent tokens
  approvals   list, approve or deny pending approvals on the running broker
  reload      re-read the config on the running broker
  status      show the running broker and its connectors
  import      add connectors from Claude Desktop, Claude Code and VS Code configs
  ps          list profiles and their state
  start       start a profile (stop, update likewise)
  logs        show a profile's recent output
  plan        show what starting (or --for update) a profile would do
  version     print version

Run 'mcp-broker <command> -h' for command flags.
`

// errUsage marks errors whose message is already a complete user-facing explanation.
var errUsage = errors.New("usage")

func main() {
	proc.RunHelper()
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, errUsage) {
			fmt.Fprintln(os.Stderr, "mcp-broker:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version.String())
		return nil
	case "init":
		return cmdInit(rest, stdout, stderr)
	case "validate":
		return cmdValidate(rest, stdout, stderr)
	case "token":
		return cmdToken(rest, stdout, stderr)
	case "serve":
		return cmdServe(rest, stdout, stderr)
	case "connect":
		return cmdConnect(rest, stdout, stderr)
	case "approvals":
		return cmdApprovals(rest, stdout, stderr)
	case "reload":
		return cmdReload(rest, stdout, stderr)
	case "status":
		return cmdStatus(rest, stdout, stderr)
	case "dashboard":
		return cmdDashboard(rest, stdout, stderr)
	case "import":
		return cmdImport(rest, stdout, stderr)
	case "ps":
		return cmdPs(rest, stdout, stderr)
	case "start", "stop", "update":
		return cmdProfileAction(cmd, rest, stdout, stderr)
	case "logs":
		return cmdLogs(rest, stdout, stderr)
	case "plan":
		return cmdPlan(rest, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
	return errUsage
}

func cmdValidate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file (default $"+config.EnvConfigPath+" or the user config dir)")
	strict := fs.Bool("strict", false, "treat warnings as errors")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	path, err := config.ResolvePath(*cfgFlag)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}

	issues := cfg.Check()
	errs, warns := 0, 0
	for _, i := range issues {
		fmt.Fprintln(stdout, i)
		if i.Warning {
			warns++
		} else {
			errs++
		}
	}
	fmt.Fprintf(stdout, "%s: %d connectors, %d agents, %d roles, %d profiles; %d errors, %d warnings\n",
		cfg.Path, len(cfg.Connectors), len(cfg.Agents), len(cfg.Roles), len(cfg.Profiles), errs, warns)
	if errs > 0 || (*strict && warns > 0) {
		return errUsage
	}
	return nil
}
