package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/update"
	"github.com/trustypangolin/mcp-broker/internal/version"
)

// cmdSelfUpdate checks GitHub for a newer release and installs its programs over the
// running ones. The tray does the same from its menu and restarts itself; here the user
// restarts whatever runs the broker.
func cmdSelfUpdate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("self-update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "only report whether a newer release exists")
	stable := fs.Bool("stable", false, "ignore pre-releases")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	c := update.NewChecker(version.Version, !*stable)
	if c == nil {
		return fmt.Errorf("this is a development build (%s): it can't tell whether a release is newer", version.Version)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r, err := c.Check(ctx)
	if err != nil {
		return err
	}
	if r == nil {
		fmt.Fprintf(stdout, "mcp-broker %s is up to date\n", version.Version)
		return nil
	}
	fmt.Fprintf(stdout, "%s is available (you have %s): %s\n", r.Version, version.Version, r.URL)
	if *check {
		return nil
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return err
	}
	files, err := c.Install(ctx, r, filepath.Dir(exe))
	if err != nil {
		return err
	}
	for _, f := range files {
		fmt.Fprintln(stdout, "replaced", f)
	}
	fmt.Fprintln(stdout, "Restart the tray (or `mcp-broker serve`) to run "+r.Version+"; clients' shims pick it up when they reconnect.")
	return nil
}
