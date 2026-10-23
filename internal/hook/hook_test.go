package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skerrysoftware-rgb/skerry/internal/hook"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/testutil"
	"github.com/skerrysoftware-rgb/skerry/internal/trust"
	"github.com/skerrysoftware-rgb/skerry/internal/workspace"
)

type harness struct {
	h      *hook.Handler
	stdout *bytes.Buffer
	env    map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	state := t.TempDir()
	h := &harness{stdout: &bytes.Buffer{}, env: map[string]string{}}
	h.h = &hook.Handler{
		Manager: &workspace.Manager{Registry: registry.Open(state), Trust: trust.Open(state)},
		Stdout:  h.stdout,
		Stderr:  &bytes.Buffer{},
		Getenv:  func(k string) string { return h.env[k] },
	}
	return h
}

func (h *harness) run(t *testing.T, sub string, input map[string]any) error {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	h.stdout.Reset()
	return h.h.Run(context.Background(), sub, bytes.NewReader(data))
}

func TestWorktreeCreatePrintsOnlyThePath(t *testing.T) {
	root := testutil.NewRepo(t)
	h := newHarness(t)
	err := h.run(t, "worktree-create", map[string]any{
		"session_id": "s1", "cwd": root, "hook_event_name": "WorktreeCreate", "name": "bold-oak-a3f2",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, ".claude", "worktrees", "bold-oak-a3f2") + "\n"
	if h.stdout.String() != want {
		t.Fatalf("stdout = %q, want exactly %q", h.stdout.String(), want)
	}
}

func TestWorktreeCreateRejectsBadInput(t *testing.T) {
	root := testutil.NewRepo(t)
	for name, input := range map[string]map[string]any{
		"wrong event":   {"cwd": root, "hook_event_name": "SessionStart", "name": "x"},
		"relative cwd":  {"cwd": "repo", "hook_event_name": "WorktreeCreate", "name": "x"},
		"traversal":     {"cwd": root, "hook_event_name": "WorktreeCreate", "name": "../../escape"},
		"slash in name": {"cwd": root, "hook_event_name": "WorktreeCreate", "name": "a/b"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if err := h.run(t, "worktree-create", input); err == nil {
				t.Fatal("accepted bad input")
			}
			if h.stdout.Len() != 0 {
				t.Fatalf("printed %q on failure", h.stdout.String())
			}
		})
	}
	if err := newHarness(t).h.Run(context.Background(), "worktree-create", strings.NewReader(strings.Repeat(" ", 2<<20))); err == nil {
		t.Error("accepted more than 1 MiB of input")
	}
}

func TestWorktreeRemoveOnlyTouchesRegisteredPaths(t *testing.T) {
	root := testutil.NewRepo(t)
	h := newHarness(t)

	// A real directory skerry never created must survive.
	foreign := filepath.Join(root, ".claude", "worktrees", "foreign")
	testutil.WriteFile(t, filepath.Join(foreign, "keep.txt"), "mine\n")
	err := h.run(t, "worktree-remove", map[string]any{"cwd": root, "hook_event_name": "WorktreeRemove", "worktree_path": foreign})
	if err == nil {
		t.Fatal("removed an unregistered path")
	}
	if _, err := os.Stat(filepath.Join(foreign, "keep.txt")); err != nil {
		t.Fatal("unregistered directory was touched")
	}

	if err := h.run(t, "worktree-create", map[string]any{"cwd": root, "hook_event_name": "WorktreeCreate", "name": "mine"}); err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSpace(h.stdout.String())
	if err := h.run(t, "worktree-remove", map[string]any{"cwd": root, "hook_event_name": "WorktreeRemove", "worktree_path": path}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists: %v", err)
	}
}

func TestWorktreeRemoveAfterManualDelete(t *testing.T) {
	root := testutil.NewRepo(t)
	h := newHarness(t)
	if err := h.run(t, "worktree-create", map[string]any{"cwd": root, "hook_event_name": "WorktreeCreate", "name": "gone"}); err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSpace(h.stdout.String())
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "worktree-remove", map[string]any{"cwd": root, "hook_event_name": "WorktreeRemove", "worktree_path": path}); err != nil {
		t.Fatal(err)
	}
	if all, _ := h.h.Manager.Registry.List(); len(all) != 0 {
		t.Errorf("registry still has %+v", all)
	}
}

func TestSessionStart(t *testing.T) {
	root := testutil.NewRepo(t)
	h := newHarness(t)
	envFile := filepath.Join(t.TempDir(), "env")
	h.env["CLAUDE_ENV_FILE"] = envFile

	// Outside any workspace: no output, no env.
	if err := h.run(t, "session-start", map[string]any{"cwd": root, "hook_event_name": "SessionStart", "source": "startup"}); err != nil {
		t.Fatal(err)
	}
	if h.stdout.Len() != 0 {
		t.Fatalf("output outside a workspace: %q", h.stdout.String())
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatal("wrote env outside a workspace")
	}

	if err := h.run(t, "worktree-create", map[string]any{"cwd": root, "hook_event_name": "WorktreeCreate", "name": "it's"}); err == nil {
		t.Fatal("quote in name accepted")
	}
	if err := h.run(t, "worktree-create", map[string]any{"cwd": root, "hook_event_name": "WorktreeCreate", "name": "ws1"}); err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSpace(h.stdout.String())

	if err := h.run(t, "session-start", map[string]any{"cwd": path, "hook_event_name": "SessionStart", "source": "startup"}); err != nil {
		t.Fatal(err)
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &out); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, h.stdout.String())
	}
	if out.HookSpecificOutput.HookEventName != "SessionStart" || !strings.Contains(out.HookSpecificOutput.AdditionalContext, `"ws1"`) {
		t.Errorf("unexpected output: %+v", out)
	}
	if !strings.Contains(out.HookSpecificOutput.AdditionalContext, "not trusted") {
		t.Error("context does not mention that the repository is untrusted")
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != "export SKERRY_WORKSPACE='ws1'\n" {
		t.Errorf("env file = %q", env)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":            "'plain'",
		"it's":             `'it'\''s'`,
		"$(rm -rf /)":      "'$(rm -rf /)'",
		"/Apps/My Tool/sk": "'/Apps/My Tool/sk'",
	} {
		if got := hook.ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func FuzzHookInput(f *testing.F) {
	f.Add([]byte(`{"cwd":"/x","hook_event_name":"WorktreeCreate","name":"a"}`))
	f.Add([]byte(`{"cwd":"/x","hook_event_name":"WorktreeRemove","worktree_path":"/x/../y"}`))
	f.Add([]byte(`{"hook_event_name":"SessionStart","cwd":"\u0000"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		state := t.TempDir()
		h := &hook.Handler{
			Manager: &workspace.Manager{Registry: registry.Open(state), Trust: trust.Open(state)},
			Stdout:  &bytes.Buffer{}, Stderr: &bytes.Buffer{},
			Getenv: func(string) string { return "" },
		}
		for sub := range hook.Events {
			_ = h.Run(context.Background(), sub, bytes.NewReader(data)) // must not panic
		}
	})
}
