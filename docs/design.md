# skerry: design

Status: draft for the first release · Last updated: 2026-10-23

skerry gives every checkout its own isolated, lightweight Linux VM with the
databases, services and dependencies that checkout needs. It plugs into
Claude Code so that every worktree Claude creates becomes a full environment,
and it detects most of what a repository needs without configuration.

The name is provisional until a trademark search is done (see
[Open questions](#16-open-questions)).

---

## 1. Problem

Parallel work on one repository uses git worktrees. Worktrees isolate files
but nothing else: every checkout shares the same ports, the same database, the
same Redis and the same running services. Two checkouts cannot run the same
service at once, a migration on one branch breaks another, and testing a
branch end to end means stopping everything else. Agents make this worse: a
developer may have five Claude sessions in five worktrees, all fighting over
`localhost:5432`.

## 2. Goals and non-goals

**Goals**

- Each checkout gets its own environment: its own Postgres, Redis, Temporal,
  dependencies and services, with no port or data conflicts.
- Isolation is a real VM boundary, but lightweight: sub-second boot and about
  140 MB of host memory for an idle workspace.
- Zero configuration for common stacks. Detection covers what it can, and a
  short `skerry.yaml` holds overrides.
- Checkout never blocks. Provisioning runs in the background with visible
  progress.
- Databases start from a realistic seed (for example a dev-database dump) in
  seconds, with the branch's migrations applied on top.
- Works transparently with Claude Code: CLI, desktop app, subagents,
  background sessions.
- Runs on macOS (Apple Silicon) and Linux; can back Claude Code's
  self-hosted cloud environments.
- Fits in 18 GB of RAM alongside a normal desktop.
- Secure by default; safe to open source.

**Non-goals (first release)**

- Production deployment or orchestration. skerry is for development.
- Windows hosts.
- Running arbitrary OCI images inside workspaces (compose services without a
  preset are reported, not run).
- Multi-node or distributed workspaces.

## 3. Concepts

| Term | Meaning |
|---|---|
| **Workspace** | One checkout plus its VM, disks, network and services. Identified by a name. |
| **Checkout** | A git worktree on the host. Claude and your editor work on these files directly. |
| **Plan** | What a workspace needs: runtimes, packages, services, setup steps, seed, migrations. Produced by detection plus `skerry.yaml`. |
| **Preset** | A packaged service definition (`postgres@17`, `redis@8`, `temporal-dev`) with readiness checks and connection outputs. |
| **Seed** | The source for a database's initial data (a dump directory, file, URL or command). |
| **Snapshot** | A stopped, read-only Postgres data disk built from a seed, cloned copy-on-write into workspaces. |
| **Enabled folder** | A folder where skerry is switched on. Checkouts under it become workspaces, and enabling it is also the approval for skerry to run those projects' commands (inside VMs). Everywhere else skerry creates plain worktrees and runs nothing. |

## 4. User experience

```console
$ skerry create fix-login            # instant: worktree + registry entry
/Users/me/src/shop/.claude/worktrees/fix-login
$ skerry watch fix-login             # background provisioning, live
  ✓ detect        node 24 · yarn 4 · postgres 17 · redis · temporal   0.2s
  ✓ vm            booted                                              0.4s
  ◐ deps          yarn install (cached 812/1,204)                     ...
  ◐ db            cloning snapshot shop-2026-08-29 → migrating  
  · services      lazy (start on first connection)
$ cd …/fix-login && yarn test        # runs inside the workspace VM
$ skerry ls
$ skerry rm fix-login                # refuses if there is uncommitted work
```

With Claude Code, none of this is typed. Claude asks for a worktree, skerry
creates the workspace, and Claude's `node`, `yarn`, `psql` and similar
commands run inside it.

## 5. Architecture

```
 host (macOS / Linux)                                    guest (Alpine microVM, one per workspace)
┌───────────────────────────────────────────────┐       ┌─────────────────────────────────────────┐
│ Claude Code ── hooks ──► skerry (CLI)         │       │ skerry-agent (PID 1)                    │
│   Bash tool ── shims ──►   │                  │       │   ├─ mounts: /work (checkout, virtiofs) │
│                            ▼                  │       │   │          / (base rootfs, ro + overlay)│
│ registry, scope, plans  skerry vm <ws>  ──────┼─vsock─┤   │          /data (disk, ext4)          │
│ (state dir, 0700)        (one process per VM) │  ssh  │   ├─ exec server (SSH over vsock)       │
│                            │  VZ / CH driver  │       │   ├─ service supervisor (lazy start)    │
│                            │  network stack ──┼──net──┤   ├─ file-event replayer                │
│                            │  port forwards   │       │   └─ services: postgres, redis, node…   │
│ caches: images, packages, snapshots (ro)      │       └─────────────────────────────────────────┘
└───────────────────────────────────────────────┘
```

- **One VM per workspace.** Services in a workspace are processes in one VM.
  Isolation between workspaces is the VM boundary. Isolation between services
  inside a workspace is not a goal: they are meant to talk to each other.
- **One host process per running VM** (`skerry vm <workspace>`). A crash
  takes down one workspace, not all of them. A central daemon is added only
  when global coordination (memory budget) needs it.
- **No root.** Everything runs as the user.

## 6. Components

### 6.1 CLI

A single static Go binary. Subcommands: `create`, `ls`, `rm`, `watch`,
`status`, `exec`, `up`, `down`, `enable`, `disable`, `seed`, `doctor`, `claude install`,
and internal ones (`hook …`, `vm …`). Hook subcommands read Claude's JSON on
stdin and print only what Claude expects on stdout. All logs go to stderr.

State lives in the OS user data directory (`~/Library/Application Support/skerry`
on macOS, `$XDG_STATE_HOME/skerry` on Linux), mode `0700`. Large caches live in
the user cache directory.

### 6.2 Engine drivers

| Driver | Platform | Use | Notes |
|---|---|---|---|
| **vz** | macOS, Apple Silicon | Default on Mac | Apple's Virtualization framework through `Code-Hex/vz` (MIT). The VM runs in Apple's sandboxed XPC process, which also serves virtio-fs. |
| **cloud-hypervisor** | Linux + KVM | Default on Linux | With virtiofsd. Both sandbox themselves (seccomp, namespaces, Landlock). |
| **firecracker** | Linux + KVM | Cloud runners | Jailer for multi-tenant use. No virtio-fs, so code is cloned inside the VM. |
| **namespaces** | Linux, no KVM | Fallback | Weaker isolation; labelled as such. |
| **process** | Any | Anthropic-hosted cloud sessions | No isolation layer; the session VM is the boundary. |
| libkrun | macOS, Linux | Optional, not default | Its own security model says the guest reaches host resources with the VM process's permissions. Only ever run inside namespaces, and never with TSI networking. |

The driver interface: `Create`, `Start`, `Stop`, `Save`, `Restore`, `Destroy`,
`Dial(vsock port)`, `Stats`. Drivers do not know about services or plans.

### 6.3 Guest image and agent

- **Base rootfs:** Alpine (musl) built on the host from Alpine's signed
  packages. The host resolves dependencies from the signed `APKINDEX`, then
  downloads and verifies each package through Alpine's chain: the release
  GPG key, then the package-signing keys, the index signature, the
  control-section hash and the data hash. Nothing unverified enters an image.
  The native Node dependencies of a large real-world TypeScript monorepo all
  ship musl builds (checked in its lockfile).
- **Kernel:** Alpine `linux-virt`, verified the same way and unpacked from its
  EFI zboot wrapper. VZ needs an uncompressed arm64 `Image`. The virtio
  drivers are modules, loaded by the agent.
- **Layout:** the base rootfs is shared **read-only** over virtio-fs. The
  guest stacks a writable overlay on it, so the host's page cache is shared
  across workspaces and an image costs no per-workspace disk.
- **Agent:** a static Go binary that runs as PID 1. It mounts filesystems,
  loads drivers, runs the exec server, supervises services, replays file
  events and reports health. Its configuration arrives on a per-VM read-only
  config share.
- **Exec protocol:** SSH (`golang.org/x/crypto/ssh`) over vsock. This gives
  multiplexing, PTYs, signals and exit codes from a mature, audited
  implementation instead of a custom parser. The host generates both the
  client key and the guest host key per workspace and pins them, so there is
  no trust-on-first-use.

### 6.4 Storage

| What | Where | Why |
|---|---|---|
| Source code | Host worktree, shared to `/work` over virtio-fs | Claude, git and your editor work on host files. Measured: host edits are visible to guest reads in ~40–65 ms. |
| Base system | Read-only virtio-fs share + guest overlay | Shared across workspaces |
| Postgres data, Redis data | Per-workspace data disk (sparse raw file, ext4 created by the guest on first boot) | Survives VM stops; fast; clonable |
| Linux `node_modules` | On the data disk, bind-mounted over each `node_modules` path inside `/work` | Native modules differ between macOS and Linux; keeps host `node_modules` for the editor |
| Host `node_modules` | APFS clone (`clonefile`) of the main checkout's | Instant; your editor's TypeScript server and ESLint keep working |
| Package caches | Per-repository cache share | Same trust domain as the repo; yarn verifies checksums from the lockfile |

Copy-on-write cloning: `clonefile` on APFS. On Linux, reflink on btrfs and
XFS. On ext4, a qcow2 overlay on a read-only base disk (Cloud Hypervisor,
`backing_files=on` only for disks skerry built; guest-writable raw disks keep
`backing_files=off` because of the qcow2 header exfiltration advisory fixed in
Cloud Hypervisor 50.1).

### 6.5 Networking

Each workspace gets its own user-mode network stack (gvisor-tap-vsock,
Apache-2.0) in its `skerry vm` process. No root, no shared bridge.

Default policy, enforced at connect time after DNS resolution (so DNS
rebinding cannot bypass it):

| Destination | Default |
|---|---|
| Public internet | Allowed (package registries, APIs) |
| The host, including the host's `localhost` | **Denied** |
| Other workspaces | **Denied** (separate stacks; no route exists) |
| Private ranges (RFC 1918, CGNAT, ULA, link-local) | **Denied** |
| Cloud metadata (169.254.169.254, fd00:ec2::254) | **Denied** |

Inbound traffic arrives only through host-side forwards bound to `127.0.0.1`,
never `0.0.0.0`:

- HTTP services: `http://<service>.<workspace>.localhost:<proxy-port>`.
  Browsers and curl resolve `*.localhost` to loopback without DNS changes.
- TCP services such as Postgres: an allocated loopback port per service,
  exported to host-side tools.

The VZ NAT attachment is not used, because it gives the guest a route to the
host.

### 6.6 Services and presets

A preset defines packages, a start command, a readiness check, data
directories and outputs (`url`, `host`, `port`). The first release ships:

- `postgres@17`: Alpine `postgresql17` and contrib, plus **pgvector built for
  17** by skerry's image builder (Alpine's pgvector package targets
  Postgres 18 only).
- `redis@8`, or `valkey`.
- `temporal-dev`: the Temporal CLI dev server, a static binary from its
  GitHub releases (MIT), pinned by checksum.
- `node@<major>`: Node from Alpine (`nodejs` 24, `nodejs-current` 26), with
  corepack for the repository's `packageManager`.

**Lazy start.** Services that listen on a port start on the first connection
to that port; the supervisor holds the socket. Workers that poll instead of
listening (for example Temporal workers) start with their dependencies or on
`skerry up`.

### 6.7 Detection

Detectors only **read** files; they never execute repository code. Each one
emits plan fragments with evidence (file and line).

| Signal | Plan |
|---|---|
| `.nvmrc`, `.node-version`, `engines`, `packageManager`, `.tool-versions`, Dockerfile `FROM` | Runtime and versions |
| `pg`, `postgres`, `drizzle-orm`, `prisma`, `@electric-sql/pglite`, `ioredis`, `redis`, `@temporalio/*` | Services |
| `.env.example` names (`DATABASE_URL`/`_DSN`, `REDIS_HOST`/`_PORT`, `TEMPORAL_*`) | Env wiring to service outputs |
| `docker-compose*.yml` | Services with a matching preset; others reported |
| Workspace packages with a `dev` script and a `*_PORT` variable | Runnable app services |
| `*_API_URL` names matching another package (`ORDERS_API_URL` → `orders-service`) | Cross-service URLs |
| `atlas.sum`, drizzle journal, `prisma/migrations`, alembic, Rails, Flyway | Migration tool and command |
| Seed scripts (`db:seed`, `database:seed`), dump directories | Seed source |

Plans are cached by a hash of the input files. A branch that changes them
gets a re-plan that shows only the differences. Unknowns are listed, not
guessed. `skerry init --ai` is an opt-in that sends a summary to Claude to
fill gaps, and says so.

**Secrets** cannot be detected. Sources, in order: the repository's `.env`
from the main checkout (copied, like `.worktreeinclude`), the OS keychain, or
a command such as the 1Password CLI. Secrets enter the VM on a memory-only
filesystem, never a disk image, and are redacted from logs.

**Profiles** pick which app services run, because a monorepo can hold dozens
of packages and running all of them is wasteful.

### 6.8 Configuration

`skerry.yaml` is optional and holds only overrides. It is versioned and has a
published JSON Schema.

```yaml
version: 1
profile: api                      # default profile
seed: { from: db/dumps/latest }
services:
  postgres: { use: postgres@17, extensions: [vector, pg_trgm] }
  api:
    path: packages/api
    run: yarn dev
    port: 3000
    env: { DATABASE_URL: "${postgres.url}", REDIS_HOST: "${redis.host}" }
profiles:
  api: [postgres, redis, temporal, api]
resources: { memory: 3GiB, cpus: 4 }
```

What a repository's config may **not** do, because a cloned repository is
untrusted input: mount host paths, read host files outside the repository,
open ports on non-loopback addresses, or change network policy. Those belong
in the user's own settings (`~/.config/skerry/config.yaml`).

### 6.9 Seeding and migrations

1. **Snapshot build (background, once per seed):** a builder VM restores the
   seed (`pg_restore -j N` for directory dumps) onto a fresh data disk,
   records the migration version, stops Postgres cleanly and freezes the disk
   read-only.
2. **Clone per workspace:** copy-on-write clone of the snapshot disk, which is
   near-instant and uses almost no space until written.
3. **Migrate:** run the repository's own migration command (for Atlas, for
   example: `atlas migrate apply --dir file://migrations --url $DATABASE_URL`).
   Only migrations newer than the snapshot apply.
4. **Keep in sync:** a new migration file is applied automatically. An
   edited or removed migration triggers a reset: re-clone the snapshot and
   reapply. A branch older than the snapshot is flagged, not silently
   migrated.
5. **Refresh:** `skerry seed refresh` builds a new snapshot in the
   background. Existing workspaces keep theirs until reset.

Snapshots may hold real data. They are `0700`, never published, deleted with
their workspace clones, and support an optional scrub step.

### 6.10 Claude Code integration

**skerry is opt-in per folder.** `skerry enable DIR` switches it on for every
repository under `DIR`; `skerry claude install` asks which folders to enable.
Elsewhere, skerry's hooks create plain worktrees exactly as Claude Code does by
default, and nothing else happens:

- worktree at `.claude/worktrees/<name>` on branch `worktree-<name>`
- starting from the remote's default branch (`origin/HEAD`), or the current
  `HEAD` when there is none (`SKERRY_BASE=head` always uses `HEAD`, like
  Claude Code's `worktree.baseRef: "head"`)
- asking again for an existing name reopens that worktree
- gitignored files matching `.worktreeinclude` are copied in
- the repository's own filter drivers and file-system monitor never run
  during creation (Claude Code does the same for filters); its git hooks
  don't run either unless `SKERRY_GIT_HOOKS=1`, because anything that can
  write to the repository, an agent included, could have planted them

In a plain worktree, the first session per repository gets one line of
context telling Claude that skerry exists and how to enable it.


Verified in the spike: the CLI's `--worktree` **and the desktop app's worktree
sessions** call `WorktreeCreate` and use the path it returns. Hooks configured
in a trusted repository's project settings also fire.

| Hook | skerry action |
|---|---|
| `WorktreeCreate` | Create the worktree and register the workspace, then return the path immediately. Provisioning starts in the background, only for trusted repositories. |
| `SessionStart` | If the session's directory is a workspace, write `PATH` (shims first) and service variables to `CLAUDE_ENV_FILE`, and add context for Claude: what's running, and `skerry wait <service>`. |
| `WorktreeRemove` | Destroy the workspace. Refuse when there are uncommitted changes or unpushed commits. Only deletes paths recorded in the registry under its own roots. |

**Command shims, not command rewriting.** A shim directory holds links named
`node`, `npm`, `npx`, `yarn`, `pnpm`, `corepack`, `tsx`, `vitest`, `psql`,
`redis-cli` and similar, all pointing at `skerry`. A shim runs the command in
the workspace VM when the current directory is inside a workspace, mapping
the host path to `/work/…`. Otherwise it runs the next real binary on `PATH`.
`git`, `gh` and everything else stay on the host, so signing keys,
credentials and the user's permission rules are untouched. A `PreToolUse`
rewrite was rejected (see review finding 3).

`skerry claude install` writes the hooks into the user's
`~/.claude/settings.json`. It shows the hooks it adds, keeps a backup, writes
atomically, keeps a symlinked settings file a symlink, refuses to replace
another tool's worktree hooks without `--force`, and is idempotent. A Claude
Code plugin packaging is planned.

**Hooks must be installed at user level.** Verified in M1: inside a worktree,
Claude reads the project settings *committed in that worktree*, so hooks
added to the main checkout's project settings never reach `SessionStart` in
worktree sessions.

Branch names: the desktop app's branch prefix setting no longer applies once
a hook creates worktrees, so skerry reads its own `branch_prefix` (default
`wip/`). Users rename branches before pushing.

### 6.11 Progress and events

Provisioning is a graph of steps (detect → image → VM → deps → seed clone →
migrate → services). Each step emits events (`started`, `progress`, `done`,
`failed`) to a per-workspace event log. Consumers:

- `skerry watch` and `skerry status`
- `skerry wait <service>`, which blocks until ready (for tests and scripts)
- The Claude Code status line (`skerry status --line`)
- The `SessionStart` context, which tells Claude what is still provisioning

### 6.12 Memory management

Measured on VZ (see [§11](#11-spike-results)):

- Only memory the guest has touched counts on the host: ~141 MB for an idle
  1 GiB guest.
- **Freed guest memory is never returned.** After the guest freed 500 MiB, the
  host still held it.
- **The balloon makes it worse:** shrinking the guest to 256 MiB raised host
  memory from 610 to 927 MB.
- **Restore brings back everything ever touched** (1.1 GB), while a cold boot
  takes ~0.45 s.

Policy:

1. **Per-workspace RAM cap** (default 3 GiB, per profile). This bounds the
   worst case.
2. **Idle means stopped.** After N minutes with no commands, connections or
   file activity, the VM shuts down cleanly. Disks keep all data, and the next
   use cold-boots in under a second, with services starting lazily.
3. **Recycle:** a workspace whose host memory exceeds its working set by a
   threshold is restarted at the next idle moment.
4. **Save and restore** only for short pauses where in-memory state matters.
   Each workspace keeps a persisted machine identity, which restore requires.
5. **No balloon on VZ.**
6. **Global budget (later, with the daemon):** stop least-recently-used idle
   workspaces when total use passes a limit. Never stop one with a running
   command or an attached session.

Budget on an 18 GB Mac: with ~9 GB taken by macOS, editor, browser and
Claude, 6 GB for skerry fits 3–4 fully active full-stack workspaces plus any
number of stopped ones.

### 6.13 File events

Linux watchers inside the guest (Vite, tsx, chokidar) never see host edits:
virtio-fs delivers no inotify events for changes made on the host (measured).
A guest-side metadata update does generate an event that Node watchers react
to (measured). So:

host watcher (FSEvents / inotify) → batched paths over vsock → agent calls
`utimensat` on each path **with its existing timestamps**.

The guest sees an attribute-change event. The host sees no change, so the
forwarding cannot loop.

## 7. Security

Isolation and networking code at OS level is where security bugs hurt most.
No one can promise a bug-free system; the design aims for layers, so that a
single bug does not hand over the host.

### 7.1 Threat model

| Asset | Threat | Primary controls |
|---|---|---|
| Host files, keys, credentials | Code running in a workspace (repository scripts, dependency install scripts, Claude's commands, prompt injection) | VM boundary; only the checkout, read-only base and caches are shared; VMM sandboxing; no host keys in guests |
| Other workspaces | A compromised workspace | Separate VMs, disks and network stacks; per-repository caches |
| Host network services, LAN, cloud metadata | Guest network access | Default-deny policy enforced after DNS resolution |
| skerry control | Other local users; malicious web pages | Control only over `0600` Unix sockets; no TCP control port; dashboard read-only with token and Origin/Host checks |
| The host itself | A malicious repository at checkout | Opt-in enabled folders; repository git hooks and filter drivers off during worktree creation; detectors never execute code; repository commands only run inside the VM; repository config cannot request host access |
| Supply chain | Tampered packages, images or releases | Signature-verified Alpine packages; checksum-pinned downloads; Go checksum database; signed releases with SBOM and provenance |
| Seed data | Leaks of copied production-like data | `0700` storage, no publishing, deletion with workspaces, optional scrubbing |
| Multi-tenant cloud | Cross-tenant access | VM per session; runner bound to one owner; firecracker jailer; no pre-warmed VM holds owner data |

### 7.2 Invariants (each gets a test)

1. A guest cannot read any host path outside its shares, including through
   symlinks or `..` in the shared checkout.
2. A guest cannot open a connection to the host, another workspace, a private
   range or a metadata address.
3. The host never acts on a path, command or size it received from a guest
   without validation. The guest is hostile.
4. Hook inputs are validated: worktree names match `^[A-Za-z0-9._-]{1,64}$`
   and are not `.` or `..`; returned paths are absolute, normalized and
   contain no symlinks.
5. `WorktreeRemove` and `skerry rm` delete only paths recorded in the
   registry under skerry's or the repository's worktree root, never a path
   taken from input.
6. Nothing from a repository executes on the host.
7. Outside enabled folders, skerry creates plain worktrees and provisions
   nothing. During worktree creation, git never runs the repository's filter
   drivers, file-system monitor or (unless the user opts in) hooks.
8. Control sockets and state are readable only by the user.
9. No listener binds a non-loopback address.
10. Secrets never reach disk images, logs or snapshots.
11. Resource limits apply per VM: CPU, memory, disk size, open files.

### 7.3 Engineering controls

- Fuzzing (Go native) on every parser: config, detectors, hook input, guest
  messages, APK and tar handling.
- Escape test suite in CI. From inside a guest it tries to read host files
  (directly, through symlinks, through `..`), reach the host, other
  workspaces and metadata, and exhaust resources.
- `gosec`, CodeQL, `govulncheck`, `go-licenses`, minimal pinned dependencies,
  Renovate.
- Signed releases (Sigstore), SBOM, SLSA provenance.
- SECURITY.md with a disclosure process; external audit before 1.0.

## 8. Cloud

| Path | How |
|---|---|
| **Self-hosted Claude Code environments** (Team/Enterprise, beta) | The orchestrator's `spawn-runner` hook calls skerry to boot a workspace VM (firecracker or cloud-hypervisor) and starts the runner inside it with the single-use work order. `checkout` uses skerry's repository cache; `post-session` saves uncommitted work and destroys the VM. `--min-idle` pre-warms VMs with dependencies and seed clones ready, but no owner data until a session binds. Install kits: Terraform modules (AWS, GCP, Azure), a host image, a Helm chart, `skerry cloud hooks install`. |
| **Anthropic-hosted cloud sessions** | The session VM is the boundary, so skerry uses the process driver. The setup script installs skerry, and a repository `SessionStart` hook runs `skerry up` in the background. Constraints: network allowlist, ~5 minute setup-script cache limit, network secrets for private seeds. |
| **SSH devboxes** | Claude sessions on an SSH host fire the same hooks. A laptop can drive many workspaces on a larger remote machine. |

## 9. Packaging and distribution

- One `skerry` binary per platform (`darwin/arm64`, `linux/amd64`,
  `linux/arm64`). The guest agent is a static Linux binary embedded in it.
- Kernel and base images are built in CI from verified sources, published to
  ghcr.io, and pinned by digest in the binary. They can also be built locally.
- Homebrew tap, an install script that verifies checksums and signatures, and
  `.deb`/`.rpm` packages. GoReleaser in CI produces checksums, an SBOM,
  Sigstore signatures and provenance.
- macOS: ad-hoc signed with the `com.apple.security.virtualization`
  entitlement for Homebrew installs. Notarization only if browser downloads
  are offered.
- License: MIT, plus a CLA.

## 10. Engineering standards

- Go, standard library first. Every dependency is justified and vetted.
- `context.Context` on everything long-running; `log/slog`; errors wrapped
  with context; no global state.
- `gofumpt`, `golangci-lint` (staticcheck, errcheck, gosec, errorlint,
  revive), `go vet`, tests with the race detector.
- Table-driven unit tests; golden tests for detection against fixture
  repositories; integration tests that boot real VMs on Linux CI with KVM and
  on a self-hosted Apple Silicon runner.
- Small, reviewed changes; a code review pass per milestone.

## 11. Spike results

Measured 2026-10-23 on an M4 Pro (48 GB), macOS 27.0, Alpine 3.24 guest with
kernel 6.18.58-virt, 1 GiB RAM and 2 vCPUs, through VZ. Repeated over 5 runs.
The spike code is kept outside this repository.

| Question | Result |
|---|---|
| Desktop app worktree sessions fire `WorktreeCreate` | **Yes**; the session uses the returned path |
| CLI `--worktree` fires it | **Yes** |
| `WorktreeRemove` in headless (`-p`) sessions | **Not fired**; skerry needs its own sweep for leftovers |
| `SessionStart` in a worktree session | Fires with the worktree as `cwd`; reads hooks from user settings and the worktree's *committed* project settings (verified in M1) |
| `CLAUDE_ENV_FILE` exports | Visible to Claude's Bash commands (verified in M1) |
| Boot: `Start()` to guest ready | **0.42–0.47 s** (kernel to init: 50 ms) |
| Host memory, idle | **~141 MB** for a 1 GiB guest |
| Host memory after the guest allocates then frees 500 MiB | **~610 MB, not returned** |
| Balloon to 256 MiB | **Increased** host memory to ~915 MB |
| Host edit visible to guest reads | **40–65 ms** |
| Host edit as guest inotify event | **Never** |
| Guest-side timestamp update as inotify event | **Yes** (attribute change) |
| Save/restore supported with virtio-fs attached | **Yes** |
| Save | **~200 ms**, 22 MiB state file |
| Restore and resume | **~210 ms**; requires the same machine identity; shared folder works after |
| Host memory after restore | **~1.1 GB** (everything touched before the save) |
| Alpine 3.24 packages | Postgres 17.11 and contrib, Node 24.22, Redis 8.8, Valkey 9.0; pgvector 0.8.1 **built for Postgres 18 only** |
| Native Node modules of a large monorepo on musl | All have musl builds |
| Cloud Hypervisor qcow2 overlays | Supported since v34; backing files off by default since 50.1 (advisory) |

Supply-chain checks on what the spike installed: Go from `homebrew/core`
(source checksum matches go.dev; bottle covered by a SLSA attestation from
Homebrew's publishing workflow); Alpine files verified from the release GPG
key down to the kernel; `Code-Hex/vz` v3.8.0 (MIT, no advisories, used by
Lima, vfkit and LinuxKit).

## 12. Design review log

Findings from the review before building, and what changed.

| # | Severity | Finding | Resolution |
|---|---|---|---|
| 1 | Blocker | libkrun's guest reaches host resources with the VM process's permissions; TSI gives the guest the host's network; macOS has no namespaces | vz on macOS, cloud-hypervisor on Linux, firecracker for cloud; libkrun optional, only inside namespaces |
| 2 | Blocker | Repository config and detected steps would run untrusted code at checkout | Opt-in enabled folders; detectors never execute; commands only in the VM; repository config cannot request host access |
| 3 | Blocker | Rewriting Claude's Bash commands breaks compound commands, permission rules and other hooks | Command shims on `PATH` via `CLAUDE_ENV_FILE`; `git`/`gh` stay on the host |
| 4 | Blocker | Unknown whether the desktop app fires `WorktreeCreate` | Verified: it does |
| 5 | High | Host edits don't reach guest watchers | Event forwarding with timestamp re-apply (§6.13); verified feasible |
| 6 | High | Default user-mode networking exposes the host's localhost | Per-workspace stack with default-deny policy (§6.5) |
| 7 | High | Local web surfaces reachable from any web page | Unix-socket control only; read-only dashboard with token and Origin/Host checks |
| 8 | High | Guest must be treated as hostile | Pinned SSH over vsock; validated inputs; registry-only deletion; per-VM limits |
| 9 | High | Copy-on-write clones fail on ext4 | qcow2 overlays on trusted read-only bases |
| 10 | High | Seeds spread real data | `0700`, no publishing, deletion, scrubbing |
| 11 | Medium | Two `node_modules` (macOS for the editor, Linux for the VM) | APFS clone on host; data-disk bind mounts in the guest |
| 12 | Medium | Lazy start doesn't fit polling workers | Workers start with dependencies |
| 13 | Medium | Suspend could interrupt work; restore memory behaviour unknown | Never stop a busy workspace; idle means stop, not save (§6.12) |
| 14 | Medium | Secrets can't be detected | Explicit sources; memory-only in the VM |
| 15 | Medium | Running every package is wasteful | Profiles |
| 16 | Medium | Shipping a kernel means patching it | Build from Alpine's maintained `linux-virt`; rebuild on each Alpine kernel update |
| 17 | Medium | `init --ai` sends repository content to an API | Opt-in, stated plainly |
| 18 | Medium | `curl | sh` installs | Homebrew first; script verifies checksums and signatures |
| 19 | Scope | Too much for one release | Release plan below |
| 20 | Product | Self-hosted cloud is Team/Enterprise only | Documented |
| 21 | Product | Trademark, ownership, audit | Open questions |

## 13. Release plan

**Release 1: macOS, local.**

| Milestone | Contents | Done when |
|---|---|---|
| M1 Foundation | CLI, state, registry, opt-in scope, git worktrees, Claude hooks, `skerry claude install` | Claude worktree sessions are created and removed through skerry |
| M2 VM core | vz driver, image builder from verified packages, agent, SSH exec over vsock, shims | `yarn --version` from a Claude session runs inside the workspace VM |
| M3 Network | Per-workspace stack, egress policy, loopback forwards, `*.localhost` proxy | Policy tests pass; Postgres reachable from host tools |
| M4 Services | Presets (Postgres 17 + pgvector, Redis, Temporal, Node), supervisor, lazy start, readiness | `skerry wait postgres` works; services start on connect |
| M5 Detection | Detectors, plan cache, `skerry.yaml`, profiles, secrets sources | A real-world monorepo gets a correct plan with no config beyond overrides |
| M6 Seed | Snapshot builder, clone, migrate, drift handling | A workspace has a seeded database with current migrations in seconds after the first snapshot |
| M7 Experience | Events, `watch`, status line, file-event forwarding, idle stop, memory cap | Hot reload works; idle workspaces stop |
| M8 Hardening | Escape tests, fuzzing, threat-model tests, docs | All invariants have passing tests |

**Release 2:** Linux (cloud-hypervisor, namespaces), CI on KVM.
**Release 3:** cloud: firecracker driver, self-hosted runner kit, process
mode, Terraform/Helm.

## 14. Repository layout

```
cmd/skerry/          entry point
internal/cli/        subcommands
internal/workspace/  registry, lifecycle
internal/gitx/       git operations
internal/scope/      enabled folders
internal/hook/       Claude hook handlers
internal/claude/     settings install
internal/engine/     driver interface + vz, …
internal/image/      APK resolution, verification, rootfs and kernel builds
internal/agent/      guest agent (built for linux)
internal/detect/     detectors
internal/plan/       plan model and cache
internal/seed/       snapshots, clones, migrations
internal/netx/       network stack, policy, forwards
docs/                design, threat model
```

## 15. Decisions

| Decision | Choice | Why |
|---|---|---|
| Language | Go | gvisor-tap-vsock, Code-Hex/vz and SSH are Go libraries; precedent in Lima, Podman, containerd |
| Unit of isolation | VM per workspace | Memory: one kernel and page cache per workspace, not per service |
| macOS engine | VZ | Apple-sandboxed VM process; measured sub-second boot |
| Exec transport | SSH over vsock | Mature protocol and implementation; less custom parsing |
| Claude integration | Hooks + shims | Verified hook coverage; shims avoid permission and compound-command problems |
| Idle handling | Stop and cold boot | Measured: restore keeps all memory; boot is ~0.45 s |
| License | MIT + CLA | Free use; keeps relicensing possible |
| Config | Optional `skerry.yaml` overrides | Detection first |

## 16. Open questions

1. **Trademark:** USPTO search on "skerry" before any public release.
2. **Default RAM cap:** 3 GiB per workspace is a guess until a large
   full-stack workspace is measured under load.
3. **gvisor-tap-vsock egress filtering:** whether its forwarder can enforce
   the policy directly, or skerry needs its own forwarder on the same netstack
   (M3).
4. **pgvector build:** in a builder VM at image-build time (needs
   `build-base` and `postgresql17-dev`), cached by version.
