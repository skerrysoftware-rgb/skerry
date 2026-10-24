#!/bin/sh
# Sets up the demo the GIF is recorded from: the "orders" project in
# /Users/Shared/skerry-demo/orders, with two branches and a workspace for
# each. Run it after `make build`, then: python3 capture.py scenes.json
set -eu

DEMO=/Users/Shared/skerry-demo
HERE=$(cd "$(dirname "$0")" && pwd)
export SKERRY_HOME="$DEMO/.skerry"
export PATH="$HERE/../../bin:$PATH"
repo="$DEMO/orders"

if [ -e "$repo" ]; then
	echo "$repo already exists; remove it (skerry rm its workspaces first) to start over" >&2
	exit 1
fi
mkdir -p "$repo"
printf 'not for the VM\n' > "$DEMO/secret.txt"

commit() { git -c user.name=demo -c user.email=demo@example.com commit --quiet -m "$1"; }

cd "$repo"
cp -R "$HERE/project/main/." .
git init --quiet -b main

# Claude Code runs skerry's hooks from the project's settings. Inside a
# worktree it reads the settings committed there, so they are committed.
skerry claude install --settings "$repo/.claude/settings.json" --yes >/dev/null
python3 -I - "$repo/.claude/settings.json" "$(command -v skerry)" <<'EOF2'
import json, sys
path = sys.argv[1]
settings = json.load(open(path))
settings["model"] = "haiku"
settings["statusLine"] = {"type": "command", "command": sys.argv[2] + " statusline", "refreshInterval": 1}
json.dump(settings, open(path, "w"), indent=2)
EOF2
git add -A
commit "orders service"

git checkout --quiet -b gift-notes
cp -R "$HERE/project/gift-notes/." .
git add -A
commit "gift notes on orders"
git checkout --quiet main
skerry enable "$repo" --yes >/dev/null

echo "demo ready in $repo"
