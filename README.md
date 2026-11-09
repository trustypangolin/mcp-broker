# mcp-broker

One MCP endpoint for every agent on the host. mcp-broker sits between agents (Claude Desktop,
Claude Code, VS Code, WSL, wslc containers) and your local MCP servers, exposes a handful of
meta-tools, lazy-loads downstream servers on demand, and decides per agent which servers and
tools are visible and which calls need approval. A Windows tray app hosts the broker and its
settings.

- Spec: [docs/SPEC.md](docs/SPEC.md)
- Permissions, agents and roles: [docs/PERMISSIONS.md](docs/PERMISSIONS.md)

Status: stable since v1.0.0. Website: https://mcpbroker.aimodel.au. License: Business Source
License 1.1, free for use on a single machine (see [License](#license)).

## Quick start (Windows)

1. Put `mcp-broker.exe` and `mcp-broker-tray.exe` in a folder that will not move, such as
   `C:\Tools\mcp-broker`.
2. Run `mcp-broker-tray.exe`. On first run it creates `%APPDATA%\mcp-broker\broker.yaml`
   with the default agents and roles and no connectors, and listens on `127.0.0.1:8765`.
3. Click the tray icon to open the dashboard (signed in automatically):
   - **Connectors**: add your MCP servers (command and arguments, or a URL). Secrets go in
     an env file, not in `broker.yaml`.
   - **Agents**: create a token for each client with *Save it for the local shim* ticked.
     The agent's card shows the command to put in the client config.
4. Point each client at the shim instead of at the servers themselves:
   - Claude Code: `claude mcp add -s user broker -- C:\Tools\mcp-broker\mcp-broker.exe connect --agent claude-code`
   - Claude Desktop (`%APPDATA%\Claude\claude_desktop_config.json`; the Microsoft Store
     build reads `%LOCALAPPDATA%\Packages\Claude_*\LocalCache\Roaming\Claude\` instead):
     `"broker": {"command": "C:\\Tools\\mcp-broker\\mcp-broker.exe", "args": ["connect", "--agent", "claude-desktop"]}`
   - VS Code (`mcp.json`):
     `"broker": {"type": "stdio", "command": "C:\\Tools\\mcp-broker\\mcp-broker.exe", "args": ["connect", "--agent", "vscode"]}`

   Remove the servers you moved behind the broker from the client's own config, or their
   tools appear twice.

### What an agent sees

An agent starts with only the broker's own tools, and only those its role lets it use:

- the connector tools if it can see any connector;
- `list_profiles` and `profile` if it can use any profile;
- `approval_status` if its approvals can be answered outside its client.

When a reload changes this, connected clients are told (`tools/list_changed`) and refresh.

To use a connector, an agent calls:

1. `list_connectors`;
2. `load_connector`, which by default returns one line per tool: name, first sentence,
   required argument names, class, and whether it needs approval;
3. `describe_tool` for the tool it wants, to get that tool's full description and input
   schema;
4. `call_tool`. If a connector rejects the arguments as not matching its schema, the
   broker adds a hint to call `describe_tool`.

A 120-tool server costs a few thousand tokens to list this way, instead of about 20K with
every schema. `load_connector` with `detail: "full"` still returns everything at once.

Approvals pop up as a notification from the tray (click it to approve or deny). Claude Code
and Claude Desktop can also ask in the app. Pending approvals are on the dashboard's
Approvals page, which also works from a phone once LAN access is on.

From the tray menu: **Allow LAN and WSL access** (asks for administrator rights once to add
Private-network and WSL firewall rules, then listens on all interfaces), **Start with
Windows**, reload, restart and the log folders. Edits to `broker.yaml` in any editor apply
on save.

Already have servers set up in other clients? **Connectors → Import…** on the dashboard
takes client configs: upload or paste `.mcp.json`, `claude_desktop_config.json`,
`~/.claude.json` (including each project's servers), VS Code `mcp.json` or `settings.json`,
Codex `config.toml`, or Cursor, Gemini CLI and Windsurf configs. You can also tick **scan
this computer**. The preview sorts every server into one of four groups:

- **already here:** the same command and arguments or URL, with no env or header the
  broker's connector doesn't already have. These are left alone.
- **variant:** the same server with a different env value, header or option value (for
  example `--profile prod` next to an existing `--profile dev`). It's imported under a name
  taken from the difference (`aws-prod`) and put in the same group as the original.
- **new:** imported with suggested tags (so the default roles pick it up), group and name.
  A name that a different connector already has gets the project or client name added.
- **skipped:** SSE servers and the broker's own shim. Servers the client has disabled are
  listed but unticked.

Edit the name, tags or group of any row, then click **Import selected**. Secret values stay
on the broker: the preview shows only their names, and they're written to
`secrets\<name>.env`.

`mcp-broker import` does the same from the command line. It shows what it would bring over (secrets go to `secrets\<name>.env`, never `broker.yaml`;
Windows location variables such as `${LOCALAPPDATA}` are written in as plain paths;
`npx mcp-remote` bridges become direct HTTP connectors). Use `--from PATH` to read any
other file with an `mcpServers` block. Add `--apply` to import, and
`--switch` to also point those clients at the broker (each original is kept as
`.bak-<time>`; for Claude Code it prints the `claude mcp` commands instead).

The dashboard's Connectors and Profiles pages fold: click a card or group heading to
collapse it (remembered per browser). **New group**, **Group…** on a card, and a group's
**Rename** and **Ungroup** write a `group:` line on each member in `broker.yaml`, and a
`group:` you type into the file shows up as a heading on the next reload. Groups are only
labels: they don't change what agents see (use `tags` and roles for that), and regrouping
a running connector doesn't restart it.

```yaml
connectors:
  cadcraft:
    group: ArtCraft MCPs
    command: C:\Program Files\CADCraft\cadcraft-cli.exe
    args: [mcp]
```

The **Tools** page is a tree per agent: group → connector → tool. Each tool shows what a
call does right now (**Allow**, **Ask** or **Deny**), the reason, and how often the agent
was asked about it. Tools that keep asking are listed at the top under **Asking you every
time**. Each tool and connector has an **Inherit / Allow / Ask / Deny** switch. The switch
writes a rule to the agent's role, so agents that share the role get the same rule:

```yaml
roles:
  owner:
    rules:
      - match: aws-luton-shared-readonly/aws___run_script   # one tool: goes first
        action: allow
      - match: excel-live/*                                  # a whole connector: goes last
        action: approve
```

**Inherit** removes the rule. Global `policy.floors` still win, and the switch shows them
as locked. **Approve + always allow** on an approval card approves the call and writes the
same allow rule for that tool.

The tree also estimates **context cost** in tokens (JSON bytes divided by 4). Different
models tokenize differently, so the figures are rough. It shows:

- the **broker context**: the meta-tools and instructions the selected agent starts with;
- for each connector, what one `load_connector` adds for that agent (its default summary,
  with the `full` size next to it), and the total for each group;
- for each tool, what `describe_tool` returns for it.

Connectors and groups are marked **light** (under 2K), **medium** (2K–10K) or **heavy** (over
10K), with the level written out as well as coloured. A denied tool is left out of that
agent's `load_connector` and `describe_tool`, so it costs nothing. Setting **Deny** on tools
an agent doesn't need shrinks its context too. A "+" means some tool sizes aren't known yet: **Refresh tools**
measures them.

The tree reads each connector's tools from the running instance. When a connector isn't
running, it uses the list saved the last time it ran (`tools-cache.json`). **Load tools**
starts a connector that has never run.

## AWS accounts

**Connectors → AWS profiles…** lists the profiles in `~/.aws/config` and attaches any of
them as a connector (`aws-<profile>`, group AWS, tag `aws`). Each one runs AWS's signing
proxy (`uvx mcp-proxy-for-aws`) as that profile against the managed AWS MCP server, so
roles and approvals apply per account and no keys go into `broker.yaml`. Tick
**read-only role** only when the IAM role itself can't write: `run_script` then runs
without approval, since AWS refuses anything else. Certificate (IAM Roles Anywhere) and
key profiles work unattended; SSO profiles stop working when the session lapses until you
run `aws sso login --profile <name>`.

## Remote servers that need a sign-in (OAuth)

Hosted MCP servers want a person to sign in. Most, such as Atlassian's, let the broker
register itself (dynamic client registration), so the URL is all you add:

```yaml
connectors:
  atlassian:
    url: https://mcp.atlassian.com/v1/mcp
```

The first time an agent loads it, the broker registers a client with the server, and
`load_connector` answers at once with the sign-in link (the tray offers it too). The
connector's card on the dashboard has a **Sign in** button that does the same. After you
approve access in the browser, the next `load_connector` lists the tools. The registration
is kept in `oauth\atlassian.client.json` and reused, including after a sign-out. The broker
registers again only when the redirect URL changes or the server no longer recognizes the
client. To limit what the broker asks for, add `oauth: {scopes: [...]}`. A connector that
sends its own `Authorization` header never signs in.

Some servers, such as Xero's, don't offer registration. For those, register an app with the
service (a PKCE or "native" app; add `http://localhost:8765/oauth/callback` as its redirect
URI), then:

```yaml
connectors:
  xero:
    url: https://api.xero.com/mcp/v1
    oauth:
      client_id: YOUR_CLIENT_ID
      # client_secret: ${XERO_CLIENT_SECRET}   # confidential clients only, from env_file
      scopes: [openid, offline_access, accounting.invoices.read, accounting.contacts.read]
```

`scopes` replaces what the server advertises, which is usually everything it can do, writes
included. Run `mcp-broker signin xero`: the broker finds the endpoints from the server's
metadata, opens the sign-in page, and saves the token in `oauth\xero.json` next to the
config, refreshing it from then on. If an agent gets there first, `load_connector` answers
at once with the sign-in link and the tray offers it as a notification. `mcp-broker status`
shows connectors still waiting for a sign-in; `mcp-broker signout xero` forgets the token.

Servers on a private network, such as a self-hosted GitLab reached over a VPN or
Tailscale, need one more line. The MCP SDK refuses to fetch sign-in metadata from private,
link-local and CGNAT addresses, so that a server can't point the broker at your local
network. Sign-in then fails with `non-public ip address`. If you trust the server, opt
that connector in:

```yaml
connectors:
  gitlab:
    url: https://gitlab.example.internal/api/v4/mcp
    oauth:
      private_network: true
      scopes: [mcp]
```

Reach the server by host name. The SDK refuses a private IP address written into a URL even
with this option set.

## Profiles: long-running apps

A profile is an app the broker runs for agents and for you, such as ComfyUI:

```yaml
profiles:
  comfyui:
    description: ComfyUI on port 8188
    cwd: G:\ComfyUI\ComfyUI
    command: G:\ComfyUI\ComfyUI\venv\Scripts\python.exe
    args: [main.py, --listen, 127.0.0.1, --port, "8188"]
    path_prepend: [G:\ComfyUI\ComfyUI\venv\Scripts]
    ports: [8188]
    health: {http: "http://127.0.0.1:8188/system_stats", start_timeout: 10m}
    stop: {timeout: 20s}         # CTRL_BREAK (SIGTERM elsewhere), then kill the tree
    update:
      - [git, -C, G:\ComfyUI\ComfyUI, pull]
```

Agents get two tools: `list_profiles`, and `profile` with `status`, `logs`, `plan`,
`start`, `stop` and `update`. `start` returns at once with state `starting` and the health
check moves it to `ready`. Every start shows the exact plan (command, arguments,
environment names, ports, bind address) and checks first for a missing command or a port
already taken (naming the process holding it, or the other profile using it: setups of
one app on the same port never run together). `requires_paths` lists folders that must
exist first, for drives that are not always mounted. `mcp-broker plan <profile>` shows the
plan without doing anything. What each role may do is under
`roles.<role>.profiles`; starting anything that listens beyond this PC always needs
approval, and so does killing a process the broker did not start (`kill_foreign`).

The broker owns every process it starts, children included, and stops them all when it
exits. You can start and stop profiles from the tray's Profiles menu, the dashboard's
Profiles page (with live logs), or `mcp-broker ps | start | stop | update | logs`.

The Profiles page also adds, edits, duplicates and removes profiles (**Add profile**,
**Edit**, **Duplicate**, **Remove**), so you don't have to edit `broker.yaml` by hand. The
form shows each entry as the file has it, with `${NAME}` unexpanded and defaults not filled
in, and a save rewrites only that entry: comments, quoting and key order are kept, and the
previous file is kept as `broker.yaml.bak`. Update steps are one command per line; quote an
argument that contains spaces. A running profile keeps its old settings until its next
start, and it can't be removed until it is stopped. Values from `env_file` are read again on
every start and update, so pinning a version there (`git reset --hard ${COMFYUI_VERSION}`)
takes effect at the next **Update…** without touching the profile.

Without the tray: `mcp-broker init`, `mcp-broker token create <agent> --save`,
`mcp-broker serve`, and `mcp-broker dashboard` to open the settings.

## Updates

The tray checks GitHub for a newer release (pre-releases included) 30 seconds after it
starts and every 6 hours after that. It never installs one by itself: a new release shows
a notification, an **Update to vX…** item at the top of the tray menu, and an **Update to
vX** button next to the version in the dashboard. **Check for updates…** in the menu checks
now.

Updating downloads the release zip for this PC and checks it against the release's
`checksums.txt`. It then renames the running `mcp-broker.exe` and `mcp-broker-tray.exe` to
`*.old-<version>.exe`, puts the new ones in their place and restarts the tray. Running
profiles stop, and agents reconnect on their own. Leftover `*.old-*` files are removed at
the next start once nothing runs them. A client's shim keeps running its old copy until the
client restarts it. The checksum catches a corrupt download; it is not a signature.

Builds labelled `dev` (plain `go build`) can't compare versions, so they never offer
updates. Without the tray, `mcp-broker self-update` installs the newest release over the
programs it runs from (`-check` only reports; `-stable` skips pre-releases). Then restart
`mcp-broker serve`.

## Commands

| Command | |
|---|---|
| `mcp-broker serve` | run the broker in a terminal (the tray does this for you) |
| `mcp-broker connect --agent NAME` | stdio shim for clients; finds the broker and token itself |
| `mcp-broker dashboard` | open the dashboard in a browser, signed in |
| `mcp-broker status` | the running broker, its agents and connectors |
| `mcp-broker ps` / `plan` / `start` / `stop` / `update` / `logs` | profiles |
| `mcp-broker reload` | re-read the config now |
| `mcp-broker approvals [approve\|deny ID]` | list or decide pending approvals |
| `mcp-broker token create\|list\|revoke` | agent tokens |
| `mcp-broker import [--apply] [--switch]` | bring servers over from client configs |
| `mcp-broker client-config <harness\|all\|list> [--agent NAME] [--http] [--wsl]` | print the snippet a client needs |
| `mcp-broker signin NAME` / `signout NAME` | sign in to an OAuth connector, or forget its token |
| `mcp-broker init` / `validate` | create a starter config / check one |
| `mcp-broker self-update [-check] [-stable]` | install the newest release over these programs |

`client-config` prints a snippet to paste into a client's config. The clients are
`claude-code`, `claude-desktop`, `vscode`, `codex`, `cursor`, `gemini` and `windsurf`. It
uses the stdio shim by default. `--http` gives the Streamable HTTP form instead, which reads
the token from `$MCP_BROKER_TOKEN`; in VS Code, a password prompt replaces the variable. Snippets never contain a token, and the command never writes
to a file. **Client config…** on each agent card in the dashboard shows the same snippets,
with a Copy button.

```
mcp-broker client-config codex                 # ~/.codex/config.toml, agent codex
mcp-broker client-config vscode --http         # .vscode/mcp.json over HTTP
mcp-broker client-config claude-code --agent wsl --wsl   # Claude Code inside WSL
```

## Building

```
go build ./cmd/mcp-broker
go build -ldflags "-H windowsgui" ./cmd/mcp-broker-tray
go test ./...
bash scripts/package.sh v0.1.0   # release archives into ./release
```

Pushing a `v*` tag runs the same script in GitHub Actions and publishes a release.

## License

mcp-broker is source-available under the [Business Source License 1.1](LICENSE).

- **Free:** production use on a single machine, including WSL, virtual machines and
  containers on that machine, as long as the broker only accepts connections from that
  machine. You can install it on as many machines as you like, each on its own.
- **Commercial license needed:** when the broker accepts connections from other machines,
  such as office PCs, laptops or phones over a LAN, VPN or the internet. See
  [pricing](https://mcpbroker.aimodel.au/pricing) or email contact@aimodel.au.
- **Becomes open source:** each version converts to the Apache License 2.0 four years after
  it is published.

Copying, modifying and non-production use are always allowed.
