package config

import (
	"strings"
	"testing"
)

const rulesBase = `roles:
  owner:   # the human's own agents
    connectors: ["*"]
    classes: {read: allow}
    rules:
      - match: "web/*" # all web tools
        action: deny
  viewer: {connectors: ["*"], classes: {read: allow}}
`

func rulesOf(t *testing.T, data []byte, role string) []Rule {
	t.Helper()
	cfg, err := Parse(data, t.TempDir())
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	return cfg.Roles[role].Rules
}

func TestSetRule(t *testing.T) {
	data := []byte(rulesBase)

	// An exact tool goes first, so it beats the hand-written glob.
	out, err := SetRule(data, "owner", "web/fetch", Allow)
	if err != nil {
		t.Fatal(err)
	}
	got := rulesOf(t, out, "owner")
	if len(got) != 2 || got[0] != (Rule{"web/fetch", Allow}) || got[1].Match != "web/*" {
		t.Fatalf("rules = %+v", got)
	}
	for _, c := range []string{"# the human's own agents", "# all web tools"} {
		if !strings.Contains(string(out), c) {
			t.Errorf("lost comment %q:\n%s", c, out)
		}
	}

	// A glob goes last; changing an existing match edits it in place.
	out, _ = SetRule(out, "owner", "aws/*", Approve)
	out, _ = SetRule(out, "owner", "web/fetch", Deny)
	got = rulesOf(t, out, "owner")
	if len(got) != 3 || got[0] != (Rule{"web/fetch", Deny}) || got[2] != (Rule{"aws/*", Approve}) {
		t.Fatalf("rules = %+v", got)
	}

	// Removing every rule removes the key; removing a missing rule changes nothing.
	for _, m := range []string{"web/fetch", "aws/*", "web/*"} {
		out, _ = SetRule(out, "owner", m, "")
	}
	if strings.Contains(string(out), "rules:") {
		t.Fatalf("empty rules key left behind:\n%s", out)
	}
	if again, _ := SetRule(out, "owner", "nope/x", ""); string(again) != string(out) {
		t.Fatal("removing a missing rule changed the file")
	}

	// An inline role gains a block rules list and still parses.
	out, err = SetRule([]byte(rulesBase), "viewer", "web/get", Allow)
	if err != nil {
		t.Fatal(err)
	}
	if got := rulesOf(t, out, "viewer"); len(got) != 1 || got[0] != (Rule{"web/get", Allow}) {
		t.Fatalf("viewer rules = %+v\n%s", got, out)
	}

	if _, err := SetRule(data, "ghost", "a/b", Allow); err == nil {
		t.Error("unknown role accepted")
	}
	if _, err := SetRule(data, "owner", "a/b", "maybe"); err == nil {
		t.Error("bad action accepted")
	}
}
