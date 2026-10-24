package project

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/skerrysoftware-rgb/skerry/internal/services"
)

// What a project runs, found the way a developer would: the workspace
// packages of a monorepo, which of them are apps (they have a dev script
// and listen on a port), and the local databases their .env files point
// to. Everything here is read from the checkout, through an os.Root, with
// size and count limits: the checkout is the VM's to write.

// App is one of the project's own services, run in the VM.
type App struct {
	Name string `yaml:"name"`
	// Dir is the app's folder, relative to the checkout.
	Dir string `yaml:"dir"`
	// Command runs it, in Dir: by default the package manager's dev script.
	Command string `yaml:"command"`
	// Ports it listens on, forwarded to the host.
	Ports []uint16 `yaml:"ports"`
	// EnvFiles are loaded into its environment (relative to the checkout),
	// later ones winning.
	EnvFiles []string `yaml:"-"`
	// Package is its package.json name, for building what it depends on.
	Package string `yaml:"-"`
	// For names the apps this one builds for, when it is a workspace
	// package's dev watcher (a library rebuilding its dist) rather than an
	// app of its own.
	For []string `yaml:"-"`
	// Refs are the local addresses its env files point to: how it reaches
	// the services and the other apps.
	Refs []Ref `yaml:"-"`
	// EnvKeys are the variables its env files set (names only), and
	// Expected the ones the project's example env files list for it.
	EnvKeys  []string `yaml:"-"`
	Expected []string `yaml:"-"`
	// Taken are ports it would listen on that an earlier app has.
	Taken []uint16 `yaml:"-"`
	// Optional apps are detected but not started unless skerry.yaml names
	// them: the repository's own dev script doesn't start them.
	Optional bool `yaml:"-"`
	// NotHTTP says its ports aren't web servers (skerry.yaml's http:
	// false), so they are forwarded as plain TCP, without the proxy.
	NotHTTP bool `yaml:"-"`
}

// Ref is a local address an app's environment names.
type Ref struct {
	Key  string
	Port uint16
}

// Database is a local Postgres database an .env file points to.
type Database struct {
	Name, User string
}

// Limits on what detection reads.
const (
	maxWorkspacePackages = 300
	maxWorkspacePatterns = 64
	maxProcfileLines     = 64
	maxApps              = 24
	maxDatabases         = 32
	maxEnvFile           = 256 << 10
)

// envFileNames are the files skerry loads into an app's environment, in
// order (later ones win), as dotenv-style tools commonly do.
var envFileNames = []string{".env", ".env.development", ".env.local", ".env.development.local"}

// portKeys are environment variables that conventionally name the port an
// app listens on.
var portKeys = []string{"PORT", "HTTP_PORT", "SERVER_PORT", "SERVER_HTTP_PORT", "APP_PORT", "API_PORT", "WEB_PORT"}

type packageJSON struct {
	Name            string            `json:"name"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Workspaces      json.RawMessage   `json:"workspaces"`
}

func readPackageJSON(root *os.Root, dir string) (packageJSON, bool) {
	var p packageJSON
	data, err := readRootFile(root, filepath.Join(dir, "package.json"), maxScan)
	if err != nil || json.Unmarshal(data, &p) != nil {
		return p, false
	}
	return p, true
}

// readRootFile reads a regular file inside root, without following links
// out of it or blocking on FIFOs, up to max bytes.
func readRootFile(root *os.Root, rel string, max int64) ([]byte, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|oNoFollow|oNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, fmt.Errorf("%s is not a small regular file", rel)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err == nil && int64(len(data)) > max {
		err = fmt.Errorf("%s is larger than %d bytes", rel, max)
	}
	return data, err
}

// workspacePackages lists the monorepo's package folders (relative to the
// checkout): from package.json workspaces (npm, Yarn, Bun) or
// pnpm-workspace.yaml. Patterns are folders, optionally ending in /* or
// /**; a single level is listed for either.
func workspacePackages(root *os.Root, rootPkg packageJSON) []string {
	var patterns []string
	if len(rootPkg.Workspaces) > 0 {
		var list []string
		var obj struct {
			Packages []string `json:"packages"`
		}
		if json.Unmarshal(rootPkg.Workspaces, &list) == nil {
			patterns = list
		} else if json.Unmarshal(rootPkg.Workspaces, &obj) == nil {
			patterns = obj.Packages
		}
	}
	if data, err := readRootFile(root, "pnpm-workspace.yaml", maxScan); err == nil {
		var ws struct {
			Packages []string `yaml:"packages"`
		}
		if yaml.Unmarshal(data, &ws) == nil {
			patterns = append(patterns, ws.Packages...)
		}
	}
	if len(patterns) > maxWorkspacePatterns {
		patterns = patterns[:maxWorkspacePatterns]
	}
	var out []string
	add := func(dir string) {
		dir = filepath.Clean(dir)
		if !filepath.IsLocal(dir) || slices.Contains(out, dir) || len(out) >= maxWorkspacePackages {
			return
		}
		if info, err := root.Lstat(filepath.Join(dir, "package.json")); err == nil && info.Mode().IsRegular() {
			out = append(out, dir)
		}
	}
	for _, pat := range patterns {
		if strings.HasPrefix(pat, "!") {
			continue
		}
		pat = filepath.FromSlash(strings.TrimSuffix(strings.TrimSuffix(pat, "/**"), "/*"))
		if !strings.ContainsAny(pat, "*?[") {
			// A folder, opened without following links or blocking.
			if info, err := root.Lstat(pat); err != nil || !info.IsDir() {
				continue
			}
			f, err := root.OpenFile(pat, os.O_RDONLY|oNoFollow|oNonblock|oDirectory, 0)
			if err != nil {
				continue
			}
			entries, _ := f.ReadDir(maxWorkspacePackages)
			f.Close()
			add(pat) // the pattern may be a package itself
			for _, e := range entries {
				if e.IsDir() && e.Name() != "node_modules" && !strings.HasPrefix(e.Name(), ".") {
					add(filepath.Join(pat, e.Name()))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// packageManager says how the project runs scripts: npm, yarn, pnpm or bun.
func packageManager(has func(string) bool) string {
	switch {
	case has("pnpm-lock.yaml"):
		return "pnpm"
	case has("yarn.lock"):
		return "yarn"
	case has("bun.lock") || has("bun.lockb"):
		return "bun"
	}
	return "npm"
}

func runScript(pm, script string) string {
	switch pm {
	case "yarn":
		return "yarn " + script
	case "pnpm":
		return "pnpm " + script
	case "bun":
		return "bun run " + script
	}
	return "npm run " + script
}

// envFiles reads the env files in dir, in order, into one map (later
// files win). Values are taken literally, after dotenv-style quotes.
func envFiles(root *os.Root, dir string) (map[string]string, []string) {
	env, found, _ := envFilesExample(root, dir)
	return env, found
}

// envFilesExample is envFiles, also saying whether the folder has a
// .env.example.
func envFilesExample(root *os.Root, dir string) (map[string]string, []string, bool) {
	env := map[string]string{}
	var found []string
	example := false
	for _, name := range append([]string{".env.example"}, envFileNames...) {
		rel := filepath.Join(dir, name)
		data, err := readRootFile(root, rel, maxEnvFile)
		if err != nil {
			continue
		}
		vars := ParseEnv(data)
		if name == ".env.example" {
			example = true
			// Only for ports, and only what real files don't say.
			for k, v := range vars {
				if slices.Contains(portKeys, k) {
					env[k] = v
				}
			}
			continue
		}
		found = append(found, filepath.ToSlash(rel))
		for k, v := range vars {
			env[k] = v
		}
	}
	return env, found, example
}

// exampleNames are the files that document a folder's variables.
var exampleNames = []string{".env.example", ".env.sample", ".env.template", ".env.dist"}

// expectedKeys lists the variables the example env files in dirs name.
func expectedKeys(root *os.Root, dirs ...string) []string {
	var out []string
	for _, d := range dirs {
		for _, name := range exampleNames {
			data, err := readRootFile(root, filepath.Join(d, name), maxEnvFile)
			if err != nil {
				continue
			}
			for k := range ParseEnv(data) {
				if !slices.Contains(out, k) {
					out = append(out, k)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func keysOf(env map[string]string) []string {
	out := slices.Collect(maps.Keys(env))
	sort.Strings(out)
	return out
}

var envLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*=\s*(.*)$`)

// ParseEnv reads dotenv-style KEY=VALUE lines: comments, blank lines,
// `export`, and single, double or no quotes. Variable references are not
// expanded.
func ParseEnv(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxEnvFile)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		m := envLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v := strings.TrimSpace(m[2])
		switch {
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			v = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(v[1 : len(v)-1])
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
		}
		out[m[1]] = v
	}
	return out
}

// appFor decides whether a package is an app: it has a dev script, and
// either names a port (in its env, or a flag) or runs a dev server whose
// port is known.
func appFor(root *os.Root, dir string, pkg packageJSON, pm string) (App, bool) {
	dev := pkg.Scripts["dev"]
	if dev == "" {
		return App{}, false
	}
	script := expandScript(pkg, dev)
	// Env files the script loads itself (dotenv -e ../../.env), then the
	// folder's own.
	env := map[string]string{}
	var files []string
	for _, f := range dotenvFiles(script, dir) {
		if data, err := readRootFile(root, f, maxEnvFile); err == nil {
			files = append(files, filepath.ToSlash(f))
			maps.Copy(env, ParseEnv(data))
		}
	}
	own, ownFiles, example := envFilesExample(root, dir)
	if example && len(ownFiles) == 0 && len(files) == 0 {
		// It documents an env file the developer hasn't made: not set up
		// locally, so not started (skerry.yaml can still list it).
		return App{}, false
	}
	maps.Copy(env, own)
	files = append(files, ownFiles...)
	ports := commandPorts(script)
	if len(ports) == 0 {
		ports = envPorts(env)
	}
	if len(ports) == 0 {
		ports = defaultPorts(script)
	}
	if len(ports) == 0 {
		return App{}, false
	}
	name := filepath.Base(dir)
	if dir == "." {
		name = "app"
	}
	return App{Name: name, Dir: filepath.ToSlash(dir), Command: runScript(pm, "dev"), Ports: ports,
		EnvFiles: files, Package: pkg.Name, Refs: localRefs(env), EnvKeys: keysOf(env),
		Expected: expectedKeys(root, envDirs(dir, files)...)}, true
}

// envDirs are the folders an app's env comes from: its own, and those of
// the env files it loads.
func envDirs(dir string, files []string) []string {
	out := []string{dir}
	for _, f := range files {
		if d := filepath.Dir(filepath.FromSlash(f)); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// scriptCall is a script running another of its package's scripts.
var scriptCall = regexp.MustCompile(`(?:npm run|yarn run|yarn|pnpm run|pnpm|bun run|bun)\s+([\w:.-]+)`)

// expandScript is a script with the scripts it runs (one level) appended,
// so "npm run with:env -- vite" shows both the env loading and the server.
func expandScript(pkg packageJSON, script string) string {
	out := script
	for _, m := range scriptCall.FindAllStringSubmatch(script, 8) {
		if called, ok := pkg.Scripts[m[1]]; ok && len(out) < 16<<10 {
			out += "\n" + called
		}
	}
	return out
}

var dotenvFlag = regexp.MustCompile(`dotenv(?:-cli)?((?:\s+(?:-e|--env-file)[ =]\S+)+)`)
var dotenvPath = regexp.MustCompile(`(?:-e|--env-file)[ =](\S+)`)

// nodeEnvFile matches node's own --env-file=FILE (and -if-exists).
var nodeEnvFile = regexp.MustCompile(`\bnode\b[^&|;\n]*?\s--env-file(?:-if-exists)?[ =](\S+)`)

// dotenvFiles are the env files a script loads with dotenv-cli, relative to
// the checkout; ones outside it are ignored.
func dotenvFiles(script, dir string) []string {
	var out []string
	add := func(f string) {
		rel := filepath.Clean(filepath.Join(dir, strings.Trim(f, `"'`)))
		if filepath.IsLocal(rel) && !slices.Contains(out, rel) && len(out) < 8 {
			out = append(out, rel)
		}
	}
	for _, m := range dotenvFlag.FindAllStringSubmatch(script, 4) {
		for _, f := range dotenvPath.FindAllStringSubmatch(m[1], 8) {
			add(f[1])
		}
	}
	for _, m := range nodeEnvFile.FindAllStringSubmatch(script, 4) {
		add(m[1])
	}
	return out
}

// portFlag matches a port given on a command line: -p 3003, --port=8080,
// PORT=3001 next dev, runserver 0.0.0.0:8000, --bind 127.0.0.1:5000.
var portFlag = regexp.MustCompile(`(?:(?:^|\s)(?:-p|--port|-P)(?:=|\s+)|(?:^|\s)PORT=|runserver\s+(?:[\w.\[\]:]*:)?|(?:-b|--bind)(?:=|\s+)[\w.\[\]]*:)(\d{2,5})\b`)

func commandPorts(cmd string) []uint16 {
	var out []uint16
	for _, m := range portFlag.FindAllStringSubmatch(cmd, 8) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n < 65536 && !slices.Contains(out, uint16(n)) {
			out = append(out, uint16(n))
		}
	}
	return out
}

func envPorts(env map[string]string) []uint16 {
	var out []uint16
	for _, k := range portKeys {
		if n, err := strconv.Atoi(env[k]); err == nil && n > 0 && n < 65536 && !slices.Contains(out, uint16(n)) {
			out = append(out, uint16(n))
		}
	}
	return out
}

// devServers are dev servers whose default port is well known, matched in
// order against a command.
var devServers = []struct {
	pattern *regexp.Regexp
	port    uint16
}{
	{regexp.MustCompile(`\bnext\s+dev\b`), 3000},
	{regexp.MustCompile(`\bnux[ti]\s+dev\b`), 3000},
	{regexp.MustCompile(`\bastro\s+dev\b`), 4321},
	{regexp.MustCompile(`\bng\s+serve\b`), 4200},
	{regexp.MustCompile(`\bwebpack(?:-dev-server|\s+serve)\b`), 8080},
	{regexp.MustCompile(`\breact-router\s+dev\b|\bremix\s+vite:dev\b|\bvite(?:\s+(?:dev|serve)\b|\s+--|\s*$|\s*[;&|\n])`), 5173},
	{regexp.MustCompile(`\bmanage(?:\.py)?\s+runserver\b`), 8000},
	{regexp.MustCompile(`\brails\s+(?:server|s)\b`), 3000},
	{regexp.MustCompile(`\bflask\s+run\b`), 5000},
	{regexp.MustCompile(`\buvicorn\b`), 8000},
}

func defaultPorts(cmd string) []uint16 {
	for _, d := range devServers {
		if d.pattern.MatchString(cmd) {
			return []uint16{d.port}
		}
	}
	return nil
}

// localHosts are the names an env value uses for this machine.
var localHosts = []string{"localhost", "127.0.0.1", "0.0.0.0", "::1"}

var hostPort = regexp.MustCompile(`^(localhost|127\.0\.0\.1):(\d{2,5})$`)

// schemePorts are the ports URLs mean without one.
var schemePorts = map[string]uint16{"postgres": 5432, "postgresql": 5432, "redis": 6379, "rediss": 6379}

// localRefs finds the local addresses in env values: URLs on this machine
// (http://localhost:4000, postgres://…@127.0.0.1/db) and host:port pairs.
// The app's own port keys are where it listens, not what it reaches.
func localRefs(env map[string]string) []Ref {
	var out []Ref
	for k, v := range env {
		if slices.Contains(portKeys, k) {
			continue
		}
		var port uint16
		if m := hostPort.FindStringSubmatch(v); m != nil {
			n, _ := strconv.Atoi(m[2])
			port = uint16(n)
		} else if u, err := url.Parse(v); err == nil && u.Scheme != "" && slices.Contains(localHosts, u.Hostname()) {
			if n, err := strconv.Atoi(u.Port()); err == nil && n > 0 && n < 65536 {
				port = uint16(n)
			} else {
				port = schemePorts[u.Scheme]
			}
		}
		if port != 0 {
			out = append(out, Ref{Key: k, Port: port})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// localDatabases finds Postgres URLs on this machine in env values: the
// databases an app expects to find locally. Whatever their port, the
// VM's Postgres answers there (see portAliases).
func localDatabases(envs []map[string]string) []Database {
	var out []Database
	for _, env := range envs {
		for _, v := range env {
			u, err := url.Parse(v)
			if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
				continue
			}
			if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" {
				continue
			}
			db := Database{Name: strings.TrimPrefix(u.Path, "/"), User: u.User.Username()}
			if db.User == "" {
				db.User = "postgres"
			}
			if db.Name == "" {
				db.Name = db.User
			}
			if !validIdent(db.Name) || !validIdent(db.User) || slices.Contains(out, db) || len(out) >= maxDatabases {
				continue
			}
			out = append(out, db)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name+out[i].User < out[j].Name+out[j].User })
	return out
}

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,62}$`)

func validIdent(s string) bool { return identPattern.MatchString(s) }

// composeFiles are the compose files skerry reads, at the root.
var composeFiles = []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

// composeDatabases finds the databases a compose file's Postgres service
// is told to create (POSTGRES_USER, POSTGRES_DB): the ones the project's
// settings expect when it runs without containers too.
func composeDatabases(root *os.Root) []Database {
	var out []Database
	for _, name := range composeFiles {
		data, err := readRootFile(root, name, maxScan)
		if err != nil {
			continue
		}
		var c struct {
			Services map[string]struct {
				Image       string    `yaml:"image"`
				Environment yaml.Node `yaml:"environment"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal(data, &c) != nil {
			continue
		}
		for _, svc := range c.Services {
			if !composePostgres.MatchString("image: " + svc.Image) {
				continue
			}
			env := map[string]string{}
			switch n := svc.Environment; n.Kind {
			case yaml.MappingNode:
				_ = n.Decode(&env)
			case yaml.SequenceNode:
				var list []string
				_ = n.Decode(&list)
				for _, kv := range list {
					if k, v, ok := strings.Cut(kv, "="); ok {
						env[k] = v
					}
				}
			}
			db := Database{User: composeValue(env["POSTGRES_USER"]), Name: composeValue(env["POSTGRES_DB"])}
			if db.User == "" {
				db.User = "postgres"
			}
			if db.Name == "" {
				db.Name = db.User
			}
			if db.Name == "postgres" || !validIdent(db.Name) || !validIdent(db.User) || slices.Contains(out, db) || len(out) >= maxDatabases {
				continue
			}
			out = append(out, db)
		}
	}
	return out
}

var composeDefault = regexp.MustCompile(`^\$\{\w+:?-([^}]*)\}$`)

// composeValue is a compose environment value, or its default when it is
// ${VAR:-default}; other variable references are unknown, so "".
func composeValue(v string) string {
	if m := composeDefault.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	if strings.Contains(v, "$") {
		return ""
	}
	return v
}

// PortAlias is a port a project's env files use for one of the services,
// other than its own: compose files often publish Postgres on 54320 so it
// doesn't clash with one installed on the machine. The VM's service
// answers there too, so the URLs work unchanged.
type PortAlias struct {
	Port    uint16
	Service string
}

// portAliases finds local Postgres and Redis URLs on other ports.
func portAliases(envs []map[string]string) []PortAlias {
	var out []PortAlias
	for _, env := range envs {
		for _, v := range env {
			u, err := url.Parse(v)
			if err != nil || !slices.Contains(localHosts, u.Hostname()) {
				continue
			}
			service := map[string]string{"postgres": "postgres", "postgresql": "postgres", "redis": "valkey", "rediss": "valkey"}[u.Scheme]
			n, err := strconv.Atoi(u.Port())
			if service == "" || err != nil || n < 1 || n > 65535 || uint16(n) == schemePorts[u.Scheme] {
				continue
			}
			a := PortAlias{Port: uint16(n), Service: service}
			if servicePort(a.Port) {
				continue // another service's own: not an alias
			}
			if !slices.ContainsFunc(out, func(o PortAlias) bool { return o.Port == a.Port }) && len(out) < 8 {
				out = append(out, a)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func servicePort(port uint16) bool {
	for _, name := range services.Names() {
		p, _ := services.Lookup(name)
		if slices.ContainsFunc(p.Ports, func(sp services.Port) bool { return sp.Guest == port }) {
			return true
		}
	}
	return false
}

// servicesFromEnv infers services from local URLs in env values.
func servicesFromEnv(envs []map[string]string) []string {
	var out []string
	for _, env := range envs {
		for k, v := range env {
			lv := strings.ToLower(v)
			local := strings.Contains(lv, "localhost") || strings.Contains(lv, "127.0.0.1")
			switch {
			case local && (strings.HasPrefix(lv, "postgres://") || strings.HasPrefix(lv, "postgresql://")):
				out = append(out, "postgres")
			case local && (strings.HasPrefix(lv, "redis://") || strings.HasPrefix(lv, "rediss://")):
				out = append(out, "valkey")
			case local && strings.Contains(strings.ToUpper(k), "TEMPORAL") && strings.HasSuffix(lv, ":7233"):
				out = append(out, "temporal")
			}
		}
	}
	return out
}

// workspacePkg is a workspace package, for building what apps import.
type workspacePkg struct {
	deps  []string // its workspace dependencies, by name
	build bool     // it has a build script
}

// buildCommand builds what the apps depend on in a monorepo, with the
// tool the repository uses, before they start: turbo or pnpm filters, or
// else each dependency's build script, dependencies first.
func buildCommand(pm string, rootPkg packageJSON, apps []App, graph map[string]workspacePkg) string {
	var pkgs []string
	for _, a := range apps {
		if a.Package != "" && a.Dir != "." && len(a.For) == 0 {
			pkgs = append(pkgs, a.Package)
		}
	}
	if len(pkgs) == 0 {
		return ""
	}
	_, turbo := rootPkg.DevDependencies["turbo"]
	if !turbo {
		_, turbo = rootPkg.Dependencies["turbo"]
	}
	var b strings.Builder
	switch {
	case turbo:
		b.WriteString(runScript(pm, "turbo") + " run build")
		if pm == "npm" {
			b.Reset()
			b.WriteString("npx turbo run build")
		}
		for _, p := range pkgs {
			fmt.Fprintf(&b, " --filter=%s", shellQuote(p+"^..."))
		}
		return b.String()
	case pm == "pnpm":
		b.WriteString("pnpm")
		for _, p := range pkgs {
			fmt.Fprintf(&b, " --filter %s", shellQuote(p+"^..."))
		}
		b.WriteString(" run --if-present build")
		return b.String()
	}
	// Dependencies before what depends on them, apps themselves left out.
	var order []string
	seen := map[string]bool{}
	var visit func(name string, depth int)
	visit = func(name string, depth int) {
		if seen[name] || depth > 32 {
			return
		}
		seen[name] = true
		for _, d := range graph[name].deps {
			visit(d, depth+1)
		}
		if graph[name].build && !slices.Contains(pkgs, name) && len(order) < 32 {
			order = append(order, name)
		}
	}
	for _, p := range pkgs {
		visit(p, 0)
	}
	var steps []string
	for _, name := range order {
		switch pm {
		case "yarn":
			steps = append(steps, "yarn workspace "+shellQuote(name)+" run build")
		case "bun":
			steps = append(steps, "bun run --filter "+shellQuote(name)+" build")
		default:
			steps = append(steps, "npm run build -w "+shellQuote(name))
		}
	}
	return strings.Join(steps, " && ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// procfileApps reads Procfile.dev, or else (if plain) Procfile: each process is an
// app, run as foreman runs it, with PORT set (5000, 5100, … unless the
// project's bin/dev says otherwise). A process that names its port uses
// that one; "release" is a deploy step, not a process.
func procfileApps(root *os.Root, plain bool) []App {
	name := "Procfile.dev"
	data, err := readRootFile(root, name, maxScan)
	if err != nil {
		if !plain {
			return nil
		}
		name = "Procfile"
		if data, err = readRootFile(root, name, maxScan); err != nil {
			return nil
		}
	}
	base := 5000
	if dev, err := readRootFile(root, filepath.Join("bin", "dev"), maxScan); err == nil {
		if m := binDevPort.FindSubmatch(dev); m != nil {
			base, _ = strconv.Atoi(string(m[1]))
		}
	}
	var recipes map[string]string
	if makefile, err := readRootFile(root, "Makefile", maxScan); err == nil {
		recipes = makeRecipes(makefile)
	}
	env, files := envFiles(root, ".")
	refs, keys, expected := localRefs(env), keysOf(env), expectedKeys(root, ".")
	var out []App
	i := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	for lines := 0; sc.Scan() && lines < maxProcfileLines && len(out) < maxApps; lines++ {
		m := procfileLine.FindStringSubmatch(sc.Text())
		if m == nil || m[1] == "release" {
			continue
		}
		proc, cmd := strings.ToLower(m[1]), strings.TrimSpace(m[2])
		port := base + 100*i
		i++
		if port > 65535 {
			port = 0
		}
		expanded := expandMake(recipes, cmd)
		ports := commandPorts(expanded)
		a := App{Name: proc, Dir: ".", Command: cmd, EnvFiles: files, Refs: refs, EnvKeys: keys, Expected: expected}
		if port != 0 {
			a.Command = fmt.Sprintf("export PORT=%d; %s", port, cmd)
			if len(ports) == 0 && (proc == "web" || strings.Contains(cmd, "PORT")) {
				ports = []uint16{uint16(port)}
			}
		}
		if len(ports) == 0 && proc == "web" {
			ports = defaultPorts(expanded)
		}
		a.Ports = ports
		if appName.MatchString(a.Name) && len(a.Command) <= maxSetupCommand && !strings.ContainsRune(a.Command, 0) {
			out = append(out, a)
		}
	}
	return out
}

var (
	procfileLine = regexp.MustCompile(`^([A-Za-z0-9_-]+):\s*(\S.*)$`)
	binDevPort   = regexp.MustCompile(`PORT(?::-|=)"?(\d{2,5})`)
	makeCall     = regexp.MustCompile(`^make\s+([\w.-]+)\s*$`)
)

// makeRecipes reads a Makefile's targets and their recipes (64 targets,
// in its first 2,000 lines), to see what "make TARGET" runs.
func makeRecipes(makefile []byte) map[string]string {
	out := map[string]string{}
	target := ""
	lines := strings.Split(string(makefile), "\n")
	if len(lines) > 2000 {
		lines = lines[:2000]
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "\t") && target != "":
			if len(out[target]) < 4<<10 {
				out[target] += "\n" + strings.TrimSpace(l)
			}
		case makeTarget.MatchString(l) && len(out) < 64:
			target = makeTarget.FindStringSubmatch(l)[1]
			out[target] += ""
		default:
			target = ""
		}
	}
	return out
}

var makeTarget = regexp.MustCompile(`^([\w.-]+):`)

// expandMake is a command with, if it is "make TARGET", that target's
// recipe appended, so the port it serves on can be read.
func expandMake(recipes map[string]string, cmd string) string {
	if m := makeCall.FindStringSubmatch(cmd); m != nil {
		return cmd + recipes[m[1]]
	}
	return cmd
}

// frameworkApp is the dev server of a Django or Rails project without a
// Procfile: what its developers run by hand.
func frameworkApp(root *os.Root, has func(string) bool) (App, bool) {
	var a App
	switch {
	case has("manage.py"):
		a = App{Name: "web", Dir: ".", Command: "python manage.py runserver", Ports: []uint16{8000}}
	case has(filepath.Join("bin", "rails")):
		a = App{Name: "web", Dir: ".", Command: "bin/rails server", Ports: []uint16{3000}}
	default:
		return a, false
	}
	env, files := envFiles(root, ".")
	a.EnvFiles, a.Refs, a.EnvKeys, a.Expected = files, localRefs(env), keysOf(env), expectedKeys(root, ".")
	return a, true
}

var (
	turboDev   = regexp.MustCompile(`\bturbo\s+(?:run\s+)?dev\b[^&|;\n]*`)
	pnpmDev    = regexp.MustCompile(`\bpnpm\b[^&|;\n]*?\s(?:-r\s+)?(?:run\s+)?dev\b`)
	filterFlag = regexp.MustCompile(`(?:--filter|-F)(?:=|\s+)["']?([^\s"']+)`)
)

// devFilters are the packages the root dev script starts, when it names
// them: turbo run dev --filter=web, pnpm --filter web dev. None means it
// starts every app (or says nothing).
func devFilters(rootPkg packageJSON) []string {
	dev, ok := rootPkg.Scripts["dev"]
	if !ok {
		return nil
	}
	dev = expandScript(rootPkg, dev)
	var out []string
	for _, cmd := range append(turboDev.FindAllString(dev, 4), pnpmDev.FindAllString(dev, 4)...) {
		for _, m := range filterFlag.FindAllStringSubmatch(cmd, 16) {
			f := strings.TrimSuffix(strings.TrimSuffix(m[1], "..."), "^")
			if f != "" && !strings.HasPrefix(f, "!") && !strings.Contains(f, "{") {
				out = append(out, f)
			}
		}
	}
	return out
}

// devPrelude is what the root dev script runs before its dev servers, as
// setup steps: "npm run codegen && turbo run dev" generates code the apps
// import. Only calls to the repository's own scripts count, and none that
// starts containers.
func devPrelude(rootPkg packageJSON, pm string) []string {
	dev, ok := rootPkg.Scripts["dev"]
	if !ok {
		return nil
	}
	var out []string
	for _, step := range strings.Split(dev, "&&") {
		step = strings.TrimSpace(step)
		m := scriptCall.FindStringSubmatch(step)
		if m == nil || m[0] != step {
			break // the dev servers, or something else: stop here
		}
		called := rootPkg.Scripts[m[1]]
		if called == "" || m[1] == "dev" || strings.Contains(called, "docker") || strings.Contains(called, "podman") ||
			len(out) >= 4 {
			break
		}
		out = append(out, runScript(pm, m[1]))
	}
	return out
}

// scan is what detection found in a checkout.
type scan struct {
	rootPkg         packageJSON
	pm              string
	packages        []string
	packageServices []string
	envServices     []string
	apps            []App
	databases       []Database
	aliases         []PortAlias
	prelude         []string
	graph           map[string]workspacePkg
}

func scanProject(dir string) scan {
	var sc scan
	root, err := os.OpenRoot(dir)
	if err != nil {
		return sc
	}
	defer root.Close()
	has := func(name string) bool {
		info, err := root.Lstat(name)
		return err == nil && info.Mode().IsRegular()
	}
	sc.rootPkg, _ = readPackageJSON(root, ".")
	sc.pm = packageManager(has)
	sc.packages = workspacePackages(root, sc.rootPkg)

	envs := []map[string]string{}
	rootEnv, _ := envFiles(root, ".")
	envs = append(envs, rootEnv)
	dirs := sc.packages
	if len(dirs) == 0 && has("package.json") {
		dirs = []string{"."}
	}
	for _, d := range dirs {
		pkg, ok := readPackageJSON(root, d)
		if !ok {
			continue
		}
		if d != "." {
			if data, err := json.Marshal(map[string]any{"dependencies": pkg.Dependencies, "devDependencies": pkg.DevDependencies}); err == nil {
				for _, s := range fromPackageJSON(string(data)) {
					if !slices.Contains(sc.packageServices, s) {
						sc.packageServices = append(sc.packageServices, s)
					}
				}
			}
			env, _ := envFiles(root, d)
			envs = append(envs, env)
		}
		if app, ok := appFor(root, d, pkg, sc.pm); ok && len(sc.apps) < maxApps {
			if app.Name = uniqueName(sc.apps, app.Name, d); app.Name != "" {
				sc.apps = append(sc.apps, app)
			}
		}
	}
	// Procfile.dev is how the project runs in development; a plain
	// Procfile (often production's) only stands in for a dev script.
	others := procfileApps(root, len(sc.apps) == 0)
	if len(others) == 0 && len(sc.apps) == 0 {
		if a, ok := frameworkApp(root, has); ok {
			others = []App{a}
		}
	}
	for _, a := range others {
		if !slices.ContainsFunc(sc.apps, func(o App) bool { return o.Name == a.Name }) && len(sc.apps) < maxApps {
			sc.apps = append(sc.apps, a)
		}
	}
	// What the repository's own dev script starts, when it says.
	if filters := devFilters(sc.rootPkg); len(filters) > 0 {
		for i := range sc.apps {
			a := &sc.apps[i]
			a.Optional = a.Package != "" && !slices.ContainsFunc(filters, func(f string) bool {
				byName, _ := path.Match(f, a.Package)
				byDir, _ := path.Match(strings.TrimSuffix(strings.TrimPrefix(f, "./"), "/"), filepath.ToSlash(a.Dir))
				return byName || byDir
			})
		}
	}
	sc.apps = append(sc.apps, devWatchers(root, sc.packages, sc.apps, sc.pm)...)
	for _, s := range servicesFromEnv(envs) {
		if !slices.Contains(sc.envServices, s) {
			sc.envServices = append(sc.envServices, s)
		}
	}
	sc.prelude = devPrelude(sc.rootPkg, sc.pm)
	sc.graph = map[string]workspacePkg{}
	for _, d := range sc.packages {
		pkg, ok := readPackageJSON(root, d)
		if !ok || pkg.Name == "" {
			continue
		}
		sc.graph[pkg.Name] = workspacePkg{build: pkg.Scripts["build"] != ""}
	}
	for _, d := range sc.packages {
		pkg, ok := readPackageJSON(root, d)
		if !ok || pkg.Name == "" {
			continue
		}
		w := sc.graph[pkg.Name]
		for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies} {
			for dep := range deps {
				if _, ok := sc.graph[dep]; ok && !slices.Contains(w.deps, dep) {
					w.deps = append(w.deps, dep)
				}
			}
		}
		sort.Strings(w.deps)
		sc.graph[pkg.Name] = w
	}
	sc.databases = localDatabases(envs)
	for _, db := range composeDatabases(root) {
		if !slices.Contains(sc.databases, db) && len(sc.databases) < maxDatabases {
			sc.databases = append(sc.databases, db)
		}
	}
	// Not a port an app listens on.
	sc.aliases = slices.DeleteFunc(portAliases(envs), func(a PortAlias) bool {
		return slices.ContainsFunc(sc.apps, func(app App) bool { return slices.Contains(app.Ports, a.Port) })
	})
	return sc
}

var appName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// uniqueName is an app's name, made unique among apps by prefixing its
// parent folder (apps/api and services/api become api and services-api);
// "" if no valid name is free.
func uniqueName(apps []App, name, dir string) string {
	taken := func(n string) bool { return slices.ContainsFunc(apps, func(a App) bool { return a.Name == n }) }
	name = strings.ToLower(name)
	if appName.MatchString(name) && !taken(name) {
		return name
	}
	if alt := strings.ToLower(filepath.Base(filepath.Dir(dir)) + "-" + filepath.Base(dir)); appName.MatchString(alt) && !taken(alt) {
		return alt
	}
	return ""
}

// dedupePorts keeps each port for the first app that listens on it: two
// apps can't share one. Later apps record the ports they lost in Taken.
func dedupePorts(apps []App, taken map[uint16]bool) {
	for i := range apps {
		a := &apps[i]
		a.Ports = slices.DeleteFunc(slices.Clone(a.Ports), func(p uint16) bool {
			if taken[p] {
				a.Taken = append(a.Taken, p)
				return true
			}
			taken[p] = true
			return false
		})
	}
}

// appsFrom applies skerry.yaml's apps: a list of detected apps' names to
// run only those, or full entries ({name, dir, command, ports}); `[]` runs
// none. Without the key, every detected app runs.
func appsFrom(node *yaml.Node, detected []App) ([]App, string, error) {
	if node == nil || node.Tag == "!!null" {
		out := withWatchers(detected, func(a App) bool { return !a.Optional && len(a.For) == 0 })
		if len(out) == 0 {
			return nil, "", nil
		}
		return out, "detected", nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, "", fmt.Errorf("apps must be a list")
	}
	if len(node.Content) > maxApps {
		return nil, "", fmt.Errorf("at most %d apps", maxApps)
	}
	var out []App
	for _, item := range node.Content {
		var a App
		switch item.Kind {
		case yaml.ScalarNode:
			i := slices.IndexFunc(detected, func(d App) bool { return d.Name == item.Value })
			if i < 0 {
				return nil, "", fmt.Errorf("no detected app named %q", item.Value)
			}
			a = detected[i]
		case yaml.MappingNode:
			var spec struct {
				Name    string `yaml:"name"`
				Dir     string `yaml:"dir"`
				Command string `yaml:"command"`
				Ports   []int  `yaml:"ports"`
				HTTP    *bool  `yaml:"http"`
			}
			if err := item.Decode(&spec); err != nil {
				return nil, "", err
			}
			if spec.Dir == "" {
				spec.Dir = "."
			}
			if !filepath.IsLocal(spec.Dir) && spec.Dir != "." {
				return nil, "", fmt.Errorf("app %q: dir must be inside the repository", spec.Name)
			}
			if strings.TrimSpace(spec.Command) == "" || len(spec.Command) > maxSetupCommand || strings.ContainsRune(spec.Command, 0) {
				return nil, "", fmt.Errorf("app %q needs a command", spec.Name)
			}
			if len(spec.Ports) > 8 {
				return nil, "", fmt.Errorf("app %q: at most 8 ports", spec.Name)
			}
			a = App{Name: spec.Name, Dir: filepath.ToSlash(filepath.Clean(spec.Dir)), Command: spec.Command,
				NotHTTP: spec.HTTP != nil && !*spec.HTTP}
			for _, port := range spec.Ports {
				if port < 1 || port > 65535 {
					return nil, "", fmt.Errorf("app %q: port %d out of range", spec.Name, port)
				}
				a.Ports = append(a.Ports, uint16(port))
			}
			for _, d := range detected {
				if d.Dir == a.Dir {
					a.EnvFiles, a.Package, a.Refs, a.EnvKeys, a.Expected = d.EnvFiles, d.Package, d.Refs, d.EnvKeys, d.Expected
				}
			}
		default:
			return nil, "", fmt.Errorf("apps entries are names or {name, dir, command, ports}")
		}
		if !appName.MatchString(a.Name) || slices.ContainsFunc(out, func(o App) bool { return o.Name == a.Name }) {
			return nil, "", fmt.Errorf("app names must be unique, lowercase letters, digits, dots, dashes: %q", a.Name)
		}
		out = append(out, a)
	}
	chosen := slices.Clone(out)
	for _, w := range withWatchers(detected, func(d App) bool {
		return slices.ContainsFunc(chosen, func(o App) bool { return o.Name == d.Name })
	}) {
		if len(w.For) > 0 && !slices.ContainsFunc(out, func(o App) bool { return o.Name == w.Name }) {
			out = append(out, w)
		}
	}
	return out, FileName, nil
}

// withWatchers is the detected apps that pick chooses, with the dev
// watchers that build for them.
func withWatchers(detected []App, pick func(App) bool) []App {
	var out []App
	var names []string
	for _, a := range detected {
		if len(a.For) == 0 && pick(a) {
			out = append(out, a)
			names = append(names, a.Name)
		}
	}
	for _, w := range detected {
		if len(w.For) == 0 {
			continue
		}
		w.For = slices.DeleteFunc(slices.Clone(w.For), func(n string) bool { return !slices.Contains(names, n) })
		if len(w.For) > 0 {
			out = append(out, w)
		}
	}
	return out
}

// devWatchers finds the workspace packages the apps depend on (directly
// or not) that have a dev script of their own: in a monorepo, an app's
// dev setup usually includes its libraries' dev watchers, which build
// what the app imports (a turbo "dev" pipeline does exactly this). They
// run alongside the apps, without ports.
func devWatchers(root *os.Root, packages []string, apps []App, pm string) []App {
	type pkgInfo struct {
		dir string
		pkg packageJSON
	}
	byName := map[string]pkgInfo{}
	for _, d := range packages {
		if pkg, ok := readPackageJSON(root, d); ok && pkg.Name != "" {
			byName[pkg.Name] = pkgInfo{d, pkg}
		}
	}
	isApp := map[string]bool{}
	for _, a := range apps {
		isApp[a.Package] = true
	}
	var out []App
	index := map[string]int{}
	for _, a := range apps {
		seen := map[string]bool{a.Package: true}
		queue := []string{a.Package}
		for len(queue) > 0 {
			name := queue[0]
			queue = queue[1:]
			info, ok := byName[name]
			if !ok {
				continue
			}
			deps := make([]string, 0, len(info.pkg.Dependencies)+len(info.pkg.DevDependencies))
			for d := range info.pkg.Dependencies {
				deps = append(deps, d)
			}
			for d := range info.pkg.DevDependencies {
				deps = append(deps, d)
			}
			sort.Strings(deps)
			for _, dep := range deps {
				if seen[dep] {
					continue
				}
				seen[dep] = true
				dinfo, ok := byName[dep]
				if !ok {
					continue
				}
				queue = append(queue, dep)
				if isApp[dep] || dinfo.pkg.Scripts["dev"] == "" {
					continue
				}
				if i, ok := index[dep]; ok {
					if !slices.Contains(out[i].For, a.Name) {
						out[i].For = append(out[i].For, a.Name)
					}
					continue
				}
				name := uniqueName(append(slices.Clone(apps), out...), filepath.Base(dinfo.dir), dinfo.dir)
				if name == "" || len(apps)+len(out) >= maxApps {
					continue
				}
				env, files := envFiles(root, dinfo.dir)
				index[dep] = len(out)
				out = append(out, App{Name: name, Dir: filepath.ToSlash(dinfo.dir), Command: runScript(pm, "dev"),
					EnvFiles: files, Package: dep, For: []string{a.Name}, EnvKeys: keysOf(env)})
			}
		}
	}
	return out
}
