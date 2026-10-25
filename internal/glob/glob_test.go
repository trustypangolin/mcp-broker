package glob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true},
		{"*", "anything/at\\all", true},
		{"comfy/server_info", "comfy/server_info", true},
		{"comfy/search_*", "comfy/search_models", true},
		{"comfy/search_*", "comfy/server_info", false},
		{"imap/*delete*", "imap/imap_bulk_delete_by_search", true},
		{"*/*digital_signature*", "word-live/add_digital_signature", true},
		{"profile/*/start", "profile/comfyui/start", true},
		{"profile/*/start", "profile/comfyui/stop", false},
		{"*\\claude.exe", `C:\Users\me\.local\bin\claude.exe`, true},
		{"*\\Code.exe", `C:\Program Files\Microsoft VS Code\code.exe`, true},
		{"?ap/x", "imap/x", false},
		{"i?ap/x", "imap/x", true},
		{"*a*a*a*b", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaac", false},
		{"abc", "abcd", false},
		{"abc*", "abc", true},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.s); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}
