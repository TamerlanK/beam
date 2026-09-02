package names

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRandom(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		name, emoji := Random()
		adj, animal, ok := strings.Cut(name, " ")
		if !ok || adj == "" || animal == "" {
			t.Fatalf("name %q is not \"Adjective Animal\"", name)
		}
		if emoji == "" || !utf8.ValidString(emoji) {
			t.Fatalf("bad emoji %q for %q", emoji, name)
		}
		seen[name] = true
	}

	if len(seen) < 50 {
		t.Fatalf("only %d distinct names in 500 draws", len(seen))
	}
}

func TestClean(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  Bob   the\tBuilder ", "Bob the Builder"},
		{"evil\x00name\n", "evilname"},
		{"", ""},
		{strings.Repeat("x", 40), strings.Repeat("x", 32)},
	}
	for _, c := range cases {
		if got := CleanName(c.in); got != c.want {
			t.Errorf("CleanName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, ok := range []string{"🦊", "🐦‍⬛", "🇩🇪", "👩🏽‍💻"} {
		if CleanEmoji(ok) != ok {
			t.Errorf("CleanEmoji rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "ab", "🦊 ", "x🦊", "\n", strings.Repeat("🦊", 20)} {
		if CleanEmoji(bad) != "" {
			t.Errorf("CleanEmoji accepted %q", bad)
		}
	}
}
