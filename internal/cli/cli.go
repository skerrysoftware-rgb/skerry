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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/claude"
	"github.com/skerrysoftware-rgb/skerry/internal/hook"
	"github.com/skerrysoftware-rgb/skerry/internal/paths"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/scope"
	"github.com/skerrysoftware-rgb/skerry/internal/shim"
	"github.com/skerrysoftware-rgb/skerry/internal/termsafe"
	"github.com/skerrysoftware-rgb/skerry/internal/vmm"
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
  skerry create <name> [--from DIR] [--base BRANCH]
                                      create a workspace and print its path
  skerry ls                           list workspaces
  skerry rm <name> [--force]          remove a workspace
  skerry up [name]                    start a workspace's VM
  skerry down [name]                  stop it
  skerry status [name]                show its services and forwarded ports
  skerry url [-w name] SERVICE        print a URL for a service, for tools on this Mac
  skerry logs [-w name] [-f] [-n N] SERVICE|console|network
  skerry reset [name] [--yes]         delete the workspace's service data
  skerry gc                           delete images and downloads no workspace needs
  skerry exec [-w name] -- CMD...     run a command inside the workspace VM
  skerry enable [DIR] [--yes]         use skerry for checkouts under DIR
  skerry disable [DIR]                stop using skerry under DIR
  skerry enable --list                list enabled folders
  skerry claude install [--enable DIR]... [--yes] [--force] [--settings FILE]
  skerry claude uninstall [--yes] [--settings FILE]
  skerry version

Claude Code runs "skerry hook <event>" itself; you don't need to.
`

// Main runs skerry and returns the process exit code.
func Main(ctx context.Context, args []string, env Env) int {
	err := run(ctx, args, env)
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	case errors.Is(err, errUsage):
		fmt.Fprintf(env.Stderr, "skerry: %v\n\n%s", err, usage)
		return 2
	default:
		// Errors can quote repository files or VM output.
		fmt.Fprintf(env.Stderr, "skerry: %s\n", termsafe.Clean(err.Error()))
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
	case "enable":
		return cmdEnable(ctx, m, rest, env)
	case "disable":
		return cmdDisable(ctx, m, rest, env)
	case "hook":
		return cmdHook(ctx, m, rest, env)
	case "up":
		return cmdUp(ctx, m, rest, env)
	case "down":
		return cmdDown(ctx, m, rest, env)
	case "exec":
		return cmdExec(ctx, m, rest, env)
	case "status":
		return cmdStatus(ctx, m, rest, env)
	case "url":
		return cmdURL(ctx, m, rest, env)
	case "logs":
		return cmdLogs(ctx, m, rest, env)
	case "reset":
		return cmdReset(ctx, m, rest, env)
	case "gc":
		return cmdGC(m, rest, env)
	case "vm":
		return cmdVM(ctx, m, rest, env)
	case "claude":
		return cmdClaude(ctx, m, rest, env)
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
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &workspace.Manager{
		VMs:          &vmm.Manager{Dirs: dirs, Self: self},
		Registry:     registry.Open(dirs.State),
		Scope:        scope.Open(dirs.State),
		BranchPrefix: env.Getenv("SKERRY_BRANCH_PREFIX"),
		BaseFromHead: env.Getenv("SKERRY_BASE") == "head",
		GitHooks:     env.Getenv("SKERRY_GIT_HOOKS") == "1",
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
		rest := fs.Args()
		// After "--", everything is positional.
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		args = rest
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
	base := fs.String("base", "", "branch or commit to start the workspace from (default: the default branch)")
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
	ws, err := m.Create(ctx, workspace.CreateOptions{From: start, Name: pos[0], CreatedBy: "cli", Base: *base})
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, ws.Path)
	if ws.Mode == registry.ModePlain {
		fmt.Fprintf(env.Stderr, "note: skerry is not enabled for %s, so this is a plain worktree. To enable it: skerry enable %s\n", ws.Repo, ws.Repo)
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
	fmt.Fprintln(tw, "NAME\tMODE\tVM\tBRANCH\tREPOSITORY\tAGE")
	for _, ws := range all {
		vm := "-"
		if ws.Mode == registry.ModeSkerry {
			vm = "stopped"
			if v, ok := m.VMs.(*vmm.Manager); ok && v.Running(ws) {
				vm = "running"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", ws.Name, ws.Mode, vm, ws.Branch, ws.Repo, age(time.Since(ws.CreatedAt)))
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
	// Removal is destructive: inside a repository, only its own workspaces.
	ws, err := findByName(ctx, m, pos[0], env, false)
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
// directory, or across all repositories when outside one. otherRepos also
// accepts a name that is unique across all repositories when the current
// one has no such workspace.
func findByName(ctx context.Context, m *workspace.Manager, name string, env Env, otherRepos bool) (registry.Workspace, error) {
	repo := ""
	if wd, err := env.Getwd(); err == nil {
		if root, err := m.Git.MainRoot(ctx, wd); err == nil {
			repo, _ = workspace.Canonical(root)
		}
	}
	ws, err := m.Registry.FindByName(repo, name)
	if errors.Is(err, registry.ErrNotFound) && repo != "" && otherRepos {
		// Not in this repository; a name unique across all of them still works.
		ws, err = m.Registry.FindByName("", name)
	}
	if errors.Is(err, registry.ErrNotFound) {
		return ws, fmt.Errorf("no workspace named %q", name)
	}
	return ws, err
}

// targetDir is the folder an enable/disable command acts on: the given
// directory, or else the repository containing the current directory, or
// else the current directory.
func targetDir(ctx context.Context, m *workspace.Manager, pos []string, env Env) (string, error) {
	switch len(pos) {
	case 0:
		wd, err := env.Getwd()
		if err != nil {
			return "", err
		}
		if root, err := m.Git.MainRoot(ctx, wd); err == nil {
			return workspace.Canonical(root)
		}
		return workspace.Canonical(wd)
	case 1:
		dir, err := workspace.Canonical(pos[0])
		if err != nil {
			return "", err
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", dir)
		}
		return dir, nil
	default:
		return "", fmt.Errorf("%w: expected at most one directory", errUsage)
	}
}

const enableWarning = `Enabling %s means every checkout Claude Code (or skerry create) makes under it
becomes a skerry workspace, and skerry will run those projects' setup, migration
and service commands inside each workspace's VM. Only enable folders whose
projects you trust.
`

func cmdEnable(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("enable", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	list := fs.Bool("list", false, "list enabled folders")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *list {
		entries, err := m.Scope.List()
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Fprintln(env.Stdout, "skerry is not enabled anywhere")
		}
		for _, e := range entries {
			fmt.Fprintf(env.Stdout, "%s\t(since %s)\n", e.Dir, e.EnabledAt.Local().Format("2006-01-02"))
		}
		return nil
	}
	dir, err := targetDir(ctx, m, pos, env)
	if err != nil {
		return err
	}
	return enable(m, dir, *yes, env)
}

func enable(m *workspace.Manager, dir string, yes bool, env Env) error {
	if covering, err := m.Scope.Covering(dir); err != nil {
		return err
	} else if covering != "" {
		fmt.Fprintf(env.Stdout, "%s is already enabled (through %s)\n", dir, covering)
		return nil
	}
	fmt.Fprintf(env.Stderr, enableWarning, dir)
	if err := confirm(env, yes, "Enable skerry for "+dir+"?"); err != nil {
		return err
	}
	if err := m.Scope.Enable(dir); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "enabled %s\n", dir)
	return nil
}

func cmdDisable(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	pos, err := parse(flag.NewFlagSet("disable", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	dir, err := targetDir(ctx, m, pos, env)
	if err != nil {
		return err
	}
	removed, err := m.Scope.Disable(dir)
	if err != nil {
		return err
	}
	switch covering, err := m.Scope.Covering(dir); {
	case err != nil:
		return err
	case covering != "":
		fmt.Fprintf(env.Stdout, "%s is still enabled through %s; run: skerry disable %s\n", dir, covering, covering)
	case removed:
		fmt.Fprintf(env.Stdout, "disabled %s; existing workspaces keep running until removed\n", dir)
	default:
		fmt.Fprintf(env.Stdout, "%s was not enabled\n", dir)
	}
	return nil
}

func cmdHook(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: hook takes one event name", errUsage)
	}
	h := &hook.Handler{
		Manager: m, Stdout: env.Stdout, Stderr: env.Stderr, Getenv: env.Getenv,
		Shims:   shim.Ensure,
		StartVM: startInBackground,
	}
	return h.Run(ctx, args[0], env.Stdin)
}

// dirList collects a repeatable flag.
type dirList []string

func (d *dirList) String() string     { return strings.Join(*d, ",") }
func (d *dirList) Set(v string) error { *d = append(*d, v); return nil }

func cmdClaude(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	if len(args) == 0 || (args[0] != "install" && args[0] != "uninstall") {
		return fmt.Errorf("%w: claude takes install or uninstall", errUsage)
	}
	install := args[0] == "install"
	fs := flag.NewFlagSet("claude", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	force := fs.Bool("force", false, "replace other tools' worktree hooks")
	settings := fs.String("settings", "", "settings file (default ~/.claude/settings.json)")
	var enableDirs dirList
	fs.Var(&enableDirs, "enable", "enable skerry for this folder (repeatable)")
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
		if install {
			return enableAfterInstall(m, enableDirs, *yes, env)
		}
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
	if install {
		return enableAfterInstall(m, enableDirs, *yes, env)
	}
	return nil
}

// enableAfterInstall enables the folders named with --enable, or asks which
// folders to enable. skerry is opt-in: with no answer, nothing changes for
// Claude Code worktrees until the user runs skerry enable.
func enableAfterInstall(m *workspace.Manager, dirs []string, yes bool, env Env) error {
	if len(dirs) == 0 && env.Interactive && !yes {
		fmt.Fprint(env.Stderr, "\nskerry only takes over checkouts in folders you enable; elsewhere worktrees\n"+
			"stay plain git worktrees, as before. Which folders should use skerry?\n"+
			"Enter paths separated by commas, or leave empty to decide later: ")
		answer, _ := bufio.NewReader(env.Stdin).ReadString('\n')
		for _, d := range strings.Split(answer, ",") {
			if d = strings.TrimSpace(d); d != "" {
				dirs = append(dirs, expandHome(d))
			}
		}
		yes = true // typing the folder is the confirmation
	}
	if len(dirs) == 0 {
		enabled, err := m.Scope.List()
		if err != nil {
			return err
		}
		if len(enabled) == 0 {
			fmt.Fprintln(env.Stdout, "skerry is not enabled for any folder yet; worktrees stay plain until you run: skerry enable DIR")
		}
		for _, e := range enabled {
			fmt.Fprintf(env.Stdout, "skerry is enabled for %s\n", e.Dir)
		}
		return nil
	}
	for _, d := range dirs {
		dir, err := workspace.Canonical(d)
		if err != nil {
			return err
		}
		if err := enable(m, dir, yes, env); err != nil {
			return err
		}
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
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

// startInBackground boots a workspace's VM without waiting for it, so a hook
// returns at once and the VM is warm by the time Claude runs a command.
func startInBackground(ws registry.Workspace) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, "up", "--quiet", ws.Name)
	cmd.Dir = ws.Path
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}
