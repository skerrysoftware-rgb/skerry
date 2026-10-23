package vmm

import "testing"

func TestQuiet(t *testing.T) {
	for rel, want := range map[string]bool{
		"node_modules/x/index.js": true,
		"web/dist/app.js":         true,
		".git/HEAD":               true,
		"scripts/build":           false,
		"src/build.ts":            false,
		"src/app.ts":              false,
	} {
		if got := quiet(rel); got != want {
			t.Errorf("quiet(%q) = %v, want %v", rel, got, want)
		}
	}
}
