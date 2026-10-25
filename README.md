# mcp-broker

One MCP endpoint for every agent on the host. mcp-broker sits between agents (Claude Desktop,
Claude Code, VS Code, WSL, wslc containers) and your local MCP servers, exposes a handful of
meta-tools, lazy-loads downstream servers on demand, and decides per agent which servers and
tools are visible and which calls need approval. A Windows tray app hosts the broker and its
settings.

- Spec: [docs/SPEC.md](docs/SPEC.md)
- Permissions, agents and roles: [docs/PERMISSIONS.md](docs/PERMISSIONS.md)

Status: early development.
