package cli

import "testing"

// A number names an issue's every key, the copy an acknowledgements route
// reads included, so `route --trace '#N'` shows why it was or wasn't thanked.
func TestKeyMatches(t *testing.T) {
	for _, tc := range []struct {
		external, key string
		want          bool
	}{
		{"issue/12", "#12", true},
		{"issue/12/generation/reopen-9", "12", true},
		{"issue/12/ack", "#12", true},
		{"pr/12", "#12", true},
		{"issue/12/ack", "#1", false},
		{"issue/120/ack", "#12", false},
		{"issue/13", "#12", false},
	} {
		if got := keyMatches(tc.external, tc.key); got != tc.want {
			t.Errorf("keyMatches(%q, %q) = %v, want %v", tc.external, tc.key, got, tc.want)
		}
	}
}
