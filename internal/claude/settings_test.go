package claude_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/claude"
)

const cmd = "'/opt/homebrew/bin/skerry'"

func TestInstallIntoEmpty(t *testing.T) {
	out, err := claude.Plan(nil, cmd, true, false)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatal(err)
	}
	for _, h := range claude.Hooks {
		groups := s.Hooks[h.Event]
		if len(groups) != 1 || groups[0].Hooks[0].Command != cmd+" hook "+h.Sub || groups[0].Hooks[0].Type != "command" {
			t.Errorf("%s = %+v", h.Event, groups)
		}
	}
}

func TestInstallPreservesOtherSettings(t *testing.T) {
	current := []byte(`{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls *)"], "deny": []},
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}],
    "SessionStart": [{"hooks": [{"type": "command", "command": "other-tool start"}]}]
  },
  "zeta": 1
}`)
	out, err := claude.Plan(current, cmd, true, false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// Top-level order is kept.
	if !(strings.Index(s, `"model"`) < strings.Index(s, `"permissions"`) &&
		strings.Index(s, `"permissions"`) < strings.Index(s, `"hooks"`) &&
		strings.Index(s, `"hooks"`) < strings.Index(s, `"zeta"`)) {
		t.Errorf("key order changed:\n%s", s)
	}
	for _, keep := range []string{"guard.sh", "other-tool start", `"Bash(ls *)"`} {
		if !strings.Contains(s, keep) {
			t.Errorf("lost %s:\n%s", keep, s)
		}
	}

	again, err := claude.Plan(out, cmd, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Errorf("install is not idempotent:\n%s\n---\n%s", out, again)
	}

	removed, err := claude.Plan(out, cmd, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(removed), "skerry") || strings.Contains(string(removed), "WorktreeCreate") {
		t.Errorf("uninstall left skerry hooks:\n%s", removed)
	}
	if !strings.Contains(string(removed), "other-tool start") || !strings.Contains(string(removed), "guard.sh") {
		t.Errorf("uninstall removed other hooks:\n%s", removed)
	}
}

func TestUninstallDropsEmptyHooks(t *testing.T) {
	installed, err := claude.Plan([]byte(`{"model":"opus"}`), cmd, true, false)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := claude.Plan(installed, cmd, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(removed)) != "{\n  \"model\": \"opus\"\n}" {
		t.Errorf("uninstall left %s", removed)
	}
}

func TestWorktreeHookConflict(t *testing.T) {
	current := []byte(`{"hooks":{"WorktreeCreate":[{"hooks":[{"type":"command","command":"svn-checkout.sh"}]}]}}`)
	if _, err := claude.Plan(current, cmd, true, false); !errors.Is(err, claude.ErrConflict) {
		t.Fatalf("Plan over a foreign WorktreeCreate = %v, want ErrConflict", err)
	}
	out, err := claude.Plan(current, cmd, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "svn-checkout.sh") {
		t.Error("forced install kept the foreign WorktreeCreate hook")
	}
}

func TestRejectsMalformedSettings(t *testing.T) {
	for _, bad := range []string{`[]`, `{"hooks": []}`, `{"hooks": {"SessionStart": {}}}`, `{"a":1} {"b":2}`, `{`} {
		if _, err := claude.Plan([]byte(bad), cmd, true, false); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestWriteKeepsSymlinkAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	link := filepath.Join(dir, ".claude", "settings.json")
	for _, d := range []string{filepath.Dir(real), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(real, []byte(`{"model":"opus"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	backup, err := claude.Write(link, []byte("{}\n"), "stamp")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("settings symlink was replaced: %v", err)
	}
	if got, _ := os.ReadFile(real); string(got) != "{}\n" {
		t.Errorf("target = %q", got)
	}
	if info, _ := os.Stat(real); info.Mode().Perm() != 0o640 {
		t.Errorf("mode changed to %o", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(backup); string(got) != `{"model":"opus"}` {
		t.Errorf("backup %s = %q", backup, got)
	}

	fresh := filepath.Join(dir, "new", "settings.json")
	if backup, err := claude.Write(fresh, []byte("{}\n"), "stamp"); err != nil || backup != "" {
		t.Errorf("Write to a new file = %q, %v", backup, err)
	}
}

func TestIsSkerryCommand(t *testing.T) {
	for c, want := range map[string]bool{
		"'/usr/local/bin/skerry' hook worktree-create": true,
		"/x/skerry hook session-start":                 true,
		"skerry hook something-else":                   false,
		"other hook session-start":                     false,
		"skerry hook session-start && rm -rf ~":        false,
	} {
		if got := claude.IsSkerryCommand(c); got != want {
			t.Errorf("IsSkerryCommand(%q) = %v", c, got)
		}
	}
}

func FuzzPlan(f *testing.F) {
	f.Add([]byte(`{"hooks":{"SessionStart":[{"hooks":[{"command":"x"}]}]}}`), true)
	f.Add([]byte(`{}`), false)
	f.Fuzz(func(t *testing.T, data []byte, install bool) {
		out, err := claude.Plan(data, cmd, install, false)
		if err != nil {
			return
		}
		if !json.Valid(out) {
			t.Fatalf("produced invalid JSON from %q: %s", data, out)
		}
		// Applying the same plan again changes nothing.
		again, err := claude.Plan(out, cmd, install, false)
		if err != nil || string(again) != string(out) {
			t.Fatalf("not idempotent for %q: %v", data, err)
		}
	})
}
