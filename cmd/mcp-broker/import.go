package main

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/importer"
)

const importUsage = `usage: mcp-broker import [--from PATH]... [--apply] [--switch] [--overwrite] [--tag NAME=T1,T2]...

Reads MCP servers from client configs (by default Claude Desktop, Claude Code's
~/.claude.json and VS Code's user mcp.json) and shows what importing them would do.
  --apply      add them to broker.yaml; secrets go to secrets\<name>.env next to it
  --switch     then point each client at the broker (original kept as .bak-<time>);
               for Claude Code, prints the claude mcp commands to run instead
`

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func cmdImport(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, importUsage); fs.PrintDefaults() }
	cfgFlag := fs.String("config", "", "config file")
	var from, tags listFlag
	fs.Var(&from, "from", "client config to read (repeatable)")
	fs.Var(&tags, "tag", "NAME=TAG1,TAG2: tags for an imported connector instead of the guessed ones (repeatable)")
	apply := fs.Bool("apply", false, "write the connectors to broker.yaml")
	switchClients := fs.Bool("switch", false, "point the clients at the broker afterwards (needs --apply unless everything is already imported)")
	overwrite := fs.Bool("overwrite", false, "replace existing connectors that have the same name")
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

	var sources []importer.Source
	for _, p := range from {
		sources = append(sources, importer.SourceFor(p))
	}
	if len(from) == 0 {
		sources = importer.DefaultSources()
	}
	if len(sources) == 0 {
		return fmt.Errorf("no client configs found; name one with --from")
	}
	opts := importer.Options{Overwrite: *overwrite, Tags: map[string][]string{}}
	for _, t := range tags {
		name, list, ok := strings.Cut(t, "=")
		if !ok {
			return fmt.Errorf("--tag %q: want NAME=TAG1,TAG2", t)
		}
		opts.Tags[name] = strings.Split(list, ",")
	}

	var servers []importer.Server
	for _, src := range sources {
		s, err := importer.Read(src)
		if err != nil {
			fmt.Fprintf(stderr, "skipping %s: %v\n", src.Path, err)
			continue
		}
		servers = append(servers, s...)
	}
	items := importer.Plan(cfg, servers, opts)
	printPlan(stdout, sources, items)

	if !*apply && !*switchClients {
		fmt.Fprintln(stdout, "\nNothing changed. Run again with --apply to add the connectors, and --switch to point the clients at the broker.")
		return nil
	}
	if *apply {
		n, err := importer.Apply(cfg, items)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nAdded %d connector(s) to %s. A running broker picks them up by itself.\n", n, cfg.Path)
		if cfg, err = config.Load(path); err != nil {
			return err
		}
	} else if slices.ContainsFunc(items, importer.Item.Imported) {
		return fmt.Errorf("--switch without --apply would leave those servers nowhere; add --apply")
	}
	if *switchClients {
		return switchAll(stdout, stderr, cfg, sources, items)
	}
	return nil
}

func printPlan(w io.Writer, sources []importer.Source, items []importer.Item) {
	for _, src := range sources {
		fmt.Fprintf(w, "\n%s (%s)\n", src.Path, src.Client)
		any := false
		for _, it := range items {
			if it.Server.Source.Path != src.Path {
				continue
			}
			any = true
			switch {
			case it.Skip != "":
				fmt.Fprintf(w, "  -  %-18s skipped: %s\n", it.Server.Name, it.Skip)
				continue
			case it.Existing:
				label := it.Name
				if it.Server.Name != it.Name {
					label = it.Server.Name + " (as " + it.Name + ")"
				}
				fmt.Fprintf(w, "  =  %-18s already in broker.yaml\n", label)
				continue
			}
			what := it.Entry.URL
			if what == "" {
				what = strings.TrimSpace(it.Entry.Command + " " + strings.Join(it.Entry.Args, " "))
			}
			if len(what) > 70 {
				what = what[:69] + "…"
			}
			fmt.Fprintf(w, "  +  %-18s %s\n", it.Name, what)
			if len(it.Entry.Tags) > 0 {
				fmt.Fprintf(w, "       tags: %s\n", strings.Join(it.Entry.Tags, ", "))
			}
			if len(it.Secrets) > 0 {
				fmt.Fprintf(w, "       secrets to %s: %s\n", it.EnvFile, strings.Join(slices.Sorted(maps.Keys(it.Secrets)), ", "))
			}
			seen := "no agent (add it to a role)"
			if len(it.Visible) > 0 {
				seen = strings.Join(it.Visible, ", ")
			}
			fmt.Fprintf(w, "       visible to: %s\n", seen)
			for _, n := range it.Notes {
				fmt.Fprintf(w, "       note: %s\n", n)
			}
		}
		if !any {
			fmt.Fprintln(w, "  (no servers)")
		}
	}
}

func switchAll(stdout, stderr io.Writer, cfg *config.Config, sources []importer.Source, items []importer.Item) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	store, err := identity.OpenStore(cfg.Server.TokensFile)
	if err != nil {
		return err
	}
	for _, src := range sources {
		agent := importer.AgentFor(src.Client)
		if _, ok := cfg.Agents[agent]; !ok {
			fmt.Fprintf(stderr, "not switching %s: there is no agent %q in %s\n", src.Path, agent, cfg.Path)
			continue
		}
		// The shim needs a saved token for the agent.
		if _, err := os.Stat(cfg.AgentTokenPath(agent)); err != nil {
			tok, e, err := store.Create(agent, "import --switch")
			if err == nil {
				err = identity.SaveToken(cfg.AgentTokenPath(agent), tok)
			}
			if err != nil {
				return fmt.Errorf("creating a token for %s: %w", agent, err)
			}
			fmt.Fprintf(stdout, "\ncreated token %s for %s, saved for the shim\n", e.ID, agent)
		}
		res, err := importer.Switch(src, items, exe)
		if err != nil {
			fmt.Fprintf(stderr, "not switching %s: %v\n", src.Path, err)
			continue
		}
		if len(res.Commands) > 0 {
			fmt.Fprintf(stdout, "\n%s is not edited. To switch Claude Code, run:\n", src.Path)
			for _, c := range res.Commands {
				fmt.Fprintln(stdout, "  "+c)
			}
			continue
		}
		fmt.Fprintf(stdout, "\nswitched %s to the broker (removed %s); the original is %s. Restart the client.\n",
			src.Path, strings.Join(res.Removed, ", "), res.Backup)
	}
	return nil
}
