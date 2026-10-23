// Package cli implements skerry's command line.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/claude"
	"github.com/skerrysoftware-rgb/skerry/internal/hook"
	"github.com/skerrysoftware-rgb/skerry/internal/paths"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/trust"
	"github.com/skerrysoftware-rgb/skerry/internal/workspace"
)

// Version is set at build time.
var Version = "dev"

// Env is everything a command touches outside the process, so tests can
// substitute it.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Interactive reports whether a person can answer prompts on Stdin.
	Interactive bool
	Getenv      func(string) string
	Getwd       func() (string, error)
	// Executable is the path hooks should run; empty means os.Executable.
	Executable string
}

// errUsage marks errors caused by wrong arguments.
var errUsage = errors.New("usage")

const usage = `skerry: an isolated environment for every checkout

Usage:
  skerry create <name> [--from DIR]   create a workspace and print its path
  skerry ls                           list workspaces
  skerry rm <name> [--force]          remove a workspace
  skerry trust [DIR] [--yes]          let skerry run this repository's commands
  skerry untrust [DIR]                revoke trust
  skerry trust --list                 list trusted repositories
  skerry claude install [--yes] [--force] [--settings FILE]
  skerry claude uninstall [--yes] [--settings FILE]
  skerry version

Claude Code runs "skerry hook <event>" itself; you don't need to.
`

// Main runs skerry and returns the process exit code.
func Main(ctx context.Context, args []string, env Env) int {
	err := run(ctx, args, env)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(env.Stderr, "skerry: %v\n\n%s", err, usage)
		return 2
	default:
		fmt.Fprintf(env.Stderr, "skerry: %v\n", err)
		return 1
	}
}

func run(ctx context.Context, args []string, env Env) error {
	if len(args) == 0 {
		fmt.Fprint(env.Stdout, usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return nil
	case "version", "--version":
		fmt.Fprintln(env.Stdout, "skerry", Version)
		return nil
	}

	m, err := newManager(env)
	if err != nil {
		return err
	}
	switch cmd {
	case "create":
		return cmdCreate(ctx, m, rest, env)
	case "ls", "list":
		return cmdList(m, rest, env)
	case "rm", "remove":
		return cmdRemove(ctx, m, rest, env)
	case "trust":
		return cmdTrust(ctx, m, rest, env)
	case "untrust":
		return cmdUntrust(ctx, m, rest, env)
	case "hook":
		return cmdHook(ctx, m, rest, env)
	case "claude":
		return cmdClaude(rest, env)
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}

func newManager(env Env) (*workspace.Manager, error) {
	dirs, err := paths.Default()
	if err != nil {
		return nil, err
	}
	if err := dirs.Ensure(); err != nil {
		return nil, err
	}
	return &workspace.Manager{
		Registry:     registry.Open(dirs.State),
		Trust:        trust.Open(dirs.State),
		BranchPrefix: env.Getenv("SKERRY_BRANCH_PREFIX"),
	}, nil
}

// parse parses flags that may appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%w: %v", errUsage, err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func cmdCreate(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	from := fs.String("from", "", "directory in the repository to start from")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: create takes one workspace name", errUsage)
	}
	start := *from
	if start == "" {
		if start, err = env.Getwd(); err != nil {
			return err
		}
	}
	ws, err := m.Create(ctx, workspace.CreateOptions{From: start, Name: pos[0], CreatedBy: "cli"})
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, ws.Path)
	if trusted, err := m.Trust.IsTrusted(ws.Repo); err == nil && !trusted {
		fmt.Fprintf(env.Stderr, "note: %s is not trusted, so skerry will not run its commands. To allow it: skerry trust %s\n", ws.Repo, ws.Repo)
	}
	return nil
}

func cmdList(m *workspace.Manager, args []string, env Env) error {
	if len(args) > 0 {
		return fmt.Errorf("%w: ls takes no arguments", errUsage)
	}
	all, err := m.Registry.List()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(env.Stdout, "no workspaces")
		return nil
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tBRANCH\tREPOSITORY\tCREATED BY\tAGE")
	for _, ws := range all {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ws.Name, ws.Branch, ws.Repo, ws.CreatedBy, age(time.Since(ws.CreatedAt)))
	}
	return tw.Flush()
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func cmdRemove(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	force := fs.Bool("force", false, "discard uncommitted changes")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: rm takes one workspace name", errUsage)
	}
	ws, err := findByName(ctx, m, pos[0], env)
	if err != nil {
		return err
	}
	res, err := m.Remove(ctx, ws, *force)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "removed %s\n", ws.Name)
	if res.BranchKept {
		fmt.Fprintf(env.Stdout, "kept branch %s: it has commits\n", res.Branch)
	}
	return nil
}

// findByName looks a name up in the repository containing the current
// directory, or across all repositories when outside one.
func findByName(ctx context.Context, m *workspace.Manager, name string, env Env) (registry.Workspace, error) {
	repo := ""
	if wd, err := env.Getwd(); err == nil {
		if root, err := m.Git.MainRoot(ctx, wd); err == nil {
			repo, _ = workspace.Canonical(root)
		}
	}
	ws, err := m.Registry.FindByName(repo, name)
	if errors.Is(err, registry.ErrNotFound) {
		return ws, fmt.Errorf("no workspace named %q", name)
	}
	return ws, err
}

func repoRoot(ctx context.Context, m *workspace.Manager, pos []string, env Env) (string, error) {
	dir := ""
	switch len(pos) {
	case 0:
		wd, err := env.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	case 1:
		dir = pos[0]
	default:
		return "", fmt.Errorf("%w: expected at most one directory", errUsage)
	}
	dir, err := workspace.Canonical(dir)
	if err != nil {
		return "", err
	}
	root, err := m.Git.MainRoot(ctx, dir)
	if err != nil {
		return "", err
	}
	return workspace.Canonical(root)
}

func cmdTrust(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("trust", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	list := fs.Bool("list", false, "list trusted repositories")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *list {
		entries, err := m.Trust.List()
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Fprintln(env.Stdout, "no trusted repositories")
		}
		for _, e := range entries {
			fmt.Fprintf(env.Stdout, "%s\t(since %s)\n", e.Root, e.TrustedAt.Local().Format("2006-01-02"))
		}
		return nil
	}
	root, err := repoRoot(ctx, m, pos, env)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "Trusting %s lets skerry run this repository's code: its git hooks when creating\n"+
		"workspaces now, and its setup, migration and service commands inside workspace VMs later.\n"+
		"Only trust repositories whose contents you trust.\n", root)
	if err := confirm(env, *yes, "Trust "+root+"?"); err != nil {
		return err
	}
	if err := m.Trust.Trust(root); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "trusted %s\n", root)
	return nil
}

func cmdUntrust(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	pos, err := parse(flag.NewFlagSet("untrust", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	root, err := repoRoot(ctx, m, pos, env)
	if err != nil {
		return err
	}
	removed, err := m.Trust.Untrust(root)
	if err != nil {
		return err
	}
	if removed {
		fmt.Fprintf(env.Stdout, "no longer trusting %s\n", root)
	} else {
		fmt.Fprintf(env.Stdout, "%s was not trusted\n", root)
	}
	return nil
}

func cmdHook(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: hook takes one event name", errUsage)
	}
	h := &hook.Handler{Manager: m, Stdout: env.Stdout, Stderr: env.Stderr, Getenv: env.Getenv}
	return h.Run(ctx, args[0], env.Stdin)
}

func cmdClaude(args []string, env Env) error {
	if len(args) == 0 || (args[0] != "install" && args[0] != "uninstall") {
		return fmt.Errorf("%w: claude takes install or uninstall", errUsage)
	}
	install := args[0] == "install"
	fs := flag.NewFlagSet("claude", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	force := fs.Bool("force", false, "replace other tools' worktree hooks")
	settings := fs.String("settings", "", "settings file (default ~/.claude/settings.json)")
	if pos, err := parse(fs, args[1:]); err != nil {
		return err
	} else if len(pos) > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, pos[0])
	}

	path := *settings
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path = filepath.Join(home, ".claude", "settings.json")
	}
	exe, err := hookExecutable(env)
	if err != nil {
		return err
	}

	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	next, err := claude.Plan(current, hook.ShellQuote(exe), install, *force)
	if err != nil {
		return err
	}
	if string(next) == string(current) {
		fmt.Fprintf(env.Stdout, "%s is already up to date\n", path)
		return nil
	}

	if install {
		fmt.Fprintf(env.Stderr, "This adds these hooks to %s:\n", path)
		for _, h := range claude.Hooks {
			fmt.Fprintf(env.Stderr, "  %-15s %s hook %s\n", h.Event, hook.ShellQuote(exe), h.Sub)
		}
		fmt.Fprintf(env.Stderr, "Claude Code will then create every worktree through skerry. Other settings are kept.\n")
	} else {
		fmt.Fprintf(env.Stderr, "This removes skerry's hooks from %s. Other settings are kept.\n", path)
	}
	if err := confirm(env, *yes, "Write "+path+"?"); err != nil {
		return err
	}

	backup, err := claude.Write(path, next, time.Now().Format("20060102-150405"))
	if err != nil {
		return err
	}
	if backup != "" {
		fmt.Fprintf(env.Stdout, "updated %s (backup: %s)\n", path, backup)
	} else {
		fmt.Fprintf(env.Stdout, "created %s\n", path)
	}
	return nil
}

// hookExecutable is the skerry path written into Claude's settings. It keeps
// symlinks (a Homebrew path survives upgrades) but refuses throwaway builds.
func hookExecutable(env Env) (string, error) {
	exe := env.Executable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return "", err
		}
	}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return "", errors.New("this skerry is a temporary `go run` build; install it first (go build -o bin/skerry ./cmd/skerry)")
	}
	return exe, nil
}

// confirm asks a yes/no question, or requires --yes when nobody can answer.
func confirm(env Env, yes bool, question string) error {
	if yes {
		return nil
	}
	if !env.Interactive {
		return errors.New("not running interactively; rerun with --yes to confirm")
	}
	fmt.Fprintf(env.Stderr, "%s [y/N] ", question)
	answer, _ := bufio.NewReader(env.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a == "y" || a == "yes" {
		return nil
	}
	return errors.New("cancelled")
}
