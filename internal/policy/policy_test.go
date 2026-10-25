package policy

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

// Tool names below are real ones from the servers in examples/broker.yaml.
func TestClassifyName(t *testing.T) {
	cases := map[string]config.Class{
		"imap_get_latest_emails":         config.Read,
		"imap_search_emails":             config.Read,
		"imap_bulk_delete_by_search":     config.Destructive,
		"imap_send_email":                config.External,
		"imap_reply_to_email":            config.External,
		"imap_upload_file":               config.External,
		"imap_add_account":               config.Write,
		"sieve_put_script":               config.Write,
		"sieve_check_script":             config.Read,
		"get_range_values":               config.Read,
		"set_cell_formula":               config.Write,
		"find_replace":                   config.Write, // find is read, replace is not
		"delete_worksheet":               config.Destructive,
		"clear_conditional_format_rules": config.Destructive,
		"get-tab-web-content":            config.Read,
		"close-browser-tabs":             config.Destructive,
		"capture-tab-screenshot":         config.Read,
		"word_live_take_snapshot":        config.Write, // no known verb
		"word_live_snapshot_status":      config.Read,
		"unprotect_document":             config.Destructive,
		"install_node":                   config.Exec,
		"launch_comfyui":                 config.Exec,
		"export_slide_to_image":          config.Write,
		"listOpenTabs":                   config.Read,
		"deleteAllTabs":                  config.Destructive,
	}
	for name, want := range cases {
		if got := classifyName(name); got != want {
			t.Errorf("classifyName(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestClassifyOverridesAndAnnotations(t *testing.T) {
	yes, no := true, false
	cn := config.Connector{ToolClasses: map[string]config.Class{"partner_generate": config.Spend, "imap_check_spam": config.Read}}

	if Classify(cn, "partner_generate", Annotations{}) != config.Spend {
		t.Error("override ignored")
	}
	// The override is authoritative even against an annotation.
	if Classify(cn, "imap_check_spam", Annotations{DestructiveHint: &yes}) != config.Read {
		t.Error("override should beat annotation")
	}
	// Annotations can raise a class...
	if Classify(cn, "get_thing", Annotations{DestructiveHint: &yes}) != config.Destructive {
		t.Error("destructiveHint should raise read to destructive")
	}
	// ...but never lower one.
	if Classify(cn, "delete_thing", Annotations{DestructiveHint: &no}) != config.Destructive {
		t.Error("destructiveHint=false must not lower delete_*")
	}
	if Classify(cn, "imap_send_email", Annotations{DestructiveHint: &yes}) != config.External {
		t.Error("annotation must not lower external to destructive")
	}
}

func loadExample(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "broker.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles["comfyui"] = config.Profile{Command: "python"}
	return cfg
}

// TestDefaultPolicy pins the default role table in docs/PERMISSIONS.md.
func TestDefaultPolicy(t *testing.T) {
	e := New(loadExample(t))
	cases := []struct {
		agent, connector, tool string
		want                   config.Action
	}{
		// owner
		{"claude-desktop", "imap", "imap_search_emails", config.Allow},
		{"claude-desktop", "imap", "imap_send_email", config.Approve},
		{"claude-desktop", "imap", "imap_bulk_delete", config.Approve},
		{"claude-desktop", "imap", "imap_add_account", config.Approve},  // floor
		{"claude-desktop", "sieve", "sieve_put_script", config.Approve}, // floor over write:allow
		{"claude-desktop", "sieve", "sieve_activate_script", config.Approve},
		{"claude-desktop", "sieve", "sieve_deactivate_scripts", config.Approve},
		{"claude-desktop", "sieve", "sieve_list_scripts", config.Allow},
		{"claude-code", "excel-live", "set_range_values", config.Allow},
		{"claude-code", "comfy", "partner_generate", config.Approve},
		{"claude-code", "comfy", "run_workflow", config.Allow},
		{"claude-code", "word-live", "add_digital_signature", config.Approve}, // floor
		{"claude-code", "strapi", "anything_get", config.Allow},

		// coder
		{"vscode", "excel-live", "set_range_values", config.Allow},
		{"vscode", "excel-live", "delete_worksheet", config.Approve},
		{"vscode", "browser-control", "get-tab-web-content", config.Allow},
		{"vscode", "browser-control", "get-recent-browser-history", config.Approve}, // role rule
		{"vscode", "comfy", "partner_generate", config.Deny},
		{"vscode", "comfy", "install_node", config.Approve},
		{"vscode", "comfy", "run_workflow", config.Allow},
		{"vscode", "imap", "imap_search_emails", config.Deny}, // not granted

		// sandbox
		{"wsl", "excel-live", "get_range_values", config.Allow},
		{"wsl", "excel-live", "set_range_values", config.Approve},
		{"wslc", "comfy", "install_node", config.Deny},
		{"wslc", "browser-control", "get-tab-web-content", config.Deny},
		{"wslc", "word-live", "word_live_get_text", config.Allow},

		// viewer
		{"lan", "excel-live", "get_range_values", config.Allow},
		{"lan", "excel-live", "set_range_values", config.Deny},
		{"lan", "browser-control", "get-tab-web-content", config.Deny}, // role rule
		{"lan", "sieve", "sieve_list_scripts", config.Deny},

		// unidentified
		{"", "excel-live", "get_range_values", config.Deny},
		{"nobody", "excel-live", "get_range_values", config.Deny},
	}
	for _, c := range cases {
		d := e.Tool(c.agent, c.connector, c.tool, Annotations{})
		if d.Action != c.want {
			t.Errorf("%s %s/%s = %s (%s), want %s", c.agent, c.connector, c.tool, d.Action, d.Reason, c.want)
		}
	}
}

func TestInvisibleConnectors(t *testing.T) {
	e := New(loadExample(t))
	d := e.Tool("vscode", "imap", "imap_search_emails", Annotations{})
	if d.Visible {
		t.Error("ungranted connector must be invisible")
	}
	got := strings.Join(e.VisibleConnectors("wsl"), ",")
	if got != "comfy,excel-live,powerpoint-live,word-live" {
		t.Errorf("wsl sees %s", got)
	}
	if len(e.VisibleConnectors("")) != 0 {
		t.Error("anonymous sees connectors")
	}
	if e.Visible("vscode", "does-not-exist") {
		t.Error("unknown connector visible")
	}
}

func TestReasonTrail(t *testing.T) {
	e := New(loadExample(t))
	d := e.Tool("claude-desktop", "sieve", "sieve_put_script", Annotations{})
	if !strings.Contains(d.Reason, "class write → allow") || !strings.Contains(d.Reason, "floor sieve/sieve_put_script → approve") {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestProfilePolicy(t *testing.T) {
	e := New(loadExample(t))
	cases := []struct {
		agent, action string
		exposed       bool
		want          config.Action
		visible       bool
	}{
		{"claude-desktop", "start", false, config.Approve, true},
		{"claude-desktop", "stop", false, config.Allow, true},
		{"claude-desktop", "update", false, config.Approve, true},
		{"claude-desktop", "kill_foreign", false, config.Deny, true}, // not granted: guard keeps deny
		{"vscode", "start", false, config.Approve, true},
		{"vscode", "update", false, config.Deny, true},
		{"wsl", "start", false, config.Deny, true},
		{"wsl", "logs", false, config.Allow, true},
		{"", "status", false, config.Deny, false},
	}
	for _, c := range cases {
		d := e.Profile(c.agent, "comfyui", c.action, c.exposed)
		if d.Action != c.want || d.Visible != c.visible {
			t.Errorf("%s %s = %s visible=%v (%s), want %s visible=%v", c.agent, c.action, d.Action, d.Visible, d.Reason, c.want, c.visible)
		}
	}
}

func TestNetworkExposedForcesApproval(t *testing.T) {
	cfg := loadExample(t)
	owner := cfg.Roles["owner"]
	owner.Profiles["start"] = config.Allow
	owner.Profiles["kill_foreign"] = config.Allow
	cfg.Roles["owner"] = owner
	e := New(cfg)

	if d := e.Profile("claude-code", "comfyui", "start", false); d.Action != config.Allow {
		t.Fatalf("baseline start = %s", d.Action)
	}
	if d := e.Profile("claude-code", "comfyui", "start", true); d.Action != config.Approve {
		t.Errorf("exposed start = %s, want approve", d.Action)
	}
	if d := e.Profile("claude-code", "comfyui", "kill_foreign", false); d.Action != config.Approve {
		t.Errorf("kill_foreign = %s, want approve", d.Action)
	}
}

func TestChannels(t *testing.T) {
	e := New(loadExample(t))
	if ch := e.Channels("claude-desktop"); len(ch) == 0 || ch[0] != config.ChanElicitation {
		t.Errorf("owner channels = %v", ch)
	}
	for _, a := range []string{"vscode", "wsl", "wslc", "lan"} {
		for _, ch := range e.Channels(a) {
			if ch == config.ChanElicitation {
				t.Errorf("%s can approve its own calls via elicitation", a)
			}
		}
	}
	if len(e.Channels("")) != 0 {
		t.Error("anonymous has approval channels")
	}
}
