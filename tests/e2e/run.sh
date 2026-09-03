#!/usr/bin/env bash
# End-to-end harness: installs the real reloop binary and drives it the
# way a user would from a shell. It covers only what needs real
# processes and real time - the daemon firing jobs, crash recovery,
# the keep-alive loop, and the supervisor. Flag parsing, exit codes,
# and output text live in the script tests under
# cmd/reloop/testdata/script and are not repeated here.
#
#   bash tests/e2e/run.sh                build, install, and test locally
#   RELOOP_BIN=/usr/local/bin/reloop run.sh    drive an already-installed binary
#
# Every command runs against an isolated --data-dir, so a real reloop
# installation on the same machine is never touched. The one exception
# is the supervisor section: the launchd/systemd unit path is global
# per user, so that section runs only with RELOOP_E2E_SUPERVISOR set
# and refuses when a unit already exists. Needs bash 3.2+ (macOS
# default) and either Go or a prebuilt binary via RELOOP_BIN.

set -u

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/reloop-e2e.XXXXXX")
DATA="$WORK/data"
OUT="$WORK/out.txt"
DAEMON_LOG="$WORK/daemon.log"
DPID=""
LOOP_PID=""
INSTALLED_SUPERVISOR=""
PASS=0
FAIL=0

# The crash test intentionally orphans the job's process tree; the odd
# sleep duration lets cleanup kill it without matching anything else.
ORPHAN_MARK="sleep 8787"

cleanup() {
	# A mid-section failure must not leave a supervisor unit pointing at
	# the about-to-be-deleted harness binary.
	[ -n "$INSTALLED_SUPERVISOR" ] && reloop uninstall >/dev/null 2>&1
	[ -n "$LOOP_PID" ] && kill -9 "$LOOP_PID" 2>/dev/null
	[ -n "$DPID" ] && kill -9 "$DPID" 2>/dev/null
	pkill -f "$ORPHAN_MARK" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

section() { printf '\n== %s ==\n' "$1"; }

# run captures combined output and the exit code for the asserts below.
run() {
	"$@" >"$OUT" 2>&1
	EXIT=$?
}

ok() { PASS=$((PASS + 1)); printf 'PASS  %s\n' "$1"; }
bad() {
	FAIL=$((FAIL + 1))
	printf 'FAIL  %s\n' "$1"
	sed 's/^/      | /' "$OUT"
}

assert_exit() { # expected description
	if [ "$EXIT" -eq "$1" ]; then ok "$2"; else bad "$2 (exit $EXIT, want $1)"; fi
}

assert_grep() { # pattern description
	if grep -Eq "$1" "$OUT"; then ok "$2"; else bad "$2 (missing /$1/)"; fi
}

assert_not_grep() { # pattern description
	if grep -Eq "$1" "$OUT"; then bad "$2 (found /$1/)"; else ok "$2"; fi
}

# wait_until polls instead of fixed sleeps: fast on a quiet machine,
# tolerant on a loaded CI runner.
wait_until() { # seconds cmd...
	local deadline=$1 i=0
	shift
	while [ "$i" -lt $((deadline * 10)) ]; do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 0.1
		i=$((i + 1))
	done
	return 1
}

section "install"
if [ -n "${RELOOP_BIN:-}" ]; then
	ok "using preinstalled binary: $RELOOP_BIN"
else
	BINDIR="$WORK/prefix/bin"
	mkdir -p "$BINDIR"
	if (cd "$ROOT" && go build -o "$BINDIR/reloop" ./cmd/reloop) >"$OUT" 2>&1; then
		ok "built and installed reloop into $BINDIR"
	else
		bad "go build ./cmd/reloop"
		exit 1
	fi
	RELOOP_BIN="$BINDIR/reloop"
fi

reloop() { "$RELOOP_BIN" --data-dir "$DATA" --quiet "$@"; }

daemon_up() { reloop status 2>/dev/null | grep -q 'daemon: *running'; }
daemon_down() { reloop status 2>/dev/null | grep -q 'daemon: *stopped'; }
job_done() { reloop show "$1" --json 2>/dev/null | grep -q '"status": "done"'; }
# Plain logs prints only the most recent run; --since widens the window
# so this counts every run's output.
run_count() { reloop logs "$1" --since 1h 2>/dev/null | grep -c "$2"; }
daemon_pid() { reloop status 2>/dev/null | sed -n 's/.*pid=\([0-9][0-9]*\).*/\1/p'; }
# respawned reports a daemon pid different from OLD_DPID.
respawned() { P=$(daemon_pid) && [ -n "$P" ] && [ "$P" != "$OLD_DPID" ]; }

start_daemon() {
	# Background the binary directly, not the reloop() function: a function
	# backgrounds as a subshell, and $! would name that subshell instead
	# of the daemon, so the kill -9 crash test would miss.
	"$RELOOP_BIN" --data-dir "$DATA" --quiet daemon >>"$DAEMON_LOG" 2>&1 &
	DPID=$!
	if wait_until 10 daemon_up; then
		ok "daemon started (pid $DPID)"
	else
		run cat "$DAEMON_LOG"
		bad "daemon failed to start"
		exit 1
	fi
}

section "fresh box"
run "$RELOOP_BIN" --help
assert_exit 0 "reloop --help exits 0"
run reloop status
assert_exit 0 "status on a fresh data dir exits 0"
assert_grep 'daemon: *stopped' "status reports stopped"

section "daemon lifecycle"
start_daemon

section "one-shot job"
run reloop add --at '+2s' --name shot -- 'echo hello-from-oneshot'
assert_exit 0 "one-shot accepted"
assert_grep 'added job' "add confirms the job"
run reloop status
assert_grep 'oneshot pending=1' "status counts the pending one-shot"
if wait_until 15 job_done shot; then
	ok "one-shot fired and completed"
else
	run reloop show shot
	bad "one-shot never completed"
fi
run reloop logs shot
assert_grep 'hello-from-oneshot' "one-shot output captured in logs"
run reloop show shot
assert_grep 'last result: *ok' "one-shot recorded ok"

section "cron job"
run reloop add --cron '@every 1s' --name tick -- 'echo tick-tock'
assert_exit 0 "cron accepted"
tick_twice() { [ "$(run_count tick tick-tock)" -ge 2 ]; }
if wait_until 10 tick_twice; then
	ok "cron fired at least twice"
else
	run reloop logs tick
	bad "cron did not fire twice"
fi
run reloop disable tick
assert_exit 0 "cron disabled"
# A run claimed before the disable may record after it. Poll until the
# count holds still for a full second, then verify a longer quiet
# window; a fixed grace sleep false-failed on loaded runners.
N1=$(run_count tick tick-tock)
for _ in 1 2 3 4 5 6 7 8; do
	sleep 1
	N=$(run_count tick tick-tock)
	[ "$N" -eq "$N1" ] && break
	N1=$N
done
sleep 2.5
N2=$(run_count tick tick-tock)
if [ "$N1" -eq "$N2" ]; then
	ok "disabled cron stopped firing ($N1 runs)"
else
	: >"$OUT"
	bad "disabled cron kept firing ($N1 -> $N2 runs)"
fi
run reloop enable tick
assert_exit 0 "cron re-enabled"
tick_resumed() { [ "$(run_count tick tick-tock)" -gt "$N2" ]; }
if wait_until 5 tick_resumed; then
	ok "re-enabled cron resumed firing"
else
	run reloop logs tick
	bad "re-enabled cron never resumed"
fi
run reloop rm tick
assert_exit 0 "cron removed"
run reloop show tick
assert_exit 3 "removed cron is gone"

section "failing job"
run reloop add --cron '@every 1s' --name kaboom -- 'echo boom >&2; exit 3'
assert_exit 0 "failing job accepted"
kaboom_failed() { reloop show kaboom 2>/dev/null | grep -Eq 'last result: *fail'; }
if wait_until 10 kaboom_failed; then
	ok "failure recorded as fail"
else
	run reloop show kaboom
	bad "failure never recorded"
fi
run reloop logs kaboom
assert_grep 'boom' "stderr of the failing job captured"
run reloop rm kaboom
assert_exit 0 "failing job removed"

section "crash recovery"
run reloop add --at '+1s' --name sleeper -- "$ORPHAN_MARK"
assert_exit 0 "long one-shot accepted"
in_flight() { reloop status 2>/dev/null | grep -q 'in flight'; }
if wait_until 10 in_flight; then
	ok "one-shot claimed and running"
else
	run reloop status
	bad "one-shot never went in flight"
fi
kill -9 "$DPID"
wait "$DPID" 2>/dev/null
DPID=""
run reloop status
assert_grep 'daemon: *stopped' "kill -9 leaves the daemon stopped"
assert_grep 'interrupted: 1 one-shot' "crash leaves a visible claimed one-shot"
start_daemon
if wait_until 10 job_done sleeper; then
	ok "startup recovery resolved the interrupted one-shot"
else
	run reloop show sleeper
	bad "recovery never resolved the one-shot"
fi
run reloop show sleeper
assert_grep 'last result: *interrupted' "outcome recorded as interrupted"
run reloop logs sleeper
assert_grep 'outcome unknown' "interruption note in logs"
pkill -f "$ORPHAN_MARK" 2>/dev/null
run reloop ls
assert_grep 'shot' "jobs survived the daemon crash"

section "graceful stop"
run reloop stop
assert_exit 0 "stop exits 0"
if wait_until 10 daemon_down; then
	ok "daemon stopped on request"
else
	bad "daemon did not stop"
fi
wait "$DPID" 2>/dev/null
DPID=""

section "keep-alive loop"
# The README's no-supervisor loop. A crash restarts the daemon and
# 'reloop stop' ends the loop. Stderr is dropped because the shell
# prints "Killed" during the crash test.
sh -c "until \"$RELOOP_BIN\" --data-dir \"$DATA\" --quiet daemon >>\"$DAEMON_LOG\" 2>&1; do sleep 1; done" 2>/dev/null &
LOOP_PID=$!
if wait_until 10 daemon_up; then
	ok "loop started the daemon"
else
	run cat "$DAEMON_LOG"
	bad "loop never started the daemon"
fi
OLD_DPID=$(daemon_pid)
kill -9 "$OLD_DPID" 2>/dev/null
if wait_until 15 respawned; then
	ok "crash respawned the daemon (pid $OLD_DPID -> $(daemon_pid))"
else
	run reloop status
	bad "loop never respawned after kill -9"
fi
run reloop stop
assert_exit 0 "stop under the loop exits 0"
loop_gone() { ! kill -0 "$LOOP_PID" 2>/dev/null; }
if wait_until 10 loop_gone; then
	ok "clean stop ended the loop"
else
	kill -9 "$LOOP_PID" 2>/dev/null
	bad "loop survived a clean stop"
fi
LOOP_PID=""
if wait_until 5 daemon_down; then
	ok "daemon stayed stopped after a clean stop"
else
	bad "daemon respawned after a clean stop"
fi

section "supervisor (real reloop install)"
# Exercises the actual launchd/systemd path: bootstrap, crash respawn,
# idempotent reinstall, stop-stays-stopped, uninstall. Guarded because
# the unit path is global per user:
#   RELOOP_E2E_SUPERVISOR=1     run, hard-fail if install fails
#   RELOOP_E2E_SUPERVISOR=auto  run, soft-skip when the runner blocks
#                            bootstrap (locked-down CI sessions)
#   unset                    skip
UNIT=""
case "$(uname -s)" in
Darwin) UNIT="$HOME/Library/LaunchAgents/dev.reloop.reloopd.plist" ;;
Linux) UNIT="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/reloopd.service" ;;
esac
SUP="${RELOOP_E2E_SUPERVISOR:-}"
if [ -z "$SUP" ]; then
	printf 'skipped: set RELOOP_E2E_SUPERVISOR=1 (or auto) to test real reloop install\n'
elif [ -z "$UNIT" ]; then
	printf 'skipped: no launchd/systemd on this platform\n'
elif [ -e "$UNIT" ]; then
	printf 'skipped: %s exists; refusing to touch a real reloop install\n' "$UNIT"
else
	run reloop install
	if [ "$EXIT" -ne 0 ] && [ "$SUP" = "auto" ]; then
		printf 'skipped: reloop install unavailable on this runner\n'
		sed 's/^/      | /' "$OUT"
	else
		assert_exit 0 "reloop install exits 0"
		INSTALLED_SUPERVISOR=1
		if [ -e "$UNIT" ]; then ok "unit file written"; else bad "unit file missing at $UNIT"; fi
		if wait_until 15 daemon_up; then
			ok "supervisor started the daemon"
		else
			run reloop status
			bad "supervised daemon never came up"
		fi
		run reloop status
		assert_grep 'supervised=yes' "status reports supervised"
		run reloop add --at '+2s' --name supshot -- 'echo hello-supervised'
		assert_exit 0 "one-shot accepted under supervisor"
		if wait_until 15 job_done supshot; then
			ok "supervised daemon serves the harness data dir"
		else
			run reloop show supshot
			bad "supervised daemon never ran the one-shot"
		fi
		OLD_DPID=$(daemon_pid)
		kill -9 "$OLD_DPID" 2>/dev/null
		if wait_until 20 respawned; then
			ok "supervisor respawned after crash (pid $OLD_DPID -> $(daemon_pid))"
		else
			run reloop status
			bad "supervisor never respawned the daemon"
		fi
		run reloop install
		assert_exit 0 "reinstall exits 0"
		run reloop stop
		assert_exit 0 "stop under supervisor exits 0"
		if wait_until 10 daemon_down; then
			ok "daemon stopped on request"
		else
			bad "daemon did not stop"
		fi
		# A clean exit must not be treated as a crash; give the
		# supervisor a respawn window and confirm it stayed quiet.
		sleep 4
		if daemon_down; then
			ok "clean stop stays stopped under supervisor"
		else
			run reloop status
			bad "supervisor respawned after a clean stop"
		fi
		run reloop uninstall
		assert_exit 0 "uninstall exits 0"
		if [ -e "$UNIT" ]; then bad "unit file still present after uninstall"; else ok "unit file removed"; fi
		INSTALLED_SUPERVISOR=""
	fi
fi

section "summary"
printf '%d passed, %d failed (binary: %s)\n' "$PASS" "$FAIL" "$RELOOP_BIN"
[ "$FAIL" -eq 0 ] || exit 1
exit 0