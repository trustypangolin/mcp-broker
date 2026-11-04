package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const editBase = `# top comment
agents:
  a: {role: r}   # agent a
roles:
  r: {connectors: ["*"]}
connectors:
  # the first one
  one:
    command: one.exe
profiles: {}
`

type entry struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args,omitempty"`
}

func TestSetEntryAddReplaceDelete(t *testing.T) {
	out, err := SetEntry([]byte(editBase), "connectors", "two", entry{Command: "two.exe", Args: []string{"-x"}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"# top comment", "# agent a", "# the first one", "two:\n    command: two.exe\n    args:\n      - -x"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	cfg, err := Parse(out, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Connectors["two"].Command != "two.exe" || cfg.Connectors["one"].Command != "one.exe" {
		t.Fatalf("connectors: %+v", cfg.Connectors)
	}

	out, err = SetEntry(out, "connectors", "one", entry{Command: "uno.exe"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "# the first one") || !strings.Contains(string(out), "uno.exe") {
		t.Fatalf("replace lost comment or value:\n%s", out)
	}

	out, err = SetEntry(out, "connectors", "one", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ = Parse(out, ""); len(cfg.Connectors) != 1 {
		t.Fatalf("delete: %+v", cfg.Connectors)
	}
}

func TestSetEntryIntoInlineOrMissingSection(t *testing.T) {
	out, err := SetEntry([]byte(editBase), "profiles", "p", map[string]string{"command": "p.exe"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "profiles:\n  p:\n    command: p.exe") {
		t.Fatalf("got:\n%s", out)
	}
	out, err = SetEntry([]byte("agents: {}\n"), "connectors", "c", entry{Command: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "connectors:\n  c:\n    command: c") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestWriteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.yaml")
	os.WriteFile(path, []byte(editBase), 0o600)
	h := Hash([]byte(editBase))

	if err := WriteFile(path, []byte("agents: {a: {role: missing}}\n"), h); err == nil {
		t.Fatal("invalid config written")
	}
	if err := WriteFile(path, []byte(editBase+"# v2\n"), "stale"); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale hash: %v", err)
	}
	if err := WriteFile(path, []byte(editBase+"# v2\n"), h); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path + ".bak"); string(b) != editBase {
		t.Fatal("backup not kept")
	}
	if b, _ := os.ReadFile(path); !strings.HasSuffix(string(b), "# v2\n") {
		t.Fatal("not written")
	}
}

func TestSetFieldsGroup(t *testing.T) {
	text := editBase + `  two:   # second
    description: Two
    command: two.exe
  three: {command: three.exe}
`
	text = strings.Replace(text, "profiles: {}\n", "", 1) + "profiles:\n  app:\n    command: app.exe\n"
	out, err := SetFields([]byte(text), "connectors", []string{"one", "two", "three"}, "group", "ArtCraft MCPs")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"# the first one", "# second", "description: Two\n    group: ArtCraft MCPs\n    command: two.exe"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	cfg, err := Parse(out, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"one", "two", "three"} {
		if cfg.Connectors[n].Group != "ArtCraft MCPs" {
			t.Errorf("%s: group %q", n, cfg.Connectors[n].Group)
		}
	}
	// Rename one, clear another.
	out, _ = SetFields(out, "connectors", []string{"one"}, "group", "Other")
	out, _ = SetFields(out, "connectors", []string{"two"}, "group", "")
	cfg, _ = Parse(out, "")
	if cfg.Connectors["one"].Group != "Other" || cfg.Connectors["two"].Group != "" || strings.Count(string(out), "group:") != 2 {
		t.Errorf("rename/clear:\n%s", out)
	}
	out, err = SetFields(out, "profiles", []string{"app"}, "group", "GPU")
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ = Parse(out, ""); cfg.Profiles["app"].Group != "GPU" {
		t.Errorf("profile group:\n%s", out)
	}
	if _, err := SetFields(out, "connectors", []string{"nope"}, "group", "x"); err == nil {
		t.Error("unknown entry accepted")
	}
	// A group name YAML would read as another type stays a string.
	out, _ = SetFields(out, "connectors", []string{"one"}, "group", "true")
	if cfg, err = Parse(out, ""); err != nil || cfg.Connectors["one"].Group != "true" {
		t.Errorf("quoted group: %v\n%s", err, out)
	}
}
