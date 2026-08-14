package routing

import (
	"strings"
	"testing"
)

// TestSanitizeUpstreamMessage valida o helper ÚNICO de sanitização de
// mensagens upstream (issue #19): colapsa whitespace, trunca em 200 chars.
func TestSanitizeUpstreamMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"vazio", "", ""},
		{"newlines colapsados", "line1\nline2\nline3", "line1 line2 line3"},
		{"tabs colapsados", "a\t\t\tb", "a b"},
		{"espaços múltiplos", "a    b", "a b"},
		{"truncado > 200", strings.Repeat("a", 300), strings.Repeat("a", 200) + "..."},
		{"preserva curto", "short message", "short message"},
		{"trim nas bordas", "  padded  ", "padded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeUpstreamMessage([]byte(tc.in))
			if got != tc.want {
				t.Errorf("SanitizeUpstreamMessage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
