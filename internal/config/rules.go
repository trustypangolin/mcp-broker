package config

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetRule returns data with the rule for match in roles.<role>.rules set to action, or
// removed when action is "". A rule already written for exactly that match is changed in
// place; a new rule for one exact tool goes first, so it beats any glob, and a new glob
// goes last, so globs written by hand still win over it. Comments and every other rule are
// kept. Rules are first-match, so this order is what the policy engine sees.
func SetRule(data []byte, role, match string, action Action) ([]byte, error) {
	if action != "" && action != Allow && action != Approve && action != Deny {
		return nil, fmt.Errorf("action is allow, approve or deny")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config is not a mapping")
	}
	roles := lookup(doc.Content[0], "roles")
	if roles == nil || roles.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("no roles in the config")
	}
	r := lookup(roles, role)
	if r == nil {
		return nil, fmt.Errorf("no role %s in the config", role)
	}
	if r.Kind == yaml.ScalarNode && r.Tag == "!!null" {
		*r = yaml.Node{Kind: yaml.MappingNode, LineComment: r.LineComment}
	}
	if r.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("roles.%s is not a mapping", role)
	}

	ki := -1
	for i := 0; i+1 < len(r.Content); i += 2 {
		if r.Content[i].Value == "rules" {
			ki = i
		}
	}
	var seq *yaml.Node
	if ki >= 0 {
		seq = r.Content[ki+1]
		if seq.Kind == yaml.ScalarNode && seq.Tag == "!!null" {
			*seq = yaml.Node{Kind: yaml.SequenceNode}
		}
		if seq.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("roles.%s.rules is not a list", role)
		}
	}

	found := -1
	if seq != nil {
		for i, it := range seq.Content {
			if m := lookup(it, "match"); m != nil && m.Value == match {
				found = i
				break
			}
		}
	}

	switch {
	case action == "" && found < 0:
		return data, nil
	case action == "":
		seq.Content = append(seq.Content[:found], seq.Content[found+1:]...)
		if len(seq.Content) == 0 {
			r.Content = append(r.Content[:ki], r.Content[ki+2:]...)
		}
	case found >= 0:
		a := lookup(seq.Content[found], "action")
		if a == nil {
			seq.Content[found].Content = append(seq.Content[found].Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "action"}, &yaml.Node{Kind: yaml.ScalarNode})
			a = lookup(seq.Content[found], "action")
		}
		a.Kind, a.Tag, a.Value, a.Style = yaml.ScalarNode, "!!str", string(action), 0
	default:
		var item yaml.Node
		if err := item.Encode(Rule{Match: match, Action: action}); err != nil {
			return nil, err
		}
		item.Style = 0
		if seq == nil {
			// A role written inline ({connectors: [...]}) becomes a block so the rules list
			// reads like the rest of the file; its own values keep their style.
			r.Style &^= yaml.FlowStyle
			seq = &yaml.Node{Kind: yaml.SequenceNode}
			r.Content = append(r.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "rules"}, seq)
		}
		seq.Style &^= yaml.FlowStyle
		if isGlob(match) {
			seq.Content = append(seq.Content, &item)
		} else {
			seq.Content = append([]*yaml.Node{&item}, seq.Content...)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	enc.Close()
	return buf.Bytes(), nil
}

func isGlob(s string) bool { return strings.ContainsAny(s, "*?[") }
