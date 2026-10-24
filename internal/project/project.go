// Package project reads what a repository needs from its workspace VMs: a
// skerry.yaml if it has one, and otherwise what skerry can tell from the
// files every project already has (package.json, compose files, go.mod...).
//
// The repository is not trusted. It can only choose among skerry's service
// presets and name guest ports to forward to the host's loopback interface;
// it cannot add packages, commands or mounts.
package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"

	"github.com/skerrysoftware-rgb/skerry/internal/services"
)

// FileName is the project config file, at the repository root.
const FileName = "skerry.yaml"

// Limits on what a repository can ask for.
const (
	maxConfig = 64 << 10
	maxScan   = 1 << 20
	maxPorts  = 16
	// maxSetup and maxSetupCommand bound the setup commands.
	maxSetup        = 32
	maxSetupCommand = 4 << 10
)

// Project is what a workspace runs.
type Project struct {
	// Services are normalized preset names.
	Services []string
	// Ports are guest ports forwarded to the host, besides the services'.
	Ports []uint16
	// Source says where Services came from: FileName, or the files
	// detection read.
	Source string
	// Setup are shell commands run in the VM, in the checkout, when a
	// workspace's data is new: installing dependencies, migrations.
	Setup []string
	// SetupSource says where Setup came from: FileName, or the lockfile
	// it was inferred from.
	SetupSource string
	// SeedPostgres is a dump in the repository (relative to its root)
	// loaded into the app database before setup, when the database is
	// empty: .sql, .sql.gz, or a pg_dump custom-format .dump.
	SeedPostgres string
	// Runtimes are the language toolchains in the VM; Node is always one.
	Runtimes []services.RuntimeChoice
	// Packages are extra packages for the VM, by name.
	Packages []string
	// SetupWatch are paths (relative to the checkout) whose change makes
	// setup run again: skerry.yaml, lockfiles, migration folders, and any
	// the project lists. A merge or rebase that brings new dependencies
	// or migrations is then picked up by the next command.
	SetupWatch []string
}

// Lockfiles and migration folders setup watches when they exist.
var (
	watchedLockfiles = []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock", "bun.lockb",
		"uv.lock", "requirements.txt", "poetry.lock", "go.sum", "Gemfile.lock"}
	watchedMigrations = []string{"migrations", "db/migrate", "prisma/migrations", "alembic/versions", "supabase/migrations"}
)

// maxWatch bounds the paths a project adds to SetupWatch.
const maxWatch = 32

type config struct {
	Services *[]string `yaml:"services"`
	Ports    []int     `yaml:"ports"`
	Runtimes []string  `yaml:"runtimes"`
	Packages []string  `yaml:"packages"`
	Setup    *[]string `yaml:"setup"`
	Watch    []string  `yaml:"setup_watch"`
	Seed     struct {
		Postgres string `yaml:"postgres"`
	} `yaml:"seed"`
}

// maxPackages bounds a project's extra packages.
const maxPackages = 32

// seedExtensions are the dump formats skerry loads.
var seedExtensions = []string{".sql", ".sql.gz", ".dump"}

// Load reads the project at dir.
func Load(dir string) (Project, error) {
	var p Project
	var runtimeSpecs []string
	var setup *[]string
	data, err := readLimited(filepath.Join(dir, FileName), maxConfig)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return p, err
	default:
		var c config
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return p, fmt.Errorf("%s: %w", FileName, err)
		}
		if len(c.Ports) > maxPorts {
			return p, fmt.Errorf("%s: at most %d ports", FileName, maxPorts)
		}
		for _, port := range c.Ports {
			if port < 1 || port > 65535 {
				return p, fmt.Errorf("%s: port %d out of range", FileName, port)
			}
			if !slices.Contains(p.Ports, uint16(port)) {
				p.Ports = append(p.Ports, uint16(port))
			}
		}
		if c.Setup != nil {
			if len(*c.Setup) > maxSetup {
				return p, fmt.Errorf("%s: at most %d setup commands", FileName, maxSetup)
			}
			for _, cmd := range *c.Setup {
				if strings.TrimSpace(cmd) == "" || len(cmd) > maxSetupCommand || strings.ContainsRune(cmd, 0) {
					return p, fmt.Errorf("%s: setup commands must be non-empty and under %d bytes", FileName, maxSetupCommand)
				}
			}
			setup = c.Setup
		}
		runtimeSpecs = c.Runtimes
		if len(c.Watch) > maxWatch {
			return p, fmt.Errorf("%s: at most %d setup_watch paths", FileName, maxWatch)
		}
		for _, w := range c.Watch {
			w = strings.TrimSuffix(w, "/")
			if !filepath.IsLocal(w) || strings.ContainsRune(w, 0) {
				return p, fmt.Errorf("%s: setup_watch paths must be inside the repository: %q", FileName, w)
			}
			p.SetupWatch = append(p.SetupWatch, filepath.ToSlash(filepath.Clean(w)))
		}
		if len(c.Packages) > maxPackages {
			return p, fmt.Errorf("%s: at most %d packages", FileName, maxPackages)
		}
		for _, pkg := range c.Packages {
			if !services.ValidPackageName(pkg) {
				return p, fmt.Errorf("%s: %q is not a package name", FileName, pkg)
			}
			if !slices.Contains(p.Packages, pkg) {
				p.Packages = append(p.Packages, pkg)
			}
		}
		sort.Strings(p.Packages)
		if seed := c.Seed.Postgres; seed != "" {
			// Read in the VM, from the checkout: it must name a file in it.
			if !filepath.IsLocal(seed) || strings.ContainsAny(seed, "\x00\n") ||
				!slices.ContainsFunc(seedExtensions, func(ext string) bool { return strings.HasSuffix(seed, ext) }) {
				return p, fmt.Errorf("%s: seed.postgres must be a .sql, .sql.gz or .dump file inside the repository", FileName)
			}
			p.SeedPostgres = filepath.ToSlash(filepath.Clean(seed))
		}
		if c.Services != nil {
			if p.Services, err = services.Normalize(*c.Services); err != nil {
				return p, fmt.Errorf("%s: %w", FileName, err)
			}
			p.Source = FileName
		}
	}
	if p.Source == "" {
		p.Services, p.Source = Detect(dir)
	}
	if p.SeedPostgres != "" && !slices.Contains(p.Services, "postgres") {
		return p, fmt.Errorf("%s: seed.postgres needs the postgres service", FileName)
	}
	if p.Runtimes, err = services.ParseRuntimes(runtimeSpecs); err != nil {
		return p, fmt.Errorf("%s: %w", FileName, err)
	}
	if setup != nil {
		p.Setup, p.SetupSource = *setup, FileName
	} else {
		p.Setup, p.SetupSource = defaultSetup(dir, p.Runtimes)
	}
	watch := []string{FileName}
	if strings.Contains(p.SetupSource, "package.json") {
		// Only when it is what setup installs from; otherwise editing a
		// script would reinstall everything.
		watch = append(watch, "package.json")
	}
	if root, err := os.OpenRoot(dir); err == nil {
		for _, f := range append(watchedLockfiles, watchedMigrations...) {
			if _, err := root.Lstat(filepath.FromSlash(f)); err == nil {
				watch = append(watch, f)
			}
		}
		root.Close()
	}
	if p.SeedPostgres != "" {
		watch = append(watch, p.SeedPostgres)
	}
	p.SetupWatch = slices.Compact(sortedCopy(append(watch, p.SetupWatch...)))
	return p, nil
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	sort.Strings(out)
	return out
}

// maxFingerprintEntries bounds how many files a fingerprint looks at.
const maxFingerprintEntries = 20000

// SetupFingerprint summarizes what setup depends on: its commands, its
// seed, and the name, size and modification time of everything under
// SetupWatch. Contents aren't read, so it is cheap to compute on every
// command; git rewrites the files it changes, which changes their times.
//
// The checkout is the VM's to write, so the walk stays inside it: it goes
// through an os.Root, which refuses any path (a planted symbolic link
// included) that leads out, and reads directories in batches, stopping at
// a fixed number of entries however large a directory is.
func (p Project) SetupFingerprint(dir string) string {
	h := sha256.New()
	fmt.Fprintf(h, "setup %q\nseed %q\n", p.Setup, p.SeedPostgres)
	root, err := os.OpenRoot(dir)
	if err != nil {
		fmt.Fprintf(h, "unreadable\n")
		return hex.EncodeToString(h.Sum(nil))
	}
	defer root.Close()
	budget := maxFingerprintEntries
	for _, w := range p.SetupWatch {
		fingerprintPath(h, root, filepath.FromSlash(w), &budget)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fingerprintPath(h io.Writer, root *os.Root, rel string, budget *int) {
	if *budget <= 0 {
		return
	}
	*budget--
	info, err := root.Lstat(rel)
	if err != nil {
		fmt.Fprintf(h, "%s missing\n", rel)
		return
	}
	if !info.IsDir() {
		fmt.Fprintf(h, "%s %v %d %d\n", rel, info.Mode().Type(), info.Size(), info.ModTime().UnixNano())
		return
	}
	if name := filepath.Base(rel); name == "node_modules" || name == ".git" || name == "__pycache__" {
		return
	}
	f, err := root.Open(rel)
	if err != nil {
		return
	}
	defer f.Close()
	for *budget > 0 {
		entries, err := f.ReadDir(256)
		for _, e := range entries {
			fingerprintPath(h, root, filepath.Join(rel, e.Name()), budget)
		}
		if err != nil {
			return
		}
	}
}

// defaultSetup infers how to install a project's dependencies from its
// lockfiles, for the runtimes it has, when skerry.yaml has no setup.
// Migrations are never guessed: a wrong command against a database is
// worse than none. `setup: []` turns this off.
func defaultSetup(dir string, runtimes []services.RuntimeChoice) ([]string, string) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, ""
	}
	defer root.Close()
	has := func(name string) bool {
		info, err := root.Lstat(name)
		return err == nil && info.Mode().IsRegular()
	}
	var cmds, from []string
	add := func(cmd, file string) { cmds, from = append(cmds, cmd), append(from, file) }
	// corepack fetches the yarn or pnpm version package.json asks for,
	// without asking and without editing package.json.
	const corepack = "COREPACK_ENABLE_DOWNLOAD_PROMPT=0 COREPACK_ENABLE_AUTO_PIN=0 corepack "
	switch {
	case !has("package.json"):
	case has("pnpm-lock.yaml"):
		add(corepack+"pnpm install --frozen-lockfile", "pnpm-lock.yaml")
	case has("yarn.lock") && has(".yarnrc.yml"):
		add(corepack+"yarn install --immutable", "yarn.lock")
	case has("yarn.lock"):
		add(corepack+"yarn install --frozen-lockfile", "yarn.lock")
	case has("bun.lock") && services.HasRuntime(runtimes, "bun"):
		add("bun install --frozen-lockfile", "bun.lock")
	case has("bun.lockb") && services.HasRuntime(runtimes, "bun"):
		add("bun install --frozen-lockfile", "bun.lockb")
	case has("package-lock.json"):
		add("npm ci", "package-lock.json")
	default:
		add("npm install", "package.json")
	}
	if services.HasRuntime(runtimes, "python") {
		switch {
		case has("uv.lock"):
			add("uv sync --frozen", "uv.lock")
		case has("requirements.txt"):
			add("uv venv --allow-existing .venv && uv pip install --python .venv -r requirements.txt", "requirements.txt")
		}
	}
	if services.HasRuntime(runtimes, "go") && has("go.mod") {
		add("go mod download", "go.mod")
	}
	if services.HasRuntime(runtimes, "ruby") && has("Gemfile") {
		add("bundle config set --local path vendor/bundle && bundle install", "Gemfile")
	}
	if len(cmds) == 0 {
		return nil, ""
	}
	return cmds, "inferred from " + strings.Join(from, ", ")
}

// detector maps a file's contents to the services it implies.
type detector struct {
	file string
	find func(text string) []string
}

// Compose images are matched on their name, the last path segment, so
// exporters and admin tools (redis_exporter, postgres-exporter,
// redis-commander) don't count.
var (
	composePostgres = regexp.MustCompile(`(?m)^\s*image:\s*["']?(?:[\w.-]+(?::\d+)?/)*(postgres|postgis|pgvector)(?:[:@"'\s]|$)`)
	composeValkey   = regexp.MustCompile(`(?m)^\s*image:\s*["']?(?:[\w.-]+(?::\d+)?/)*(redis|redis-stack|redis-stack-server|valkey)(?:[:@"'\s]|$)`)
	composeTemporal = regexp.MustCompile(`(?m)^\s*image:\s*["']?(?:[\w.-]+(?::\d+)?/)*temporalio/(auto-setup|server|temporal|admin-tools)(?:[:@"'\s]|$)`)
)

var detectors = []detector{
	{"package.json", fromPackageJSON},
	{"docker-compose.yml", fromCompose},
	{"docker-compose.yaml", fromCompose},
	{"compose.yml", fromCompose},
	{"compose.yaml", fromCompose},
	{"go.mod", fromGoMod},
	{"requirements.txt", fromRequirements},
	{"pyproject.toml", fromPyproject},
	{"Gemfile", fromGemfile},
}

var (
	goModules = map[string][]string{
		"postgres": {"github.com/jackc/pgx", "github.com/lib/pq"},
		"valkey":   {"github.com/redis/go-redis", "github.com/valkey-io/valkey-go"},
		"temporal": {"go.temporal.io/sdk"},
	}
	pythonPackages = map[string][]string{
		"postgres": {"psycopg", "psycopg2", "psycopg2-binary", "psycopg-binary", "asyncpg"},
		"valkey":   {"redis", "valkey"},
		"temporal": {"temporalio"},
	}
	rubyGems = map[string][]string{
		"postgres": {"pg"},
		"valkey":   {"redis"},
	}
)

// pythonRequirement is the package name at the start of a requirement
// ("redis[hiredis]>=5" → "redis").
var pythonRequirement = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)`)

var nodePackages = map[string][]string{
	"postgres": {"pg", "postgres", "pg-promise", "pgvector"},
	"valkey":   {"redis", "ioredis", "@redis/client", "iovalkey", "@valkey/valkey-glide", "bullmq"},
	"temporal": {"@temporalio/client", "@temporalio/worker"},
}

// Detect guesses the services a project uses from files at its root.
func Detect(dir string) (names []string, source string) {
	var files []string
	for _, d := range detectors {
		data, err := readLimited(filepath.Join(dir, d.file), maxScan)
		if err != nil {
			continue
		}
		found := d.find(string(data))
		if len(found) == 0 {
			continue
		}
		files = append(files, d.file)
		for _, n := range found {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	if len(files) == 0 {
		return nil, ""
	}
	return names, "detected from " + strings.Join(files, ", ")
}

func fromPackageJSON(text string) []string {
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal([]byte(text), &pkg) != nil {
		return nil
	}
	var out []string
	for service, names := range nodePackages {
		for _, n := range names {
			_, dep := pkg.Dependencies[n]
			_, dev := pkg.DevDependencies[n]
			if dep || dev {
				out = append(out, service)
				break
			}
		}
	}
	return out
}

func fromCompose(text string) []string {
	var out []string
	if composePostgres.MatchString(text) {
		out = append(out, "postgres")
	}
	if composeValkey.MatchString(text) {
		out = append(out, "valkey")
	}
	if composeTemporal.MatchString(text) {
		out = append(out, "temporal")
	}
	return out
}

// fromGoMod reads go.mod's direct requirements; indirect ones are pulled
// in by other modules (migration tools, test containers) and say nothing
// about what the project runs.
func fromGoMod(text string) []string {
	var mods []string
	inBlock := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "require ("):
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimPrefix(line, "require ")
		case !inBlock:
			continue
		}
		if strings.Contains(line, "// indirect") {
			continue
		}
		if f := strings.Fields(line); len(f) > 0 {
			mods = append(mods, f[0])
		}
	}
	return matchNames(mods, goModules, func(have, want string) bool {
		return have == want || strings.HasPrefix(have, want+"/")
	})
}

func fromRequirements(text string) []string {
	var names []string
	for _, line := range strings.Split(text, "\n") {
		if m := pythonRequirement.FindStringSubmatch(line); m != nil && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			names = append(names, normalizePython(m[1]))
		}
	}
	return matchNames(names, pythonPackages, equal)
}

// fromPyproject reads the dependency lists of a pyproject.toml: PEP 621's
// dependencies and optional-dependencies, PEP 735's dependency-groups, and
// Poetry's dependency tables. Text anywhere else (a description, another
// tool's settings) doesn't count.
func fromPyproject(text string) []string {
	var (
		names   []string
		table   string
		inArray bool // inside a requirement list
	)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !inArray && strings.HasPrefix(line, "[") && !strings.HasPrefix(line, "[[") {
			table = strings.Trim(line, "[] ")
			continue
		}
		key, value, isKey := strings.Cut(line, "=")
		key = strings.Trim(strings.TrimSpace(key), `"'`)
		switch {
		case inArray:
			value, isKey = line, false
		case !isKey:
			continue
		case poetryTable(table):
			names = append(names, normalizePython(key))
			continue
		case table == "project" && key == "dependencies",
			table == "project.optional-dependencies", table == "dependency-groups":
			value = strings.TrimSpace(value)
			inArray = strings.HasPrefix(value, "[")
		default:
			continue
		}
		for _, m := range pythonQuoted.FindAllStringSubmatch(value, -1) {
			names = append(names, normalizePython(m[1]))
		}
		// The list ends at a ] outside quotes and comments: extras such
		// as "psycopg[binary]" don't end it.
		bare := pythonString.ReplaceAllString(value, `""`)
		if i := strings.Index(bare, "#"); i >= 0 {
			bare = bare[:i]
		}
		if strings.Contains(bare, "]") {
			inArray = false
		}
	}
	return matchNames(names, pythonPackages, equal)
}

// pythonString matches a quoted TOML string.
var pythonString = regexp.MustCompile(`"[^"]*"|'[^']*'`)

// pythonQuoted finds the package name at the start of a quoted requirement.
var pythonQuoted = regexp.MustCompile(`["']\s*([A-Za-z0-9][A-Za-z0-9._-]*)`)

func poetryTable(table string) bool {
	if table == "tool.poetry.dependencies" || table == "tool.poetry.dev-dependencies" {
		return true
	}
	return strings.HasPrefix(table, "tool.poetry.group.") && strings.HasSuffix(table, ".dependencies")
}

func fromGemfile(text string) []string {
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^\s*gem\s+["']([\w-]+)["']`).FindAllStringSubmatch(text, -1) {
		names = append(names, m[1])
	}
	return matchNames(names, rubyGems, equal)
}

func normalizePython(name string) string {
	return strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(name))
}

func equal(a, b string) bool { return a == b }

// matchNames returns the services any of names implies.
func matchNames(names []string, table map[string][]string, match func(have, want string) bool) []string {
	var out []string
	for service, wants := range table {
		if slices.ContainsFunc(names, func(have string) bool {
			return slices.ContainsFunc(wants, func(want string) bool { return match(have, want) })
		}) {
			out = append(out, service)
		}
	}
	return out
}

// readLimited reads a regular file of at most max bytes. Symlinks are not
// followed: the checkout is writable from the VM, and a link could point
// anywhere on the host.
//
// The file is checked through the open descriptor, not the path, so the VM
// can't swap in a FIFO (which would block the open) or anything else
// between the check and the read.
func readLimited(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow|oNonblock, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symbolic link", path)
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err == nil && int64(len(data)) > max {
		err = fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return data, err
}
