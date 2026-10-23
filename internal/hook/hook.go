// Package hook handles Claude Code hook events. Claude passes a JSON object
// on stdin; for WorktreeCreate it reads the worktree path from stdout, so
// stdout carries nothing else and all diagnostics go to stderr.
package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/workspace"
)

// maxInput bounds how much of stdin a hook reads.
const maxInput = 1 << 20

// Input is the subset of Claude's hook input skerry uses.
type Input struct {
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	// Name is the requested worktree name (WorktreeCreate).
	Name string `json:"name"`
	// WorktreePath is the worktree being removed (WorktreeRemove).
	WorktreePath string `json:"worktree_path"`
	// Source says why a session started (SessionStart).
	Source string `json:"source"`
}

// Handler runs hook events against a workspace manager.
type Handler struct {
	Manager *workspace.Manager
	Stdout  io.Writer
	Stderr  io.Writer
	// Getenv reads the environment; tests replace it.
	Getenv func(string) string
}

// Events maps the skerry subcommand to Claude's event name.
var Events = map[string]string{
	"worktree-create": "WorktreeCreate",
	"worktree-remove": "WorktreeRemove",
	"session-start":   "SessionStart",
}

// Run handles one event.
func (h *Handler) Run(ctx context.Context, sub string, stdin io.Reader) error {
	event, ok := Events[sub]
	if !ok {
		return fmt.Errorf("unknown hook %q", sub)
	}
	in, err := readInput(stdin)
	if err != nil {
		return err
	}
	if in.HookEventName != event {
		return fmt.Errorf("skerry hook %s received a %q event; check the hook configuration", sub, in.HookEventName)
	}
	if in.Cwd == "" || !filepath.IsAbs(in.Cwd) {
		return fmt.Errorf("hook input has no absolute cwd")
	}

	switch event {
	case "WorktreeCreate":
		return h.worktreeCreate(ctx, in)
	case "WorktreeRemove":
		return h.worktreeRemove(ctx, in)
	default:
		return h.sessionStart(in)
	}
}

func readInput(r io.Reader) (Input, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil {
		return Input{}, fmt.Errorf("read hook input: %w", err)
	}
	if len(data) > maxInput {
		return Input{}, errors.New("hook input is larger than 1 MiB")
	}
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		return Input{}, fmt.Errorf("parse hook input: %w", err)
	}
	return in, nil
}

func (h *Handler) worktreeCreate(ctx context.Context, in Input) error {
	ws, err := h.Manager.Create(ctx, workspace.CreateOptions{From: in.Cwd, Name: in.Name, CreatedBy: "claude"})
	if err != nil {
		return err
	}
	// Claude reads the last non-empty stdout line as the worktree path.
	_, err = fmt.Fprintln(h.Stdout, ws.Path)
	return err
}

func (h *Handler) worktreeRemove(ctx context.Context, in Input) error {
	if in.WorktreePath == "" || !filepath.IsAbs(in.WorktreePath) {
		return errors.New("hook input has no absolute worktree_path")
	}
	path, err := workspace.Canonical(in.WorktreePath)
	if errors.Is(err, os.ErrNotExist) {
		// Deleted by hand: resolve its parent instead, so skerry can still
		// drop its record (and git's) for the missing directory.
		clean := filepath.Clean(in.WorktreePath)
		parent, perr := workspace.Canonical(filepath.Dir(clean))
		if perr != nil {
			return nil // nothing left to clean up against
		}
		path = filepath.Join(parent, filepath.Base(clean))
	} else if err != nil {
		return err
	}
	ws, err := h.Manager.Registry.FindByPath(path)
	if err != nil || ws.Path != path {
		// Only remove what skerry created and recorded.
		return fmt.Errorf("%s is not a skerry workspace; leaving it in place", path)
	}
	res, err := h.Manager.Remove(ctx, ws, false)
	if err != nil {
		return err
	}
	if res.BranchKept {
		fmt.Fprintf(h.Stderr, "skerry: kept branch %s because it has commits\n", res.Branch)
	}
	return nil
}

func (h *Handler) sessionStart(in Input) error {
	ws, err := h.Manager.Find(in.Cwd)
	if errors.Is(err, registry.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return nil // not a skerry workspace: nothing to add
	}
	if err != nil {
		return err
	}

	if envFile := h.getenv("CLAUDE_ENV_FILE"); envFile != "" {
		if err := appendEnv(envFile, map[string]string{"SKERRY_WORKSPACE": ws.Name}); err != nil {
			return err
		}
	}

	trusted, err := h.Manager.Trust.IsTrusted(ws.Repo)
	if err != nil {
		return err
	}
	return json.NewEncoder(h.Stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": sessionContext(ws, trusted),
		},
	})
}

func sessionContext(ws registry.Workspace, trusted bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This session runs in skerry workspace %q at %s, on branch %s.", ws.Name, ws.Path, ws.Branch)
	fmt.Fprintf(&b, " The branch name is a placeholder: rename it to describe the change before pushing.")
	if !trusted {
		fmt.Fprintf(&b, " The repository is not trusted by skerry, so no isolated environment is provisioned;"+
			" the user can enable it with `skerry trust %s`.", ws.Repo)
	}
	return b.String()
}

// appendEnv adds export lines to Claude's env file, which it sources before
// every Bash command. Other hooks write to the same file, so only append.
func appendEnv(path string, vars map[string]string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open CLAUDE_ENV_FILE: %w", err)
	}
	defer f.Close()
	for name, value := range vars {
		if _, err := fmt.Fprintf(f, "export %s=%s\n", name, ShellQuote(value)); err != nil {
			return err
		}
	}
	return nil
}

// ShellQuote quotes s for a POSIX shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (h *Handler) getenv(name string) string {
	if h.Getenv != nil {
		return h.Getenv(name)
	}
	return os.Getenv(name)
}
