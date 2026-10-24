package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/skerrysoftware-rgb/skerry/internal/agent"
	"github.com/skerrysoftware-rgb/skerry/internal/fsutil"
	"github.com/skerrysoftware-rgb/skerry/internal/project"
	"github.com/skerrysoftware-rgb/skerry/internal/registry"
	"github.com/skerrysoftware-rgb/skerry/internal/termsafe"
)

// setupLogCap bounds the setup log, which the VM's commands write.
const setupLogCap = 4 << 20

// setupRecord says setup has run against the workspace's current data. It
// is deleted whenever the data is (a new disk, a reset), so setup runs
// again: seeding, then the project's commands, such as migrations.
type setupRecord struct {
	At     time.Time `json:"at"`
	Failed string    `json:"failed,omitempty"`
	// Inputs fingerprints what setup ran against (project.SetupFingerprint);
	// when it changes (a merge brought new dependencies or migrations),
	// setup runs again.
	Inputs string `json:"inputs,omitempty"`
}

// The seed scripts load a dump in one transaction, so a failed or
// interrupted load leaves the database empty (and the next run tries
// again) rather than half-loaded and taken for done.
const (
	// countTables sets $tables to the app database's table count; under
	// set -e, a psql failure ends the script.
	countTables = `tables=$(psql -tAc "select count(*) from pg_tables where schemaname = 'public'")`
	// psql applies --single-transaction only to -f and -c input, so
	// standard input is read as -f -.
	loadSQL     = `psql -v ON_ERROR_STOP=1 -q --single-transaction`
	loadDump    = `pg_restore --no-owner --no-privileges --exit-on-error --single-transaction -d "$PGDATABASE"`
	gunzipFirst = `gunzip -c | `
)

// seedScript loads $SKERRY_SEED (a path in the checkout) into the app
// database, if the database has no tables yet.
const seedScript = `set -e -o pipefail
` + countTables + `
if [ "$tables" != 0 ]; then
	echo "skerry: the database already has tables; not loading $SKERRY_SEED"
	exit 0
fi
echo "skerry: loading $SKERRY_SEED"
case "$SKERRY_SEED" in
*.sql.gz) gunzip -c -- "$SKERRY_SEED" | ` + loadSQL + ` -f - ;;
*.sql) ` + loadSQL + ` -f "$SKERRY_SEED" ;;
*) ` + loadDump + ` -- "$SKERRY_SEED" ;;
esac`

// errSetupFailed marks a setup command that ran and failed, as opposed to
// one cut short (the VM stopped), which is tried again next time.
var errSetupFailed = errors.New("setup failed")

// ensureSetup runs the project's seed and setup commands once for the
// workspace's current data. Concurrent callers wait for the one that
// runs them. A failure is recorded rather than retried on every command;
// skerry setup runs it again.
func (m *Manager) ensureSetup(ctx context.Context, c *agent.Client, ws registry.Workspace, proj project.Project) error {
	if len(proj.Setup) == 0 && proj.SeedPostgres == "" {
		return nil
	}
	p, err := m.paths(ws)
	if err != nil {
		return err
	}
	inputs := m.setupInputs(ws, p, proj)
	if done, err := m.setupDone(p, inputs); done || err != nil {
		return err
	}
	unlock, err := m.waitLock(ctx, p.setup+".lock", p.progress)
	if err != nil {
		return err
	}
	defer unlock()
	if done, err := m.setupDone(p, inputs); done || err != nil {
		return err // another process just ran it
	}
	if rec, err := readSetup(p.setup); err == nil && rec.Inputs != inputs {
		m.progressf("setup's inputs changed (dependencies, migrations or skerry.yaml): running it again")
	}

	log, err := fsutil.OpenCappedLog(p.setupLog, setupLogCap)
	if err != nil {
		return err
	}
	defer log.Close()
	var out io.Writer = log
	if m.Progress != nil {
		// What the VM prints reaches the terminal only after cleaning.
		out = io.MultiWriter(log, &termsafe.Writer{W: m.Progress})
	}
	m.progressf("setting up workspace %s", ws.Name)
	started := time.Now()

	// The phase names the step, not the command: the command text is the
	// repository's, and the phase is shown as skerry's own words.
	steps, step := len(proj.Setup), 0
	if proj.SeedPostgres != "" {
		steps++
	}
	run := func(label string, args []string, env map[string]string) error {
		fmt.Fprintf(out, "skerry: %s\n", label)
		step++
		m.phase(PhaseSetup, fmt.Sprintf("step %d of %d", step, steps))
		code, err := c.Run(ctx, agent.Cmd{Args: args, Dir: ws.Path, Env: env, Stdout: out, Stderr: out})
		if err == nil && code != 0 {
			err = fmt.Errorf("%w: %s exited with status %d", errSetupFailed, label, code)
		}
		return err
	}
	var failure error
	if proj.SeedPostgres != "" {
		failure = run("seeding postgres from "+proj.SeedPostgres,
			[]string{"sh", "-c", seedScript}, map[string]string{"SKERRY_SEED": proj.SeedPostgres, agent.EnvNoVenv: "1"})
	}
	for _, cmd := range proj.Setup {
		if failure != nil {
			break
		}
		failure = run(cmd, []string{"sh", "-c", cmd}, nil)
	}
	if ctx.Err() != nil {
		// Interrupted: the agent stops the command within a few seconds.
		// Keep the lock until then, so a second run can't start alongside
		// it, and try again next time.
		time.Sleep(6 * time.Second)
		return ctx.Err()
	}
	if failure != nil && (!errors.Is(failure, errSetupFailed) || stoppedSince(p, started)) {
		// The VM went away, or was being stopped (its services stop first,
		// so commands fail): not the project's failure. Try again next time.
		return failure
	}
	rec := setupRecord{At: time.Now().UTC(), Inputs: inputs}
	if failure == nil {
		// Setup may change its own inputs (npm install writes a lockfile):
		// record them as they are now, so that doesn't run it again.
		if after, err := project.Load(ws.Path); err == nil {
			rec.Inputs = m.setupInputs(ws, p, after)
		}
	}
	if failure != nil {
		rec.Failed = failure.Error()
		m.phase(PhaseFailed, rec.Failed)
		m.progressf("WARNING: workspace setup failed: %s; see skerry logs setup, and rerun it with skerry setup", termsafe.Clean(rec.Failed))
	}
	data, _ := json.Marshal(rec)
	return fsutil.WriteFileAtomic(p.setup, data, 0o600)
}

// setupInputs is what setup depends on: the project's inputs, and the
// image the VM is running (so setup runs again once a VM with new runtimes
// or packages boots).
func (m *Manager) setupInputs(ws registry.Workspace, p wsPaths, proj project.Project) string {
	image, _ := os.ReadFile(p.image)
	return proj.SetupFingerprint(ws.Path) + " image " + strings.TrimSpace(string(image))
}

// stoppedSince reports whether the workspace was asked to stop after t.
func stoppedSince(p wsPaths, t time.Time) bool {
	info, err := os.Stat(p.stopping)
	return err == nil && info.ModTime().After(t)
}

// setupDone reports whether setup has run for the current data and the
// current inputs, warning (without failing) when it ran and failed.
func (m *Manager) setupDone(p wsPaths, inputs string) (bool, error) {
	rec, err := readSetup(p.setup)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if rec.Inputs != inputs {
		return false, nil
	}
	if rec.Failed != "" {
		m.progressf("WARNING: this workspace's setup failed (%s); see skerry logs setup, and rerun it with skerry setup", termsafe.Clean(rec.Failed))
	}
	return true, nil
}

// lockCtx takes a file lock, waiting until it is free or ctx ends.
func lockCtx(ctx context.Context, path string) (func(), error) {
	for {
		unlock, ok, err := fsutil.TryLock(path)
		if err != nil || ok {
			return unlock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// SetupBusy reports whether setup (or a seed) is running for the
// workspace now.
func (m *Manager) SetupBusy(ws registry.Workspace) bool {
	p, err := m.paths(ws)
	return err == nil && setupBusy(p)
}

func setupBusy(p wsPaths) bool {
	unlock, ok, err := fsutil.TryLock(p.setup + ".lock")
	if err != nil || !ok {
		return err == nil
	}
	unlock()
	return false
}

func readSetup(path string) (setupRecord, error) {
	var rec setupRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(data, &rec)
}

// SetupStatus describes the last setup run: "" if none has run against the
// current data.
func (m *Manager) SetupStatus(ws registry.Workspace) string {
	p, err := m.paths(ws)
	if err != nil {
		return ""
	}
	rec, err := readSetup(p.setup)
	switch {
	case errors.Is(err, os.ErrNotExist), err != nil:
		return ""
	case rec.Failed != "":
		return "failed " + rec.At.Local().Format(time.DateTime) + ": " + rec.Failed
	}
	return "done " + rec.At.Local().Format(time.DateTime)
}

// ForgetSetup makes the next start run setup again.
func (m *Manager) ForgetSetup(ws registry.Workspace) error {
	p, err := m.paths(ws)
	if err != nil {
		return err
	}
	if err := os.Remove(p.setup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// openDump opens a dump the user named for skerry seed. Files in the
// workspace or its repository are refused: the VM (or whoever wrote the
// repository) controls them, and a symbolic link there could point at any
// of the user's files, which would then be sent into the VM. A dump in the
// repository belongs in seed.postgres instead, which is read in the VM.
func openDump(ws registry.Workspace, file string) (*os.File, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	refuse := func(p string) error {
		for _, dir := range []string{ws.Path, ws.Repo} {
			if within(p, dir) {
				return fmt.Errorf("%s leads into %s, which the VM or the repository controls; "+
					"to load a dump from the repository, name it in skerry.yaml (seed: postgres: ...)", file, dir)
			}
		}
		return nil
	}
	// Follow the chain of links one hop at a time, refusing if any hop is
	// in (or resolves into) the checkout: a link there could be redirected
	// anywhere.
	path := abs
	for hops := 0; ; hops++ {
		parent, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		path = filepath.Join(parent, filepath.Base(path))
		if err := refuse(path); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			break
		}
		if hops == 40 {
			return nil, fmt.Errorf("%s: too many links", file)
		}
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", file)
	}
	if err == nil {
		// Check where the file actually opened is, in case a directory
		// was swapped in the meantime.
		var opened string
		if opened, err = fsutil.FDPath(f); err == nil {
			err = refuse(opened)
		}
	}
	if err == nil {
		err = syscall.SetNonblock(int(f.Fd()), false)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if runtime.GOOS == "darwin" { // case-insensitive by default
		rel, err = filepath.Rel(strings.ToLower(dir), strings.ToLower(path))
	}
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// Seed loads a dump from this Mac into the workspace's app database, which
// must be empty. Its format comes from its name: .sql, .sql.gz or .dump.
// The project's setup (migrations) is not run first, and runs at the next
// start, on top of the dump.
func (m *Manager) Seed(ctx context.Context, ws registry.Workspace, file string, out io.Writer) error {
	var script string
	switch {
	case strings.HasSuffix(file, ".sql.gz"):
		script = gunzipFirst + loadSQL + " -f -"
	case strings.HasSuffix(file, ".sql"):
		script = loadSQL + " -f -"
	case strings.HasSuffix(file, ".dump"):
		script = loadDump
	default:
		return fmt.Errorf("%s: use a .sql, .sql.gz or pg_dump custom-format .dump file", file)
	}
	f, err := openDump(ws, file)
	if err != nil {
		return err
	}
	defer f.Close()
	c, err := m.boot(ctx, ws)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := m.waitServices(ctx, c); err != nil {
		return err
	}
	// Setup mustn't run while the dump loads, nor finish after it.
	p, err := m.paths(ws)
	if err != nil {
		return err
	}
	unlock, err := lockCtx(ctx, p.setup+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	check := countTables + `
if [ "$tables" != 0 ]; then
	echo "skerry: the app database isn't empty; empty it with skerry reset --empty" >&2
	exit 3
fi`
	code, err := c.Run(ctx, agent.Cmd{Args: []string{"sh", "-c", "set -e -o pipefail\n" + check + "\n" + script},
		Env: map[string]string{agent.EnvNoVenv: "1"}, Stdin: f, Stdout: out, Stderr: out})
	if ctx.Err() != nil {
		time.Sleep(6 * time.Second) // let the agent stop the load before setup may start
		return ctx.Err()
	}
	if err == nil && code != 0 {
		err = fmt.Errorf("loading %s failed (status %d)", file, code)
	}
	if err != nil {
		return err
	}
	return removeIfExists(p.setup) // setup (migrations) runs on top of it next
}
