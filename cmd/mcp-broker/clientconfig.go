package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/trustypangolin/mcp-broker/internal/clientcfg"
	"github.com/trustypangolin/mcp-broker/internal/config"
)

const clientConfigUsage = `usage: mcp-broker client-config <harness|all|list> [--agent NAME] [--http] [--url URL]
                                 [--wsl] [--exe PATH] [--config PATH]

Prints the config snippet an MCP client needs to use the broker. Nothing is written: paste
the snippet into the file named above it. Snippets never contain a token.

Harnesses: claude-code, claude-desktop, vscode, codex, cursor, gemini, windsurf

  --agent NAME  agent the client connects as (default: the harness name, e.g. codex)
  --http        connect over Streamable HTTP with a bearer token from $MCP_BROKER_TOKEN,
                instead of the stdio shim (mcp-broker connect)
  --url URL     broker MCP endpoint for --http (default: from broker.yaml)
  --wsl         the client runs in WSL: run the shim by its /mnt/<drive>/ path
  --exe PATH    mcp-broker executable for the shim (default: this one)
`

func cmdClientConfig(args []string, stdout, stderr io.Writer) error {
	// Allow the harness before or after the flags.
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	fs := flag.NewFlagSet("client-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, clientConfigUsage) }
	agent := fs.String("agent", "", "agent name")
	useHTTP := fs.Bool("http", false, "connect over Streamable HTTP")
	urlFlag := fs.String("url", "", "broker MCP endpoint")
	wsl := fs.Bool("wsl", false, "client runs in WSL")
	exeFlag := fs.String("exe", "", "mcp-broker executable")
	cfgFlag := fs.String("config", "", "broker.yaml")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != 1 {
		fmt.Fprint(stderr, clientConfigUsage)
		return errUsage
	}

	if pos[0] == "list" {
		for _, h := range clientcfg.Harnesses() {
			transport := "stdio, http"
			if !h.HTTP {
				transport = "stdio"
			}
			fmt.Fprintf(stdout, "%-15s %-15s %-12s %s\n", h.ID, h.Title, transport, h.File)
		}
		return nil
	}
	var hs []clientcfg.Harness
	if pos[0] == "all" {
		for _, h := range clientcfg.Harnesses() {
			if !*useHTTP || h.HTTP {
				hs = append(hs, h)
			}
		}
	} else {
		h, ok := clientcfg.Find(pos[0])
		if !ok {
			return fmt.Errorf("unknown harness %q; try mcp-broker client-config list", pos[0])
		}
		hs = []clientcfg.Harness{h}
	}

	var cfg *config.Config
	if p, err := config.ResolvePath(*cfgFlag); err == nil {
		cfg, _ = config.Load(p)
	}
	exe := *exeFlag
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
	}
	if *wsl {
		exe = clientcfg.WSLPath(exe)
	}
	target := clientcfg.Target{Exe: exe, Agent: *agent, HTTP: *useHTTP}
	if *useHTTP {
		target.URL = findURL(*urlFlag, cfg)
	}

	for i, h := range hs {
		s, err := clientcfg.Render(h, target)
		if err != nil {
			return err
		}
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		writeSnippet(stdout, s)
		warnAgent(stderr, cfg, s)
	}
	return nil
}

func writeSnippet(w io.Writer, s clientcfg.Snippet) {
	how := "stdio shim"
	if s.HTTP {
		how = "Streamable HTTP"
	}
	c := "//" // JSON files don't take comments, so headings are clearly outside the snippet
	if s.Format == "toml" {
		c = "#"
	}
	fmt.Fprintf(w, "%s %s: agent %s, %s\n%s File: %s\n", c, s.Harness.Title, s.Agent, how, c, s.Harness.File)
	fmt.Fprint(w, s.Text)
	if s.Command != "" {
		fmt.Fprintf(w, "%s Or run: %s\n", c, s.Command)
	}
	for _, n := range s.Notes {
		fmt.Fprintf(w, "%s %s\n", c, n)
	}
}

// warnAgent points out what the snippet still needs on the broker side.
func warnAgent(w io.Writer, cfg *config.Config, s clientcfg.Snippet) {
	if cfg == nil {
		return
	}
	if _, ok := cfg.Agents[s.Agent]; !ok {
		fmt.Fprintf(w, "note: agent %s isn't in %s yet; add it (dashboard Agents tab) or the client connects as anonymous and sees nothing\n", s.Agent, cfg.Path)
		return
	}
	if !s.HTTP {
		if _, err := os.Stat(cfg.AgentTokenPath(s.Agent)); err != nil {
			fmt.Fprintf(w, "note: no saved token for %s; run: mcp-broker token create %s --save\n", s.Agent, s.Agent)
		}
	}
}
