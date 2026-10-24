//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
	"github.com/skerrysoftware-rgb/skerry/internal/project"
	"github.com/skerrysoftware-rgb/skerry/internal/services"
)

// The project's own services (its apps: an API, a web app, a worker) run
// under a supervisor of their own, like the backing services, but only
// when asked: the developer starts them when ready to try their changes
// (skerry start), by name or all at once. The host asks by changing
// appsWant, which says which apps should run; its generation goes up to
// restart the running ones (after setup runs again, with new
// dependencies, say).

const (
	appsRun  = "/run/skerry-apps"
	appsWant = appsRun + "/want"
	appsLock = appsRun + "/lock"
	appsLogs = "/var/log/skerry/apps"
	appLog   = 4 << 20
)

// wantedApps is which apps should run.
type wantedApps struct {
	Run []string `json:"run"`
	Gen int      `json:"gen"`
}

func readWanted() wantedApps {
	var w wantedApps
	if data, err := os.ReadFile(appsWant); err == nil {
		_ = json.Unmarshal(data, &w)
	}
	return w
}

// changeApps updates which apps should run, under a lock so two requests
// don't lose each other's change.
func changeApps(change func(w *wantedApps, all []string) error) error {
	cfg, err := ReadConfig(shareMount)
	if err != nil {
		return err
	}
	var all []string
	for _, a := range cfg.Apps {
		all = append(all, a.Name)
	}
	if err := os.MkdirAll(appsRun, 0o755); err != nil {
		return err
	}
	unlock, err := fsutil.Lock(appsLock)
	if err != nil {
		return err
	}
	defer unlock()
	w := readWanted()
	if err := change(&w, all); err != nil {
		return err
	}
	data, _ := json.Marshal(w)
	return fsutil.WriteFileAtomic(appsWant, data, 0o644)
}

// pick is names, checked against all; none means all of them.
func pick(names, all []string) ([]string, error) {
	if len(names) == 0 {
		return all, nil
	}
	for _, n := range names {
		if !slices.Contains(all, n) {
			return nil, fmt.Errorf("no app named %q (apps: %s)", n, strings.Join(all, ", "))
		}
	}
	return names, nil
}

// StartApps starts apps (all of them, if none are named).
func StartApps(names []string) error {
	return changeApps(func(w *wantedApps, all []string) error {
		names, err := pick(names, all)
		for _, n := range names {
			if !slices.Contains(w.Run, n) {
				w.Run = append(w.Run, n)
			}
		}
		return err
	})
}

// StopApps stops apps (all of them, if none are named).
func StopApps(names []string) error {
	return changeApps(func(w *wantedApps, all []string) error {
		names, err := pick(names, all)
		w.Run = slices.DeleteFunc(w.Run, func(n string) bool { return slices.Contains(names, n) })
		return err
	})
}

// RestartApps restarts the apps that are running, if any.
func RestartApps() error {
	return changeApps(func(w *wantedApps, all []string) error {
		w.Gen++
		return nil
	})
}

// RunApps supervises the apps until SIGTERM, starting and stopping them as
// appsWant says.
func RunApps() error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	cfg, err := ReadConfig(shareMount)
	if err != nil {
		return err
	}
	for _, d := range []string{appsRun, appsLogs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	base := map[string]string{
		"PATH": baseEnv["PATH"], "HOME": "/root", "LANG": "C.UTF-8",
		"COREPACK_ENABLE_DOWNLOAD_PROMPT": "0", "COREPACK_ENABLE_AUTO_PIN": "0",
	}
	for k, v := range services.Env(cfg.Services, cfg.Secret) {
		base[k] = v
	}

	type runningApp struct {
		cancel context.CancelFunc
		done   chan struct{}
		gen    int
	}
	running := map[string]*runningApp{}
	// halt stops apps together, so stopping several takes as long as the
	// slowest, not their sum.
	halt := func(names []string) {
		for _, n := range names {
			running[n].cancel()
		}
		for _, n := range names {
			<-running[n].done
			delete(running, n)
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			halt(slices.Collect(maps.Keys(running)))
			return nil
		case <-tick.C:
		}
		w := readWanted()
		var stale []string
		for name, r := range running {
			if !slices.Contains(w.Run, name) || r.gen != w.Gen {
				stale = append(stale, name)
			}
		}
		halt(stale)
		select {
		case <-stop: // asked to stop while those stopped: start nothing
			halt(slices.Collect(maps.Keys(running)))
			return nil
		default:
		}
		for _, a := range cfg.Apps {
			if _, ok := running[a.Name]; slices.Contains(w.Run, a.Name) && !ok {
				ctx, cancel := context.WithCancel(context.Background())
				r := &runningApp{cancel: cancel, done: make(chan struct{}), gen: w.Gen}
				running[a.Name] = r
				go func() {
					defer close(r.done)
					superviseApp(ctx, a, base, cfg.WorkspacePath)
				}()
			}
		}
	}
}

// superviseApp runs one app until ctx ends, restarting it with backoff.
func superviseApp(ctx context.Context, a App, base map[string]string, workspace string) {
	st := AppState{Name: a.Name, State: "starting", Since: time.Now().UTC()}
	save := func() {
		data, _ := json.Marshal(st)
		_ = fsutil.WriteFileAtomic(filepath.Join(appsRun, a.Name+".state"), data, 0o644)
	}
	log, err := fsutil.OpenCappedLog(filepath.Join(appsLogs, a.Name+".log"), appLog)
	if err != nil {
		st.State, st.LastExit = "stopped", "can't open its log: "+err.Error()
		save()
		return
	}
	defer log.Close()
	backoff := minBackoff
	for ctx.Err() == nil {
		cmd := exec.Command("/bin/sh", "-c", a.Command)
		cmd.Dir = a.Dir
		cmd.Env = appEnv(a, base, workspace)
		cmd.Stdout, cmd.Stderr = log, log
		cmd.WaitDelay = 3 * time.Second
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
		fmt.Fprintf(log, "skerry: %s: starting: %s\n", time.Now().UTC().Format(time.RFC3339), a.Command)
		begun := time.Now()
		if err := cmd.Start(); err != nil {
			st.State, st.LastExit = "restarting", err.Error()
			save()
		} else {
			st.State, st.PID, st.Since = "running", cmd.Process.Pid, begun.UTC()
			save()
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			select {
			case err := <-exited:
				// What it left running would hold its ports.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				st.LastExit = fmt.Sprint(err)
				if err == nil {
					st.LastExit = "exited"
				}
			case <-ctx.Done():
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
				select {
				case <-exited:
				case <-time.After(stopGrace):
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					<-exited
				}
				st.State, st.PID = "stopped", 0
				save()
				return
			}
			fmt.Fprintf(log, "skerry: %s: %s\n", time.Now().UTC().Format(time.RFC3339), st.LastExit)
			if time.Since(begun) > healthyAfter {
				backoff = minBackoff
			}
			st.State, st.PID = "restarting", 0
			st.Restarts++
			save()
		}
		select {
		case <-ctx.Done():
			st.State = "stopped"
			save()
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// appEnv is an app's environment: the agent's (PATH, the services'
// variables), the project's virtualenv if it has one, then the app's env
// files, which win: they are how the project configures itself.
func appEnv(a App, base map[string]string, workspace string) []string {
	env := map[string]string{}
	for k, v := range base {
		env[k] = v
	}
	if venv := filepath.Join(workspace, ".venv"); fileExists(filepath.Join(venv, "bin", "python")) {
		env["VIRTUAL_ENV"] = venv
		env["PATH"] = filepath.Join(venv, "bin") + ":" + env["PATH"]
	}
	for _, f := range a.EnvFiles {
		data, err := os.ReadFile(f)
		if err != nil || len(data) > 256<<10 {
			continue
		}
		for k, v := range project.ParseEnv(data) {
			if k != "PATH" {
				env[k] = v
			}
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// AppStates reports every app's state, for the host: whether its process
// runs, whether its ports accept connections, and, when it isn't running
// well, the end of its output (where an app says what it is missing).
func AppStates() ([]AppState, error) {
	ports := map[string][]uint16{}
	if cfg, err := ReadConfig(shareMount); err == nil {
		for _, a := range cfg.Apps {
			ports[a.Name] = a.Ports
		}
	}
	paths, _ := filepath.Glob(filepath.Join(appsRun, "*.state"))
	var out []AppState
	for _, p := range paths {
		var st AppState
		data, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(data, &st) != nil {
			continue
		}
		if st.State == "stopped" {
			out = append(out, st) // as asked: nothing to check
			continue
		}
		for _, port := range ports[st.Name] {
			if st.State == "running" && listening(port) {
				st.Listening = append(st.Listening, port)
			} else {
				st.Closed = append(st.Closed, port)
			}
		}
		if st.State != "running" || st.Restarts > 0 || len(st.Closed) > 0 {
			st.Output = logTail(filepath.Join(appsLogs, st.Name+".log"), 5)
		}
		out = append(out, st)
	}
	return out, nil
}

func listening(port uint16) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		if c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))), 300*time.Millisecond); err == nil {
			c.Close()
			return true
		}
	}
	return false
}

// logTail is the last n lines of a log, from its last 8 KiB.
func logTail(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > 8<<10 {
		_, _ = f.Seek(-8<<10, io.SeekEnd)
	}
	data, _ := io.ReadAll(io.LimitReader(f, 8<<10))
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		if len(l) > 300 {
			lines[i] = l[:300] + "…"
		}
	}
	return lines
}
