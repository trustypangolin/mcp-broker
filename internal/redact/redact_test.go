package redact

import "testing"

func TestIsSecretKey(t *testing.T) {
	for _, k := range []string{"FAL_KEY", "replicate_api_token", "EXTENSION_SECRET", "DB_PASSWORD", "Authorization_Token"} {
		if !IsSecretKey(k) {
			t.Errorf("IsSecretKey(%q) = false", k)
		}
	}
	for _, k := range []string{"VIRTUAL_ENV", "EXTENSION_PORT", "MCP_AUTHOR"} {
		if IsSecretKey(k) {
			t.Errorf("IsSecretKey(%q) = true", k)
		}
	}
}

func TestRedactor(t *testing.T) {
	r := New()
	r.Add("abcd", "abcdefgh", "xy", "")
	got := r.String("token=abcdefgh other=abcd port=xy")
	want := "token=<redacted> other=<redacted> port=xy"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	var nilR *Redactor
	if nilR.String("abcd") != "abcd" {
		t.Error("nil Redactor should pass strings through")
	}
}
