package project

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigWins(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"pg":"8"}}`)
	write(t, dir, FileName, "services: [redis, temporal]\nports: [3000, 3000, 5173]\n")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Services, []string{"temporal", "valkey"}) || p.Source != FileName {
		t.Errorf("services = %v from %q", p.Services, p.Source)
	}
	if !slices.Equal(p.Ports, []uint16{3000, 5173}) {
		t.Errorf("ports = %v", p.Ports)
	}
}

func TestSetupAndSeed(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"pg":"8"}}`)
	write(t, dir, FileName, "setup:\n  - npm ci\n  - npm run migrate\nseed:\n  postgres: db/./seed.sql.gz\n")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Setup, "|") != "npm ci|npm run migrate" || p.SeedPostgres != "db/seed.sql.gz" {
		t.Errorf("got setup %q, seed %q", p.Setup, p.SeedPostgres)
	}
	if !slices.Equal(p.Services, []string{"postgres"}) {
		t.Errorf("detection stopped working with a config that names no services: %v", p.Services)
	}
	for name, text := range map[string]string{
		"absolute seed":    "services: [postgres]\nseed: {postgres: /etc/passwd.sql}\n",
		"escaping seed":    "services: [postgres]\nseed: {postgres: ../secrets.sql}\n",
		"other extension":  "services: [postgres]\nseed: {postgres: db/seed.sh}\n",
		"seed without pg":  "services: []\nseed: {postgres: seed.sql}\n",
		"empty setup step": "setup: ['']\n",
	} {
		dir := t.TempDir()
		write(t, dir, FileName, text)
		if _, err := Load(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEmptyServicesTurnsDetectionOff(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"pg":"8"}}`)
	write(t, dir, FileName, "services: []\n")
	p, err := Load(dir)
	if err != nil || len(p.Services) != 0 {
		t.Errorf("services = %v, %v; want none", p.Services, err)
	}
}

func TestConfigRejects(t *testing.T) {
	for name, text := range map[string]string{
		"unknown service": "services: [mysql]\n",
		"unknown key":     "services: [postgres]\ncommand: rm -rf /\n",
		"bad port":        "ports: [70000]\n",
		"zero port":       "ports: [0]\n",
	} {
		dir := t.TempDir()
		write(t, dir, FileName, text)
		if _, err := Load(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDetect(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"pg":"8","@temporalio/worker":"1"},"devDependencies":{"ioredis":"5"}}`)
	write(t, dir, "docker-compose.yml", "services:\n  db:\n    image: pgvector/pgvector:pg17\n")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Services, []string{"postgres", "temporal", "valkey"}) {
		t.Errorf("services = %v", p.Services)
	}
	if p.Source != "detected from package.json, docker-compose.yml" {
		t.Errorf("source = %q", p.Source)
	}
}

func TestDetectAvoidsLookalikes(t *testing.T) {
	cases := map[string]string{
		"docker-compose.yml": "services:\n  m:\n    image: oliver006/redis_exporter:v1\n  p:\n    image: prometheuscommunity/postgres-exporter\n  c:\n    image: rediscommander/redis-commander\n  r:\n    image: postgrest/postgrest\n",
		"go.mod":             "module x\n\nrequire (\n\tgithub.com/lib/pq v1.10.9 // indirect\n\tgithub.com/golang-migrate/migrate/v4 v4.17.0\n)\n",
		"requirements.txt":   "fakeredis==2.0\ntypes-redis\n# redis is not used\n",
		"pyproject.toml":     "[project]\ndescription = \"talks about redis and postgres\"\nkeywords = [\"redis\", \"asyncpg\"]\ndependencies = [\"fakeredis>=2\"]\n\n[tool.other]\nredis = true\n",
	}
	for file, text := range cases {
		dir := t.TempDir()
		write(t, dir, file, text)
		if names, _ := Detect(dir); len(names) != 0 {
			t.Errorf("%s: detected %v", file, names)
		}
	}
}

func TestDetectEachEcosystem(t *testing.T) {
	cases := map[string][2]string{
		"go.mod":           {"module x\n\nrequire github.com/jackc/pgx/v5 v5.7.0\n\nrequire (\n\tgo.temporal.io/sdk v1.30.0\n)\n", "postgres temporal"},
		"requirements.txt": {"redis[hiredis]>=5\nPsycopg2-Binary==2.9\n", "postgres valkey"},
		"pyproject.toml":   {"[project]\nname = \"x\"\ndependencies = [\n  \"fastapi[standard]\",  # see [docs]\n  \"temporalio>=1.7\",\n  \"asyncpg\",\n]\n\n[tool.poetry.group.dev.dependencies]\nredis = \"^5\"\n", "postgres temporal valkey"},
		"Gemfile":          {"source 'https://rubygems.org'\ngem 'pg', '~> 1.5'\ngem \"redis\"\n", "postgres valkey"},
		"compose.yaml":     {"services:\n  db:\n    image: postgres:17\n  cache:\n    image: docker.io/valkey/valkey:8\n  t:\n    image: temporalio/auto-setup:1.25\n", "postgres temporal valkey"},
	}
	for file, c := range cases {
		dir := t.TempDir()
		write(t, dir, file, c[0])
		names, _ := Detect(dir)
		if got := strings.Join(names, " "); got != c[1] {
			t.Errorf("%s: detected %q, want %q", file, got, c[1])
		}
	}
}

func TestDetectIgnoresSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "package.json")
	write(t, filepath.Dir(outside), "package.json", `{"dependencies":{"pg":"8"}}`)
	if err := os.Symlink(outside, filepath.Join(dir, "package.json")); err != nil {
		t.Fatal(err)
	}
	if names, _ := Detect(dir); len(names) != 0 {
		t.Errorf("followed a symlink: %v", names)
	}
}

func TestNothingToDetect(t *testing.T) {
	p, err := Load(t.TempDir())
	if err != nil || len(p.Services) != 0 || p.Source != "" {
		t.Errorf("got %+v, %v", p, err)
	}
}

func TestRuntimesPackagesAndDefaultSetup(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"x"}`)
	write(t, dir, "pnpm-lock.yaml", "lockfileVersion: 9\n")
	write(t, dir, "uv.lock", "version = 1\n")
	write(t, dir, FileName, "runtimes: [python@3.12, go]\npackages: [ffmpeg, ffmpeg]\n")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range p.Runtimes {
		got = append(got, r.String())
	}
	if strings.Join(got, " ") != "go@1.27 node@24 python@3.12" {
		t.Errorf("runtimes = %v", got)
	}
	if strings.Join(p.Packages, " ") != "ffmpeg" {
		t.Errorf("packages = %v", p.Packages)
	}
	if len(p.Setup) != 2 || !strings.Contains(p.Setup[0], "pnpm install --frozen-lockfile") || p.Setup[1] != "uv sync --frozen" {
		t.Errorf("default setup = %q (%s)", p.Setup, p.SetupSource)
	}
	for name, text := range map[string]string{
		"unknown runtime": "runtimes: [cobol]\n",
		"bad version":     "runtimes: [python@2.7]\n",
		"bad package":     "packages: ['../x']\n",
		"escaping watch":  "setup_watch: [../other]\n",
	} {
		d := t.TempDir()
		write(t, d, FileName, text)
		if _, err := Load(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// setup: [] turns the inferred setup off.
	write(t, dir, FileName, "setup: []\n")
	if p, err := Load(dir); err != nil || len(p.Setup) != 0 {
		t.Errorf("setup: [] gave %q, %v", p.Setup, err)
	}
}

func TestSetupFingerprintFollowsChanges(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{}`)
	write(t, dir, "package-lock.json", "{}")
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "migrations/001.sql", "create table a();")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := p.SetupFingerprint(dir)
	if again := p.SetupFingerprint(dir); again != before {
		t.Fatal("fingerprint isn't stable")
	}
	// A merge brings a new migration.
	write(t, dir, "migrations/002.sql", "alter table a add b int;")
	if p.SetupFingerprint(dir) == before {
		t.Error("a new migration didn't change the fingerprint")
	}
	// Unrelated edits don't.
	after := p.SetupFingerprint(dir)
	write(t, dir, "README.md", "hello")
	if p.SetupFingerprint(dir) != after {
		t.Error("an unrelated file changed the fingerprint")
	}
}
