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
