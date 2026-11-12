package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/server"
	"github.com/trustypangolin/mcp-broker/internal/skillgen"
	"github.com/trustypangolin/mcp-broker/skills"
)

const skillUsage = `usage: mcp-broker skill                                   print the mcp-broker-api skill
       mcp-broker skill -o DIR [-format F] [-base URL]   write it (no broker needed)
       mcp-broker skill --agent NAME -o DIR [-format F] [-target broker|direct]
                        [-base URL] [-portable] [-connectors a,b] [-no-meta]
       mcp-broker skill -formats                         list the formats

The mcp-broker-api skill teaches an agent the broker's HTTP API (/api/v1): for clients
that can't use MCP (or aren't allowed to), it is all they need.

With --agent, the running broker writes a skill per connector the agent can see (it
starts them to read their tools) into DIR, laid out for -format:
  -target broker (default)  calls go to the broker's API with the agent's token
  -target direct            calls go straight to each server, with a bundled no-install
                            client, for computers where no broker runs; secrets are never
                            included, the skills name the variables to set
  -portable                 for copying elsewhere: don't name this computer's token file
  -base URL                 the API URL the skills use (e.g. a central broker)
Token and broker are found as for 'mcp-broker connect'.
`

func cmdSkill(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, skillUsage) }
	out := fs.String("o", "", "directory to write into (for Claude Code: .claude/skills's parent, i.e. the project root, with -format claude)")
	agent := fs.String("agent", "", "generate connector skills for this agent (needs the running broker)")
	format := fs.String("format", "claude", "layout: "+formatIDs())
	target := fs.String("target", "broker", "broker or direct")
	base := fs.String("base", "", "API URL the skills use (default: the broker this reaches)")
	portable := fs.Bool("portable", false, "files go to another computer: don't name the local token file")
	noMeta := fs.Bool("no-meta", false, "leave out the mcp-broker-api skill")
	conns := fs.String("connectors", "", "with --agent: only these connectors (comma-separated)")
	listFormats := fs.Bool("formats", false, "list the formats and exit")
	urlFlag := fs.String("url", "", "with --agent: broker MCP endpoint (as for connect)")
	tokenFile := fs.String("token-file", "", "with --agent: file holding the agent token")
	cfgFlag := fs.String("config", "", "broker.yaml, to locate token and URL")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fs.Usage()
		return errUsage
	}
	if *listFormats {
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		for _, f := range skillgen.Formats {
			fmt.Fprintf(tw, "%s\t%s\n", f.ID, f.Title)
		}
		return tw.Flush()
	}
	if _, ok := skillgen.FormatByID(*format); !ok {
		fmt.Fprintf(stderr, "unknown format %q (%s)\n", *format, formatIDs())
		return errUsage
	}
	if *agent != "" {
		if *out == "" {
			fmt.Fprintln(stderr, "--agent needs -o DIR")
			return errUsage
		}
		q := url.Values{"format": {*format}, "target": {*target}}
		if *base != "" {
			q.Set("base", *base)
		}
		if *portable {
			q.Set("portable", "1")
		}
		if *noMeta {
			q.Set("meta", "0")
		}
		if *conns != "" {
			q.Set("connectors", *conns)
		}
		return agentSkills(*agent, *out, q, *urlFlag, *tokenFile, *cfgFlag, stdout)
	}
	if *out == "" {
		_, err := io.WriteString(stdout, skills.BrokerAPI)
		return err
	}
	files, err := skillgen.Build(nil, skillgen.Options{Format: *format, Meta: true, APIBase: *base})
	if err != nil {
		return err
	}
	n, err := writeFiles(files, *out)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote the mcp-broker-api skill (%d files) into %s\n", n, *out)
	return nil
}

func formatIDs() string {
	var ids []string
	for _, f := range skillgen.Formats {
		ids = append(ids, f.ID)
	}
	return strings.Join(ids, ", ")
}

func agentSkills(agent, out string, q url.Values, urlFlag, tokenFile, cfgFlag string, stdout io.Writer) error {
	var cfg *config.Config
	if p, err := config.ResolvePath(cfgFlag); err == nil {
		cfg, _ = config.Load(p)
	}
	token, err := findToken(tokenFile, agent, cfg)
	if err != nil {
		return err
	}
	endpoint := strings.TrimSuffix(strings.TrimSuffix(findURL(urlFlag, cfg), "/"), "/mcp") + "/api/v1/skills?" + q.Encode()
	req, _ := http.NewRequest(http.MethodPost, endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "mcp-broker-skill")
	fmt.Fprintln(stdout, "generating skills (the broker starts each connector; this may take a minute)")
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	files, err := unzipFiles(body)
	if err != nil {
		return err
	}
	if _, err := writeFiles(files, out); err != nil {
		return err
	}
	var report []server.SkillReport
	json.Unmarshal([]byte(resp.Header.Get("X-Mcpb-Skills")), &report)
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SKILL\tTOOLS\tFROM\tNOTE")
	for _, c := range report {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", c.Skill, c.Tools, c.Source, c.Error)
	}
	tw.Flush()
	fmt.Fprintf(stdout, "wrote %d connector skills into %s (README.md there says how to install them)\n", len(report), out)
	return nil
}

// unzipFiles reads the broker's zip, refusing any path that could land outside the target.
func unzipFiles(data []byte) ([]skillgen.File, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("reading the broker's zip: %w", err)
	}
	var out []skillgen.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, skillgen.File{Path: f.Name, Data: b})
	}
	return out, nil
}

// writeFiles writes generated files under dir. The README goes in as
// mcp-broker-skills-README.md, so it can't overwrite a project's own README.
func writeFiles(files []skillgen.File, dir string) (int, error) {
	for _, f := range files {
		name := path.Clean(f.Path)
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, `\`) || strings.Contains(name, ":") {
			return 0, fmt.Errorf("unexpected path in the skills: %q", f.Path)
		}
		if name == "README.md" {
			name = "mcp-broker-skills-README.md"
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(dst, f.Data, 0o644); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}
