package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/policy"
)

// ToolMeta is what the dashboard needs to show and classify a tool without the connector
// running.
type ToolMeta struct {
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	DestructiveHint *bool  `json:"destructive_hint,omitempty"`
	// Bytes is the size of the tool's entry in a load_connector result (name, full
	// description, input schema): what it costs an agent's context. 0 when unknown (a cache
	// written before sizes were kept).
	Bytes int `json:"bytes,omitempty"`
	// SummaryBytes is the size of its line in load_connector's default summary.
	SummaryBytes int `json:"summary_bytes,omitempty"`
}

// Annotations returns the hints the classifier considers.
func (t ToolMeta) Annotations() policy.Annotations {
	return policy.Annotations{DestructiveHint: t.DestructiveHint}
}

// ToolList is a connector's tools: live from a running instance, or the list it had when it
// last ran (Seen), or nothing when it never has.
type ToolList struct {
	Tools []ToolMeta `json:"tools"`
	Seen  time.Time  `json:"seen,omitzero"`
	Live  bool       `json:"live"`
}

// maxToolDesc keeps the cache small: the dashboard shows one line, not the whole manual.
const maxToolDesc = 240

func metaOf(tools []*mcp.Tool) []ToolMeta {
	out := make([]ToolMeta, 0, len(tools))
	for _, t := range tools {
		m := ToolMeta{Name: t.Name, Description: t.Description, Bytes: entryBytes(t), SummaryBytes: summaryLineBytes(t)}
		if r := []rune(m.Description); len(r) > maxToolDesc {
			m.Description = string(r[:maxToolDesc]) + "…"
		}
		if t.Annotations != nil {
			m.DestructiveHint = t.Annotations.DestructiveHint
		}
		out = append(out, m)
	}
	return out
}

// entryBytes is the size of a tool's entry in load_connector's result, laid out as it is
// there: indented, as an element of the tools array. Class and requires_approval vary by
// agent; the longest values are counted.
func entryBytes(t *mcp.Tool) int {
	b, err := json.MarshalIndent(toolInfo{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, Class: "destructive", Approval: true}, "    ", "  ")
	if err != nil {
		return 0
	}
	return len(b) + len(",\n    ")
}

// summaryLineBytes is the size of a tool's line in load_connector's summary, the longest
// class and requires_approval counted.
func summaryLineBytes(t *mcp.Tool) int {
	b, err := json.Marshal(toolSummary{Name: t.Name, Summary: summarize(t.Description), Class: "destructive", Approval: true})
	if err != nil {
		return 0
	}
	return len(b) + len(",\n")
}

// SummaryWrapperBytes is what load_connector's summary adds around a connector's tool lines.
func SummaryWrapperBytes(name string) int {
	return len(summaryResult(name, nil).Content[0].(*mcp.TextContent).Text)
}

// LoadWrapperBytes is what load_connector's result adds around the tools of a connector
// named name.
func LoadWrapperBytes(name string) int {
	b, _ := json.MarshalIndent(map[string]any{"connector": name, "tools": []int{}}, "", "  ")
	return len(b) + len("\n  ")
}

// remember records a connector's tool list and saves the catalog, so the dashboard can show
// and set policy for tools of connectors that are not running.
func (g *Gateway) remember(name string, tools []*mcp.Tool) {
	l := ToolList{Tools: metaOf(tools), Seen: time.Now().UTC()}
	g.catMu.Lock()
	defer g.catMu.Unlock()
	g.loadCatalogLocked()
	g.catalog[name] = l
	if g.opts.ToolCache == "" {
		return
	}
	data, err := json.MarshalIndent(g.catalog, "", " ")
	if err == nil {
		os.MkdirAll(filepath.Dir(g.opts.ToolCache), 0o700)
		tmp := g.opts.ToolCache + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, g.opts.ToolCache)
		}
	}
	if err != nil {
		g.log.Warn("saving tool cache", "err", err)
	}
}

func (g *Gateway) loadCatalogLocked() {
	if g.catalog != nil {
		return
	}
	g.catalog = map[string]ToolList{}
	if g.opts.ToolCache == "" {
		return
	}
	if data, err := os.ReadFile(g.opts.ToolCache); err == nil {
		if err := json.Unmarshal(data, &g.catalog); err != nil {
			g.log.Warn("reading tool cache", "err", err)
			g.catalog = map[string]ToolList{}
		}
	}
}

// Tools returns a connector's tools: from a running instance when there is one, else the
// list remembered from its last run.
func (g *Gateway) Tools(name string) ToolList {
	g.mu.Lock()
	var live *downstream
	for _, d := range g.conns {
		if d.name == name && d.loaded() {
			live = d
			break
		}
	}
	g.mu.Unlock()
	if live != nil {
		if _, tools := live.snapshot(); len(tools) > 0 {
			return ToolList{Tools: metaOf(tools), Seen: time.Now().UTC(), Live: true}
		}
	}
	g.catMu.Lock()
	defer g.catMu.Unlock()
	g.loadCatalogLocked()
	return g.catalog[name]
}

// Discover starts a connector, as agent's instance when it runs one per agent, so its tools
// can be listed and set up before any agent calls it. The idle timer stops it again.
func (g *Gateway) Discover(ctx context.Context, name, agent string) (ToolList, error) {
	d, ok := g.conn(name, agent)
	if !ok {
		return ToolList{}, fmt.Errorf("no connector %s", name)
	}
	start := time.Now()
	wasLoaded := d.loaded()
	if err := d.ensure(ctx); err != nil {
		msg := g.red.String(err.Error())
		g.write(audit.Event{Kind: audit.KindProcessStart, Connector: d.key, Error: msg, Detail: "loaded from the dashboard", DurationMS: time.Since(start).Milliseconds()})
		return ToolList{}, fmt.Errorf("%s", msg)
	}
	if !wasLoaded {
		e := audit.Event{Kind: audit.KindProcessStart, Connector: d.key, Detail: "loaded from the dashboard", DurationMS: time.Since(start).Milliseconds()}
		if p := d.process(); p != nil {
			e.PID = p.PID()
		}
		g.write(e)
	}
	_, tools := d.snapshot()
	return ToolList{Tools: metaOf(tools), Seen: time.Now().UTC(), Live: true}, nil
}
