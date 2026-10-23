package termsafe

import (
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"plain text\n":                "plain text\n",
		"tab\tand ünïcode ✓":          "tab\tand ünïcode ✓",
		"\x1b]52;c;ZXZpbA==\x07":      "?]52;c;ZXZpbA==?",
		"\x1b[2J\x1b[Hfake prompt $ ": "?[2J?[Hfake prompt $ ",
		"c1 \u009b31m csi":            "c1 ?31m csi",
		"bad utf8 \xff here":          "bad utf8 ? here",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriterKeepsSplitRunes(t *testing.T) {
	var out strings.Builder
	w := &Writer{W: &out}
	s := "✓ done\r\nok\x1b[0m"
	for i := 0; i < len(s); i++ {
		if _, err := w.Write([]byte{s[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if got := out.String(); got != "✓ done\nok?[0m" {
		t.Errorf("got %q", got)
	}
}
