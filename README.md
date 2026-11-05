# mcp-broker

One MCP endpoint for every agent on the host. mcp-broker sits between agents (Claude Desktop,
Claude Code, VS Code, WSL, wslc containers) and your local MCP servers, exposes a handful of
meta-tools, lazy-loads downstream servers on demand, and decides per agent which servers and
tools are visible and which calls need approval. A Windows tray app hosts the broker and its
settings.

- Spec: [docs/SPEC.md](docs/SPEC.md)
- Permissions, agents and roles: [docs/PERMISSIONS.md](docs/PERMISSIONS.md)

Status: early development.

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

Without the tray: `mcp-broker init`, `mcp-broker token create <agent> --save`,
`mcp-broker serve`, and `mcp-broker dashboard` to open the settings.

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
