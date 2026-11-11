package gateway

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/testutil/fakemcp"
)

// The estimates are what load_connector really returns: the per-tool sizes plus the
// wrapper come to the result's text, give or take the per-agent class labels.
func TestContextSizesMatchLoadConnector(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	full, isErr := h.call("load_connector", map[string]any{"name": "fake", "detail": "full"})
	if isErr {
		t.Fatal(full)
	}
	summary, _ := h.call("load_connector", map[string]any{"name": "fake"})
	tools := h.gw.Tools("fake").Tools
	estFull, estSummary := LoadWrapperBytes("fake"), SummaryWrapperBytes("fake")
	for _, tm := range tools {
		estFull += tm.Bytes
		estSummary += tm.SummaryBytes
	}
	// The estimates count every tool as "destructive" with requires_approval: never under.
	if estFull < len(full) || estFull > len(full)+len(tools)*40 {
		t.Errorf("full: estimate %d bytes, load_connector returned %d", estFull, len(full))
	}
	if estSummary < len(summary) || estSummary > len(summary)+len(tools)*40 {
		t.Errorf("summary: estimate %d bytes, load_connector returned %d", estSummary, len(summary))
	}
	if len(summary)*2 > len(full) {
		t.Errorf("the summary (%d bytes) should be well under the full list (%d)", len(summary), len(full))
	}

	// The broker's own share, for the session's (all-tools) server.
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.Tools)
	want := len(b) + len(h.session.InitializeResult().Instructions)
	if got := h.gw.BrokerContextBytes(""); got != want {
		t.Errorf("broker context %d bytes, want %d", got, want)
	}
}

func TestToolCacheSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools-cache.json")
	cfg := &config.Config{Connectors: map[string]config.Connector{"aws": {Command: "aws.exe"}}}
	yes := true
	g := New(Options{Config: cfg, ToolCache: path})
	g.remember("aws", []*mcp.Tool{
		{Name: "run_script", Description: strings.Repeat("x", 1000)},
		{Name: "drop", Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes}},
	}, &ServerMeta{Name: "aws-mcp", Instructions: "Runs AWS CLI scripts. Use run_script for everything."})
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
	if l.Server == nil || l.Server.Name != "aws-mcp" {
		t.Errorf("server info lost: %+v", l.Server)
	}
	// No description in the config: agents get one from the cached server info and tools.
	if got, want := g.AgentDescription("aws"), "Runs AWS CLI scripts. Tools: run_script, drop"; got != want {
		t.Errorf("AgentDescription = %q, want %q", got, want)
	}
	// The size counts the whole description, though the cache keeps a short one.
	if b := l.Tools[0].Bytes; b < 1000 || l.Tools[1].Bytes == 0 {
		t.Errorf("sizes after restart: %d, %d", b, l.Tools[1].Bytes)
	}
	if l := g.Tools("never-ran"); len(l.Tools) != 0 || !l.Seen.IsZero() {
		t.Errorf("unknown connector = %+v", l)
	}
}
