package config

import (
	"reflect"
	"strings"
	"testing"
)

// Every Profile field must have a ProfileEntry field of the same YAML name, or a dashboard
// save silently drops it from broker.yaml.
func TestProfileEntryMirrorsProfile(t *testing.T) {
	names := func(typ reflect.Type) map[string]reflect.Type {
		out := map[string]reflect.Type{}
		for i := range typ.NumField() {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			out[name] = f.Type
		}
		return out
	}
	entry := names(reflect.TypeFor[ProfileEntry]())
	for name := range names(reflect.TypeFor[Profile]()) {
		if _, ok := entry[name]; !ok {
			t.Errorf("ProfileEntry has no %q field", name)
		}
	}
	for sub, pair := range map[string][2]reflect.Type{
		"health": {reflect.TypeFor[Health](), reflect.TypeFor[HealthEntry]()},
		"stop":   {reflect.TypeFor[Stop](), reflect.TypeFor[StopEntry]()},
	} {
		have := names(pair[1])
		for name := range names(pair[0]) {
			if _, ok := have[name]; !ok {
				t.Errorf("%s entry has no %q field", sub, name)
			}
		}
	}
}

const comfyProfile = `# my broker
profiles:
  # the main one
  comfy:
    description: ComfyUI
    cwd: G:\Ai
    command: G:\venv\Scripts\python.exe
    args:
      - main.py
      - --port
      - "8188"
      - --enable-cors-header
      - "*"
    env_file: G:\Ai\.env
    env:
      UV_LINK_MODE: copy
    path_prepend: ['G:\venv\Scripts']
    ports: [8188]
    # I: is not always unlocked
    requires_paths: ['I:\in']
    health:
      http: http://127.0.0.1:8188/system_stats
      start_timeout: 10m
    stop:
      timeout: 30s
    update:
      - [git, -C, 'G:\ComfyUI', fetch, --tags]
      - [git, -C, 'G:\ComfyUI', reset, --hard, "${COMFYUI_VERSION}"]
  other:
    command: other.exe
`

// A form round trip writes back what was read: no load-time defaults, no absolute env_file,
// ${NAME} unexpanded, and the person's comments and quoting kept.
func TestProfileEntryRoundTrip(t *testing.T) {
	var e ProfileEntry
	if ok, err := GetEntry([]byte(comfyProfile), "profiles", "comfy", &e); !ok || err != nil {
		t.Fatalf("get: %v %v", ok, err)
	}
	if e.EnvFile != `G:\Ai\.env` || e.Health.Interval != "" || e.Update[1][5] != "${COMFYUI_VERSION}" || e.Ports[0] != 8188 {
		t.Fatalf("entry: %+v", e)
	}
	out, err := SetEntry([]byte(comfyProfile), "profiles", "comfy", e)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != comfyProfile {
		t.Errorf("unchanged save changed the file:\n%s", out)
	}

	e.Description = "ComfyUI 0.44"
	e.Update = append(e.Update, FlowList{"uv", "pip", "install", "comfy-cli"})
	e.Normalize()
	out, err = SetEntry([]byte(comfyProfile), "profiles", "comfy", e)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"# the main one\n  comfy:", "# I: is not always unlocked\n    requires_paths: ['I:\\in']",
		"description: ComfyUI 0.44\n", `- [git, -C, 'G:\ComfyUI', reset, --hard, "${COMFYUI_VERSION}"]`,
		"- [uv, pip, install, comfy-cli]\n", "  other:\n    command: other.exe",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	cfg, err := Parse(out, "")
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Profiles["comfy"]; len(p.Update) != 3 || p.Health.StartTimeout.String() != "10m0s" || p.Health.Interval.String() != "3s" {
		t.Errorf("parsed: %+v", p)
	}
}

// Maps keep the file's key order (Go encodes them sorted) and its inline/block style; new
// keys go last.
func TestSetEntryKeepsKeyOrder(t *testing.T) {
	in := "connectors:\n  c:\n    command: c.exe\n    env: {ZED: '1', ALPHA: '2'}\n    tool_classes:\n      zz: read\n      aa: write\n"
	out, err := SetEntry([]byte(in), "connectors", "c", ConnectorEntry{Command: "c.exe",
		Env: map[string]string{"ALPHA": "2", "ZED": "1", "MID": "3"}, ToolClasses: map[string]string{"aa": "write", "zz": "read"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "connectors:\n  c:\n    command: c.exe\n    env: {ZED: '1', ALPHA: '2', MID: \"3\"}\n    tool_classes:\n      zz: read\n      aa: write\n"
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestProfileEntryNormalize(t *testing.T) {
	e := ProfileEntry{Command: "x", Health: &HealthEntry{}, Stop: &StopEntry{}, Update: []FlowList{{}, {"a"}}}
	e.Normalize()
	if e.Health != nil || e.Stop != nil || len(e.Update) != 1 {
		t.Errorf("normalized: %+v", e)
	}
	out, err := SetEntry([]byte(editBase), "profiles", "p", e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "  p:\n    command: x\n    update:\n      - [a]\n") {
		t.Errorf("written:\n%s", out)
	}
}
