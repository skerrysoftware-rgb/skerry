// Package services defines the backing services a workspace can run in its
// VM: Postgres (with pgvector), Valkey (Redis-compatible) and the Temporal
// dev server. Each workspace gets its own copy with its own data, on a disk
// that survives restarts.
//
// Services are presets: a project names them, and skerry decides how they
// are installed, configured and run. A repository therefore cannot make a
// workspace run arbitrary daemons or open arbitrary ports on the host.
package services

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
)

// Port is a port a service listens on in the guest.
type Port struct {
	Name  string `json:"name"`
	Guest uint16 `json:"guest"`
	// Forward exposes it on the host's loopback interface by default.
	// Ports without authentication are forwarded only when the project
	// lists them, since any local user can reach a loopback port.
	Forward bool `json:"-"`
	// Web says it serves HTTP, so its URL uses a .localhost name.
	Web bool `json:"-"`
}

// Preset is one service skerry knows how to run.
type Preset struct {
	Name string
	// Packages installs the service in the workspace image.
	Packages []string
	// Ports are forwarded to the host's loopback interface; the first one
	// is the service's main port.
	Ports []Port
	// User and UID are the unprivileged account the service runs as.
	User string
	UID  int
	// Description is shown by skerry status.
	Description string
}

var presets = map[string]Preset{
	"postgres": {
		Name: "postgres",
		// contrib has the extensions projects expect, as the official
		// image has them (pg_trgm, hstore, citext, uuid-ossp…); the
		// headers let drivers that compile against libpq (Ruby's pg,
		// Python's psycopg2) install.
		Packages:    []string{"postgresql-17", "postgresql-17-client", "postgresql-17-contrib", "postgresql-17-dev", "pgvector-17"},
		Ports:       []Port{{Name: "postgres", Guest: 5432, Forward: true}},
		User:        "postgres",
		UID:         70,
		Description: "PostgreSQL 17 with pgvector",
	},
	"valkey": {
		Name:        "valkey",
		Packages:    []string{"valkey", "valkey-cli"},
		Ports:       []Port{{Name: "valkey", Guest: 6379, Forward: true}},
		User:        "valkey",
		UID:         101,
		Description: "Valkey 8 (Redis-compatible)",
	},
	"temporal": {
		Name:        "temporal",
		Packages:    []string{"temporal"},
		Ports:       []Port{{Name: "temporal", Guest: 7233}, {Name: "temporal-ui", Guest: 8233, Web: true}},
		User:        "temporal",
		UID:         102,
		Description: "Temporal dev server",
	},
}

var aliases = map[string]string{
	"postgresql": "postgres",
	"pg":         "postgres",
	"pgvector":   "postgres",
	"redis":      "valkey",
}

// Lookup finds a preset by name or alias.
func Lookup(name string) (Preset, bool) {
	if real, ok := aliases[name]; ok {
		name = real
	}
	p, ok := presets[name]
	return p, ok
}

// Names lists the presets.
func Names() []string {
	names := make([]string, 0, len(presets))
	for n := range presets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Normalize resolves aliases, drops duplicates and sorts, so equal sets of
// services always produce the same image.
func Normalize(names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		p, ok := Lookup(n)
		if !ok {
			return nil, fmt.Errorf("unknown service %q (known: %v)", n, Names())
		}
		if !slices.Contains(out, p.Name) {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Packages lists the packages the services need.
func Packages(names []string) []string {
	var out []string
	for _, n := range names {
		if p, ok := Lookup(n); ok {
			out = append(out, p.Packages...)
		}
	}
	return out
}

// Database is the database created for the workspace's app.
const Database = "app"

// Password derives a service's own password from the workspace secret, so
// a password one service gives away (Redis AUTH travels in the clear)
// opens nothing else.
func Password(secret, service string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("skerry service password\x00" + service))
	return hex.EncodeToString(mac.Sum(nil))
}

// Env is the environment commands in the guest see, so apps and tools find
// the workspace's services without configuration. Every service listens in
// the guest, so these use localhost. secret is the workspace secret.
func Env(names []string, secret string) map[string]string {
	env := map[string]string{}
	for _, n := range names {
		secret := Password(secret, n)
		switch n {
		case "postgres":
			env["DATABASE_URL"] = "postgres://postgres:" + secret + "@localhost:5432/" + Database + "?sslmode=disable"
			env["PGHOST"] = "localhost"
			env["PGPORT"] = "5432"
			env["PGUSER"] = "postgres"
			env["PGPASSWORD"] = secret
			env["PGDATABASE"] = Database
		case "valkey":
			env["REDIS_URL"] = "redis://default:" + secret + "@localhost:6379/0"
			env["VALKEY_URL"] = env["REDIS_URL"]
			env["REDISCLI_AUTH"] = secret
		case "temporal":
			env["TEMPORAL_ADDRESS"] = "localhost:7233"
			env["TEMPORAL_NAMESPACE"] = "default"
		}
	}
	return env
}

// HostURL is how a tool on the host reaches a forwarded port. web says the
// port serves HTTP: those URLs use host, a per-workspace name under
// .localhost, which browsers resolve to loopback without DNS and which
// keeps the workspace's cookies apart from every other local app's.
// secret is the workspace secret.
//
// A URL is only good while the workspace runs: once it stops, another
// local user could listen on the port and collect the password.
func HostURL(name, host string, hostPort uint16, secret string, web bool) string {
	switch {
	case name == "postgres":
		return fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/%s?sslmode=disable", Password(secret, name), hostPort, Database)
	case name == "valkey":
		return fmt.Sprintf("redis://default:%s@127.0.0.1:%d/0", Password(secret, name), hostPort)
	case web:
		return fmt.Sprintf("http://%s.localhost:%d", host, hostPort)
	}
	return fmt.Sprintf("127.0.0.1:%d", hostPort)
}
