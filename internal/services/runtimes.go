package services

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Runtime is a language toolchain a workspace's VM can have. Like services,
// runtimes are presets: a project names one (and optionally a version),
// and skerry knows which signed packages that means.
type Runtime struct {
	Name string
	// Versions maps each supported version to its packages; Default is
	// used when the project names none.
	Versions map[string][]string
	Default  string
	// Commands run in the VM, from Claude Code sessions, when the project
	// has this runtime; without it they run on the host as before.
	Commands []string
}

var runtimes = map[string]Runtime{
	"node": {
		Name: "node",
		Versions: map[string][]string{
			"20": {"nodejs-20", "npm", "corepack"},
			"22": {"nodejs-22", "npm", "corepack"},
			"24": {"nodejs-24", "npm", "corepack"},
			"26": {"nodejs-26", "npm", "corepack"},
		},
		Default:  "24",
		Commands: []string{"node", "npm", "npx", "corepack", "yarn", "pnpm"},
	},
	"python": {
		Name: "python",
		Versions: map[string][]string{
			"3.11": {"python-3.11", "py3.11-pip", "uv"},
			"3.12": {"python-3.12", "py3.12-pip", "uv"},
			"3.13": {"python-3.13", "py3.13-pip", "uv"},
			"3.14": {"python-3.14", "py3.14-pip", "uv"},
		},
		Default:  "3.13",
		Commands: []string{"python", "python3", "pip", "pip3", "uv", "uvx"},
	},
	"go": {
		Name: "go",
		Versions: map[string][]string{
			"1.24": {"go-1.24"},
			"1.25": {"go-1.25"},
			"1.26": {"go-1.26"},
			"1.27": {"go-1.27"},
		},
		Default:  "1.27",
		Commands: []string{"go", "gofmt"},
	},
	"ruby": {
		Name: "ruby",
		Versions: map[string][]string{
			"3.2": {"ruby-3.2", "ruby3.2-bundler"},
			"3.3": {"ruby-3.3", "ruby3.3-bundler"},
			"3.4": {"ruby-3.4", "ruby3.4-bundler"},
		},
		Default:  "3.4",
		Commands: []string{"ruby", "gem", "bundle", "irb", "rake"},
	},
	"java": {
		Name: "java",
		Versions: map[string][]string{
			"17": {"openjdk-17-default-jdk"},
			"21": {"openjdk-21-default-jdk"},
			"25": {"openjdk-25-default-jdk"},
		},
		Default:  "21",
		Commands: []string{"java", "javac", "jar"},
	},
	"bun": {
		Name:     "bun",
		Versions: map[string][]string{"latest": {"bun"}},
		Default:  "latest",
		Commands: []string{"bun", "bunx"},
	},
	"deno": {
		Name:     "deno",
		Versions: map[string][]string{"latest": {"deno"}},
		Default:  "latest",
		Commands: []string{"deno"},
	},
}

// RuntimeChoice is a runtime at a version.
type RuntimeChoice struct {
	Name    string
	Version string
}

func (c RuntimeChoice) String() string { return c.Name + "@" + c.Version }

// RuntimeNames lists the runtimes skerry offers.
func RuntimeNames() []string {
	names := make([]string, 0, len(runtimes))
	for n := range runtimes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ParseRuntimes reads "python", "python@3.12" and the like. Node is always
// included (at its default version unless one is named), because skerry's
// shims route Node tooling into every workspace. The result is sorted.
func ParseRuntimes(specs []string) ([]RuntimeChoice, error) {
	chosen := map[string]string{}
	for _, spec := range specs {
		name, version, _ := strings.Cut(strings.TrimSpace(spec), "@")
		r, ok := runtimes[name]
		if !ok {
			return nil, fmt.Errorf("unknown runtime %q (known: %v)", name, RuntimeNames())
		}
		if version == "" {
			version = r.Default
		}
		if _, ok := r.Versions[version]; !ok {
			return nil, fmt.Errorf("runtime %s has no version %q (available: %v)", name, version, versionsOf(r))
		}
		if prev, dup := chosen[name]; dup && prev != version {
			return nil, fmt.Errorf("runtime %s is listed at two versions", name)
		}
		chosen[name] = version
	}
	if _, ok := chosen["node"]; !ok {
		chosen["node"] = runtimes["node"].Default
	}
	out := make([]RuntimeChoice, 0, len(chosen))
	for n, v := range chosen {
		out = append(out, RuntimeChoice{n, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func versionsOf(r Runtime) []string {
	vs := make([]string, 0, len(r.Versions))
	for v := range r.Versions {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return vs
}

// RuntimePackages lists the packages for chosen runtimes.
func RuntimePackages(chosen []RuntimeChoice) []string {
	var out []string
	for _, c := range chosen {
		out = append(out, runtimes[c.Name].Versions[c.Version]...)
	}
	return out
}

// HasRuntime reports whether chosen includes the named runtime.
func HasRuntime(chosen []RuntimeChoice, name string) bool {
	return slices.ContainsFunc(chosen, func(c RuntimeChoice) bool { return c.Name == name })
}

// RuntimeFor returns the runtime a command belongs to, if any.
func RuntimeFor(command string) (string, bool) {
	for name, r := range runtimes {
		if slices.Contains(r.Commands, command) {
			return name, true
		}
	}
	return "", false
}

// RuntimeCommands lists the commands a runtime routes into the VM.
func RuntimeCommands(name string) []string { return runtimes[name].Commands }

// AllRuntimeCommands lists every command any runtime routes into the VM.
func AllRuntimeCommands() []string {
	var out []string
	for _, n := range RuntimeNames() {
		out = append(out, runtimes[n].Commands...)
	}
	return out
}

// packageName is what an extra package name may look like.
var packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9.+_-]{0,63}$`)

// ValidPackageName reports whether name can be a package name.
func ValidPackageName(name string) bool { return packageName.MatchString(name) }
