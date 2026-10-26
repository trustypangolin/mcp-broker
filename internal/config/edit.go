package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SetEntry returns data with section.key (a top-level mapping such as "connectors" or
// "agents") set to value, or removed when value is nil. Comments and the order of other
// entries are kept; the edited entry is re-encoded.
func SetEntry(data []byte, section, key string, value any) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 { // empty file
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config is not a mapping")
	}
	sec := lookup(root, section)
	if sec == nil || sec.Kind != yaml.MappingNode || sec.Style&yaml.FlowStyle != 0 {
		// Missing, null (`profiles:`), or written inline (`profiles: {}`): replace with a
		// block mapping so the new entry reads like the rest of the file.
		if value == nil {
			return data, nil
		}
		n := &yaml.Node{Kind: yaml.MappingNode}
		if sec != nil && sec.Kind == yaml.MappingNode {
			n.Content = sec.Content
			n.LineComment = sec.LineComment
		}
		if sec == nil {
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: section}, n)
			sec = n
		} else {
			*sec = *n
		}
	}

	idx := -1
	for i := 0; i+1 < len(sec.Content); i += 2 {
		if sec.Content[i].Value == key {
			idx = i
			break
		}
	}
	if value == nil {
		if idx >= 0 {
			sec.Content = append(sec.Content[:idx], sec.Content[idx+2:]...)
		}
	} else {
		var v yaml.Node
		if err := v.Encode(value); err != nil {
			return nil, err
		}
		if idx >= 0 {
			v.HeadComment, v.LineComment = sec.Content[idx+1].HeadComment, sec.Content[idx+1].LineComment
			sec.Content[idx+1] = &v
		} else {
			sec.Content = append(sec.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &v)
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

func lookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// Hash identifies a version of the config text, so an editor can detect that the file
// changed underneath it.
func Hash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:8])
}

// WriteFile validates data as a config for path and replaces the file with it, keeping the
// previous version as <path>.bak. If expectHash is set and the file on disk no longer
// matches it, nothing is written and ErrChanged is returned.
func WriteFile(path string, data []byte, expectHash string) error {
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if expectHash != "" && Hash(old) != expectHash {
		return ErrChanged
	}
	if _, err := Parse(data, filepath.Dir(path)); err != nil {
		return err
	}
	if old != nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ErrChanged reports a write refused because the file changed since it was read.
var ErrChanged = fmt.Errorf("the config file changed since it was loaded; reload and try again")
