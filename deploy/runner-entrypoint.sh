#!/bin/sh
# file: deploy/runner-entrypoint.sh
#!/bin/sh
set -e

# Each replica must have a distinct consumer name in the Redis consumer group: two runners
# sharing an identity would each see the other's pending entries as their own, and
# heartbeats would cross. The container hostname is unique per replica, so derive from it.
export ARENA_RUNNER_ID="${ARENA_RUNNER_ID:-runner-$(hostname)}"

# Fail loudly and early if the socket is missing, rather than judging every submission as
# an internal error for the next hour.
if [ "${ARENA_SANDBOX:-docker}" = "docker" ] && [ ! -S /var/run/docker.sock ]; then
  echo "FATAL: /var/run/docker.sock is not mounted; the runner cannot create sandboxes" >&2
  exit 1
fi

mkdir -p "${ARENA_BOX_ROOT:-/var/tmp/arena-boxes}"
chmod 0777 "${ARENA_BOX_ROOT:-/var/tmp/arena-boxes}"

echo "runner id=$ARENA_RUNNER_ID slots=${ARENA_RUNNER_SLOTS:-2} sandbox=${ARENA_SANDBOX:-docker}"
exec /app/runner "$@"
