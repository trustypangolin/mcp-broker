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
   - Claude Desktop (`%APPDATA%\Claude\claude_desktop_config.json`):
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

Already have servers set up in Claude Desktop, Claude Code or VS Code? `mcp-broker import`
shows what it would bring over (secrets go to `secrets\<name>.env`, never `broker.yaml`;
`npx mcp-remote` bridges become direct HTTP connectors). Add `--apply` to import, and
`--switch` to also point those clients at the broker (each original is kept as
`.bak-<time>`; for Claude Code it prints the `claude mcp` commands instead).

Without the tray: `mcp-broker init`, `mcp-broker token create <agent> --save`,
`mcp-broker serve`, and `mcp-broker dashboard` to open the settings.

## Commands

| Command | |
|---|---|
| `mcp-broker serve` | run the broker in a terminal (the tray does this for you) |
| `mcp-broker connect --agent NAME` | stdio shim for clients; finds the broker and token itself |
| `mcp-broker dashboard` | open the dashboard in a browser, signed in |
| `mcp-broker status` | the running broker, its agents and connectors |
| `mcp-broker reload` | re-read the config now |
| `mcp-broker approvals [approve\|deny ID]` | list or decide pending approvals |
| `mcp-broker token create\|list\|revoke` | agent tokens |
| `mcp-broker import [--apply] [--switch]` | bring servers over from client configs |
| `mcp-broker init` / `validate` | create a starter config / check one |

## Building

```
go build ./cmd/mcp-broker
go build -ldflags "-H windowsgui" ./cmd/mcp-broker-tray
go test ./...
```
