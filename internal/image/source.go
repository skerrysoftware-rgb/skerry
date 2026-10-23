package image

import (
	"net/url"
	"strings"
)

// Source is a set of signed package repositories, verified with its own
// pinned keys only.
type Source struct {
	// Name labels the source and selects its pinned keys and cache folder.
	Name string
	// Repos are repository URLs without the architecture, searched in
	// order: <repo>/<arch>/APKINDEX.tar.gz.
	Repos []string
	Keys  Keys
}

// Alpine is the source of the guest kernel.
func Alpine() (Source, error) {
	keys, err := TrustedKeys("alpine")
	if err != nil {
		return Source{}, err
	}
	const base = "https://dl-cdn.alpinelinux.org/alpine/v3.24/"
	return Source{Name: "alpine", Repos: []string{base + "main", base + "community"}, Keys: keys}, nil
}

// Wolfi is the source of the guest system. It is glibc-based, so packages
// with native code built for ordinary Linux (such as Temporal's worker)
// run unchanged.
func Wolfi() (Source, error) {
	keys, err := TrustedKeys("wolfi")
	if err != nil {
		return Source{}, err
	}
	return Source{Name: "wolfi", Repos: []string{"https://packages.wolfi.dev/os"}, Keys: keys}, nil
}

// WolfiArch is Wolfi's and Alpine's name for an architecture.
func WolfiArch(goarch string) string {
	if goarch == "amd64" {
		return "x86_64"
	}
	return "aarch64"
}

// repoKey turns a repository URL into a folder name for the cache.
func repoKey(repo string) string {
	u, err := url.Parse(repo)
	if err != nil {
		return strings.NewReplacer("/", "_", ":", "_").Replace(repo)
	}
	return strings.NewReplacer("/", "_", ":", "_").Replace(u.Host + u.Path)
}
