// Package skills holds the agent skills that ship with mcp-broker. Each is a directory
// that can be copied as-is into an agent's skills folder (for Claude Code,
// ~/.claude/skills/); the broker also serves them, so the copy an agent reads always
// matches the running version.
package skills

import _ "embed"

// BrokerAPI is mcp-broker-api/SKILL.md: how to use the broker's HTTP API (/api/v1) when
// an MCP client isn't available. The broker serves it at /api/v1/skill.md.
//
//go:embed mcp-broker-api/SKILL.md
var BrokerAPI string
