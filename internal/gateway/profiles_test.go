package gateway

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

// withProfile adds a profile running the test binary as a sleeper, and profile
// permissions: owner may start with approval, coder sees no profiles.
func withProfile(networkExposed bool) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Profiles["app"] = config.Profile{
			Description: "Sleeper", Command: os.Args[0], Env: map[string]string{"FAKEMCP_SLEEPER": "1"},
			NetworkExposed: networkExposed,
			Stop:           config.Stop{Timeout: 2 * time.Second},
			Health:         config.Health{StartTimeout: time.Minute, Interval: time.Second},
		}
		r := cfg.Roles["owner"]
		r.Profiles = map[string]config.Action{"status": config.Allow, "logs": config.Allow, "plan": config.Allow, "start": config.Approve, "stop": config.Allow}
		cfg.Roles["owner"] = r
		r = cfg.Roles["outsider"]
		r.Profiles = map[string]config.Action{"status": config.Allow, "start": config.Allow}
		cfg.Roles["outsider"] = r
	}
}

func (h *harness) profile(action string, extra map[string]any) (string, bool) {
	h.t.Helper()
	args := map[string]any{"name": "app", "action": action}
	for k, v := range extra {
		args[k] = v
	}
	return h.call("profile", args)
}

func TestProfileStartNeedsApprovalThenStops(t *testing.T) {
	h := newHarness(t, "", withProfile(false))
	out, _ := h.call("list_profiles", nil)
	if !strings.Contains(out, `"app"`) || !strings.Contains(out, `"stopped"`) {
		t.Fatalf("list_profiles = %s", out)
	}
	if out, _ := h.as("coder").call("list_profiles", nil); strings.Contains(out, "app") {
		t.Fatalf("coder sees the profile: %s", out)
	}
	if out, isErr := h.profile("status", nil); !isErr || !strings.Contains(out, "unknown profile") {
		t.Fatalf("coder status = %s", out)
	}

	h.as("owner")
	if out, _ := h.profile("plan", nil); !strings.Contains(out, "command:") || !strings.Contains(out, "FAKEMCP_SLEEPER") {
		t.Fatalf("plan = %s", out)
	}

	var asked string
	h.elicit = func(p *mcp.ElicitParams) *mcp.ElicitResult {
		asked = p.Message
		return &mcp.ElicitResult{Action: "decline"}
	}
	if out, isErr := h.profile("start", nil); !isErr || !strings.Contains(out, "not approved") {
		t.Fatalf("declined start = %s", out)
	}
	if !strings.Contains(asked, "wants to start app") || !strings.Contains(asked, "command:") {
		t.Fatalf("approval prompt did not show the plan: %q", asked)
	}
	if h.sup.Status("app").State != "stopped" {
		t.Fatal("started without approval")
	}

	h.elicit = func(*mcp.ElicitParams) *mcp.ElicitResult {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}
	}
	out, isErr := h.profile("start", nil)
	if isErr || !strings.Contains(out, "start app") {
		t.Fatalf("approved start = %s", out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.sup.Status("app").State != "ready" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	out, _ = h.profile("status", nil)
	var st struct{ State string }
	json.Unmarshal([]byte(out), &st)
	if st.State != "ready" {
		t.Fatalf("status = %s", out)
	}
	if out, isErr := h.profile("start", nil); !isErr || !strings.Contains(out, "already") {
		t.Fatalf("second start = %s", out)
	}

	out, isErr = h.profile("stop", nil) // allow: no prompt
	if isErr || !strings.Contains(out, `"stopped"`) {
		t.Fatalf("stop = %s", out)
	}
}

func TestNetworkExposedStartAlwaysNeedsApproval(t *testing.T) {
	h := newHarness(t, "", withProfile(true))
	// outsider's role allows start outright, but the hard guard still asks.
	h.as("outsider")
	h.elicit = func(*mcp.ElicitParams) *mcp.ElicitResult { return &mcp.ElicitResult{Action: "decline"} }
	out, _ := h.profile("start", nil)
	if !strings.Contains(out, "pending_approval") {
		t.Fatalf("exposed start = %s", out)
	}
	if h.sup.Status("app").State != "stopped" {
		t.Fatal("network-exposed start went ahead without approval")
	}
}
