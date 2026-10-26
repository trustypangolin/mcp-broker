package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/identity"
)

const tokenUsage = `usage: mcp-broker token <create|list|revoke> [flags]

  create <agent> [--label L] [--save]   issue a token (shown once)
  list                                  list tokens (ids and agents, never tokens)
  revoke <token-id|agent>               revoke one token, or all of an agent's tokens
`

func cmdToken(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, tokenUsage)
		return errUsage
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("token "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "config file")
	label := fs.String("label", "", "note stored with the token (create)")
	save := fs.Bool("save", false, "also write the token to agent-tokens/<agent>.token for the local shim (create)")
	// Allow flags after the positional argument: `token create vscode --save`.
	var pos []string
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return errUsage
		}
		rest = fs.Args()
		if len(rest) > 0 {
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
	}

	path, err := config.ResolvePath(*cfgFlag)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	store, err := identity.OpenStore(cfg.Server.TokensFile)
	if err != nil {
		return err
	}

	switch sub {
	case "create":
		if len(pos) != 1 {
			return fmt.Errorf("token create: need exactly one agent name")
		}
		agent := pos[0]
		if _, ok := cfg.Agents[agent]; !ok {
			return fmt.Errorf("token create: no agent %q in %s", agent, cfg.Path)
		}
		tok, e, err := store.Create(agent, *label)
		if err != nil {
			return err
		}
		if *save {
			p := cfg.AgentTokenPath(agent)
			if err := identity.SaveToken(p, tok); err != nil {
				return fmt.Errorf("token created (%s) but saving failed: %w", e.ID, err)
			}
			fmt.Fprintf(stderr, "saved to %s\n", p)
		}
		fmt.Fprintf(stderr, "created %s for agent %s; this is the only time the token is shown:\n", e.ID, agent)
		fmt.Fprintln(stdout, tok)
		return nil

	case "list":
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tAGENT\tCREATED\tLABEL")
		for _, e := range store.List() {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.ID, e.Agent, e.Created.Local().Format("2006-01-02 15:04"), e.Label)
		}
		return w.Flush()

	case "revoke":
		if len(pos) != 1 {
			return fmt.Errorf("token revoke: need a token id or agent name")
		}
		n, err := store.Revoke(pos[0])
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("token revoke: nothing matches %q", pos[0])
		}
		if _, ok := cfg.Agents[pos[0]]; ok {
			// Revoking an agent also removes its saved shim token.
			if err := os.Remove(cfg.AgentTokenPath(pos[0])); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(stderr, "warning: %v\n", err)
			}
		}
		fmt.Fprintf(stdout, "revoked %d token(s)\n", n)
		return nil
	}
	fmt.Fprint(stderr, tokenUsage)
	return errUsage
}
