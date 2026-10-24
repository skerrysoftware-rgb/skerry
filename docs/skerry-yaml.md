# `skerry.yaml` reference

`skerry.yaml` sits at the root of a repository and says what each of its
workspaces needs. It is optional: without it, skerry detects services from
the files the project already has and installs dependencies from its
lockfile. Add one to choose explicitly, or to add what can't be detected
(runtimes other than Node, migrations, a seed).

```yaml
# What runs in the VM besides your code.
services: [postgres, valkey, temporal]   # [] turns detection off
runtimes: [python@3.12, go]              # Node is always there
packages: [ffmpeg, imagemagick]          # extra packages, by name

# Getting a workspace ready, once its data is new.
seed:
  postgres: db/seed.sql.gz               # loaded into an empty database first
setup:                                   # then these, in order
  - npm ci
  - npm run migrate
setup_watch: [db/schema.prisma]          # also re-run setup when these change

# Reaching the VM from your machine.
ports: [3000]
```

Every key is optional. Unknown keys are errors, so a typo doesn't go
unnoticed.

## What happens, in order

When a workspace starts (for a Claude Code session, as soon as the session
opens, in the background):

1. **Image.** skerry builds the VM image for the project's runtimes,
   services and packages, or reuses one: images are shared by every
   workspace that needs the same things, and kept for a week before being
   rebuilt with the newest packages. The first build for a project takes a
   minute or two; later ones are instant.
2. **Boot.** The VM starts (about half a second) and its services start.
   skerry waits until each one answers.
3. **Seed.** If `seed.postgres` is set and the database has no tables,
   skerry loads the dump, in one transaction. A database that already has
   data (from a snapshot, or an earlier setup) is left alone.
4. **Setup.** The `setup` commands run one at a time, in the order listed,
   each as `sh -c COMMAND` in the checkout, inside the VM, with the
   services' variables (`DATABASE_URL` and the rest) set. The first command
   that fails stops the rest; the failure is recorded, shown in
   `skerry status` and the status line, and not retried until something
   changes or you run `skerry setup`.
5. **Ready.** Commands that run in the VM go ahead.

None of this blocks a session. Claude (or you) can read and edit code and
use git while it happens. Only commands that run in the VM wait for it,
and while they wait they print each step. `skerry status` and the status
line show where it has got to, and `skerry logs setup` shows the setup
commands' output.

Steps 3 and 4 run when a workspace's data is new (a new workspace, or after
`skerry reset`), and again whenever setup's inputs change: the setup
commands, `skerry.yaml`, the project's lockfiles, common migration folders
(`migrations`, `db/migrate`, `prisma/migrations`, `alembic/versions`,
`supabase/migrations`) and anything in `setup_watch`. So after a
`git merge` or `git rebase` that brings new dependencies or migrations,
the next command runs setup first. `skerry setup` runs it again by hand.
Setup's own changes (`npm install` writing `package-lock.json`, say) don't
count: what it depends on is recorded after it finishes.
Setup commands should therefore be safe to repeat, as `npm ci` and
migration tools are.

## `services`

Postgres 17 with pgvector (`postgres`), Valkey, which speaks the Redis
protocol (`valkey`, or `redis`), and the Temporal dev server (`temporal`).
Without the key, skerry detects them from `package.json`, compose files,
`go.mod`, Python and Ruby manifests. See the README for what each sets up
and the variables commands get.

## `runtimes`

Language toolchains in the VM, as `name` or `name@version`:

| Runtime | Versions (default first) | Commands that run in the VM |
|---|---|---|
| `node` (always present) | 24, 20, 22, 26 | `node`, `npm`, `npx`, `corepack`, `yarn`, `pnpm` |
| `python` | 3.13, 3.11, 3.12, 3.14 | `python`, `python3`, `pip`, `pip3`, `uv`, `uvx` |
| `go` | 1.27, 1.24, 1.25, 1.26 | `go`, `gofmt` |
| `ruby` | 3.4, 3.2, 3.3 | `ruby`, `gem`, `bundle`, `irb`, `rake` |
| `java` | 21, 17, 25 | `java`, `javac`, `jar` |
| `bun` | latest | `bun`, `bunx` |
| `deno` | latest | `deno` |

In Claude Code sessions, a runtime's commands run in the VM only if the
project lists it; otherwise they run on your machine, as they would
without skerry. Once a workspace has had a runtime, its commands keep
running in the VM even if `skerry.yaml` stops listing it: code in the VM
can edit that file, and must not be able to move commands onto your
machine that way. A Python project's `.venv` is active for commands in the
VM.

A running VM keeps the image it started with. When `skerry.yaml` asks for
new runtimes, services or packages, skerry says so; they take effect when
the VM restarts (`skerry down`, then any command), and setup runs again
then.

## `packages`

Extra packages from the same signed repository the image is built from
(Wolfi), by name: `ffmpeg`, `imagemagick`, `postgresql-17-contrib`… Up to
32, and an image's packages may add up to 3 GB at most. Their install
scripts run in a build VM, never on your machine.

## `seed`

`seed.postgres` names a dump inside the repository: `.sql`, `.sql.gz`, or a
`pg_dump` custom-format `.dump`. It is read inside the VM. To load a dump
that isn't in the repository (an anonymized copy of production, say), use
`skerry seed FILE` from your machine, and `skerry snapshot save` to make it
the starting point of every new workspace.

## `setup`

Shell commands, run as described above. Without the key, skerry infers
how to install dependencies from the lockfiles it finds, for the runtimes
the project has:

| Found | Runs |
|---|---|
| `pnpm-lock.yaml` | `corepack pnpm install --frozen-lockfile` |
| `yarn.lock` (+ `.yarnrc.yml`) | `corepack yarn install --immutable` (or `--frozen-lockfile` for Yarn 1) |
| `bun.lock` (with the `bun` runtime) | `bun install --frozen-lockfile` |
| `package-lock.json` | `npm ci` |
| `package.json` only | `npm install` |
| `uv.lock` (with `python`) | `uv sync --frozen` |
| `requirements.txt` (with `python`) | `uv venv .venv` and `uv pip install -r requirements.txt` |
| `go.mod` (with `go`) | `go mod download` |
| `Gemfile` (with `ruby`) | `bundle install` into `vendor/bundle` |

Migrations are never guessed: list them yourself. `setup: []` turns setup
off (an empty `setup:` or `setup: ~` counts as absent, so setup is
inferred).

Dependencies install into the checkout (`node_modules`, `.venv`), which is
shared with your machine, so they are Linux builds: tools on your machine
that load native modules from them won't work. Keep your machine's own
copies elsewhere, or run those tools in the VM too.

## `setup_watch`

Paths in the repository (files or folders) whose change re-runs setup, on
top of the ones skerry always watches (listed above). For example, a
schema file a migration tool reads: `setup_watch: [db/schema.prisma]`.

## `ports`

Ports in the VM to forward to your machine's `127.0.0.1`, besides the
services' own, for example a dev server's. Up to 16. `skerry status` shows
where each one is.
