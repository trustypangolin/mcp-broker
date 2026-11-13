package dashboard

import (
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

const rolesBase = `# my broker
approvals:
  timeout: 120s   # keep this comment
  notify: [tray]
agents:
  vscode: {role: viewer}
roles:
  viewer:
    connectors: ["*", "!tag:mail"]   # everything but mail
    classes: {read: allow}
    rules:
      - {match: "excel/get_*", action: allow}
  spare: {connectors: []}
connectors:
  excel: {command: excel.exe, group: Office}
  word: {command: word.exe, group: Office}
  imap: {command: imap.exe, tags: [mail]}
audit: {path: audit.jsonl}
`

func TestRolesPage(t *testing.T) {
	f := newFixture(t, rolesBase)
	f.signIn()

	st, out := f.do("GET", "/ui/api/roles", nil, nil)
	if st != 200 {
		t.Fatalf("list roles: %d %v", st, out)
	}
	roles := out["roles"].([]any)
	var viewer map[string]any
	for _, r := range roles {
		if m := r.(map[string]any); m["name"] == "viewer" {
			viewer = m
		}
	}
	if viewer == nil || len(viewer["visible"].([]any)) != 2 || viewer["agents"].([]any)[0] != "vscode" {
		t.Fatalf("viewer role view wrong: %v", viewer)
	}

	// Untick word: the hand-written selectors stay, an exception is added.
	if st, out := f.do("PUT", "/ui/api/roles/viewer", map[string]any{"visible": []string{"excel"}, "classes": map[string]string{"read": "allow", "write": "approve"}}, nil); st != 200 {
		t.Fatalf("put role: %d %v", st, out)
	}
	r := f.b.Config().Roles["viewer"]
	if !slices.Equal(r.Connectors, []string{"*", "!tag:mail", "!word"}) || r.Classes["write"] != config.Approve || len(r.Rules) != 1 {
		t.Fatalf("role after save: %+v", r)
	}
	data, _ := os.ReadFile(f.b.path)
	if !strings.Contains(string(data), "# everything but mail") || !strings.Contains(string(data), "excel/get_*") {
		t.Errorf("save lost a comment or the rules:\n%s", data)
	}

	// The whole Office group on again for a role with nothing: a group selector.
	if st, out := f.do("PUT", "/ui/api/roles/spare", map[string]any{"visible": []string{"excel", "word"}}, nil); st != 200 {
		t.Fatalf("put spare: %d %v", st, out)
	}
	if got := f.b.Config().Roles["spare"].Connectors; !slices.Equal(got, []string{"group:Office"}) {
		t.Errorf("spare selectors %q", got)
	}

	if st, _ := f.do("PUT", "/ui/api/roles/spare?new=1", map[string]any{}, nil); st != http.StatusConflict {
		t.Errorf("creating an existing role: %d", st)
	}
	if st, _ := f.do("PUT", "/ui/api/roles/fresh?new=1", map[string]any{"visible": []string{}}, nil); st != 200 {
		t.Errorf("creating a role: %d", st)
	}
	if st, _ := f.do("PUT", "/ui/api/roles/viewer", map[string]any{"classes": map[string]string{"nope": "allow"}}, nil); st != http.StatusBadRequest {
		t.Errorf("unknown class accepted: %d", st)
	}
	if st, _ := f.do("DELETE", "/ui/api/roles/viewer", nil, nil); st != http.StatusConflict {
		t.Errorf("deleting a role in use: %d", st)
	}
	if st, _ := f.do("DELETE", "/ui/api/roles/anonymous", nil, nil); st != http.StatusNotFound && st != http.StatusConflict {
		t.Errorf("deleting anonymous: %d", st)
	}
	if st, _ := f.do("DELETE", "/ui/api/roles/fresh", nil, nil); st != 200 {
		t.Errorf("deleting an unused role: %d", st)
	}
	if _, ok := f.b.Config().Roles["fresh"]; ok {
		t.Error("fresh is still there")
	}
}

func TestApprovalSettings(t *testing.T) {
	f := newFixture(t, rolesBase)
	f.signIn()
	st, out := f.do("GET", "/ui/api/settings/approvals", nil, nil)
	if s := out["settings"].(map[string]any); st != 200 || s["timeout"] != 120.0 || s["wait"] != 25.0 || s["check_every"] != 15.0 || s["remind"] != 0.0 {
		t.Fatalf("settings: %d %v", st, out)
	}
	if st, out := f.do("PUT", "/ui/api/settings/approvals", approvalSettings{Timeout: 300, Wait: 50, CheckEvery: 20, Remind: 60}, nil); st != 200 {
		t.Fatalf("put settings: %d %v", st, out)
	}
	a := f.b.Config().Approvals
	if a.Timeout != 5*time.Minute || a.Wait != 50*time.Second || a.CheckEvery != 20*time.Second || a.Remind != time.Minute || len(a.Notify) != 1 {
		t.Errorf("approvals after save: %+v", a)
	}
	data, _ := os.ReadFile(f.b.path)
	if !strings.Contains(string(data), "timeout: 5m") || !strings.Contains(string(data), "# keep this comment") {
		t.Errorf("approvals written as:\n%s", data)
	}
	// Reminders off removes the key.
	f.do("PUT", "/ui/api/settings/approvals", approvalSettings{Timeout: 300, Wait: 50, CheckEvery: 20}, nil)
	if data, _ := os.ReadFile(f.b.path); strings.Contains(string(data), "remind") {
		t.Errorf("remind 0 left the key:\n%s", data)
	}
	if st, _ := f.do("PUT", "/ui/api/settings/approvals", approvalSettings{Timeout: 30, Wait: 10, CheckEvery: 60}, nil); st != http.StatusBadRequest {
		t.Errorf("check_every beyond the timeout accepted: %d", st)
	}
}
