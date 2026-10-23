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
	"sort"
	"strings"

	"github.com/skerrysoftware-rgb/skerry/internal/guard"
	"github.com/skerrysoftware-rgb/skerry/internal/project"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/services"
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
	// Shims returns the shim directory for skerry workspaces; nil skips it.
	Shims func() (string, error)
	// StartVM boots a workspace's VM in the background; nil skips it.
	StartVM func(registry.Workspace)
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

	var context string
	if ws.Mode == registry.ModeSkerry {
		if envFile := h.getenv("CLAUDE_ENV_FILE"); envFile != "" {
			lines := []string{"export SKERRY_WORKSPACE=" + ShellQuote(ws.Name)}
			if h.Shims != nil {
				dir, err := h.Shims()
				if err != nil {
					return err
				}
				// Shims first, so node, npm, yarn... run inside the VM.
				lines = append(lines, "export PATH="+ShellQuote(dir)+`:"$PATH"`)
			}
			if err := appendLines(envFile, lines); err != nil {
				return err
			}
		}
		if h.StartVM != nil {
			h.StartVM(ws) // boot in the background so the first command is quick
		}
		warnings := plantedWarnings(ws)
		context = fmt.Sprintf("This session runs in skerry workspace %q at %s, on branch %s."+
			" The workspace has its own lightweight Linux VM (Wolfi), isolated from other workspaces:"+
			" its own processes, ports, /tmp and services. The checkout is mounted at the same path inside the VM."+
			" `node`, `npm`, `npx`, `yarn`, `pnpm` and `corepack` run inside the VM automatically;"+
			" every other command, git included, runs on the host as usual."+
			" To run anything else inside the VM, use `skerry exec -- <command>`"+
			" (for example `skerry exec -- sh -c 'ls /tmp'`)."+
			" The VM can reach the public internet (package registries, APIs) but not this machine, its local network or cloud metadata.",
			ws.Name, ws.Path, ws.Branch) + servicesContext(ws) + warnings
	} else {
		// A plain worktree: say once per repository that skerry is
		// available, then stay out of the way.
		first, err := h.Manager.Scope.FirstHint(ws.Repo)
		if err != nil || !first {
			return err
		}
		context = fmt.Sprintf("skerry is installed but not enabled for %s, so this is a plain git worktree."+
			" If the user wants each checkout of this project to get its own isolated environment"+
			" (its own databases, services and dependencies in a lightweight VM), they can run `skerry enable %s`."+
			" Mention this only if it is relevant to the task.", ws.Repo, ws.Repo)
	}
	return json.NewEncoder(h.Stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": context,
		},
	})
}

// servicesContext tells Claude which services the workspace runs and how
// commands find them.
func servicesContext(ws registry.Workspace) string {
	proj, err := project.Load(ws.Path)
	if err != nil {
		// The error can quote the file, which the repository (and the VM)
		// controls; keep that text out of Claude's context.
		return fmt.Sprintf(" This project's %s is invalid, so the workspace runs no services; `skerry status` shows why.", project.FileName)
	}
	if len(proj.Services) == 0 {
		return setupContext(proj)
	}
	var vars []string
	for k := range services.Env(proj.Services, "x") {
		vars = append(vars, k)
	}
	sort.Strings(vars)
	return fmt.Sprintf(" The VM runs this workspace's own %s (%s), with data that persists across restarts and is separate from every other workspace."+
		" Commands in the VM get %s, so apps and migrations connect without configuration; use the variables rather than hard-coding URLs."+
		" They are set in the environment, so they win over the same names in a .env file (dotenv loaders don't override existing variables)."+
		" Tools such as psql, valkey-cli and temporal are available through `skerry exec -- <tool>`."+
		" `skerry status` lists the services and the ports they are forwarded to on this machine.",
		strings.Join(proj.Services, ", "), proj.Source, strings.Join(vars, ", ")) + setupContext(proj)
}

// setupContext explains the project's setup commands, if it has any.
func setupContext(proj project.Project) string {
	if len(proj.Setup) == 0 && proj.SeedPostgres == "" {
		return ""
	}
	return " When the workspace's data is new, skerry seeds it and runs the project's setup commands from skerry.yaml" +
		" (such as installing dependencies and migrations) before the first command; `skerry logs setup` shows their output," +
		" and `skerry setup` runs them again, for example after new migrations."
}

// appendLines adds lines to Claude's env file, which it sources before every
// Bash command. Other hooks write to the same file, so only append.
func appendLines(path string, lines []string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open CLAUDE_ENV_FILE: %w", err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := fmt.Fprintln(f, line); err != nil {
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

// plantedWarnings checks the checkout for files a VM may have planted for
// host tools, quarantines them, and returns text for Claude's context.
//
// The paths and the .git contents are the VM's to choose, so none of them
// goes into Claude's context: only what kind of thing was found, in skerry's
// own words.
func plantedWarnings(ws registry.Workspace) string {
	var b strings.Builder
	if err := guard.CheckPointer(ws); err != nil {
		b.WriteString(" WARNING: this worktree's .git pointer was changed from inside the VM. Do not run git in this worktree; tell the user.")
	}
	found, _ := guard.Scan(context.Background(), ws)
	if len(found) > 0 {
		kinds := map[string]bool{}
		for _, f := range found {
			kinds[f.Kind] = true
		}
		var list []string
		for k := range kinds {
			list = append(list, k)
		}
		sort.Strings(list)
		fmt.Fprintf(&b, " WARNING: skerry found %d item(s) in this worktree that a VM may have planted for host tools (%s) and renamed each with a .skerry-quarantine- suffix."+
			" Tell the user; do not restore them without their review.", len(found), strings.Join(list, "; "))
	}
	return b.String()
}
