package awsconfig

import (
	"strings"
	"testing"
)

const cfgText = `[sso-session corp]
sso_start_url = https://example.awsapps.com/start
sso_region = ap-southeast-2

[default]
region = us-east-1

[profile dev]
sso_session = corp
sso_account_id = 111111111111
sso_role_name = Dev
region = ap-southeast-2

[profile  ra-readonly ]
credential_process = signer credential-process --certificate c.pem --private-key k.pem
region = ap-southeast-2
s3 =
  max_concurrent_requests = 5

[profile deploy]
role_arn = arn:aws:iam::222222222222:role/Deploy
source_profile = dev

[profile ci]
role_arn = arn:aws:iam::222222222222:role/CI
source_profile = keyed
`

const credText = `[keyed]
aws_access_key_id = AKIAEXAMPLE
aws_secret_access_key = secret
`

func TestProfiles(t *testing.T) {
	ps := Profiles(parse(strings.NewReader(cfgText)), parse(strings.NewReader(credText)))
	want := map[string]Profile{
		"default":     {Name: "default", Kind: Unknown, Region: "us-east-1"},
		"dev":         {Name: "dev", Kind: SSO, Region: "ap-southeast-2", Interactive: true},
		"ra-readonly": {Name: "ra-readonly", Kind: Process, Region: "ap-southeast-2"},
		"deploy":      {Name: "deploy", Kind: AssumeRole, Interactive: true},
		"ci":          {Name: "ci", Kind: AssumeRole},
		"keyed":       {Name: "keyed", Kind: Keys},
	}
	if len(ps) != len(want) {
		t.Fatalf("got %d profiles: %+v", len(ps), ps)
	}
	for i, p := range ps {
		if p != want[p.Name] {
			t.Errorf("%s: got %+v, want %+v", p.Name, p, want[p.Name])
		}
		if i > 0 && ps[i-1].Name > p.Name {
			t.Error("not sorted")
		}
	}
}
