package image

import "testing"

// The index comes from the network; it is verified before parsing, but the
// parser must never panic whatever a signer (or a bug) puts in it.
func FuzzParseIndex(f *testing.F) {
	f.Add("P:busybox\nV:1.37.0-r30\nC:Q1abc\nS:500\nD:so:libc.so.6\np:cmd:sh=1\nk:100\n\nP:x\nV:1-r0\nC:Q1\nS:1")
	f.Add("P:../evil\nV:1\nC:Q1\nS:1")
	f.Fuzz(func(t *testing.T, text string) {
		pkgs, err := ParseIndex(text, "repo")
		if err != nil {
			return
		}
		r := NewResolver(pkgs)
		for _, p := range pkgs {
			_, _ = r.Resolve([]string{p.Name})
		}
	})
}

func FuzzCompareVersions(f *testing.F) {
	f.Add("1.2.3-r1", "1.2.3_rc1-r0")
	f.Add("3.13.16_git20261016-r2", "3.13.16-r2")
	f.Fuzz(func(t *testing.T, a, b string) {
		if CompareVersions(a, b) != -CompareVersions(b, a) {
			t.Errorf("CompareVersions(%q, %q) is not antisymmetric", a, b)
		}
		if CompareVersions(a, a) != 0 {
			t.Errorf("CompareVersions(%q, itself) != 0", a)
		}
	})
}
