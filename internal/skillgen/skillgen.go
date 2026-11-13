// Package skillgen turns connectors into agent skills and lays them out the way each kind of
// agent expects them: Claude and Codex skill folders, a zip per skill for claude.ai upload,
// Cursor and Windsurf rules, Copilot instructions, and AGENTS.md / GEMINI.md.
//
// A skill reaches its connector one of two ways (Target):
//   - broker: over a running mcp-broker's HTTP API (/api/v1), with an agent token. The
//     broker keeps the secrets and applies policy, approvals and audit.
//   - direct: straight to the connector's own MCP server, for computers where no broker
//     runs. The skill carries a server.json (with ${VAR} placeholders where secrets go,
//     never their values) and a no-install client, mcp_call.py / mcp_call.ps1.
//
// The package only renders: the gateway gathers the connectors (and their tools, as the
// agent's policy filters them) and hands them over.
package skillgen

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/trustypangolin/mcp-broker/skills"
)

//go:embed scripts/mcp_call.py scripts/mcp_call.ps1
var scripts embed.FS

// Target is where a skill's calls go.
type Target string

const (
	TargetBroker Target = "broker"
	TargetDirect Target = "direct"
)

// Tool is one tool as a skill describes it, already filtered by the agent's policy.
type Tool struct {
	Name, Summary, Description string
	Required                   []string
	Schema                     any // nil when only a cached list is known
	Class                      string
	Approval                   bool // the broker would ask the user first
}

// Direct is how to reach a connector's own server, for TargetDirect. Values may hold
// ${VAR} references, resolved from the environment on the computer that runs it; secret
// values are never here.
type Direct struct {
	Transport string            `json:"transport"` // stdio or http
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	// Vars are the environment variables the user must set: the ${VAR}s above that
	// server.json doesn't define itself.
	Vars []string `json:"-"`
	// OAuth: the server needs an OAuth sign-in, which mcp_call can't do.
	OAuth bool `json:"-"`
}

// Connector is everything a skill says about one connector.
type Connector struct {
	Name, Description string
	Aliases           []string
	Instructions      string // the server's own, from initialize
	Tools             []Tool
	Source            string // live, cache or none: where Tools came from
	Err               string // why not live
	Direct            *Direct
}

// Options picks what Build makes.
type Options struct {
	Target Target
	Format string // a Formats ID; empty means claude
	Agent  string // the agent the skills are for (broker target)
	// APIBase is the broker's /api/v1 URL the skills default to (broker target).
	APIBase string
	// TokenFile is where the agent's token is saved on this computer; empty for files
	// that go elsewhere (the skill then reads $MCP_BROKER_TOKEN only).
	TokenFile string
	// Meta adds the mcp-broker-api skill (broker target only).
	Meta bool
}

// File is one output file, at a slash-separated path.
type File struct {
	Path string
	Data []byte
}

// FormatInfo describes an output format.
type FormatInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Install says where the files go, for the README and the dashboard.
	Install string `json:"install"`
}

// Formats are the layouts Build can produce.
var Formats = []FormatInfo{
	{"claude", "Claude Code / Agent SDK skills", "Copy `.claude/skills/` into the project root, or its folders into `~/.claude/skills/` for every project."},
	{"claude-upload", "claude.ai / Claude Desktop (one zip per skill)", "In Claude, open Settings → Capabilities → Skills and upload each `.zip` as a skill. Don't unzip them."},
	{"codex", "Codex skills", "Copy `.codex/skills/` into the project root, or its folders into `~/.codex/skills/`."},
	{"agents-md", "AGENTS.md (Codex, Jules, Amp, other agents)", "Put `mcp-broker/` in the project root and add `AGENTS.md`'s section to the project's AGENTS.md (or use it as is)."},
	{"cursor", "Cursor rules", "Copy `.cursor/rules/` and `mcp-broker/` into the project root."},
	{"windsurf", "Windsurf rules", "Copy `.windsurf/rules/` and `mcp-broker/` into the project root."},
	{"copilot", "GitHub Copilot instructions", "Copy `.github/instructions/` and `mcp-broker/` into the project root."},
	{"gemini", "Gemini CLI (GEMINI.md)", "Put `mcp-broker/` in the project root and add `GEMINI.md`'s section to the project's GEMINI.md (or use it as is)."},
}

// FormatByID returns the format, or false.
func FormatByID(id string) (FormatInfo, bool) {
	if id == "" {
		id = "claude"
	}
	for _, f := range Formats {
		if f.ID == id {
			return f, true
		}
	}
	return FormatInfo{}, false
}

// DefaultAPIBase is the loopback API URL; the meta-skill is written with it.
const DefaultAPIBase = "http://127.0.0.1:8765/api/v1"

// skill is a rendered skill before layout. Body and asset text use {{assets}} for the
// path of the skill's own files and {{table}} for the tool table.
type skill struct {
	name, description string
	body, table       string
	assets            []File
}

// Build renders conns (and the meta-skill when asked) in the format, plus a README.md.
func Build(conns []Connector, o Options) ([]File, error) {
	f, ok := FormatByID(o.Format)
	if !ok {
		return nil, fmt.Errorf("unknown format %q", o.Format)
	}
	if o.Target == "" {
		o.Target = TargetBroker
	}
	if o.Target != TargetBroker && o.Target != TargetDirect {
		return nil, fmt.Errorf("unknown target %q (broker or direct)", o.Target)
	}
	if o.APIBase == "" {
		o.APIBase = DefaultAPIBase
	}
	var sks []skill
	if o.Meta && o.Target == TargetBroker {
		sks = append(sks, metaSkill(o.APIBase))
	}
	names := SkillNames(conns)
	for i, c := range conns {
		sks = append(sks, connectorSkill(c, names[i], o))
	}
	files := layout(f.ID, sks)
	files = append(files, File{Path: "README.md", Data: []byte(readme(f, conns, names, o))})
	return files, nil
}

// Zip packs files into a zip archive.
func Zip(files []File) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	now := time.Now()
	for _, f := range files {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.Path, Method: zip.Deflate, Modified: now})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---- layout ----

// tableInline is how big a tool table may be inside a rule file before it moves out to
// TOOLS.md; rule files are loaded whole, and Windsurf caps their size.
const tableInline = 4000

func layout(format string, sks []skill) []File {
	var out []File
	switch format {
	case "claude", "codex":
		dir := ".claude/skills/"
		if format == "codex" {
			dir = ".codex/skills/"
		}
		for _, s := range sks {
			out = append(out, folderSkill(dir+s.name+"/", s)...)
		}
	case "claude-upload":
		for _, s := range sks {
			z, _ := Zip(folderSkill(s.name+"/", s))
			out = append(out, File{Path: s.name + ".zip", Data: z})
		}
	case "cursor", "windsurf":
		for _, s := range sks {
			assets := "mcp-broker/" + s.name + "/"
			body, extra := placeTable(s, assets, len(s.table) > tableInline)
			var front, path string
			if format == "cursor" {
				front = "---\ndescription: " + yamlString(s.description) + "\nglobs:\nalwaysApply: false\n---\n\n"
				path = ".cursor/rules/" + s.name + ".mdc"
			} else {
				front = "---\ntrigger: model_decision\ndescription: " + yamlString(s.description) + "\n---\n\n"
				path = ".windsurf/rules/" + s.name + ".md"
			}
			out = append(out, File{Path: path, Data: []byte(front + rootNote + fill(body, assets))})
			out = append(out, assetFiles(assets, s.assets, extra)...)
		}
	case "copilot":
		for _, s := range sks {
			assets := "mcp-broker/" + s.name + "/"
			front := "---\napplyTo: \"**\"\ndescription: " + yamlString(s.description) + "\n---\n\n"
			ptr := fmt.Sprintf("# %s\n\nWhen to use: %s\n\nBefore using it, read `%sSKILL.md` (paths are relative to the project root). It says how to call it and lists its tools.\n", s.name, s.description, assets)
			out = append(out, File{Path: ".github/instructions/" + s.name + ".instructions.md", Data: []byte(front + ptr)})
			out = append(out, pointedSkill(assets, s)...)
		}
	case "agents-md", "gemini":
		file := "AGENTS.md"
		if format == "gemini" {
			file = "GEMINI.md"
		}
		var b strings.Builder
		b.WriteString("## Tools through mcp-broker\n\n")
		b.WriteString("Each entry below is a skill: when a task matches its description, read its SKILL.md first (paths are relative to the project root). It says how to call the tools and lists them.\n\n")
		for _, s := range sks {
			assets := "mcp-broker/" + s.name + "/"
			fmt.Fprintf(&b, "- **%s** (`%sSKILL.md`): %s\n", s.name, assets, s.description)
			out = append(out, pointedSkill(assets, s)...)
		}
		out = append([]File{{Path: file, Data: []byte(b.String())}}, out...)
	}
	return out
}

const rootNote = "> Paths in this rule are relative to the project root.\n\n"

// folderSkill is a skill as a folder: SKILL.md with name and description frontmatter, its
// files beside it.
func folderSkill(dir string, s skill) []File {
	body, extra := placeTable(s, "", false)
	front := "---\nname: " + s.name + "\ndescription: " + yamlString(s.description) + "\n---\n\n"
	if slices.ContainsFunc(s.assets, func(f File) bool { return strings.HasPrefix(f.Path, "scripts/") }) {
		front += "> Paths of this skill's own files (`server.json`, `scripts/`, `tools/`) are relative to the folder this SKILL.md is in.\n\n"
	}
	out := []File{{Path: dir + "SKILL.md", Data: []byte(front + fill(body, ""))}}
	return append(out, assetFiles(dir, s.assets, extra)...)
}

// pointedSkill is a skill an index file points at: mcp-broker/<name>/SKILL.md plus files,
// with paths written from the project root.
func pointedSkill(assets string, s skill) []File {
	body, extra := placeTable(s, assets, false)
	front := "---\nname: " + s.name + "\ndescription: " + yamlString(s.description) + "\n---\n\n"
	out := []File{{Path: assets + "SKILL.md", Data: []byte(front + rootNote + fill(body, assets))}}
	return append(out, assetFiles(assets, s.assets, extra)...)
}

// placeTable puts the tool table into the body, or (out) into a TOOLS.md beside it.
func placeTable(s skill, assets string, out bool) (string, []File) {
	if s.table == "" {
		return strings.Replace(s.body, "{{table}}", "", 1), nil
	}
	if !out {
		return strings.Replace(s.body, "{{table}}", s.table, 1), nil
	}
	link := "The tool list is in [TOOLS.md]({{assets}}TOOLS.md): read it to pick a tool.\n"
	return strings.Replace(s.body, "{{table}}", link, 1), []File{{Path: "TOOLS.md", Data: []byte("# Tools\n\n" + s.table)}}
}

func assetFiles(prefix string, assets, extra []File) []File {
	var out []File
	for _, a := range append(slices.Clone(assets), extra...) {
		data := a.Data
		if strings.HasSuffix(a.Path, ".md") {
			data = []byte(fill(string(data), prefix))
		}
		out = append(out, File{Path: prefix + a.Path, Data: data})
	}
	return out
}

func fill(s, assets string) string { return strings.ReplaceAll(s, "{{assets}}", assets) }

// ---- meta-skill ----

// metaSkill is the mcp-broker-api skill, pointed at apiBase.
func metaSkill(apiBase string) skill {
	text := skills.BrokerAPI
	desc := ""
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		if i := strings.Index(rest, "\n---\n"); i >= 0 {
			for _, line := range strings.Split(rest[:i], "\n") {
				if v, ok := strings.CutPrefix(line, "description: "); ok {
					desc = v
				}
			}
			text = strings.TrimLeft(rest[i+5:], "\n")
		}
	}
	if apiBase != DefaultAPIBase {
		text = strings.ReplaceAll(text, DefaultAPIBase, apiBase)
	}
	return skill{name: "mcp-broker-api", description: desc, body: text}
}

// ---- connector skills ----

func connectorSkill(c Connector, name string, o Options) skill {
	s := skill{name: name, description: description(c, o.Target)}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", c.Name)
	if d := strings.TrimSpace(c.Description); d != "" {
		b.WriteString(d + "\n\n")
	}
	if len(c.Aliases) > 0 {
		b.WriteString("Also called: " + strings.Join(c.Aliases, ", ") + ".\n\n")
	}
	if ins := strings.TrimSpace(c.Instructions); ins != "" {
		b.WriteString("## What the server says about itself\n\n" + clip(ins, 3000) + "\n\n")
	}
	if o.Target == TargetDirect {
		directSection(&b, c, &s)
	} else {
		brokerSection(&b, c, o)
	}
	b.WriteString("## Tools\n\n")
	switch {
	case len(c.Tools) == 0 && c.Source == "none":
		if o.Target == TargetDirect {
			b.WriteString("These files don't list the tools (the broker never saw them). Run the client with `--list` (above) to see them.\n")
		} else {
			fmt.Fprintf(&b, "The broker hasn't seen this connector's tools yet. List them with `curl -s -H \"Authorization: Bearer $TOKEN\" \"$API/connectors/%s/tools\"` and get one tool's schema with `\"$API/connectors/%s/tools/TOOL\"`.\n", c.Name, c.Name)
		}
		s.body = b.String()
		return s
	case len(c.Tools) == 0:
		b.WriteString("None of this connector's tools are available to this agent.\n")
		s.body = b.String()
		return s
	}
	if c.Source == "cache" {
		if o.Target == TargetDirect {
			b.WriteString("This list is from when the connector last ran, without input schemas. Run the client with `--list` to get them.\n\n")
		} else {
			fmt.Fprintf(&b, "This list is from when the connector last ran, without input schemas. Get a tool's schema before calling it: `curl -s -H \"Authorization: Bearer $TOKEN\" \"$API/connectors/%s/tools/TOOL\"`.\n\n", c.Name)
		}
	}
	if o.Target == TargetDirect {
		b.WriteString("Tools marked **approval** are ones the broker would have asked the user about: ask the user and wait for a yes before calling one.\n\n")
	} else {
		b.WriteString("Tools marked **approval** wait for the user (HTTP 202, above).\n\n")
	}
	b.WriteString("Before calling a tool the first time, read its file (linked in the table): full description, example arguments and input schema.\n\n")
	b.WriteString("{{table}}")
	s.body = b.String()

	var t strings.Builder
	t.WriteString("| Tool | What it does | Required arguments | Class |\n|---|---|---|---|\n")
	files := map[string]bool{}
	for _, tl := range c.Tools {
		file := ToolFile(tl.Name, files)
		class := tl.Class
		if tl.Approval {
			class += ", **approval**"
		}
		args := slices.Clone(tl.Required)
		for i, a := range args {
			args[i] = "`" + a + "`"
		}
		fmt.Fprintf(&t, "| [`%s`]({{assets}}%s) | %s | %s | %s |\n", tl.Name, file, mdCell(tl.Summary), strings.Join(args, ", "), class)
		s.assets = append(s.assets, File{Path: file, Data: []byte(toolDoc(c, name, tl, o.Target))})
	}
	s.table = t.String()
	return s
}

// description is the frontmatter description: what makes an agent pick the skill.
func description(c Connector, target Target) string {
	var b strings.Builder
	d := strings.TrimSpace(c.Description)
	if d == "" {
		d = "The " + c.Name + " connector"
	}
	b.WriteString(strings.TrimRight(d, ". ") + ".")
	if len(c.Aliases) > 0 {
		b.WriteString(" Also called: " + strings.Join(c.Aliases, ", ") + ".")
	}
	if target == TargetDirect {
		fmt.Fprintf(&b, " Use for any task involving %s; its tools are called directly with a bundled script, no MCP client needed.", c.Name)
	} else {
		fmt.Fprintf(&b, " Use for any task involving %s; its tools are called over mcp-broker's HTTP API, no MCP client needed.", c.Name)
	}
	if n := len(c.Tools); n > 0 {
		b.WriteString(" Tools: ")
		for i, t := range c.Tools {
			next := t.Name
			if i > 0 {
				next = ", " + next
			}
			if b.Len()+len(next) > maxDescription-20 {
				fmt.Fprintf(&b, ", +%d more", n-i)
				break
			}
			b.WriteString(next)
		}
		b.WriteString(".")
	}
	return clip(b.String(), maxDescription)
}

const maxDescription = 1000

func brokerSection(b *strings.Builder, c Connector, o Options) {
	b.WriteString("## How to call a tool\n\n")
	who := "the agent's"
	if o.Agent != "" {
		who = fmt.Sprintf("agent `%s`'s", o.Agent)
	}
	fmt.Fprintf(b, "Every tool is one HTTP request to mcp-broker with %s token. No MCP client is involved.\n\n", who)
	b.WriteString("```bash\n")
	fmt.Fprintf(b, "API=\"${MCP_BROKER_API:-%s}\"\n", o.APIBase)
	if o.TokenFile != "" {
		fmt.Fprintf(b, "TOKEN=\"${MCP_BROKER_TOKEN:-$(cat '%s')}\"   # never print it\n", o.TokenFile)
	} else {
		b.WriteString("TOKEN=\"$MCP_BROKER_TOKEN\"   # never print it or write it into a file\n")
	}
	fmt.Fprintf(b, "curl -s -H \"Authorization: Bearer $TOKEN\" -H \"Content-Type: application/json\" \\\n  -d '{\"arguments\": {...}}' \"$API/connectors/%s/tools/TOOL/call\"\n", c.Name)
	b.WriteString("```\n\n")
	if o.TokenFile == "" {
		b.WriteString("- If `MCP_BROKER_TOKEN` isn't set, ask the user for the token (the broker's owner issues it). Don't save it anywhere.\n")
	}
	b.WriteString("- Put the tool's arguments under `\"arguments\"`. For long or awkward JSON, write the body to a file and send `--data-binary @body.json`.\n")
	b.WriteString("- The connector doesn't have to be running: the broker starts it on the first call, which can take a few seconds.\n")
	b.WriteString("- The answer is JSON. Read `content[].text` (some tools also return `image` blocks with base64 `data`). `\"isError\": true` means the call failed; the text says why.\n")
	b.WriteString("- **HTTP 202** means the call is waiting for the user's approval. Tell the user it is waiting in the mcp-broker tray or dashboard, wait with `curl -s -H \"Authorization: Bearer $TOKEN\" \"$API/approvals/APPROVAL_ID?wait=60\"` until `structuredContent.status` is `approved` (while it is `pending`, pause `retry_after_seconds` between checks), then send the identical call again with `\"approval_id\": \"APPROVAL_ID\"` next to `\"arguments\"`. `denied` or `expired`: stop and ask the user.\n")
	b.WriteString("- 401: the token is missing or wrong. 403: this computer or program isn't allowed to use that token. Connection refused: the broker isn't running or isn't reachable at `$API`. Tell the user rather than working around it.\n")
	b.WriteString("- In Windows PowerShell use `Invoke-RestMethod` (not `curl`), and build the body with `ConvertTo-Json -Depth 20`.\n")
	b.WriteString("- The `mcp-broker-api` skill has the whole API, if it's installed.\n\n")
}

func directSection(b *strings.Builder, c Connector, s *skill) {
	d := c.Direct
	if d == nil {
		d = &Direct{}
	}
	b.WriteString("## How to call a tool\n\n")
	fmt.Fprintf(b, "Calls go **straight to the %s server**, without mcp-broker, so no broker policy, approvals or audit apply.\n\n", c.Name)
	if d.Transport == "http" {
		fmt.Fprintf(b, "The server is at `%s`. ", d.URL)
	} else {
		fmt.Fprintf(b, "The server is a program that runs on this computer: `%s`. If it's installed somewhere else here, fix the path in `{{assets}}server.json`. ", strings.TrimSpace(d.Command+" "+strings.Join(d.Args, " ")))
	}
	b.WriteString("Its settings are in [server.json]({{assets}}server.json).\n\n")
	if d.OAuth {
		b.WriteString("**This server needs an OAuth sign-in, which the bundled client can't do.** Use it through mcp-broker instead (download its skill with \"through a broker\").\n\n")
	}
	if len(d.Vars) > 0 {
		b.WriteString("Set these environment variables first. Their values aren't in these files (some are secrets); ask the user for them and never write them into a file: ")
		for i, v := range d.Vars {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("`" + v + "`")
		}
		b.WriteString(".\n\n")
	}
	b.WriteString("Call a tool with the bundled client. Nothing to install: it needs Python 3.8+, or Windows PowerShell 5.1.\n\n")
	b.WriteString("```bash\n")
	b.WriteString("python {{assets}}scripts/mcp_call.py --server {{assets}}server.json --call TOOL --args '{\"name\": \"value\"}'\n")
	b.WriteString("```\n\n")
	b.WriteString("```powershell\n")
	b.WriteString("& '{{assets}}scripts/mcp_call.ps1' -Server '{{assets}}server.json' -Call TOOL -ArgsJson '@args.json'\n")
	b.WriteString("```\n\n")
	b.WriteString("- `--list` (`-List`) instead of `--call` lists the tools with their schemas.\n")
	b.WriteString("- Pass long or awkward JSON as a file: `--args @args.json`. In PowerShell always use a file (`-ArgsJson '@args.json'`): Windows PowerShell strips quotes from JSON passed on a command line.\n")
	b.WriteString("- **Each run starts the server and stops it again**, so state such as an open document doesn't carry over. Put calls that build on each other in a steps file, run in order on one server: `--steps steps.json` (`-Steps`), where the file is `[{\"tool\": \"a\", \"arguments\": {...}}, {\"tool\": \"b\", \"arguments\": {...}}]`.\n")
	b.WriteString("- It prints the server's JSON-RPC response: read `result.content[].text`; `result.isError: true` means the tool failed. Exit status 0 ok, 2 tool error, 1 anything else (the message is on stderr). If PowerShell refuses to run the script (execution policy), use the Python one.\n\n")
	srv := struct {
		Name string `json:"name"`
		*Direct
	}{c.Name, d}
	s.assets = append(s.assets,
		File{Path: "server.json", Data: []byte(plainJSON(srv, "  ") + "\n")},
		File{Path: "scripts/mcp_call.py", Data: script("scripts/mcp_call.py")},
		File{Path: "scripts/mcp_call.ps1", Data: script("scripts/mcp_call.ps1")})
}

func script(name string) []byte {
	b, _ := scripts.ReadFile(name)
	return b
}

// toolDoc is one tool's file.
func toolDoc(c Connector, skillName string, t Tool, target Target) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s / %s\n\n", c.Name, t.Name)
	if target == TargetDirect {
		fmt.Fprintf(&b, "Call it with the bundled client: `--call %s --args '{...}'` (setup is in the `%s` skill).\n\n", t.Name, skillName)
	} else {
		fmt.Fprintf(&b, "`POST $API/connectors/%s/tools/%s/call` with the body `{\"arguments\": {...}}` (the curl line and the token are in the `%s` skill).\n\n", c.Name, t.Name, skillName)
	}
	fmt.Fprintf(&b, "Class: %s", t.Class)
	if t.Approval {
		if target == TargetDirect {
			b.WriteString(" · **the broker would ask the user first**: ask them and wait for a yes")
		} else {
			b.WriteString(" · **needs the user's approval**: expect HTTP 202 first, then retry with `approval_id`")
		}
	}
	b.WriteString("\n\n")
	if d := strings.TrimSpace(t.Description); d != "" {
		b.WriteString(d + "\n\n")
	}
	if t.Schema == nil {
		if target == TargetDirect {
			b.WriteString("The input schema wasn't available when this was generated: run the client with `--list` to get it.\n")
		} else {
			fmt.Fprintf(&b, "The input schema wasn't available when this was generated. Get it before calling: `GET $API/connectors/%s/tools/%s`.\n", c.Name, t.Name)
		}
		return b.String()
	}
	ex := ExampleArgs(t.Schema)
	if target == TargetDirect {
		fmt.Fprintf(&b, "Example arguments: `%s`\n\n", plainJSON(ex, ""))
	} else {
		fmt.Fprintf(&b, "Example body: `%s`\n\n", plainJSON(map[string]any{"arguments": ex}, ""))
	}
	if schema := plainJSON(t.Schema, "  "); schema != "" {
		b.WriteString("Input schema:\n\n```json\n" + schema + "\n```\n")
	}
	return b.String()
}

// ---- README ----

func readme(f FormatInfo, conns []Connector, names []string, o Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# mcp-broker skills: %s\n\n", f.Title)
	fmt.Fprintf(&b, "Generated %s", time.Now().Format("2006-01-02 15:04"))
	if o.Agent != "" {
		fmt.Fprintf(&b, " for agent `%s`", o.Agent)
	}
	b.WriteString(".\n\n## Install\n\n" + f.Install + "\n\n## Before use\n\n")
	if o.Target == TargetDirect {
		b.WriteString("These skills call each server **directly**; no broker is involved, so its policies, approvals and audit log don't apply. ")
		b.WriteString("Each needs its server installed on this computer (local programs) or reachable (URLs), and the environment variables its skill lists. ")
		b.WriteString("Nothing secret is in these files: the variables hold the secrets, set on the computer that uses them. ")
		b.WriteString("The bundled client needs Python 3.8+ or Windows PowerShell 5.1.\n\n")
	} else {
		fmt.Fprintf(&b, "These skills call mcp-broker at `%s` (set `MCP_BROKER_API` to use another address), which must be running and reachable. ", o.APIBase)
		if o.TokenFile != "" {
			b.WriteString("They read the agent's token from `MCP_BROKER_TOKEN`, or the token file saved on this computer.\n\n")
		} else {
			b.WriteString("Set `MCP_BROKER_TOKEN` to the agent's token where the agent runs; the broker's owner issues it (`mcp-broker token create <agent>`). The token is not in these files.\n\n")
		}
	}
	b.WriteString("## Skills\n\n| Skill | Connector | Tools | From | Note |\n|---|---|---|---|---|\n")
	if o.Meta && o.Target == TargetBroker {
		b.WriteString("| mcp-broker-api | (all) | | | the HTTP API itself |\n")
	}
	for i, c := range conns {
		note := c.Err
		if c.Direct != nil && c.Direct.OAuth {
			note = "needs an OAuth sign-in: use it through a broker"
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n", names[i], c.Name, len(c.Tools), c.Source, mdCell(note))
	}
	return b.String()
}

// ---- helpers ----

// SkillNames is the skill name Build gives each connector, in order.
func SkillNames(conns []Connector) []string {
	used := map[string]bool{"mcp-broker-api": true}
	out := make([]string, len(conns))
	for i, c := range conns {
		out[i] = SkillName(c.Name, used)
	}
	return out
}

var nonSkill = regexp.MustCompile(`[^a-z0-9]+`)

// SkillName is a connector's skill name and folder: lowercase letters, digits and hyphens
// (what agent harnesses accept), prefixed broker- so they sort together and can't clash
// with the user's own skills; used keeps two connectors that slug alike distinct.
func SkillName(connector string, used map[string]bool) string {
	s := strings.Trim(nonSkill.ReplaceAllString(strings.ToLower(connector), "-"), "-")
	if s == "" {
		s = "connector"
	}
	base := "broker-" + s
	if len(base) > 60 {
		base = strings.TrimRight(base[:60], "-")
	}
	name := base
	for i := 2; used[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used[name] = true
	return name
}

var nonFile = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// ToolFile is a tool's file in a skill, tools/<name>.md, the name made safe for every
// filesystem; used keeps apart tools that clean up alike or differ only in case.
func ToolFile(tool string, used map[string]bool) string {
	s := strings.Trim(nonFile.ReplaceAllString(tool, "_"), "._")
	if s == "" {
		s = "tool"
	}
	if len(s) > 100 {
		s = s[:100]
	}
	name := s
	for i := 2; used[strings.ToLower(name)]; i++ {
		name = fmt.Sprintf("%s_%d", s, i)
	}
	used[strings.ToLower(name)] = true
	return "tools/" + name + ".md"
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func mdCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

func yamlString(s string) string { return plainJSON(s, "") } // a JSON string is a valid YAML double-quoted scalar

// plainJSON is v as JSON without HTML escaping (an agent should read <path>, not its
// <-escaped form); indent "" is compact.
func plainJSON(v any, indent string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if enc.Encode(v) != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

// ExampleArgs is an arguments object with a placeholder for each required property, typed
// from the schema, to show the shape of a call.
func ExampleArgs(schema any) map[string]any {
	raw, err := json.Marshal(schema)
	if err != nil {
		return map[string]any{}
	}
	var s struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for _, name := range s.Required {
		typ, _ := s.Properties[name].Type.(string)
		if l, ok := s.Properties[name].Type.([]any); ok && len(l) > 0 {
			typ, _ = l[0].(string)
		}
		switch typ {
		case "integer", "number":
			out[name] = 0
		case "boolean":
			out[name] = false
		case "array":
			out[name] = []any{}
		case "object":
			out[name] = map[string]any{}
		default:
			out[name] = "<" + name + ">"
		}
	}
	return out
}

// VarRefs lists the ${NAME} references in s ($$ is a literal $, not a reference).
func VarRefs(s string) []string {
	s = strings.ReplaceAll(s, "$$", "")
	var out []string
	for _, m := range varRef.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

var varRef = regexp.MustCompile(`\$\{([^}]+)\}`)
