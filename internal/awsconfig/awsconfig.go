// Package awsconfig lists the profiles in the AWS shared config files, so the dashboard
// can attach them as connectors. It reads names, kinds, regions, account IDs and role
// names only: never keys, tokens or credential_process command lines (from those it takes
// only a --role-arn value that is exactly an IAM role ARN).
package awsconfig

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	// AccountID and Role are the account and IAM role the profile acts as, when the config
	// says (sso_account_id / sso_role_name, role_arn, or a credential_process --role-arn).
	AccountID string `json:"account_id,omitempty"`
	Role      string `json:"role,omitempty"`
	// Interactive is true when credentials can lapse and need a person to sign in again.
	Interactive bool `json:"interactive"`
}

// roleARN matches an IAM role ARN and captures its account and role name (the last path
// element).
var roleARN = regexp.MustCompile(`^arn:aws[a-z-]*:iam::(\d{12}):role/(?:[\w+=,.@-]+/)*([\w+=,.@-]+)$`)

// accountDigits matches an AWS account ID.
var accountDigits = regexp.MustCompile(`^\d{12}$`)

// parseRoleARN returns the account and role name in arn, or "" twice.
func parseRoleARN(arn string) (account, role string) {
	if m := roleARN.FindStringSubmatch(strings.Trim(arn, `"'`)); m != nil {
		return m[1], m[2]
	}
	return "", ""
}

// processRoleARN finds the --role-arn argument of a credential_process command line (IAM
// Roles Anywhere's aws_signing_helper) and returns it only if it is a role ARN. Nothing
// else on the line is looked at, as it may name key files or carry secrets.
func processRoleARN(cmd string) string {
	f := strings.Fields(cmd)
	for i, a := range f {
		v, ok := strings.CutPrefix(a, "--role-arn=")
		if !ok && a == "--role-arn" && i+1 < len(f) {
			v, ok = f[i+1], true
		}
		if ok {
			if acct, _ := parseRoleARN(v); acct != "" {
				return strings.Trim(v, `"'`)
			}
		}
	}
	return ""
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
		switch {
		case kv["sso_account_id"] != "":
			if accountDigits.MatchString(kv["sso_account_id"]) {
				p.AccountID = kv["sso_account_id"]
			}
			p.Role = kv["sso_role_name"]
		case kv["role_arn"] != "":
			p.AccountID, p.Role = parseRoleARN(kv["role_arn"])
		case kv["credential_process"] != "":
			p.AccountID, p.Role = parseRoleARN(processRoleARN(kv["credential_process"]))
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Profile) int { return strings.Compare(a.Name, b.Name) })
	return out
}
