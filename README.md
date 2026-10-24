# skerry

**An isolated, lightweight Linux VM for every checkout: parallel work that
can't collide, and code that can't reach your Mac.**

skerry is for developers who work on several branches at once, and for coding
agents like Claude Code that run many sessions side by side. It does two
things:

- **Parallel work without collisions.** Each checkout (a git worktree) gets
  its own small, fast VM with its own processes, ports, filesystem and
  services: its own Postgres (with pgvector), Valkey (Redis) and Temporal,
  with data that survives restarts. Two branches can run different
  migrations against "the" database at the same time, and neither notices
  the other.
- **Isolation you can rely on.** Everything a workspace runs (dependency
  install scripts, repository scripts, commands an AI agent decides to run)
  runs inside the VM, not on your Mac. Code in a workspace:
  - can't read files on your Mac outside its own checkout
  - can reach the internet (package registries, APIs) but not your Mac,
    your local network, other workspaces or cloud metadata
  - can't modify the system image it runs from (shared read-only)
  - can't redirect your git through the checkout: the worktree's `.git`
    pointer is locked, and planted repositories or Claude Code settings are
    quarantined after every command and every 15 seconds

  Each of these is tested by attacking a real VM from the inside
  ([scripts/escape-test.sh](scripts/escape-test.sh)), and the parsers that
  read VM or repository input are fuzzed. Creating a
  worktree also never runs the repository's git filter drivers, or its hooks
  unless you opt in. What isn't covered yet is listed under
  [Security](#security).

![Two live Claude Code sessions side by side, each in its own skerry workspace: one installs, migrates and places an order on main; the other merges the gift-notes branch, applies its extra migration and places an order with a gift note. Each has its own Postgres and Temporal, so both get order #1.](docs/demo/demo.gif)

*Two real Claude Code sessions, recorded live with
[asciinema](docs/demo/demo.cast) (idle time trimmed to 2 s; nothing else
edited). Each runs in its own skerry workspace with its own Postgres and
Temporal: both install, migrate and place order #1, and the second one's
branch adds a column the first one's database never sees.*

> **Status: early development, macOS on Apple Silicon.** Workspaces, Claude
> Code integration, per-workspace VMs, networking and services work today.
> Linux hosts are next. See
> [What works today](#what-works-today).

---

## The problem

Git worktrees give each branch its own files, but everything else is shared:

```console
$ node server.js          # worktree A
listening on :3000
$ node server.js          # worktree B, at the same time
Error: listen EADDRINUSE: address already in use :::3000
```

The same goes for the database (one branch's migration breaks another's), for
Redis keys, background workers and `/tmp`. With five Claude sessions in five
worktrees, they all fight over `localhost`.

## What skerry does

Two branches of the same project, each in its own skerry workspace. Neither
has a `skerry.yaml`: skerry saw `pg` and `@temporalio/*` in `package.json`.

```console
checkout $ skerry exec -- npm run -s migrate        # branch main
applied 001_orders.sql
orders(id, item, status, created_at)

gifts $ skerry exec -- npm run -s migrate           # branch gift-notes
applied 001_orders.sql
applied 002_gift_notes.sql
orders(id, item, status, created_at, gift_note)

gifts $ skerry exec -- npm run -s order -- teapot 'Happy birthday!'
order #1: teapot (gift note: "Happy birthday!")
  reserved
  payment timed out, retrying
  paid
  shipped
workflow order-1 completed
```

Each workspace ran its own migrations against its own Postgres, and its own
Temporal server ran the workflow (and retried the failed step). The demo
project is in [docs/demo](docs/demo).

- **Real isolation, still light.** Each workspace is a Linux VM on Apple's
  Virtualization framework. It boots in about half a second, and commands
  start in it in about 50 ms. With Postgres and Temporal, it is ready in
  about six seconds.
- **Services, per workspace.** Postgres 17 with pgvector, Valkey (a
  Redis-compatible fork) and the Temporal dev server, each with its data on
  a disk of its own that survives restarts. Commands in the VM get
  `DATABASE_URL`, `REDIS_URL` and `TEMPORAL_ADDRESS`, so apps connect
  without configuration; `skerry status` shows where each is on your Mac.
- **Your files stay where they are.** The checkout is shared into the VM at
  the *same path* as on your Mac, so your editor, git and Claude's file tools
  work on the host as usual, and paths in errors and stack traces match.
- **Commands go to the right place automatically.** In Claude Code
  sessions, `node`, `npm`, `npx`, `yarn`, `pnpm` and `corepack` run in the
  VM. Everything else, git included, runs on your Mac with your keys and
  settings. In a terminal, `skerry exec -- CMD` runs anything in the VM.
- **Built for Claude Code.** skerry plugs into Claude Code's worktree hooks.
  Every worktree Claude creates (CLI `--worktree`, desktop app, subagents,
  background sessions) becomes a workspace, and Claude is told how it works.
- **Opt-in.** skerry only acts in folders you enable. Everywhere else,
  worktrees stay plain worktrees, made exactly as Claude Code makes them.

The design, threat model and measurements are in
[docs/design.md](docs/design.md).

## What works today

| | |
|---|---|
| ✅ Workspaces: create, list, remove (refuses to lose uncommitted or unpushed work) | |
| ✅ Opt-in per folder; plain worktrees everywhere else | |
| ✅ Claude Code hooks: CLI, desktop app, subagents, background sessions | verified |
| ✅ One VM per workspace on macOS (Apple Silicon) | ~0.5 s boot |
| ✅ Commands in the VM: `skerry exec`, and shims for Node tooling | ~50 ms per command |
| ✅ Networking: internet yes; your Mac, LAN and cloud metadata no | |
| ✅ Services: Postgres 17 + pgvector, Valkey, Temporal; data kept per workspace | ready in ~6 s |
| ✅ Detecting services from `package.json`, compose files, `go.mod`, Python files; `skerry.yaml` overrides | |
| ✅ Images built only from signature-verified Wolfi and Alpine packages | |
| ✅ Seeding from a dump, setup commands (migrations), snapshots cloned per workspace | |
| ✅ Hot reload: file changes on your Mac reach watchers in the VM | |
| ✅ Idle VMs stop by themselves; CPU and memory limits | |
| 🚧 Linux hosts; cloud and self-hosted runners | |

## Requirements

- macOS on Apple Silicon (built and tested on macOS 27)
- Go 1.27+ and git, to build
- Network access the first time an image is built: packages come from
  Wolfi (packages.wolfi.dev) and the kernel from Alpine. About 100 MB, more
  with services.

## Build

```bash
make build
```

This builds the guest agent for Linux, embeds it in `bin/skerry`, and signs
`bin/skerry` with the `com.apple.security.virtualization` entitlement so it
can start VMs. A plain `go build` produces a binary without the agent or the
signature.

## Try it

```bash
# 1. Enable skerry for a folder of projects (asks for confirmation)
bin/skerry enable ~/src/myproject

# 2. Make a workspace
cd ~/src/myproject
bin/skerry create try-it                 # prints the worktree path
cd .claude/worktrees/try-it

# 3. Run something in its VM (the first run builds the image: a minute or two)
bin/skerry exec -- uname -a
bin/skerry exec -- npm install

# 4. See its services, if it has any, and reach them from your Mac
bin/skerry status
psql "$(bin/skerry url postgres)"

# 5. Stop and remove it (removing deletes its data too)
bin/skerry down try-it
bin/skerry rm try-it
```

## Use it with Claude Code

Claude Code can run a session in its own git worktree: a separate checkout of
the repository, on its own branch, so several sessions can work on different
tasks without overwriting each other's files. From the terminal that is
`claude --worktree <name>` (the worktree lands in `.claude/worktrees/<name>`);
in the desktop app, turn on **Worktree** when starting a session. skerry
hooks into exactly that moment: in an enabled folder, the worktree also gets
its own VM, so sessions stop colliding on ports, processes and databases too.

```bash
bin/skerry claude install
```

This adds three hooks to `~/.claude/settings.json`: `WorktreeCreate`,
`WorktreeRemove` and `SessionStart`. It shows them first, keeps a backup, and
asks which folders to enable. Then start Claude in a worktree as usual:

```bash
claude --worktree fix-login
```

or turn on **Worktree** for a new session in the desktop app. Inside an
enabled folder, the session gets its own workspace, its VM starts in the
background, and Claude is told how the workspace works. Outside enabled
folders, nothing changes.

Install the hooks at user level, not in a project: inside a worktree, Claude
Code reads the project settings committed in that worktree, so project-level
hooks don't follow sessions there. `bin/skerry claude uninstall` removes them.

**Or install the Claude Code plugin**, which carries the same three hooks
(skerry itself must still be installed and on your `PATH`):

```text
/plugin marketplace add skerrysoftware-rgb/skerry
/plugin install skerry@skerry
```

Use one or the other, not both: two `WorktreeCreate` hooks would both try
to make the worktree.

## Services and `skerry.yaml`

skerry looks at the files a project already has and starts what it uses:

| Service | Started when the project has… | In the VM |
|---|---|---|
| `postgres`: PostgreSQL 17 with pgvector | `pg`, `postgres`, `pg-promise` (npm); `pgx`, `lib/pq` (Go); `psycopg`, `asyncpg` (Python); `pg` (Ruby); a `postgres`/`pgvector` image in a compose file | `DATABASE_URL`, `PG*`; database `app` with `vector` installed |
| `valkey`: Valkey 8 (Redis-compatible) | `redis`, `ioredis`, `bullmq`, `iovalkey` (npm); `go-redis`, `valkey-go`; `redis` (Python, Ruby); a `redis`/`valkey` image | `REDIS_URL`, `VALKEY_URL` |
| `temporal`: Temporal dev server and UI | `@temporalio/client` or `/worker`; `go.temporal.io/sdk`; `temporalio` (Python); a `temporalio/*` image | `TEMPORAL_ADDRESS` |

To choose explicitly, add a `skerry.yaml` at the repository root:

```yaml
services: [postgres, temporal]   # [] turns detection off
ports: [3000]                    # also forward these ports from the VM
```

## Seeding and setup

A workspace can start with real data, migrated for its own branch:

```yaml
seed:
  postgres: db/seed.sql.gz   # .sql, .sql.gz or a pg_dump .dump, in the repository
setup:                       # run in the VM, in the checkout, when the data is new
  - npm ci
  - npm run migrate
```

When a workspace's data is new, skerry loads the seed (only into an empty
database), then runs the setup commands, and records the result
(`skerry status`, `skerry logs setup`). Commands that need the workspace
wait for setup to finish. `skerry setup` runs it again, for example after
pulling new migrations.

Dumps that aren't in the repository, such as an anonymized copy of
production, load from your Mac: `skerry reset --empty`, then
`skerry seed ~/dumps/prod.dump`. To avoid loading a large dump into every
workspace, load it once and save a snapshot:

```bash
skerry seed ~/dumps/prod.dump    # in one workspace; setup then migrates it
skerry snapshot save             # saves it as the repository's default
```

Every new workspace of the repository then starts from the snapshot, as
an APFS clone that is instant and takes no space until it changes, and
runs its own branch's setup on top. `skerry reset` returns a workspace to
the snapshot; `skerry reset --empty` to nothing. `skerry snapshot ls` and
`skerry snapshot rm NAME` manage them.

The variables are set in the environment of every command in the VM, so
they take precedence over the same names in a `.env` file: dotenv loaders
don't override variables that are already set.

A repository can only pick from these presets: it can't add packages,
daemons or commands to the image. Each service runs as its own unprivileged
user and listens only inside the VM, with its own password, generated per
workspace. Its data lives on a disk of
its own (a sparse file: it takes only the space used) that survives restarts
and is deleted with the workspace; `skerry reset` empties it.

Postgres and Valkey, and every port in `ports`, are forwarded to
`127.0.0.1` on your Mac, at a port chosen once per workspace and kept
across restarts, and reach the VM over skerry's own connection to it, so
servers that listen only on `localhost` in the VM work too. `skerry status`
lists them; `skerry url SERVICE` prints a URL, password included, for tools
such as `psql` or a database GUI. Use a URL only while the workspace runs:
when it is stopped, another local user could take the port. Web
ports get a URL such as `http://checkout.localhost:53165`: browsers resolve
`*.localhost` to your Mac without DNS, and the name keeps the workspace's
cookies apart from other local apps'. Temporal has no password, so its
ports are forwarded only if you list them (`ports: [8233]` for the UI).

## Hot reload, idle stop and the status line

Watchers in a VM (dev servers, test runners) see edits you make on your
Mac: skerry watches the checkout and raises a file event in the VM for
each changed file (for a new or deleted file, an event on its directory). Changes under `.git`, `node_modules`, `dist`, `build` and similar
output directories aren't forwarded.

A workspace that nothing has used for 30 minutes (no command running
through skerry, no forwarded connection) stops its VM; the next command
starts it again. Its data stays. Forwarded ports don't start it: after an
idle stop, run `skerry up` before connecting from a database tool.

To see the workspace in Claude Code's status line, add this to
`~/.claude/settings.json`:

```json
"statusLine": { "type": "command", "command": "skerry statusline" }
```

It prints, for example, `skerry checkout: running · postgres, temporal`,
and nothing outside skerry workspaces.

## Commands

| Command | What it does |
|---|---|
| `skerry enable [DIR]` / `disable [DIR]` | Turn skerry on or off for a folder and everything under it |
| `skerry enable --list` | Show enabled folders |
| `skerry create NAME [--base BRANCH]` | Make a workspace (worktree on branch `worktree-NAME`, from the default branch or `BRANCH`) and print its path |
| `skerry ls` | List workspaces and their mode (`skerry` or `plain`) |
| `skerry up [NAME]` / `down [NAME]` | Start or stop a workspace's VM |
| `skerry exec [-w NAME] -- CMD…` | Run a command in the workspace's VM |
| `skerry status [NAME]` | Show its services and the ports they are forwarded to |
| `skerry url [-w NAME] SERVICE` | Print a URL for a service, for tools on your Mac |
| `skerry logs [-w NAME] [-f] SERVICE` | Show a service's log; `console` and `network` show the VM's console and refused connections |
| `skerry reset [NAME] [--snapshot S \| --empty]` | Replace the workspace's service data: as a new workspace's, a snapshot's, or empty |
| `skerry setup [NAME]` | Run the project's seed and setup commands again |
| `skerry seed [-w NAME] FILE` | Load a `.sql`, `.sql.gz` or `.dump` file from your Mac into the workspace's empty Postgres |
| `skerry snapshot save [S]` / `ls` / `rm S` | Save the workspace's service data for new workspaces (`default`), list or delete snapshots |
| `skerry gc` | Delete images and downloads no workspace needs |
| `skerry rm NAME [--force]` | Remove a workspace; `--force` discards uncommitted changes but never deletes a branch with commits |
| `skerry claude install` / `uninstall` | Add or remove the Claude Code hooks |

Settings, through environment variables for now:

| Variable | Effect |
|---|---|
| `SKERRY_BASE=head` | Start new worktrees at your current commit instead of the remote's default branch |
| `SKERRY_GIT_HOOKS=1` | Let the repository's git hooks run when creating worktrees (off by default) |
| `SKERRY_BRANCH_PREFIX` | Prefix for new branches (default `worktree-`) |
| `SKERRY_SHIMS=off` | Make the shims always run your Mac's own binaries |
| `SKERRY_HOME` | Keep all of skerry's state and caches under one directory |
| `SKERRY_IDLE_STOP` | Stop a workspace's VM after it has been unused this long (default `30m`; `off` never). The next command starts it again |
| `SKERRY_CPUS`, `SKERRY_MEMORY` | CPUs and memory per VM (default 4 and `3g`; at most all CPUs and half the Mac's memory) |
| `SKERRY_FILE_EVENTS=off` | Don't forward file changes from your Mac to watchers in the VM |

## Security

Workspaces run untrusted code, so skerry is built to contain it.

**What protects you**

- **VM boundary.** Each workspace is a Linux VM on Apple's Virtualization
  framework, which runs it in a separate, sandboxed process. The VM sees only
  its checkout, the read-only system image and its own config.
- **A network that only goes out.** A VM's network card connects to a
  user-mode network stack inside skerry, not to a bridge or your Mac's
  network. Every connection is checked after DNS resolution: public
  addresses are allowed; loopback, private and link-local ranges (cloud
  metadata included), carrier-grade NAT, multicast, your Mac's own
  addresses and every network it is directly attached to (VPNs included)
  are refused, as is outgoing mail (port 25). A VM can hold at most 512 TCP
  and 128 UDP flows, so it can't exhaust your Mac's ports. DNS is answered
  through your Mac's resolver. Refused connections are logged
  (`skerry logs network`).
- **Services reachable only from your Mac.** Forwarded ports listen on
  `127.0.0.1` only. Postgres and Valkey each require their own password,
  which other local users can't read; services without a password aren't
  forwarded unless you ask. Output from the VM that skerry prints itself
  (logs, errors) is stripped of terminal control sequences.
- **Locked git metadata.** The worktree's `.git` pointer is made immutable,
  and skerry's own git commands name the repository explicitly, so a VM
  can't redirect them. After every command, at session start and every
  15 seconds while a VM runs, skerry scans the checkout for planted
  repositories and Claude Code settings and quarantines them.
- **Opt-in.** Nothing happens outside folders you enable. Creating a
  worktree never runs the repository's filter drivers, or its hooks unless
  you opt in.
- **Pinned keys.** Host and VM authenticate each other with per-workspace SSH
  keys that skerry generates. Nothing is trusted on first use.
- **Verified images.** Every package is checked against a signed index
  (Wolfi's and Alpine's pinned keys) down to each file, and extraction
  can't write outside the image directory.
- **Careful deletion and bounded logs.** skerry only deletes worktrees it
  created, never a branch that holds commits, and caps the logs a VM can
  write on your disk.

**Known gaps**

- The checkout is shared read-write: that is how your edits reach the VM and
  its output reaches you. Code in the VM can change your source files, and
  files git tracks under `.claude/`. Review changes as you would any
  dependency's.
- Something running in the background in a VM could plant a file between
  scans (up to 15 seconds).
- No disk quota yet: a VM can fill the disk through its checkout, and its
  service data disk can grow to 64 GB.
- The Temporal dev server and its UI have no authentication. If you
  forward them, any process of any user on your Mac can reach them.
- A web page served from a VM and opened in your browser is a local page:
  it can send requests to other services on your Mac's loopback interface,
  as any local web app can. Open workspace apps through their
  `*.localhost` URLs.
- A snapshot's data reaches every new workspace of its repository, whatever
  branch it is on, and code in those VMs can reach the internet. Don't save
  data in a snapshot that you wouldn't give to every branch you check out
  (an outside contributor's included).
- Setup commands from `skerry.yaml` run in the VM, without asking, when a
  workspace's data is new, as `npm install` would.
- A VM can reach anything public, from your Mac's public address: code in
  it can send what it can read (the checkout) to the internet, and reach
  services that trust your IP address or a public range routed over your
  VPN. An allowlist mode is planned.
- No external security review yet.

The full threat model, and the rules every change is tested against, are in
[docs/design.md](docs/design.md#7-security).

## Project layout

```
cmd/skerry/          the CLI
cmd/skerry-agent/    the guest agent (Linux, embedded in the CLI)
internal/agent/      agent: boot, SSH server, host client
internal/engine/     VM drivers (Apple Virtualization framework)
internal/vmm/        workspace VMs: runner, up/down, image finalize
internal/netx/       user-mode network and its policy
internal/services/   service presets: Postgres, Valkey, Temporal
internal/project/    skerry.yaml and service detection
internal/image/      verified images: packages, kernel, initramfs
internal/workspace/  worktrees and their lifecycle
internal/hook/       Claude Code hook handlers
internal/scope/      enabled folders
scripts/             escape tests to run against a real workspace
docs/demo/           the demo project and the scripts that record the GIF
docs/design.md       design, threat model, measurements, release plan
```

## License

MIT. See [LICENSE](LICENSE).
