package project

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func monorepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"package.json":              `{"name":"root","workspaces":["packages/*"],"devDependencies":{"turbo":"2"}}`,
		"yarn.lock":                 "",
		".yarnrc.yml":               "nodeLinker: node-modules\n",
		"packages/api/package.json": `{"name":"@acme/api","scripts":{"dev":"tsx watch src/index.ts"},"dependencies":{"pg":"8","@acme/lib":"workspace:*"}}`,
		"packages/api/.env":         "SERVER_HTTP_PORT=5183\nDATABASE_DSN=postgresql://app_user:secret@localhost:5432/acme\n",
		"packages/web/package.json": `{"name":"@acme/web","scripts":{"dev":"vite"},"dependencies":{"@acme/ui":"workspace:*"}}`,
		"packages/web/.env":         "API_URL=http://localhost:5183\n",
		"packages/ui/package.json":  `{"name":"@acme/ui","scripts":{"dev":"rollup --watch"}}`,
		"packages/lib/package.json": `{"name":"@acme/lib","scripts":{"build":"tsc"}}`,
		"packages/mcp/package.json": `{"name":"@acme/mcp","scripts":{"dev":"tsx watch src/index.ts"},"dependencies":{"@temporalio/client":"1"}}`,
		"packages/mcp/.env.example": "PORT=5174\n",
	}
	for name, text := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Dir(path), filepath.Base(path), text)
	}
	return dir
}

func TestMonorepoDetection(t *testing.T) {
	p, err := Load(monorepo(t))
	if err != nil {
		t.Fatal(err)
	}
	// Services from the packages (and the .env URL), not the root.
	if !slices.Equal(p.Services, []string{"postgres", "temporal"}) {
		t.Errorf("services = %v (%s)", p.Services, p.Source)
	}
	var names []string
	for _, a := range p.Apps {
		names = append(names, a.Name)
	}
	// api and web are apps; ui is web's dev watcher; lib has no dev
	// script; mcp has only a .env.example, so it isn't set up locally.
	if strings.Join(names, " ") != "api web ui" {
		t.Fatalf("apps = %v", names)
	}
	api, web, ui := p.Apps[0], p.Apps[1], p.Apps[2]
	if api.Dir != "packages/api" || api.Command != "yarn dev" || !slices.Equal(api.Ports, []uint16{5183}) ||
		!slices.Equal(api.EnvFiles, []string{"packages/api/.env"}) {
		t.Errorf("api = %+v", api)
	}
	if !slices.Equal(web.Ports, []uint16{5173}) {
		t.Errorf("web (vite) ports = %v", web.Ports)
	}
	if len(ui.Ports) != 0 || !slices.Equal(ui.For, []string{"web"}) {
		t.Errorf("ui watcher = %+v", ui)
	}
	if len(p.Databases) != 1 || p.Databases[0] != (Database{Name: "acme", User: "app_user"}) {
		t.Errorf("databases = %+v", p.Databases)
	}
	// Setup installs, then builds what the apps import, with turbo.
	last := p.Setup[len(p.Setup)-1]
	if !strings.HasPrefix(last, "yarn turbo run build") || !strings.Contains(last, "'@acme/api^...'") {
		t.Errorf("setup = %q", p.Setup)
	}
}

func TestAppsConfig(t *testing.T) {
	dir := monorepo(t)
	write(t, dir, FileName, "apps:\n  - api\n  - name: worker\n    dir: packages/lib\n    command: yarn start\n")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Apps) != 2 || p.Apps[0].Name != "api" || p.Apps[1].Command != "yarn start" {
		t.Errorf("apps = %+v", p.Apps)
	}
	for name, text := range map[string]string{
		"unknown name":   "apps: [nope]\n",
		"escaping dir":   "apps:\n  - {name: x, dir: ../x, command: run}\n",
		"no command":     "apps:\n  - {name: x, dir: packages/lib}\n",
		"bad name":       "apps:\n  - {name: 'X Y', dir: packages/lib, command: run}\n",
		"duplicate name": "apps:\n  - api\n  - api\n",
	} {
		write(t, dir, FileName, text)
		if _, err := Load(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	write(t, dir, FileName, "apps: []\n")
	if p, err := Load(dir); err != nil || len(p.Apps) != 0 {
		t.Errorf("apps: [] gave %v, %v", p.Apps, err)
	}
}

func TestParseEnv(t *testing.T) {
	env := ParseEnv([]byte("# comment\nexport A=1\nB=\"two words\"\nC='raw $X'\nD=plain # note\nE=\n bad line\n"))
	want := map[string]string{"A": "1", "B": "two words", "C": "raw $X", "D": "plain", "E": ""}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if len(env) != len(want) {
		t.Errorf("parsed %v", env)
	}
}

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Dir(path), filepath.Base(path), text)
	}
	return dir
}

func TestWiringAndMissingEnv(t *testing.T) {
	dir := monorepo(t)
	write(t, filepath.Join(dir, "packages/web"), ".env", "API_URL=http://localhost:5183\nAUTH_URL=http://127.0.0.1:4000/x\n")
	write(t, filepath.Join(dir, "packages/web"), ".env.example", "API_URL=\nAUTH_URL=\nSTRIPE_KEY=\n")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	api, web := p.Apps[0], p.Apps[1]
	if got := p.Wiring(api); len(got) != 1 || got[0] != (Wire{Key: "DATABASE_DSN", Port: 5432, To: "postgres"}) {
		t.Errorf("api wiring = %+v", got)
	}
	want := []Wire{{Key: "API_URL", Port: 5183, To: "api"}, {Key: "AUTH_URL", Port: 4000}}
	if got := p.Wiring(web); !slices.Equal(got, want) {
		t.Errorf("web wiring = %+v", got)
	}
	if got := p.MissingEnv(web); !slices.Equal(got, []string{"STRIPE_KEY"}) {
		t.Errorf("web missing = %v", got)
	}
}

func TestDevScriptFiltersAndPorts(t *testing.T) {
	dir := tree(t, map[string]string{
		"package.json":           `{"name":"root","workspaces":["apps/*"],"scripts":{"dev":"npm run gen && turbo run dev --filter=@x/main","gen":"lingui compile"},"devDependencies":{"turbo":"2"}}`,
		"package-lock.json":      "{}",
		".env":                   "PORT=3000\n",
		"apps/main/package.json": `{"name":"@x/main","scripts":{"dev":"npm run with:env -- react-router dev","with:env":"dotenv -e ../../.env --"}}`,
		"apps/docs/package.json": `{"name":"@x/docs","scripts":{"dev":"next dev"}}`,
		"apps/api/package.json":  `{"name":"@x/api","scripts":{"dev":"next dev -p 3003"}}`,
	})
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Apps) != 1 || p.Apps[0].Name != "main" || !slices.Equal(p.Apps[0].Ports, []uint16{3000}) ||
		!slices.Equal(p.Apps[0].EnvFiles, []string{".env"}) {
		t.Fatalf("apps = %+v", p.Apps)
	}
	other := map[string]App{}
	for _, a := range p.OtherApps {
		other[a.Name] = a
	}
	if !slices.Equal(other["api"].Ports, []uint16{3003}) {
		t.Errorf("api = %+v", other["api"])
	}
	// docs would take main's port: main starts, so it keeps it.
	if len(other["docs"].Ports) != 0 || !slices.Equal(other["docs"].Taken, []uint16{3000}) {
		t.Errorf("docs = %+v", other["docs"])
	}
	if !strings.Contains(p.Setup[len(p.Setup)-1], "--filter='@x/main^...'") || strings.Contains(p.Setup[len(p.Setup)-1], "docs") {
		t.Errorf("build = %q", p.Setup[len(p.Setup)-1])
	}
	// The dev script's own first step runs in setup, before the build.
	if i := slices.Index(p.Setup, "npm run gen"); i < 0 || i != len(p.Setup)-2 {
		t.Errorf("setup = %q", p.Setup)
	}
	// Naming one in skerry.yaml runs it.
	write(t, dir, FileName, "apps: [docs]\n")
	if p, err = Load(dir); err != nil || len(p.Apps) != 1 || p.Apps[0].Name != "docs" {
		t.Errorf("named = %+v, %v", p.Apps, err)
	}
}

func TestProcfileAndFrameworkApps(t *testing.T) {
	dir := tree(t, map[string]string{
		"pyproject.toml": "[project]\nrequires-python = \"==3.12.*\"\ndependencies = [\"psycopg\"]\n",
		"manage.py":      "",
		"Makefile":       "run:\n\tpython -m manage runserver 0.0.0.0:8000\n\nother:\n\techo\n",
		"Procfile":       "release: python manage.py migrate\nweb: make run\nworker: celery -A app worker\n",
	})
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Runtimes) != 2 || p.Runtimes[1].String() != "python@3.12" {
		t.Errorf("runtimes = %v (%s)", p.Runtimes, p.RuntimesSource)
	}
	if len(p.Apps) != 2 || p.Apps[0].Name != "web" || !slices.Equal(p.Apps[0].Ports, []uint16{8000}) ||
		p.Apps[0].Command != "export PORT=5000; make run" || p.Apps[1].Name != "worker" || len(p.Apps[1].Ports) != 0 {
		t.Errorf("apps = %+v", p.Apps)
	}
	// Without a Procfile, Django's own dev server.
	if err := os.Remove(filepath.Join(dir, "Procfile")); err != nil {
		t.Fatal(err)
	}
	if p, err = Load(dir); err != nil || len(p.Apps) != 1 || p.Apps[0].Command != "python manage.py runserver" {
		t.Errorf("apps = %+v, %v", p.Apps, err)
	}

	rails := tree(t, map[string]string{
		"Gemfile": "gem \"rails\"\ngem \"pg\"\n", ".ruby-version": "3.3.6\n", "bin/rails": "",
		"Procfile.dev": "web: bin/rails server -p 3000\ncss: bin/rails tailwindcss:watch\n",
		"bin/dev":      "#!/usr/bin/env sh\nexport PORT=\"${PORT:-3000}\"\n",
	})
	if p, err = Load(rails); err != nil {
		t.Fatal(err)
	}
	if p.Runtimes[1].String() != "ruby@3.3" || len(p.Apps) != 2 || !slices.Equal(p.Apps[0].Ports, []uint16{3000}) ||
		p.Apps[1].Command != "export PORT=3100; bin/rails tailwindcss:watch" {
		t.Errorf("rails = %v %+v", p.Runtimes, p.Apps)
	}
}

func TestRuntimesKeyWins(t *testing.T) {
	dir := tree(t, map[string]string{"go.mod": "module x\n\ngo 1.25.1\n", FileName: "runtimes: []\n"})
	p, err := Load(dir)
	if err != nil || len(p.Runtimes) != 1 {
		t.Errorf("runtimes = %v, %v", p.Runtimes, err)
	}
	write(t, dir, FileName, "")
	if p, err = Load(dir); err != nil || len(p.Runtimes) != 2 || p.Runtimes[0].String() != "go@1.25" {
		t.Errorf("runtimes = %v, %v", p.Runtimes, err)
	}
}

func TestPortAliases(t *testing.T) {
	dir := tree(t, map[string]string{
		"package.json": `{"name":"x","scripts":{"dev":"next dev"},"dependencies":{"next":"15"}}`,
		".env":         "DATABASE_URL=postgres://u:p@127.0.0.1:54320/app\nREDIS_URL=redis://localhost:63790\nCACHE=redis://localhost:6379\n",
	})
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []PortAlias{{54320, "postgres"}, {63790, "valkey"}}
	if !slices.Equal(p.PortAliases, want) || !slices.Equal(p.Services, []string{"postgres", "valkey"}) {
		t.Errorf("aliases = %v, services = %v", p.PortAliases, p.Services)
	}
	if !slices.Equal(p.Databases, []Database{{Name: "app", User: "u"}}) {
		t.Errorf("databases = %v", p.Databases)
	}
	for _, w := range p.Wiring(p.Apps[0]) {
		if w.To == "" {
			t.Errorf("unwired: %+v", w)
		}
	}
}

func TestDetectionEdgeCases(t *testing.T) {
	load := func(files map[string]string) Project {
		t.Helper()
		p, err := Load(tree(t, files))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	ports := func(a App) []uint16 { return a.Ports }

	// A PORT= prefix names the port; vite build --watch is a library's
	// watcher, not a dev server; a glob filter on folders selects apps.
	p := load(map[string]string{
		"package.json":          `{"name":"r","workspaces":["apps/*","libs/*"],"scripts":{"dev":"turbo run dev --filter=./apps/*"},"devDependencies":{"turbo":"2"}}`,
		"package-lock.json":     "{}",
		"apps/web/package.json": `{"name":"web","scripts":{"dev":"PORT=3001 next dev"},"dependencies":{"ui":"*"}}`,
		"libs/ui/package.json":  `{"name":"ui","scripts":{"dev":"vite build --watch","build":"vite build"}}`,
	})
	if len(p.Apps) != 2 || p.Apps[0].Name != "web" || !slices.Equal(ports(p.Apps[0]), []uint16{3001}) ||
		p.Apps[1].Name != "ui" || len(p.Apps[1].For) != 1 {
		t.Errorf("apps = %+v", p.Apps)
	}

	// Yarn without turbo builds each dependency with its own script.
	p = load(map[string]string{
		"package.json":           `{"name":"r","workspaces":["apps/*","libs/*"]}`,
		"yarn.lock":              "",
		"apps/web/package.json":  `{"name":"web","scripts":{"dev":"vite"},"dependencies":{"ui":"*"}}`,
		"libs/ui/package.json":   `{"name":"ui","scripts":{"build":"tsc"},"dependencies":{"core":"*"}}`,
		"libs/core/package.json": `{"name":"core","scripts":{"build":"tsc"}}`,
	})
	if got := p.Setup[len(p.Setup)-1]; got != "yarn workspace 'core' run build && yarn workspace 'ui' run build" {
		t.Errorf("build = %q", got)
	}

	// Ports go to the apps that run: naming site gives it 3000.
	dir := tree(t, map[string]string{
		"package.json":            `{"name":"r","workspaces":["apps/*"]}`,
		"apps/admin/package.json": `{"name":"admin","scripts":{"dev":"next dev"}}`,
		"apps/site/package.json":  `{"name":"site","scripts":{"dev":"next dev"}}`,
		FileName:                  "apps: [site]\n",
	})
	if p, _ = Load(dir); len(p.Apps) != 1 || !slices.Equal(ports(p.Apps[0]), []uint16{3000}) {
		t.Errorf("apps = %+v", p.Apps)
	}

	// Same folder names get distinct app names.
	p = load(map[string]string{
		"package.json":              `{"name":"r","workspaces":["apps/*","services/*"]}`,
		"apps/api/package.json":     `{"name":"a","scripts":{"dev":"next dev"}}`,
		"services/api/package.json": `{"name":"b","scripts":{"dev":"next dev -p 4000"}}`,
	})
	if len(p.Apps) != 2 || p.Apps[0].Name != "api" || p.Apps[1].Name != "services-api" {
		t.Errorf("apps = %+v", p.Apps)
	}

	// A production Procfile doesn't run beside the dev script.
	p = load(map[string]string{
		"package.json": `{"name":"r","scripts":{"dev":"nodemon server.js"}}`,
		".env":         "PORT=4000\n",
		"Procfile":     "web: npm start\nworker: node worker.js\n",
	})
	if len(p.Apps) != 1 || p.Apps[0].Name != "app" {
		t.Errorf("apps = %+v", p.Apps)
	}

	// services: [] keeps Postgres out, env URLs or not; apps: (null) is
	// no key.
	p = load(map[string]string{
		".env":   "DATABASE_URL=postgres://u@localhost/app\n",
		FileName: "services: []\napps:\n",
	})
	if len(p.Services) != 0 || len(p.Databases) != 0 {
		t.Errorf("services = %v, databases = %v", p.Services, p.Databases)
	}

	// The lower bound of requires-python.
	p = load(map[string]string{"pyproject.toml": "[project]\nrequires-python = \"<3.13,>=3.11\"\n"})
	if p.Runtimes[1].String() != "python@3.11" {
		t.Errorf("runtimes = %v", p.Runtimes)
	}
}

func TestWorkspacePatternFIFODoesNotBlock(t *testing.T) {
	dir := tree(t, map[string]string{"package.json": `{"name":"r","workspaces":["pipe"]}`})
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Skip(err)
	}
	done := make(chan struct{})
	go func() { _, _ = Load(dir); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}

func TestLargeProcfileAndMakefileAreCheap(t *testing.T) {
	procfile := strings.Repeat("_: make a\n", 50000)
	makefile := strings.Repeat("a:\n\techo hi\n", 40000)
	dir := tree(t, map[string]string{"Procfile": procfile, "Makefile": makefile})
	start := time.Now()
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Load took %s", d)
	}
}

func TestAppNamesForStart(t *testing.T) {
	p, err := Load(monorepo(t))
	if err != nil {
		t.Fatal(err)
	}
	// All of them, with web's watcher.
	if got, err := p.AppNames(nil); err != nil || strings.Join(got, " ") != "api web ui" {
		t.Errorf("all = %v, %v", got, err)
	}
	if got, err := p.AppNames([]string{"web"}); err != nil || strings.Join(got, " ") != "web ui" {
		t.Errorf("web = %v, %v", got, err)
	}
	if got, err := p.AppNames([]string{"api"}); err != nil || strings.Join(got, " ") != "api" {
		t.Errorf("api = %v, %v", got, err)
	}
	if _, err := p.AppNames([]string{"nope"}); err == nil {
		t.Error("unknown app accepted")
	}
}

func TestComposeDatabases(t *testing.T) {
	p, err := Load(tree(t, map[string]string{
		"docker-compose.yml": "services:\n  db:\n    image: postgres:17\n    environment:\n      - POSTGRES_USER=shop\n      - POSTGRES_DB=shop_dev\n" +
			"  other:\n    image: pgvector/pgvector:pg17\n    environment:\n      POSTGRES_DB: ${DB:-analytics}\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []Database{{Name: "shop_dev", User: "shop"}, {Name: "analytics", User: "postgres"}}
	for _, w := range want {
		if !slices.Contains(p.Databases, w) {
			t.Errorf("databases = %v, want %v", p.Databases, w)
		}
	}
}

func TestEnvShapeDropsSecrets(t *testing.T) {
	got := envShape(map[string]string{
		"PORT":         "5183",
		"DATABASE_URL": "postgres://shop:hunter2@localhost:5432/shop?sslmode=disable",
		"API_URL":      "localhost:4000",
		"STRIPE_KEY":   "sk_live_abc",
		"REMOTE_DB":    "postgres://u:pw@db.example.com/x",
	})
	want := map[string]string{
		"PORT": "5183", "DATABASE_URL": "postgres://shop@localhost:5432/shop", "API_URL": "localhost:4000",
		"STRIPE_KEY": "", "REMOTE_DB": "",
	}
	if !maps.Equal(got, want) {
		t.Errorf("envShape = %v, want %v", got, want)
	}
}
