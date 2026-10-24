package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/agent"
	"github.com/skerrysoftware-rgb/skerry/internal/guard"
	"github.com/skerrysoftware-rgb/skerry/internal/paths"
	"github.com/skerrysoftware-rgb/skerry/internal/project"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/services"
	"github.com/skerrysoftware-rgb/skerry/internal/shim"
	"github.com/skerrysoftware-rgb/skerry/internal/termsafe"
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

// cmdStart starts the project's apps in a workspace's VM (all of them, or
// the ones named), starting the VM first if needed: skerry start [-w NAME]
// [APP...]. Apps run only when asked: a workspace starting (for a Claude
// Code session, say) prepares its services and dependencies, not them.
func cmdStart(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	name := fs.String("w", "", "workspace name (default: the one containing the current directory)")
	apps, err := parse(fs, args)
	if err != nil {
		return err
	}
	var wsArgs []string
	if *name != "" {
		wsArgs = []string{*name}
	}
	ws, err := resolveWorkspace(ctx, m, wsArgs, env)
	if err != nil {
		return err
	}
	v, err := newVMM(env, true)
	if err != nil {
		return err
	}
	c, err := v.Up(ctx, ws)
	if err != nil {
		return err
	}
	c.Close()
	started, err := v.StartApps(ctx, ws, apps)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "starting %s in %s\n", strings.Join(started, ", "), ws.Name)
	if st, _, err := v.Status(ws); err == nil {
		for _, f := range st.Forwards {
			if slices.Contains(started, f.Name) {
				where := fmt.Sprintf("127.0.0.1:%d", f.Host)
				if f.Web {
					where = services.HostURL(f.Name, st.Hostname, f.Host, "", true)
				}
				fmt.Fprintf(env.Stdout, "  %s (port %d in the VM): %s\n", f.Name, f.Guest, where)
			}
		}
	}
	fmt.Fprintln(env.Stdout, "skerry status shows whether each one answers; skerry logs APP shows its output; skerry stop stops them")
	return nil
}

// cmdStop stops apps (all, or the ones named): skerry stop [-w NAME] [APP...].
func cmdStop(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	name := fs.String("w", "", "workspace name (default: the one containing the current directory)")
	apps, err := parse(fs, args)
	if err != nil {
		return err
	}
	var wsArgs []string
	if *name != "" {
		wsArgs = []string{*name}
	}
	ws, err := resolveWorkspace(ctx, m, wsArgs, env)
	if err != nil {
		return err
	}
	v, err := newVMM(env, false)
	if err != nil {
		return err
	}
	if !v.Running(ws) {
		fmt.Fprintf(env.Stdout, "%s isn't running, so neither are its apps\n", ws.Name)
		return nil
	}
	if err := v.StopApps(ctx, ws, apps); err != nil {
		return err
	}
	what := "the apps"
	if len(apps) > 0 {
		what = strings.Join(apps, ", ")
	}
	fmt.Fprintf(env.Stdout, "stopping %s in %s\n", what, ws.Name)
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
	// Paths and .git contents are the VM's to choose: strip anything that
	// would act on the terminal.
	if err := guard.CheckPointer(ws); err != nil {
		fmt.Fprintf(env.Stderr, "skerry: WARNING: %s. Don't run git in %s until you've checked it.\n", termsafe.Clean(err.Error()), ws.Path)
	}
	found, err := guard.Scan(ctx, ws)
	for _, f := range found {
		fmt.Fprintf(env.Stderr, "skerry: WARNING: found %s\n", termsafe.Clean(f.String()))
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "skerry: checking %s for planted files: %s\n", ws.Path, termsafe.Clean(err.Error()))
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
	// A runtime the workspace has never had isn't in its VM: run the
	// machine's own, as without skerry. "Never had" counts skerry's own
	// record too, so code in the VM can't move a runtime's commands onto
	// the host by editing skerry.yaml.
	if runtime, ok := services.RuntimeFor(name); ok && runtime != "node" {
		v, err := newVMM(env, false)
		if err != nil {
			return err
		}
		recorded := slices.Contains(v.RecordedRuntimes(ws), runtime)
		if !recorded {
			proj, err := project.Load(ws.Path)
			if err != nil {
				return fmt.Errorf("%s: %w (set SKERRY_SHIMS=off to run %s on this machine)", ws.Path, err, name)
			}
			if !services.HasRuntime(proj.Runtimes, runtime) {
				return execReal(name, args)
			}
		}
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

// cmdStatus shows a workspace's VM, services and forwarded ports.
func cmdStatus(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	pos, err := parse(flag.NewFlagSet("status", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, m, pos, env)
	if err != nil {
		return err
	}
	if ws.Mode != registry.ModeSkerry {
		fmt.Fprintf(env.Stdout, "%s is a plain worktree (no VM)\n", ws.Name)
		return nil
	}
	v, err := newVMM(env, false)
	if err != nil {
		return err
	}
	switch pr := v.ReadProgress(ws); {
	case pr.Busy():
		fmt.Fprintf(env.Stdout, "%s is being prepared: %s\n", ws.Name, pr)
	case pr.Phase == vmm.PhaseStartFailed:
		fmt.Fprintf(env.Stdout, "%s failed to start: %s\n", ws.Name, termsafe.Clean(pr.Detail))
	}
	if !v.Running(ws) {
		proj, err := project.Load(ws.Path)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "%s is stopped\n", ws.Name)
		if len(proj.Services) > 0 {
			fmt.Fprintf(env.Stdout, "services when started: %s (%s)\n", strings.Join(proj.Services, ", "), proj.Source)
		}
		if len(proj.Apps) > 0 {
			var names []string
			for _, a := range proj.Apps {
				names = append(names, a.Name)
			}
			fmt.Fprintf(env.Stdout, "apps: %s (%s); skerry start runs them\n", strings.Join(names, ", "), proj.AppsSource)
		}
		return nil
	}
	st, _, err := v.Status(ws)
	if err != nil {
		return fmt.Errorf("%s is running, but its status is unavailable: %w", ws.Name, err)
	}
	fmt.Fprintf(env.Stdout, "%s is running\n", ws.Name)
	if len(st.Services) > 0 {
		fmt.Fprintf(env.Stdout, "services: %s (%s)\n", strings.Join(st.Services, ", "), st.Source)
	}
	if setup := v.SetupStatus(ws); setup != "" {
		fmt.Fprintf(env.Stdout, "setup: %s\n", termsafe.Clean(setup))
	}
	proj, _ := project.Load(ws.Path)
	states, err := v.AppStates(ctx, ws)
	if err == nil && !slices.ContainsFunc(states, func(s agent.AppState) bool { return s.State != "stopped" }) && len(proj.Apps) > 0 {
		var names []string
		for _, a := range proj.Apps {
			if len(a.For) == 0 {
				names = append(names, a.Name)
			}
		}
		fmt.Fprintf(env.Stdout, "apps: none running; skerry start runs %s\n", strings.Join(names, ", "))
	}
	if err == nil && slices.ContainsFunc(states, func(s agent.AppState) bool { return s.State != "stopped" }) {
		fmt.Fprintln(env.Stdout, "apps:")
		for _, a := range states {
			line := fmt.Sprintf("  %s: %s", a.Name, a.State)
			if len(a.Listening) > 0 {
				line += ", answering on " + joinPorts(a.Listening)
			}
			if len(a.Closed) > 0 {
				line += ", not answering on " + joinPorts(a.Closed)
			}
			if a.Restarts > 0 {
				line += fmt.Sprintf(" (restarted %d times; last: %s)", a.Restarts, a.LastExit)
			}
			fmt.Fprintln(env.Stdout, termsafe.Clean(line))
			for _, o := range a.Output {
				fmt.Fprintln(env.Stdout, "    | "+termsafe.Clean(o))
			}
			// What its env points at that nothing in the VM serves.
			if i := slices.IndexFunc(proj.Apps, func(p project.App) bool { return p.Name == a.Name }); i >= 0 {
				for _, w := range proj.Wiring(proj.Apps[i]) {
					if w.To == "" {
						fmt.Fprintf(env.Stdout, "    %s points to localhost:%d, where nothing in the VM listens\n", termsafe.Clean(w.Key), w.Port)
					}
				}
			}
		}
	}
	if snaps, err := v.Snapshots(ws.Repo); err == nil && len(snaps) > 0 {
		var names []string
		for _, s := range snaps {
			names = append(names, s.Name)
		}
		fmt.Fprintf(env.Stdout, "snapshots: %s\n", strings.Join(names, ", "))
	}
	if len(st.Forwards) > 0 {
		tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "\nPORT\tIN VM\tON THIS MAC")
		for _, f := range st.Forwards {
			where := fmt.Sprintf("127.0.0.1:%d", f.Host)
			if f.Web {
				where = services.HostURL(f.Name, st.Hostname, f.Host, "", true)
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\n", f.Name, f.Guest, where)
		}
		tw.Flush()
		var vars []string
		for k := range services.Env(st.Services, "") {
			if strings.HasSuffix(k, "_URL") && k != "VALKEY_URL" || strings.HasSuffix(k, "_ADDRESS") {
				vars = append(vars, k)
			}
		}
		sort.Strings(vars)
		fmt.Fprintf(env.Stdout, "\nCommands in the VM reach services through %s.\n", strings.Join(vars, ", "))
		fmt.Fprintln(env.Stdout, "For a URL that works from this Mac, password included, run: skerry url <service>")
	}
	if slices.Contains(st.Services, "temporal") && !slices.ContainsFunc(st.Forwards, func(f vmm.Forward) bool { return f.Name == "temporal-ui" }) {
		fmt.Fprintln(env.Stdout, "\nThe Temporal UI has no password, so it isn't forwarded; to see it, add 8233 to ports in skerry.yaml.")
	}
	return nil
}

// cmdURL prints the URL a host tool uses to reach a workspace service,
// password included: psql "$(skerry url postgres)".
func cmdURL(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("url", flag.ContinueOnError)
	name := fs.String("w", "", "workspace name (default: the one containing the current directory)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: url needs a service (postgres, valkey, temporal-ui) or a forwarded port", errUsage)
	}
	var wsArgs []string
	if *name != "" {
		wsArgs = []string{*name}
	}
	ws, err := resolveWorkspace(ctx, m, wsArgs, env)
	if err != nil {
		return err
	}
	v, err := newVMM(env, false)
	if err != nil {
		return err
	}
	if !v.Running(ws) {
		return fmt.Errorf("%s is stopped; start it with: skerry up %s", ws.Name, ws.Name)
	}
	st, secret, err := v.Status(ws)
	if err != nil {
		return err
	}
	want := pos[0]
	if p, ok := services.Lookup(want); ok {
		want = p.Ports[0].Name
	}
	for _, f := range st.Forwards {
		if f.Name == want || strconv.Itoa(int(f.Guest)) == want {
			fmt.Fprintln(env.Stdout, services.HostURL(f.Name, st.Hostname, f.Host, secret, f.Web))
			return nil
		}
	}
	return fmt.Errorf("%s does not run %s", ws.Name, pos[0])
}

// cmdLogs shows a service's log from the VM, or the VM's console or
// refused network connections from the host.
func cmdLogs(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	name := fs.String("w", "", "workspace name (default: the one containing the current directory)")
	follow := fs.Bool("f", false, "keep printing new lines")
	lines := fs.Int("n", 100, "number of lines")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *lines < 1 {
		return fmt.Errorf("%w: logs needs one of: %s, setup, console, network", errUsage, strings.Join(services.Names(), ", "))
	}
	var wsArgs []string
	if *name != "" {
		wsArgs = []string{*name}
	}
	ws, err := resolveWorkspace(ctx, m, wsArgs, env)
	if err != nil {
		return err
	}
	v, err := newVMM(env, false)
	if err != nil {
		return err
	}
	tail := []string{"tail", "-n", strconv.Itoa(*lines)}
	if *follow {
		tail = append(tail, "-F") // logs are rotated by renaming
	}
	switch pos[0] {
	case "console", "network", "setup":
		// Host-side logs; tail runs on this Mac.
		cmd := exec.CommandContext(ctx, "/usr/bin/tail", append(tail[1:], filepath.Join(v.StateDir(ws), pos[0]+".log"))...)
		cmd.Stdout, cmd.Stderr = &termsafe.Writer{W: env.Stdout}, &termsafe.Writer{W: env.Stderr}
		return cmd.Run()
	}
	logFile := ""
	if p, ok := services.Lookup(pos[0]); ok {
		logFile = "/var/log/skerry/" + p.Name + ".log"
	} else if proj, err := project.Load(ws.Path); err == nil && slices.ContainsFunc(proj.Apps, func(a project.App) bool { return a.Name == pos[0] }) {
		logFile = "/var/log/skerry/apps/" + pos[0] + ".log"
	} else {
		return fmt.Errorf("%w: unknown log %q (a service, an app, setup, console or network)", errUsage, pos[0])
	}
	if !v.Running(ws) {
		return fmt.Errorf("%s is stopped; service logs live in the VM", ws.Name)
	}
	c, err := v.Connect(ws)
	if err != nil {
		return err
	}
	defer c.Close()
	code, err := c.Run(ctx, agent.Cmd{
		Args:   append(tail, logFile),
		Stdout: &termsafe.Writer{W: env.Stdout}, Stderr: &termsafe.Writer{W: env.Stderr},
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// cmdReset deletes a workspace's service data, so its services start empty
// at the next start.
func cmdReset(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	snapshot := fs.String("snapshot", "", "start over from this snapshot")
	empty := fs.Bool("empty", false, "start over empty, even if the repository has a default snapshot")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *snapshot != "" && *empty {
		return fmt.Errorf("%w: --snapshot and --empty don't go together", errUsage)
	}
	ws, err := resolveWorkspace(ctx, m, pos, env)
	if err != nil {
		return err
	}
	if err := confirm(env, *yes, fmt.Sprintf("Delete all service data (databases, queues, workflow history) in workspace %s?", ws.Name)); err != nil {
		return err
	}
	v, err := newVMM(env, false)
	if err != nil {
		return err
	}
	if *snapshot != "" && !v.HasSnapshot(ws.Repo, *snapshot) {
		return fmt.Errorf("no snapshot named %s; see skerry snapshot ls", *snapshot)
	}
	if v.SetupBusy(ws) {
		return fmt.Errorf("%s is being set up; reset it when setup has finished", ws.Name)
	}
	if err := v.Down(ws); err != nil {
		return err
	}
	if err := v.ResetData(ws, *snapshot, !*empty); err != nil {
		return err
	}
	switch {
	case *snapshot != "":
		fmt.Fprintf(env.Stdout, "%s's service data is now a copy of snapshot %s; setup runs again at the next start\n", ws.Name, *snapshot)
	default:
		fmt.Fprintf(env.Stdout, "%s's service data is deleted; it starts over, and setup runs again, at the next start\n", ws.Name)
	}
	return nil
}

// cmdSnapshot saves, lists and deletes a repository's data snapshots.
func cmdSnapshot(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: snapshot needs save, ls or rm", errUsage)
	}
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	name := fs.String("w", "", "workspace name (default: the one containing the current directory)")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	var wsArgs []string
	if *name != "" {
		wsArgs = []string{*name}
	}
	v, err := newVMM(env, true)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, m, wsArgs, env)
	if err != nil && args[0] != "save" && *name == "" {
		// Listing and deleting work from the repository itself too, even
		// once it has no workspaces left.
		wd, werr := env.Getwd()
		if werr != nil {
			return err
		}
		root, rerr := m.Git.MainRoot(ctx, wd)
		if rerr != nil {
			return err
		}
		if ws.Repo, rerr = workspace.Canonical(root); rerr != nil {
			return err
		}
		err = nil
	}
	if err != nil {
		return err
	}
	switch args[0] {
	case "save":
		if v.SetupBusy(ws) {
			return fmt.Errorf("%s is being set up; save the snapshot when setup has finished (skerry logs setup)", ws.Name)
		}
		snap := vmm.DefaultSnapshot
		if len(pos) > 1 {
			return fmt.Errorf("%w: snapshot save takes at most one name", errUsage)
		}
		if len(pos) == 1 {
			snap = pos[0]
		}
		proj, err := project.Load(ws.Path)
		if err != nil {
			return err
		}
		wasRunning := v.Running(ws)
		if err := v.Down(ws); err != nil {
			return err
		}
		if err := v.SaveSnapshot(ws, snap, proj.Services); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "saved %s's service data as snapshot %s\n", ws.Name, snap)
		if snap == vmm.DefaultSnapshot {
			fmt.Fprintln(env.Stdout, "new workspaces of this repository start from it")
		}
		if wasRunning {
			c, err := v.Up(ctx, ws)
			if err != nil {
				return err
			}
			c.Close()
		}
		return nil
	case "ls", "list":
		list, err := v.Snapshots(ws.Repo)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(env.Stdout, "no snapshots")
			return nil
		}
		tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tFROM\tSERVICES\tSAVED")
		for _, s := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, s.Workspace, strings.Join(s.Services, ", "), s.Created.Local().Format(time.DateTime))
		}
		return tw.Flush()
	case "rm", "remove":
		if len(pos) != 1 {
			return fmt.Errorf("%w: snapshot rm takes one name", errUsage)
		}
		if err := v.RemoveSnapshot(ws.Repo, pos[0]); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "deleted snapshot %s\n", pos[0])
		return nil
	}
	return fmt.Errorf("%w: snapshot needs save, ls or rm", errUsage)
}

// cmdSetup runs the project's seed and setup commands again.
func cmdSetup(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	pos, err := parse(flag.NewFlagSet("setup", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(ctx, m, pos, env)
	if err != nil {
		return err
	}
	v, err := newVMM(env, true)
	if err != nil {
		return err
	}
	if err := v.ForgetSetup(ws); err != nil {
		return err
	}
	proj, err := project.Load(ws.Path)
	if err != nil {
		return err
	}
	c, err := v.Up(ctx, ws)
	if err != nil {
		return err
	}
	c.Close()
	status := v.SetupStatus(ws)
	switch {
	case len(proj.Setup) == 0 && proj.SeedPostgres == "":
		fmt.Fprintln(env.Stdout, "this project has no seed or setup commands in skerry.yaml")
	case status == "":
		return errors.New("setup did not finish (the workspace stopped, or was interrupted); run skerry setup again")
	case strings.HasPrefix(status, "failed"):
		return fmt.Errorf("setup %s; see skerry logs setup", termsafe.Clean(status))
	default:
		fmt.Fprintf(env.Stdout, "setup: %s\n", status)
	}
	return nil
}

// cmdSeed loads a database dump from this Mac into the workspace.
func cmdSeed(ctx context.Context, m *workspace.Manager, args []string, env Env) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	name := fs.String("w", "", "workspace name (default: the one containing the current directory)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: seed takes one dump file (.sql, .sql.gz or .dump)", errUsage)
	}
	var wsArgs []string
	if *name != "" {
		wsArgs = []string{*name}
	}
	ws, err := resolveWorkspace(ctx, m, wsArgs, env)
	if err != nil {
		return err
	}
	v, err := newVMM(env, true)
	if err != nil {
		return err
	}
	if err := v.Seed(ctx, ws, pos[0], &termsafe.Writer{W: env.Stderr}); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "loaded %s into %s\n", pos[0], ws.Name)
	return nil
}

// cmdGC deletes cached images and packages that nothing needs.
func cmdGC(m *workspace.Manager, args []string, env Env) error {
	if len(args) > 0 {
		return fmt.Errorf("%w: gc takes no arguments", errUsage)
	}
	all, err := m.Registry.List()
	if err != nil {
		return err
	}
	v, err := newVMM(env, false)
	if err != nil {
		return err
	}
	removed, err := v.Collect(all)
	images, packages := 0, 0
	for _, r := range removed {
		if strings.HasPrefix(r, "image ") {
			images++
		} else {
			packages++
		}
	}
	fmt.Fprintf(env.Stdout, "deleted %d images and %d downloaded packages\n", images, packages)
	return err
}

// cmdStatusline prints one line about the workspace Claude Code is in, for
// its status line ("statusLine": {"type": "command", "command": "skerry
// statusline"}). It reads Claude's JSON on stdin, never starts anything,
// and prints nothing outside skerry workspaces.
func cmdStatusline(m *workspace.Manager, env Env) error {
	var in struct {
		Cwd       string `json:"cwd"`
		Workspace struct {
			CurrentDir string `json:"current_dir"`
		} `json:"workspace"`
	}
	_ = json.NewDecoder(io.LimitReader(env.Stdin, 1<<20)).Decode(&in)
	dir := in.Workspace.CurrentDir
	if dir == "" {
		dir = in.Cwd
	}
	if dir == "" {
		var err error
		if dir, err = env.Getwd(); err != nil {
			return nil
		}
	}
	ws, err := m.Find(dir)
	if err != nil || ws.Mode != registry.ModeSkerry {
		return nil
	}
	v, err := newVMM(env, false)
	if err != nil {
		return nil
	}
	line := "skerry " + ws.Name + ": "
	if pr := v.ReadProgress(ws); pr.Busy() {
		line += "preparing · " + pr.String()
	} else if pr.Phase == vmm.PhaseStartFailed {
		line += "failed to start · skerry status"
	} else if !v.Running(ws) {
		line += "stopped"
	} else {
		line += "running"
		if st, _, err := v.Status(ws); err == nil && len(st.Services) > 0 {
			line += " · " + strings.Join(st.Services, ", ")
		}
		if setup := v.SetupStatus(ws); strings.HasPrefix(setup, "failed") {
			line += " · setup failed"
		}
	}
	fmt.Fprintln(env.Stdout, termsafe.Clean(line))
	return nil
}

func joinPorts(ports []uint16) string {
	var out []string
	for _, p := range ports {
		out = append(out, strconv.Itoa(int(p)))
	}
	return strings.Join(out, ", ")
}

// appPorts describes where an app listens.
func appPorts(a project.App) string {
	if len(a.For) > 0 {
		return "builds for " + strings.Join(a.For, ", ")
	}
	what := "port " + joinPorts(a.Ports)
	if len(a.Ports) == 0 {
		what = "no port"
	}
	for _, p := range a.Taken {
		what += fmt.Sprintf("; port %d is another app's", p)
	}
	return what
}

// printAppChecks says what an app's env files point to in the VM, and the
// variables its example env file names that nothing sets: how to tell
// the apps will find each other and have the configuration they need.
func printAppChecks(w io.Writer, proj project.Project, a project.App, indent string, listEnv bool) {
	if len(a.EnvFiles) > 0 {
		fmt.Fprintf(w, "%senv from %s (%d variables)\n", indent, strings.Join(a.EnvFiles, ", "), len(a.EnvKeys))
	}
	for _, wire := range proj.Wiring(a) {
		to := wire.To
		if to == "" {
			to = "nothing in the VM listens there"
		}
		fmt.Fprintf(w, "%s%s → localhost:%d: %s\n", indent, wire.Key, wire.Port, to)
	}
	// An example env file lists what an app can read, not what it needs:
	// most have defaults. What it can't do without shows when it runs.
	switch missing := proj.MissingEnv(a); {
	case len(missing) == 0:
	case listEnv:
		fmt.Fprintf(w, "%snot set, named in an example env file: %s\n", indent, strings.Join(missing, ", "))
	default:
		fmt.Fprintf(w, "%s%d variables named in an example env file aren't set (skerry plan --env lists them)\n", indent, len(missing))
	}
}

// cmdPlan prints what skerry would set up for the project in DIR (default:
// the current directory): what it detected or was told, and from where.
func cmdPlan(args []string, env Env) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	listEnv := fs.Bool("env", false, "list the unset variables example env files name")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	dir := "."
	if len(pos) > 1 {
		return fmt.Errorf("%w: plan takes at most one folder", errUsage)
	}
	if len(pos) == 1 {
		dir = pos[0]
	}
	proj, err := project.Load(dir)
	if err != nil {
		return err
	}
	w := &termsafe.Writer{W: env.Stdout}
	or := func(s, none string) string {
		if s == "" {
			return none
		}
		return s
	}
	var runtimes []string
	for _, r := range proj.Runtimes {
		runtimes = append(runtimes, r.String())
	}
	fmt.Fprintf(w, "services:  %s\n", or(strings.Join(proj.Services, ", "), "none"))
	if proj.Source != "" {
		fmt.Fprintf(w, "           (%s)\n", proj.Source)
	}
	fmt.Fprintf(w, "runtimes:  %s\n", strings.Join(runtimes, ", "))
	if proj.RuntimesSource != "" {
		fmt.Fprintf(w, "           (%s)\n", proj.RuntimesSource)
	}
	if len(proj.Packages) > 0 {
		fmt.Fprintf(w, "packages:  %s\n", strings.Join(proj.Packages, ", "))
	}
	for _, a := range proj.PortAliases {
		fmt.Fprintf(w, "port:      %s also answers on %d, as the project's env files expect\n", a.Service, a.Port)
	}
	for _, db := range proj.Databases {
		fmt.Fprintf(w, "database:  %s (owner %s), from the project's env files\n", db.Name, db.User)
	}
	if proj.SeedPostgres != "" {
		fmt.Fprintf(w, "seed:      %s\n", proj.SeedPostgres)
	}
	if len(proj.Setup) == 0 {
		fmt.Fprintln(w, "setup:     none")
	} else {
		fmt.Fprintf(w, "setup:     (%s)\n", proj.SetupSource)
		for i, s := range proj.Setup {
			fmt.Fprintf(w, "  %d. %s\n", i+1, s)
		}
	}
	if len(proj.Apps) == 0 {
		fmt.Fprintln(w, "apps:      none")
	} else {
		fmt.Fprintf(w, "apps:      (%s)\n", proj.AppsSource)
		for _, a := range proj.Apps {
			fmt.Fprintf(w, "  %-24s %s: %s (%s)\n", a.Name, a.Dir, a.Command, appPorts(a))
			if len(a.For) == 0 {
				printAppChecks(w, proj, a, "      ", *listEnv)
			}
		}
	}
	if len(proj.OtherApps) > 0 {
		fmt.Fprintln(w, "not started (the repository's dev script or skerry.yaml leaves them out; list them in apps: to run them):")
		for _, a := range proj.OtherApps {
			fmt.Fprintf(w, "  %-24s %s: %s (%s)\n", a.Name, a.Dir, a.Command, appPorts(a))
		}
	}
	return nil
}
