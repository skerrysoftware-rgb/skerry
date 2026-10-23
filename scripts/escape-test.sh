#!/bin/sh
# Attacks a running skerry workspace from inside its VM and checks that
# every boundary holds. Run it from inside a workspace whose VM may be
# disturbed (it plants files and probes the network):
#
#   cd .claude/worktrees/some-test-workspace && scripts/escape-test.sh
#
# It exits non-zero if any attack succeeds.
set -u

SKERRY=${SKERRY:-skerry}
failed=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; failed=1; }

# expect_fail NAME CMD: the command, run inside the VM, must fail.
expect_fail() {
	name=$1
	shift
	if "$SKERRY" exec -- sh -c "$*" >/dev/null 2>&1; then fail "$name"; else pass "$name"; fi
}

# --- Network: only the public internet is reachable.
"$SKERRY" exec -- curl -sS -m 10 -o /dev/null https://registry.npmjs.org/ >/dev/null 2>&1 &&
	pass "public internet is reachable" || fail "public internet is reachable"
lan=$(ipconfig getifaddr en0 2>/dev/null || true)
for target in 169.254.169.254:80 192.168.127.1:22 192.168.127.1:53 10.0.0.1:80 172.17.0.1:80 \
	100.100.100.200:80 192.0.0.192:80 ${lan:+$lan:22}; do
	expect_fail "can't reach $target" "curl -sS -m 5 -o /dev/null http://$target/"
done
expect_fail "can't send mail (port 25)" "curl -sS -m 5 -o /dev/null telnet://smtp.gmail.com:25"

# --- Files: only the checkout is shared.
expect_fail "can't read the host's home directory" "ls $HOME/.ssh"
expect_fail "can't read the host's /etc" "cat /private/etc/hosts"
# The system image is shared read-only: writes land in memory and are
# gone after a restart.
"$SKERRY" exec -- sh -c 'echo planted > /usr/bin/planted' >/dev/null 2>&1
"$SKERRY" down >/dev/null && "$SKERRY" up --quiet
expect_fail "writes to the system image don't persist" "test -e /usr/bin/planted"

# --- Git: the VM can't redirect host git.
expect_fail "can't rewrite the .git pointer" 'echo "gitdir: /tmp/evil" > .git'
"$SKERRY" exec -- sh -c 'mkdir -p planted/.git && printf "[core]\n\tfsmonitor = ./x.sh\n" > planted/.git/config' >/dev/null 2>&1
"$SKERRY" exec -- true >/dev/null 2>&1 # every command ends with a scan
if [ -e planted/.git/config ]; then fail "planted repository is quarantined"; else pass "planted repository is quarantined"; fi
"$SKERRY" exec -- sh -c 'mkdir -p .claude && echo "{\"hooks\":{}}" > .claude/settings.local.json' >/dev/null 2>&1
"$SKERRY" exec -- true >/dev/null 2>&1
if [ -e .claude/settings.local.json ]; then fail "planted Claude settings are quarantined"; else pass "planted Claude settings are quarantined"; fi

# --- Other workspaces: no route between VMs.
expect_fail "can't reach another workspace's services" "curl -sS -m 5 -o /dev/null http://192.168.127.3:5432/"

[ "$failed" -eq 0 ] && echo "all boundaries held" || echo "SOME BOUNDARIES FAILED"
exit "$failed"
