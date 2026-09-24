#!/bin/sh
# Durable crash capture + auto-restart supervisor for the Plurality server.
#
# Runs /app/Plurality as a child. Whenever the server process exits for any
# reason it records the exact exit code / signal plus recent server output to
# /app/data/crash.log (persistent volume), then immediately restarts it. This
# (a) makes every death observable even when nothing else captures stderr and
# (b) keeps the UI available after a crash instead of waiting for an external
# restart.
#
# PID 1 is still tini (see dockerfile ENTRYPOINT) so zombies are reaped and
# signals (SIGTERM on `docker stop`) are forwarded to this script, which
# forwards them to the server and exits cleanly.
set -u

# Enable core dumps + full Go crash traces so a native crash (C segv in
# sqlite/vec/http) leaves evidence on the persistent volume instead of dying
# invisibly. ulimit -c unlimited allows core files; GOTRACEBACK=crash makes
# the Go runtime print ALL goroutine stacks before aborting on a fatal/native
# error. Coredumps land in /app/data/cores (persistent).
mkdir -p /app/data/cores
ulimit -c unlimited 2>/dev/null || true
export GOTRACEBACK=crash

LOG=/app/data/crash.log
OUT=/app/data/server.log
mkdir -p /app/data
echo "=== supervisor.sh $(date -u +%FT%T) pid $$ (trap TERM/INT) ===" >> "$LOG"

forward() {
  echo "=== supervisor: received signal $(date -u +%FT%T); stopping server ===" >> "$LOG"
  kill -TERM "$SPID" 2>/dev/null
  exit 0
}
trap forward TERM INT

# PR #30 made /app/Plurality a SHIM that execs this supervisor, so running
# /app/Plurality here would recurse infinitely (startup loop). Run the real
# binary; fall back to the old direct path on pre-#30 images.
BIN=/app/Plurality.bin
[ -x "$BIN" ] || BIN=/app/Plurality
restart_delay=0
while true; do
  echo "=== $(date -u +%FT%T) starting $BIN (attempt after ${restart_delay}s) ===" >> "$LOG"
  "$BIN" >> "$OUT" 2>&1 &
  SPID=$!
  wait "$SPID"
  rc=$?
  {
    echo "== server exited rc=$rc at $(date -u +%FT%T) =="
    echo "--- uptime $(awk '{printf "%.1fs", $1}' /proc/uptime) ---"
    echo "--- server log tail ---"
    tail -n 40 "$OUT" 2>/dev/null | tail -n 40
    echo "--- end ---"
  } >> "$LOG"
  # brief pause so a hard crash loop doesn't spin the CPU; then restart
  sleep 2
done
