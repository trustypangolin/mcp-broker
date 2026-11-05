package dashboard

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestImportPlanAndApply(t *testing.T) {
	f := newFixture(t, base) // has connector fake: {command: fake.exe}
	f.signIn()
	mcpJSON := `{"mcpServers": {
  "fake": {"command": "fake.exe"},
  "fake-staging": {"command": "fake.exe", "env": {"STAGE": "staging"}},
  "notes": {"command": "notes.exe", "env": {"NOTES_API_KEY": "s3cret-value"}},
  "off": {"command": "off.exe", "disabled": true}
}}`
	files := []map[string]string{{"name": ".mcp.json", "content": mcpJSON}}

	code, out := f.do("POST", "/ui/api/import/plan", map[string]any{"files": files}, nil)
	if code != 200 {
		t.Fatalf("plan: %d %v", code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "s3cret-value") {
		t.Fatal("the plan sent a secret value to the browser")
	}
	items := map[string]map[string]any{}
	for _, it := range out["items"].([]any) {
		it := it.(map[string]any)
		items[it["server"].(string)] = it
	}
	if items["fake"]["status"] != "existing" {
		t.Errorf("fake: %v", items["fake"])
	}
	v := items["fake-staging"]
	if v["status"] != "variant" || v["variant_of"] != "fake" || v["groups_existing"] != true {
		t.Errorf("fake-staging: %v", v)
	}
	if n := items["notes"]; n["status"] != "new" || n["secrets"].([]any)[0] != "NOTES_API_KEY" {
		t.Errorf("notes: %v", n)
	}
	if items["off"]["disabled"] != true {
		t.Errorf("off: %v", items["off"])
	}

	// Import with edits: rename notes, leave out off.
	choices := map[string]any{
		items["notes"]["id"].(string): map[string]any{"name": "my-notes", "tags": []string{"office"}},
		items["off"]["id"].(string):   map[string]any{"skip": true},
	}
	code, out = f.do("POST", "/ui/api/import/apply", map[string]any{"files": files, "choices": choices}, nil)
	if code != 200 {
		t.Fatalf("apply: %d %v", code, out)
	}
	if added := out["added"].([]any); len(added) != 2 {
		t.Errorf("added: %v", added)
	}
	cfg := f.b.Config()
	if cn, ok := cfg.Connectors["my-notes"]; !ok || cn.Tags[0] != "office" || cn.EnvFile == "" {
		t.Errorf("my-notes: %+v", cn)
	}
	if _, ok := cfg.Connectors["off"]; ok {
		t.Error("left-out server imported")
	}
	if cfg.Connectors["fake"].Group == "" || cfg.Connectors["fake"].Group != cfg.Connectors["fake-staging"].Group {
		t.Errorf("variant group: fake=%q staging=%q", cfg.Connectors["fake"].Group, cfg.Connectors["fake-staging"].Group)
	}
	text, _ := os.ReadFile(cfg.Path)
	if strings.Contains(string(text), "s3cret-value") || !strings.Contains(string(text), "# my broker") {
		t.Error("broker.yaml got a secret, or lost its comments")
	}

	// Nothing to do the second time, but the server left out.
	_, out = f.do("POST", "/ui/api/import/plan", map[string]any{"files": files}, nil)
	for _, it := range out["items"].([]any) {
		it := it.(map[string]any)
		if s := it["status"]; (s == "new" || s == "variant") && it["server"] != "off" {
			t.Errorf("planned again: %v", it)
		}
	}

	if code, _ := f.do("POST", "/ui/api/import/plan", map[string]any{}, nil); code != 400 {
		t.Errorf("empty request: %d", code)
	}
	code, out = f.do("POST", "/ui/api/import/plan", map[string]any{"files": []map[string]string{{"name": "x.json", "content": "{}"}}}, nil)
	if code != 200 || out["sources"].([]any)[0].(map[string]any)["error"] == nil {
		t.Errorf("a file with no servers is reported per source: %d %v", code, out)
	}
}
