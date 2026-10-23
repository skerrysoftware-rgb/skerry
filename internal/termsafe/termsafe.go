// Package termsafe strips terminal control sequences from text that came
// from a VM or a repository before it reaches the user's terminal, so it
// can't retitle the window, write the clipboard (OSC 52), forge hyperlinks
// or redraw what the user sees.
package termsafe

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Clean replaces control characters other than newline and tab with '?',
// as it does a carriage return that doesn't end a line (which would let
// text overwrite what came before it) and the bidirectional controls that
// reorder what is displayed.
func Clean(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !needsCleaning(s) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unsafe(r) {
			b.WriteByte('?')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func needsCleaning(s string) bool {
	for _, r := range s {
		if unsafe(r) {
			return true
		}
	}
	return false
}

// unsafe reports C0 and C1 controls (ESC, BEL, CSI...) and DEL, except
// newline, carriage return and tab.
func unsafe(r rune) bool {
	switch r {
	case '\n', '\t':
		return false
	}
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == utf8.RuneError ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f
}

// Writer cleans everything written through it.
type Writer struct {
	W       io.Writer
	partial []byte // an incomplete UTF-8 sequence from the last write
}

func (w *Writer) Write(p []byte) (int, error) {
	data := append(w.partial, p...)
	w.partial = nil
	// Hold back a rune split across writes rather than mangle it.
	cut := len(data)
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	w.partial = append([]byte(nil), data[cut:]...)
	if _, err := io.WriteString(w.W, Clean(string(data[:cut]))); err != nil {
		return 0, err
	}
	return len(p), nil
}
