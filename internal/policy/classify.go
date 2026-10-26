package policy

import (
	"strings"
	"unicode"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

// Annotations are the MCP tool annotation hints the classifier considers. Pointers
// distinguish "not set" from false; per the MCP spec destructiveHint defaults to true, but
// an absent hint must not turn every unannotated tool destructive, so only an explicit true
// counts.
type Annotations struct {
	DestructiveHint *bool
}

// verbs maps name tokens to the class they imply. Read verbs only classify a tool as read
// when nothing in the name implies a stronger effect; names with no known verb are write.
var verbs = map[string]config.Class{}

func init() {
	add := func(c config.Class, words ...string) {
		for _, w := range words {
			verbs[w] = c
		}
	}
	add(config.Read, "get", "list", "read", "search", "find", "query", "status", "info", "describe",
		"show", "inspect", "check", "validate", "verify", "count", "stats", "diagnose", "discover",
		"which", "capture", "view", "lookup", "preview")
	add(config.Write, "set", "add", "create", "update", "insert", "replace", "move", "rename", "format",
		"write", "save", "apply", "merge", "unmerge", "sort", "append", "edit", "modify", "toggle",
		"accept", "activate", "deactivate", "flag", "unflag", "mark", "download", "copy", "convert",
		"resolve", "undo", "track", "put", "import", "export", "generate", "run", "emit", "fetch")
	add(config.Destructive, "delete", "remove", "clear", "trash", "purge", "drop", "destroy", "wipe",
		"reset", "reject", "truncate", "kill", "close", "overwrite", "unprotect")
	add(config.External, "send", "forward", "reply", "share", "publish", "post", "push", "upload")
	add(config.Exec, "install", "uninstall", "exec", "execute", "eval", "shell", "spawn", "launch")
}

// Classify decides a tool's class: the connector's tool_classes override is authoritative;
// otherwise the strictest of the name heuristic and the downstream annotations wins, so a
// server cannot talk its own tools down to a laxer class.
func Classify(cn config.Connector, tool string, ann Annotations) config.Class {
	if c, ok := cn.ToolClasses[tool]; ok {
		return c
	}
	c := classifyName(tool)
	if ann.DestructiveHint != nil && *ann.DestructiveHint && c.Rank() < config.Destructive.Rank() {
		c = config.Destructive
	}
	return c
}

func classifyName(tool string) config.Class {
	var best config.Class
	sawRead := false
	for _, tok := range tokens(tool) {
		c, ok := verbs[tok]
		if !ok {
			continue
		}
		if c == config.Read {
			sawRead = true
			continue
		}
		if best == "" || c.Rank() > best.Rank() {
			best = c
		}
	}
	switch {
	case best != "":
		return best
	case sawRead:
		return config.Read
	default:
		return config.Write
	}
}

// tokens splits a tool name on punctuation and camelCase: "imap_bulkDelete-x" →
// [imap bulk delete x].
func tokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}
