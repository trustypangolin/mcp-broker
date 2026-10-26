package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/trustypangolin/mcp-broker/internal/supervisor"
)

// cmdPs lists profiles on the running broker.
func cmdPs(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	do, err := adminClient(*cfgFlag)
	if err != nil {
		return err
	}
	resp, err := do(http.MethodGet, "/admin/profiles", nil)
	if err != nil {
		return err
	}
	var out struct{ Profiles []supervisor.Status }
	if err := decodeOK(resp, &out); err != nil {
		return err
	}
	if len(out.Profiles) == 0 {
		fmt.Fprintln(stdout, "no profiles configured")
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE\tSTATE\tPID\tUPTIME\tPORTS\tHEALTH")
	for _, p := range out.Profiles {
		pid := ""
		if p.PID != 0 {
			pid = fmt.Sprint(p.PID)
		}
		health := p.Health
		if p.Error != "" {
			health = p.Error
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%s\n", p.Profile, p.State, pid, p.Uptime, p.Ports, health)
	}
	return w.Flush()
}

// cmdProfileAction is start, stop and update: the user acting, so no approval.
func cmdProfileAction(action string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintf(stderr, "usage: mcp-broker %s <profile> [--config PATH]\n", action)
		return errUsage
	}
	do, err := adminClient(*cfgFlag)
	if err != nil {
		return err
	}
	name := pos[0]
	resp, err := do(http.MethodPost, "/admin/profiles/"+url.PathEscape(name)+"/"+action, nil)
	if err != nil {
		return err
	}
	var st supervisor.Status
	if err := decodeOK(resp, &st); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: %s", name, st.State)
	if st.PID != 0 {
		fmt.Fprintf(stdout, " (pid %d)", st.PID)
	}
	if st.ExitCode != nil {
		fmt.Fprintf(stdout, ", exit code %d", *st.ExitCode)
	}
	fmt.Fprintln(stdout)
	if action != "stop" {
		fmt.Fprintf(stdout, "follow it with: mcp-broker ps, mcp-broker logs %s\n", name)
	}
	return nil
}

func cmdLogs(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	n := fs.Int("n", 100, "lines")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: mcp-broker logs <profile> [-n LINES] [--config PATH]")
		return errUsage
	}
	do, err := adminClient(*cfgFlag)
	if err != nil {
		return err
	}
	resp, err := do(http.MethodGet, fmt.Sprintf("/admin/profiles/%s/logs?n=%d", url.PathEscape(pos[0]), *n), nil)
	if err != nil {
		return err
	}
	var out struct{ Lines []string }
	if err := decodeOK(resp, &out); err != nil {
		return err
	}
	if len(out.Lines) > 0 {
		fmt.Fprintln(stdout, strings.Join(out.Lines, "\n"))
	}
	return nil
}

// parseInterleaved parses flags that may come before or after positional arguments
// (`start web --config x`), returning the positional ones.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos, rest = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
}
