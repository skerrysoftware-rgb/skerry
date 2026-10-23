package image

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Package is one entry in an APKINDEX.
type Package struct {
	Name     string
	Version  string
	Checksum string // C: field
	Size     int64
	Depends  []string
	Provides []string
	// Priority breaks ties between packages providing the same name (k: field).
	Priority int
	// Repo is the repository the index came from ("main", "community").
	Repo string
}

// File is the package's file name in its repository.
func (p Package) File() string { return p.Name + "-" + p.Version + ".apk" }

// ParseIndex parses APKINDEX text.
func ParseIndex(text, repo string) ([]Package, error) {
	var pkgs []Package
	for _, block := range strings.Split(text, "\n\n") {
		var p Package
		for _, line := range strings.Split(block, "\n") {
			if len(line) < 2 || line[1] != ':' {
				continue
			}
			value := line[2:]
			switch line[0] {
			case 'P':
				p.Name = value
			case 'V':
				p.Version = value
			case 'C':
				p.Checksum = value
			case 'S':
				size, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("package %s: bad size %q", p.Name, value)
				}
				p.Size = size
			case 'D':
				p.Depends = strings.Fields(value)
			case 'p':
				p.Provides = strings.Fields(value)
			case 'k':
				p.Priority, _ = strconv.Atoi(value)
			}
		}
		if p.Name == "" {
			continue
		}
		if p.Version == "" || p.Checksum == "" || p.Size <= 0 {
			return nil, fmt.Errorf("package %s: incomplete index entry", p.Name)
		}
		if strings.ContainsAny(p.Name+p.Version, "/\\") || strings.Contains(p.Name+p.Version, "..") {
			return nil, fmt.Errorf("package %q %q: unsafe name", p.Name, p.Version)
		}
		p.Repo = repo
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// Resolver finds the packages needed to install a set of names.
type Resolver struct {
	byName    map[string]Package
	providers map[string][]Package
}

// NewResolver indexes packages. Earlier packages win name clashes, so pass
// higher-precedence repositories first.
func NewResolver(pkgs []Package) *Resolver {
	r := &Resolver{byName: map[string]Package{}, providers: map[string][]Package{}}
	for _, p := range pkgs {
		if _, dup := r.byName[p.Name]; dup {
			continue
		}
		r.byName[p.Name] = p
		for _, prov := range p.Provides {
			name := depName(prov)
			r.providers[name] = append(r.providers[name], p)
		}
	}
	for _, list := range r.providers {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Priority > list[j].Priority })
	}
	return r
}

// Resolve returns the packages for names and everything they depend on,
// dependencies first.
func (r *Resolver) Resolve(names []string) ([]Package, error) {
	var (
		order []Package
		state = map[string]int{} // 1 visiting, 2 done
		visit func(dep, from string) error
	)
	visit = func(dep, from string) error {
		if strings.HasPrefix(dep, "!") {
			return nil // a conflict, not a requirement
		}
		p, ok := r.lookup(depName(dep))
		if !ok {
			if from == "" {
				return fmt.Errorf("no package provides %q", dep)
			}
			return fmt.Errorf("no package provides %q (needed by %s)", dep, from)
		}
		switch state[p.Name] {
		case 2:
			return nil
		case 1:
			return nil // dependency cycle; install order within it doesn't matter
		}
		state[p.Name] = 1
		for _, d := range p.Depends {
			if err := visit(d, p.Name); err != nil {
				return err
			}
		}
		state[p.Name] = 2
		order = append(order, p)
		return nil
	}
	for _, n := range names {
		if err := visit(n, ""); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func (r *Resolver) lookup(name string) (Package, bool) {
	if p, ok := r.byName[name]; ok {
		return p, true
	}
	if list := r.providers[name]; len(list) > 0 {
		return list[0], true
	}
	return Package{}, false
}

// depName strips a version constraint: "so:libz.so.1=1.3" → "so:libz.so.1".
func depName(dep string) string {
	if i := strings.IndexAny(dep, "<>=~"); i > 0 {
		return dep[:i]
	}
	return dep
}
