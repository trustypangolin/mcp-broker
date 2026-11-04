package gateway

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trustypangolin/mcp-broker/internal/config"
)

func TestToolCacheSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools-cache.json")
	cfg := &config.Config{Connectors: map[string]config.Connector{"aws": {Command: "aws.exe"}}}
	yes := true
	g := New(Options{Config: cfg, ToolCache: path})
	g.remember("aws", []*mcp.Tool{
		{Name: "run_script", Description: strings.Repeat("x", 1000)},
		{Name: "drop", Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes}},
	})
	g.Close()

	g = New(Options{Config: cfg, ToolCache: path})
	defer g.Close()
	l := g.Tools("aws")
	if l.Live || l.Seen.IsZero() || len(l.Tools) != 2 {
		t.Fatalf("cached list = %+v", l)
	}
	if n := len([]rune(l.Tools[0].Description)); n > maxToolDesc+1 {
		t.Errorf("description kept %d runes", n)
	}
	if h := l.Tools[1].DestructiveHint; h == nil || !*h {
		t.Error("destructive hint lost")
	}
	if l := g.Tools("never-ran"); len(l.Tools) != 0 || !l.Seen.IsZero() {
		t.Errorf("unknown connector = %+v", l)
	}
}
