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
}

type config struct {
	Services *[]string `yaml:"services"`
	Ports    []int     `yaml:"ports"`
}

// Load reads the project at dir.
func Load(dir string) (Project, error) {
	var p Project
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
		if c.Services != nil {
			if p.Services, err = services.Normalize(*c.Services); err != nil {
				return p, fmt.Errorf("%s: %w", FileName, err)
			}
			p.Source = FileName
			return p, nil
		}
	}
	p.Services, p.Source = Detect(dir)
	return p, nil
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
