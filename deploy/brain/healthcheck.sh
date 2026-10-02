#!/bin/sh
# Brain liveness probe with a restart watchdog.
#
# Compose never restarts an unhealthy container, so a Brain process that
# keeps running without serving /health stays down indefinitely. After the
# current process has answered at least once, consecutive failures terminate
# it and restart: unless-stopped replaces the container. Startup failures are
# left to start_period and `compose up --wait`.
set -u

limit=${BRAIN_HEALTH_RESTART_FAILURES:-20}
state=/tmp/brain-health

child=$(cat /proc/1/task/1/children 2>/dev/null || true)
child=${child%% *}
started=
if [ -n "$child" ]; then
	started=$(cut -d ' ' -f 22 "/proc/$child/stat" 2>/dev/null || true)
fi

# GNU wget retries a timed-out read up to 20 times; one bounded attempt must
# finish inside the 5-second Docker probe timeout or no failure is recorded.
if timeout 4 wget -q -t 1 -T 3 -O /dev/null http://127.0.0.1:3000/health; then
	printf '%s\n' "$started" > "$state.served"
	rm -f "$state.failures"
	exit 0
fi

# Count only failures of a process that has served before; the start time
# distinguishes it from a replacement after an in-place restart.
[ -n "$started" ] || exit 1
[ "$(cat "$state.served" 2>/dev/null || true)" = "$started" ] || exit 1

failures=1
read -r previous count 2>/dev/null < "$state.failures" || previous=
if [ "$previous" = "$started" ]; then
	failures=$((count + 1))
fi
printf '%s %s\n' "$started" "$failures" > "$state.failures"

if [ "$failures" -eq "$limit" ]; then
	echo "brain health failed $failures times; sending SIGTERM to $child" > /proc/1/fd/2
	kill -TERM "$child" 2>/dev/null || true
elif [ "$failures" -gt "$limit" ]; then
	echo "brain did not stop after SIGTERM; sending SIGKILL to $child" > /proc/1/fd/2
	kill -KILL "$child" 2>/dev/null || true
fi
exit 1
