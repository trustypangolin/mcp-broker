package config

import (
	"maps"
	"slices"
	"testing"
)

func TestSeesGroupSelector(t *testing.T) {
	aws := Connector{Group: "AWS"}
	r := Role{Connectors: []string{"group:aws", "!aws-admin"}}
	if !r.Sees("aws-ro", aws) || r.Sees("aws-admin", aws) || r.Sees("imap", Connector{}) {
		t.Error("group selector evaluation wrong")
	}
	if (Role{Connectors: []string{"group:"}}).Sees("x", Connector{}) {
		t.Error("an empty group selector must not match ungrouped connectors")
	}
}

func TestRoleSelectors(t *testing.T) {
	conns := map[string]Connector{
		"aws-ro":    {Group: "AWS"},
		"aws-admin": {Group: "AWS"},
		"imap":      {Tags: []string{"mail"}},
		"excel":     {Group: "Office"},
		"word":      {Group: "Office"},
		"comfy":     {},
	}
	visible := func(sels []string) []string {
		var out []string
		for _, n := range slices.Sorted(maps.Keys(conns)) {
			if (Role{Connectors: sels}).Sees(n, conns[n]) {
				out = append(out, n)
			}
		}
		return out
	}
	set := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	for _, tc := range []struct {
		name    string
		current []string
		want    []string
		expect  []string
	}{
		{"unchanged keeps hand-written list", []string{"*", "!tag:mail"}, []string{"aws-admin", "aws-ro", "comfy", "excel", "word"}, []string{"*", "!tag:mail"}},
		{"untick one", []string{"*", "!tag:mail"}, []string{"aws-ro", "comfy", "excel", "word"}, []string{"*", "!tag:mail", "!aws-admin"}},
		{"whole group off", []string{"*", "!tag:mail"}, []string{"aws-admin", "aws-ro", "comfy"}, []string{"*", "!tag:mail", "!group:Office"}},
		{"whole group on from nothing", nil, []string{"excel", "word"}, []string{"group:Office"}},
		{"retick drops the exception", []string{"*", "!tag:mail", "!aws-admin"}, []string{"aws-admin", "aws-ro", "comfy", "excel", "word"}, []string{"*", "!tag:mail"}},
		{"group back on drops its selector", []string{"*", "!group:Office"}, []string{"aws-admin", "aws-ro", "comfy", "excel", "imap", "word"}, []string{"*"}},
		{"mixed group keeps the group selector", []string{"group:AWS"}, []string{"aws-ro"}, []string{"group:AWS", "!aws-admin"}},
		{"everything off", []string{"*"}, nil, []string{"*", "!group:AWS", "!group:Office", "!comfy", "!imap"}},
	} {
		got := RoleSelectors(tc.current, conns, set(tc.want...))
		if !slices.Equal(got, tc.expect) {
			t.Errorf("%s: selectors %q, want %q", tc.name, got, tc.expect)
		}
		if v := visible(got); !slices.Equal(v, tc.want) {
			t.Errorf("%s: sees %q, want %q", tc.name, v, tc.want)
		}
	}
}
