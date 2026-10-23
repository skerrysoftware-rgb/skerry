package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/skerrysoftware-rgb/skerry/internal/agent"
	"github.com/skerrysoftware-rgb/skerry/internal/guard"
	"github.com/skerrysoftware-rgb/skerry/internal/paths"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/shim"
	"github.com/skerrysoftware-rgb/skerry/internal/vmm"
	"github.com/skerrysoftware-rgb/skerry/internal/workspace"
)

// exitCode is an error that carries a command's exit status through Main
// without printing anything.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func newVMM(env Env, progress bool) (*vmm.Manager, error) {
	dirs, err := paths.Default()
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	m := &vmm.Manager{Dirs: dirs, Self: self}
	if progress {
		m.Progress = env.Stderr
	}
	return m, nil
}

// resolveWorkspace finds a workspace by name, or else the one containing the
// current directory.
func resolveWorkspace(ctx context.Context, m *workspace.Manager, pos []string, env Env) (registry.Workspace, error) {
	switch len(pos) {
	case 0:
		wd, err := env.Getwd()
		if err != nil {
			return registry.Workspace{}, err
		}
		ws, err := m.Find(wd)
		if errors.Is(err, registry.ErrNotFound) {
			return ws, errors.New("not inside a skerry workspace; name one, or cd into it")
		}
		return ws, err
	case 1:
		return findByName(ctx, m, pos[0], env, true)
	default:
		return registry.Workspace{}, fmt.Errorf("%w: expected at most one workspace name", errUsage)
	}
}

func cmdUp(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "no progress output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, m, pos, env)
	if err != nil {
		return err
	}
	v, err := newVMM(env, !*quiet)
	if err != nil {
		return err
	}
	c, err := v.Up(ctx, ws)
	if err != nil {
		return err
	}
	c.Close()
	if !*quiet {
		fmt.Fprintf(env.Stdout, "%s is running\n", ws.Name)
	}
	return nil
}

func cmdDown(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	pos, err := parse(flag.NewFlagSet("down", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, m, pos, env)
	if err != nil {
		return err
	}
	v, err := newVMM(env, false)
	if err != nil {
		return err
	}
	if err := v.Down(ws); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s is stopped\n", ws.Name)
	return nil
}

// cmdExec runs a command in a workspace VM: skerry exec [-w NAME] -- CMD...
func cmdExec(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	name := fs.String("w", "", "workspace name (default: the one containing the current directory)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	cmd := fs.Args()
	if len(cmd) == 0 {
		return fmt.Errorf("%w: exec needs a command", errUsage)
	}
	var pos []string
	if *name != "" {
		pos = []string{*name}
	}
	ws, err := resolveWorkspace(ctx, m, pos, env)
	if err != nil {
		return err
	}
	return runInWorkspace(ctx, ws, cmd, env)
}

// forwardedEnv lists host variables that mean the same thing in the guest.
var forwardedEnv = []string{
	"TERM", "COLORTERM", "NO_COLOR", "FORCE_COLOR", "LANG", "LC_ALL", "LC_CTYPE",
	"CI", "NODE_ENV", "NODE_OPTIONS", "DEBUG", "TZ",
}

func runInWorkspace(ctx context.Context, ws registry.Workspace, args []string, env Env) error {
	v, err := newVMM(env, true)
	if err != nil {
		return err
	}
	c, err := v.Up(ctx, ws)
	if err != nil {
		return err
	}
	defer c.Close()

	// The checkout is mounted at the same path inside the VM.
	dir := ws.Path
	if wd, err := env.Getwd(); err == nil {
		if real, err := workspace.Canonical(wd); err == nil && (real == ws.Path || strings.HasPrefix(real, ws.Path+string(filepath.Separator))) {
			dir = real
		}
	}
	vars := map[string]string{"SKERRY_WORKSPACE": ws.Name}
	for _, k := range forwardedEnv {
		if v := env.Getenv(k); v != "" {
			vars[k] = v
		}
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)

	// Signals are forwarded to the command rather than cancelling the run.
	code, err := c.Run(context.WithoutCancel(ctx), agent.Cmd{
		Args: args, Dir: dir, Env: vars,
		Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr, Signals: sigs,
	})
	// Scan however the command ended, even if it was interrupted.
	reportPlanted(context.WithoutCancel(ctx), ws, env)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// reportPlanted checks the checkout for files the VM may have planted for
// host tools, quarantines them, and tells the user.
func reportPlanted(ctx context.Context, ws registry.Workspace, env Env) {
	if err := guard.CheckPointer(ws); err != nil {
		fmt.Fprintf(env.Stderr, "skerry: WARNING: %v. Don't run git in %s until you've checked it.\n", err, ws.Path)
	}
	found, err := guard.Scan(ctx, ws)
	for _, f := range found {
		fmt.Fprintf(env.Stderr, "skerry: WARNING: found %s\n", f)
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "skerry: checking %s for planted files: %v\n", ws.Path, err)
	}
}

// cmdVM holds internal VM commands; `skerry vm run ID` is the runner.
func cmdVM(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	if len(args) != 2 || args[0] != "run" {
		return fmt.Errorf("%w: vm run WORKSPACE-ID", errUsage)
	}
	all, err := m.Registry.List()
	if err != nil {
		return err
	}
	for _, ws := range all {
		if ws.ID == args[1] {
			v, err := newVMM(env, false)
			if err != nil {
				return err
			}
			return v.Run(ctx, ws)
		}
	}
	return fmt.Errorf("no workspace with id %s", args[1])
}

// IsShim reports whether skerry was invoked through a shim link.
func IsShim(argv0 string) bool { return shim.Is(argv0) }

// Shim runs a shimmed command: inside the workspace VM when the current
// directory is in a skerry workspace, otherwise the next real binary on PATH.
func Shim(ctx context.Context, argv0 string, args []string, env Env) int {
	name := filepath.Base(argv0)
	err := runShim(ctx, name, args, env)
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	default:
		fmt.Fprintf(env.Stderr, "skerry (%s): %v\n", name, err)
		return 1
	}
}

func runShim(ctx context.Context, name string, args []string, env Env) error {
	if env.Getenv("SKERRY_SHIMS") == "off" {
		return execReal(name, args)
	}
	wd, err := env.Getwd()
	if err != nil {
		return execReal(name, args) // no current directory: not in a workspace
	}
	m, err := newManager(env)
	if err != nil {
		// Fail closed: running repository code on the host by accident is
		// worse than an error.
		return fmt.Errorf("can't tell whether %s is a skerry workspace: %w (set SKERRY_SHIMS=off to run %s on this machine)", wd, err, name)
	}
	ws, err := m.Find(wd)
	switch {
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return execReal(name, args)
	case err != nil:
		return fmt.Errorf("can't tell whether %s is a skerry workspace: %w (set SKERRY_SHIMS=off to run %s on this machine)", wd, err, name)
	case ws.Mode != registry.ModeSkerry:
		return execReal(name, args)
	}
	return runInWorkspace(ctx, ws, append([]string{name}, args...), env)
}

// execReal replaces this process with the next binary called name on PATH,
// skipping the shim directory.
func execReal(name string, args []string) error {
	shimDir, _ := shim.Dir()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || dir == shimDir {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		if real, err := filepath.EvalSymlinks(candidate); err == nil {
			if self, err := os.Executable(); err == nil {
				if selfReal, err := filepath.EvalSymlinks(self); err == nil && real == selfReal {
					continue // another link to skerry
				}
			}
		}
		return syscall.Exec(candidate, append([]string{name}, args...), os.Environ())
	}
	return fmt.Errorf("%s: command not found", name)
}
