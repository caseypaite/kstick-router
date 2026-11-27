package smscfg

import (
	"strings"
	"testing"
)

func TestRandomKeyName(t *testing.T) {
	seen := map[string]bool{}
	for _, w := range keyWords {
		if seen[w] {
			t.Fatalf("duplicate word %q", w)
		}
		seen[w] = true
	}
	for i := 0; i < 500; i++ {
		n := RandomKeyName(func(s string) bool { return s == "x" })
		p := strings.Split(n, "-")
		if len(p) != 3 || p[0] == p[1] || p[1] == p[2] || p[0] == p[2] || !ValidKeyName(n) {
			t.Fatalf("bad name %q", n)
		}
	}
}
