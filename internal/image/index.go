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
	// Repo is the repository URL the index came from.
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
	providers map[string][]provider
}

// provider is a package that provides a name, at the version its provides
// entry gives ("glibc=2.44-r9"), if any.
type provider struct {
	Package
	provided string
}

// NewResolver indexes packages. Earlier packages win name clashes, so pass
// higher-precedence repositories first.
func NewResolver(pkgs []Package) *Resolver {
	r := &Resolver{byName: map[string]Package{}, providers: map[string][]provider{}}
	// Rolling repositories (Wolfi) list many versions of a package; keep the
	// newest. Between repositories, the earlier one wins.
	repoOrder := map[string]int{}
	for _, p := range pkgs {
		if _, ok := repoOrder[p.Repo]; !ok {
			repoOrder[p.Repo] = len(repoOrder)
		}
		cur, dup := r.byName[p.Name]
		switch {
		case !dup:
		case repoOrder[p.Repo] > repoOrder[cur.Repo]:
			continue
		case repoOrder[p.Repo] == repoOrder[cur.Repo] && CompareVersions(p.Version, cur.Version) <= 0:
			continue
		}
		r.byName[p.Name] = p
	}
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := r.byName[n]
		for _, prov := range p.Provides {
			name, version, _ := strings.Cut(prov, "=")
			r.providers[name] = append(r.providers[name], provider{p, version})
		}
	}
	return r
}

// CompareVersions orders apk versions such as "24.22.1-r2" or
// "3.13.16_git20261016-r2": dotted numbers (with optional letters), then
// suffixes (_alpha < _beta < _pre < _rc < none < _cvs < _svn < _git < _hg
// < _p), then the -rN release. It returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	va, vb := parseVersion(a), parseVersion(b)
	n := max(len(va.parts), len(vb.parts))
	for i := 0; i < n; i++ {
		var pa, pb versionPart
		if i < len(va.parts) {
			pa = va.parts[i]
		}
		if i < len(vb.parts) {
			pb = vb.parts[i]
		}
		if c := cmpInt(pa.num, pb.num); c != 0 {
			return c
		}
		if c := strings.Compare(pa.letter, pb.letter); c != 0 {
			return c
		}
	}
	n = max(len(va.suffixes), len(vb.suffixes))
	for i := 0; i < n; i++ {
		sa, sb := versionSuffix{rank: suffixNone}, versionSuffix{rank: suffixNone}
		if i < len(va.suffixes) {
			sa = va.suffixes[i]
		}
		if i < len(vb.suffixes) {
			sb = vb.suffixes[i]
		}
		if c := cmpInt(sa.rank, sb.rank); c != 0 {
			return c
		}
		if c := cmpInt(sa.num, sb.num); c != 0 {
			return c
		}
	}
	return cmpInt(va.release, vb.release)
}

type versionPart struct {
	num    int64
	letter string
}

type versionSuffix struct {
	rank int64
	num  int64
}

type version struct {
	parts    []versionPart
	suffixes []versionSuffix
	release  int64
}

const suffixNone = 4

var suffixRanks = map[string]int64{"alpha": 0, "beta": 1, "pre": 2, "rc": 3, "cvs": 5, "svn": 6, "git": 7, "hg": 8, "p": 9}

func parseVersion(v string) version {
	var out version
	if i := strings.LastIndex(v, "-r"); i >= 0 {
		out.release, _ = strconv.ParseInt(v[i+2:], 10, 64)
		v = v[:i]
	}
	main, rest, _ := strings.Cut(v, "_")
	for _, part := range strings.Split(main, ".") {
		digits := 0
		for digits < len(part) && part[digits] >= '0' && part[digits] <= '9' {
			digits++
		}
		n, _ := strconv.ParseInt(part[:digits], 10, 64)
		out.parts = append(out.parts, versionPart{num: n, letter: part[digits:]})
	}
	if rest != "" {
		for _, sfx := range strings.Split(rest, "_") {
			name := strings.TrimRight(sfx, "0123456789")
			n, _ := strconv.ParseInt(sfx[len(name):], 10, 64)
			rank, ok := suffixRanks[name]
			if !ok {
				rank = suffixNone
			}
			out.suffixes = append(out.suffixes, versionSuffix{rank: rank, num: n})
		}
	}
	return out
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
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
		p, ok := r.lookup(depName(dep), state)
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

// lookup picks the package for a name the way apk does: a package already
// chosen that satisfies it wins, so one library is never installed twice
// under two names; otherwise the highest version on offer, whether a
// package of that name or one providing it ("glibc-2.44" provides
// "glibc=2.44-r9"), then the higher provider priority.
func (r *Resolver) lookup(name string, chosen map[string]int) (Package, bool) {
	real, hasReal := r.byName[name]
	if hasReal && chosen[real.Name] > 0 {
		return real, true
	}
	for _, p := range r.providers[name] {
		if chosen[p.Name] > 0 {
			return p.Package, true
		}
	}
	// Versioned candidates: the package of that name, and providers that
	// give a version. The highest version wins, then the priority.
	best, bestVersion, found := Package{}, "", false
	if hasReal {
		best, bestVersion, found = real, real.Version, true
	}
	for _, p := range r.providers[name] {
		if p.provided == "" {
			continue
		}
		c := 1
		if found {
			c = CompareVersions(p.provided, bestVersion)
		}
		if c > 0 || c == 0 && p.Priority > best.Priority {
			best, bestVersion, found = p.Package, p.provided, true
		}
	}
	if found {
		return best, true
	}
	// Only unversioned providers: the highest priority wins; the
	// providers are in name order, so ties always break the same way.
	for _, p := range r.providers[name] {
		if !found || p.Priority > best.Priority {
			best, found = p.Package, true
		}
	}
	return best, found
}

// depName strips a version constraint: "so:libz.so.1=1.3" → "so:libz.so.1".
func depName(dep string) string {
	if i := strings.IndexAny(dep, "<>=~"); i > 0 {
		return dep[:i]
	}
	return dep
}
