#!/usr/bin/env bash
# Boots the systemd-enabled e2e container and runs the harness in it
# as an unprivileged lingering user, so 'reloop install' talks to a real
# systemd --user manager end to end. Run from anywhere:
#
#   bash tests/e2e/systemd.sh
#
# Needs Docker. --privileged is required for systemd as PID 1.

set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
IMG=reloop-e2e-systemd

docker build -f "$ROOT/tests/e2e/Dockerfile.systemd" -t "$IMG" "$ROOT"

CID=$(docker run -d --privileged --cgroupns=host \
	-v /sys/fs/cgroup:/sys/fs/cgroup:rw "$IMG")
trap 'docker rm -f "$CID" >/dev/null' EXIT

# Boot can settle as "degraded" (units irrelevant to us may fail in a
# container); only the user manager below has to work.
echo "waiting for systemd to boot..."
for i in $(seq 1 60); do
	state=$(docker exec "$CID" systemctl is-system-running 2>/dev/null || true)
	case "$state" in running | degraded) break ;; esac
	[ "$i" -eq 60 ] && {
		echo "systemd never booted (state: ${state:-none})"
		exit 1
	}
	sleep 1
done

# Lingering starts tester's user manager without a login session and
# creates /run/user/<uid>, where systemctl --user finds its bus.
docker exec "$CID" loginctl enable-linger tester
TUID=$(docker exec "$CID" id -u tester)
for i in $(seq 1 30); do
	if docker exec "$CID" test -S "/run/user/$TUID/bus"; then break; fi
	[ "$i" -eq 30 ] && {
		echo "user bus for tester never appeared"
		exit 1
	}
	sleep 1
done

docker exec -u tester \
	-e HOME=/home/tester \
	-e XDG_RUNTIME_DIR="/run/user/$TUID" \
	-e DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$TUID/bus" \
	-e RELOOP_BIN=/usr/local/bin/reloop \
	-e RELOOP_E2E_SUPERVISOR=1 \
	"$CID" bash /usr/local/share/reloop/e2e.sh