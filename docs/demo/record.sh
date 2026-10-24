#!/bin/sh
# Records the demo: two real, interactive Claude Code sessions side by side
# in tmux, each in its own skerry workspace, captured with asciinema and
# rendered with agg. Nothing is replayed or retyped: the requests are sent
# to the live sessions as keystrokes, and the recording is what happened.
#
# Needs: make build, setup.sh, asciinema, agg and tmux, and Claude Code
# trusted for /Users/Shared/skerry-demo/orders (run `claude` there once,
# from a terminal). Writes demo.cast (the raw recording) and demo.gif.
set -eu

DEMO=/Users/Shared/skerry-demo
REPO="$DEMO/orders"
HERE=$(cd "$(dirname "$0")" && pwd)
BIN=$(cd "$HERE/../../bin" && pwd)
CAST="$HERE/demo.cast"
GIF="$HERE/demo.gif"
COLS=200
ROWS=50

# Its own tmux server and no personal config: no hostname in a status bar,
# no prompt with a user name. UTF-8 throughout, or tmux replaces Claude
# Code's symbols with underscores.
export LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8
t() { tmux -u -L skerry-demo -f /dev/null "$@"; }

# Start from fresh workspaces: remove the ones a previous recording made,
# and their branches.
for ws in feature-a feature-b; do
	(cd "$REPO" && SKERRY_HOME="$DEMO/.skerry" "$BIN/skerry" rm "$ws" --force >/dev/null 2>&1 || true)
	git -C "$REPO" branch -D "worktree-$ws" >/dev/null 2>&1 || true
done
if SKERRY_HOME="$DEMO/.skerry" "$BIN/skerry" ls | grep -qE '^feature-[ab] '; then
	echo "couldn't remove the previous recording's workspaces" >&2
	exit 1
fi

shell="env -i HOME=$HOME USER=$USER TERM=xterm-256color LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8 SKERRY_HOME=$DEMO/.skerry PATH=$BIN:/opt/homebrew/bin:/usr/bin:/bin:$HOME/.local/bin PS1='orders \$ ' /bin/sh"

t kill-server 2>/dev/null || true
t new-session -d -s demo -x "$COLS" -y "$ROWS" -c "$REPO" "$shell"
t set -g status off
t set -g pane-border-status top
t set -g pane-border-format " #{pane_title} "
t split-window -h -t demo -c "$REPO" "$shell"
t select-pane -t demo.0 -T "Claude Code session 1"
t select-pane -t demo.1 -T "Claude Code session 2"

rm -f "$CAST"
TERM=xterm-256color asciinema rec --headless --overwrite --window-size "${COLS}x${ROWS}" -c "tmux -u -L skerry-demo -f /dev/null attach -t demo" "$CAST" &
rec=$!
sleep 2

# send PANE TEXT: types TEXT into the pane and presses Enter.
send() { t send-keys -t "demo.$1" -l "$2"; t send-keys -t "demo.$1" Enter; }

# idle PANE: waits until Claude in PANE is waiting for input: it has
# stopped working (no "esc to interrupt") and the pane, apart from the
# input line (whose hint rotates), has stopped changing for a few seconds.
idle() {
	last="" same=0
	for _ in $(seq 1 900); do
		now=$(t capture-pane -p -t "demo.$1" | grep -v '❯')
		if [ "$now" = "$last" ]; then same=$((same + 1)); else same=0; fi
		last=$now
		if [ "$same" -ge 4 ] && printf '%s' "$now" | grep -q 'auto mode on' &&
			! printf '%s' "$now" | grep -q 'esc to interrupt'; then
			return 0
		fi
		sleep 1
	done
	return 1
}

send 0 "claude -w feature-a --allowedTools Bash"
send 1 "claude -w feature-b --allowedTools Bash"
idle 0
idle 1
send 0 "Install dependencies, run the migrations, then place an order for an espresso machine. Show me the orders table at the end."
send 1 "Merge the gift-notes branch, install dependencies, run the migrations, then place an order for a teapot with the gift note 'Happy birthday!'. Show me the orders table at the end."
idle 0
idle 1
sleep 3
# Stop recording with the sessions still on screen, then end them.
kill -9 "$rec" 2>/dev/null || true
wait "$rec" 2>/dev/null || true
t kill-server 2>/dev/null || true

# Nothing personal may be in the recording.
if grep -iE "$(id -un)|$(hostname -s)${PRIVATE_WORDS:+|$PRIVATE_WORDS}" "$CAST" >/dev/null; then
	echo "the recording contains personal data; not rendering it" >&2
	exit 1
fi
# Claude Code's symbols (⏺ ⎿ ⏵) are in STIX Two Math; the rest in Menlo.
agg --idle-time-limit 2 --font-size 13 --font-family "Menlo,STIX Two Math,Apple Symbols" \
	--font-dir /System/Library/Fonts --font-dir /System/Library/Fonts/Supplemental "$CAST" "$GIF"
echo "wrote $GIF"
