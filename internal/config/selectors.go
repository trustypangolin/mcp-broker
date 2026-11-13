package config

import (
	"maps"
	"slices"
	"strings"

	"github.com/trustypangolin/mcp-broker/internal/glob"
)

// Sees reports whether the role grants connector cn (named connector). Selectors are
// evaluated in order and the last one that matches decides, so ["*", "!tag:mail"] means
// everything except mail. A selector is a glob on the name, tag:<tag>, or group:<group>
// (the dashboard heading the connector is filed under); tags and groups match ignoring
// case, and ! in front excludes.
func (r Role) Sees(connector string, cn Connector) bool {
	seen := false
	for _, sel := range r.Connectors {
		if hit, neg := selectorHit(sel, connector, cn); hit {
			seen = !neg
		}
	}
	return seen
}

func selectorHit(sel, connector string, cn Connector) (hit, neg bool) {
	neg = strings.HasPrefix(sel, "!")
	s := strings.TrimPrefix(sel, "!")
	if t, ok := strings.CutPrefix(s, "tag:"); ok {
		return slices.ContainsFunc(cn.Tags, func(x string) bool { return strings.EqualFold(x, t) }), neg
	}
	if g, ok := strings.CutPrefix(s, "group:"); ok {
		return cn.Group != "" && strings.EqualFold(cn.Group, g), neg
	}
	return glob.Match(s, connector), neg
}

// exactName reports whether sel (without !) names one connector: no glob, tag or group.
func exactName(s string) bool {
	return !strings.HasPrefix(s, "tag:") && !strings.HasPrefix(s, "group:") && !strings.ContainsAny(s, "*?")
}

// RoleSelectors returns the selectors a role needs so that it sees exactly the connectors
// in want (by name) out of conns, starting from the selectors it has now. What a person
// wrote by hand (*, globs, tags) is kept, so connectors added later still follow it; only
// the differences are added at the end, where the last-match rule makes them win:
//
//   - a group whose members are all wanted, or all unwanted, gets group:<name> or
//     !group:<name> (so new members of that group follow it too);
//   - any other connector that still differs gets its own name or !name.
//
// Exact names and group selectors that the new list makes redundant are dropped first, so
// ticking a connector on and off again doesn't pile up entries.
func RoleSelectors(current []string, conns map[string]Connector, want map[string]bool) []string {
	names := slices.Sorted(maps.Keys(conns))
	groups := map[string][]string{} // lower-cased group → members
	groupName := map[string]string{}
	for _, n := range names {
		if g := conns[n].Group; g != "" {
			k := strings.ToLower(g)
			groups[k] = append(groups[k], n)
			groupName[k] = g
		}
	}
	uniform := func(k string) (bool, bool) { // (all members agree, their value)
		ms := groups[k]
		v := want[ms[0]]
		for _, m := range ms[1:] {
			if want[m] != v {
				return false, false
			}
		}
		return true, v
	}

	// Keep everything but exact names of known connectors, and group selectors for groups
	// the new state decides as a whole (they are re-added below if still needed).
	var base []string
	for _, sel := range current {
		s := strings.TrimPrefix(sel, "!")
		if exactName(s) {
			if _, ok := conns[s]; ok {
				continue
			}
		}
		if g, ok := strings.CutPrefix(s, "group:"); ok {
			if _, known := groups[strings.ToLower(g)]; known {
				if same, _ := uniform(strings.ToLower(g)); same {
					continue
				}
			}
		}
		base = append(base, sel)
	}
	sees := func(sels []string, n string) bool { return Role{Connectors: sels}.Sees(n, conns[n]) }

	out := slices.Clone(base)
	for _, k := range slices.Sorted(maps.Keys(groups)) {
		same, v := uniform(k)
		if !same {
			continue
		}
		differs := false
		for _, m := range groups[k] {
			if sees(out, m) != v {
				differs = true
			}
		}
		if differs {
			sel := "group:" + groupName[k]
			if !v {
				sel = "!" + sel
			}
			out = append(out, sel)
		}
	}
	for _, n := range names {
		if sees(out, n) != want[n] {
			if want[n] {
				out = append(out, n)
			} else {
				out = append(out, "!"+n)
			}
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}
