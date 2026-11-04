// Package awsconfig lists the profiles in the AWS shared config files, so the dashboard
// can attach them as connectors. It reads names, kinds and regions only: never keys,
// tokens or credential_process command lines.
package awsconfig

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Kind is how a profile gets credentials.
type Kind string

const (
	SSO        Kind = "sso"         // IAM Identity Center: needs `aws sso login` now and then
	Process    Kind = "process"     // credential_process, e.g. IAM Roles Anywhere certificates
	AssumeRole Kind = "assume-role" // role_arn on top of another profile
	Keys       Kind = "keys"        // static keys in the credentials file
	Unknown    Kind = "unknown"
)

// Profile is one AWS profile.
type Profile struct {
	Name   string `json:"name"`
	Kind   Kind   `json:"kind"`
	Region string `json:"region,omitempty"`
	// Interactive is true when credentials can lapse and need a person to sign in again.
	Interactive bool `json:"interactive"`
}

// Paths returns the config and credentials files, honouring AWS_CONFIG_FILE and
// AWS_SHARED_CREDENTIALS_FILE as the AWS CLI does.
func Paths() (config, credentials string) {
	home, _ := os.UserHomeDir()
	config = os.Getenv("AWS_CONFIG_FILE")
	if config == "" {
		config = filepath.Join(home, ".aws", "config")
	}
	credentials = os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if credentials == "" {
		credentials = filepath.Join(home, ".aws", "credentials")
	}
	return config, credentials
}

// Load reads the profiles from the default files. Missing files are not an error.
func Load() ([]Profile, error) {
	cfgPath, credPath := Paths()
	cfg, err := readFile(cfgPath)
	if err != nil {
		return nil, err
	}
	cred, err := readFile(credPath)
	if err != nil {
		return nil, err
	}
	return Profiles(cfg, cred), nil
}

func readFile(path string) (map[string]map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f), nil
}

// parse reads INI sections into section -> key -> value. Indented lines (sub-settings
// such as `s3 =` blocks) are skipped.
func parse(r io.Reader) map[string]map[string]string {
	out := map[string]map[string]string{}
	var cur map[string]string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' && strings.HasSuffix(line, "]") {
			name := strings.Join(strings.Fields(line[1:len(line)-1]), " ")
			if out[name] == nil {
				out[name] = map[string]string{}
			}
			cur = out[name]
			continue
		}
		if cur == nil || raw[0] == ' ' || raw[0] == '\t' {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			cur[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return out
}

// Profiles merges parsed config and credentials sections into a sorted profile list.
func Profiles(cfg, cred map[string]map[string]string) []Profile {
	byName := map[string]map[string]string{}
	for sec, kv := range cfg {
		switch {
		case sec == "default":
			byName["default"] = kv
		case strings.HasPrefix(sec, "profile "):
			byName[strings.TrimPrefix(sec, "profile ")] = kv
		}
	}
	keys := map[string]bool{}
	for sec, kv := range cred {
		if kv["aws_access_key_id"] != "" {
			keys[sec] = true
		}
		if byName[sec] == nil {
			byName[sec] = map[string]string{}
		}
	}
	var out []Profile
	for name, kv := range byName {
		p := Profile{Name: name, Region: kv["region"], Kind: Unknown}
		switch {
		case kv["sso_session"] != "" || kv["sso_start_url"] != "":
			p.Kind, p.Interactive = SSO, true
		case kv["credential_process"] != "":
			p.Kind = Process
		case kv["role_arn"] != "":
			p.Kind = AssumeRole
			// MFA or an SSO source profile means a person is needed again later.
			p.Interactive = kv["mfa_serial"] != ""
			if src := byName[kv["source_profile"]]; src != nil && (src["sso_session"] != "" || src["sso_start_url"] != "") {
				p.Interactive = true
			}
		case keys[name]:
			p.Kind = Keys
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Profile) int { return strings.Compare(a.Name, b.Name) })
	return out
}
