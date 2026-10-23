package termsafe

import (
	"strings"
	"testing"
)

func FuzzClean(f *testing.F) {
	f.Add("\x1b]52;c;ZXZpbA==\x07plain")
	f.Add("\xc2\x9b31m")
	f.Fuzz(func(t *testing.T, s string) {
		out := Clean(s)
		for _, r := range out {
			if unsafe(r) {
				t.Fatalf("Clean(%q) kept control %U", s, r)
			}
		}
		var b strings.Builder
		w := &Writer{W: &b}
		for i := 0; i < len(s); i += 3 {
			_, _ = w.Write([]byte(s[i:min(i+3, len(s))]))
		}
		for _, r := range b.String() {
			if unsafe(r) && r != '?' {
				t.Fatalf("Writer passed control %U", r)
			}
		}
	})
}
