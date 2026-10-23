# skerry

An isolated, lightweight environment for every checkout.

Git worktrees isolate files, but every checkout still shares the same ports,
database and services. skerry gives each checkout its own small Linux VM with
the databases, services and dependencies it needs, and plugs into Claude Code
so every worktree Claude creates becomes a full environment.

> **Status: early development.** Milestone 1 (workspaces, trust and Claude
> Code hooks) works. VMs, services and seeding are in progress. See
> [docs/design.md](docs/design.md) for the design and release plan.

## Build

Requires Go 1.27+ and git.

```bash
go build -o bin/skerry ./cmd/skerry
```

## Use

```bash
skerry create fix-login        # new worktree on branch wip/fix-login; prints its path
skerry ls                      # list workspaces
skerry rm fix-login            # refuses if there is uncommitted or unpushed work
skerry trust                   # allow skerry to run this repository's commands
```

### With Claude Code

```bash
skerry claude install          # adds hooks to ~/.claude/settings.json (shows them first, keeps a backup)
```

After that, every worktree session Claude Code starts (CLI `--worktree`,
desktop app, subagents, background sessions) is created through skerry.
Install hooks at user level: inside a worktree, Claude reads the project
settings committed in that worktree, so project-level hooks don't follow
sessions there.

`skerry claude uninstall` removes them again.

## Security

skerry runs nothing from a repository until you `skerry trust` it, and only
ever deletes worktrees it created. See the threat model in
[docs/design.md](docs/design.md#7-security).

## License

MIT. See [LICENSE](LICENSE).
