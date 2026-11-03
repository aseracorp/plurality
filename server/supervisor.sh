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
# Server output is mirrored to BOTH the persistent /app/data/server.log file
# AND the container's stdout (fd 1). This makes `docker logs` (and Cosmos'
# container-log viewer) work: before, `"$BIN" >> "$OUT" 2>&1` redirected every
# byte into server.log and nothing ever reached container stdout, so `docker
# logs` came back EMPTY for a healthy, logging server (observed live on
# Simplify).
#
# Mechanical design: the server stays the DIRECT child of this script ($SPID
# is the server, never a pipeline middleman), so `wait $SPID` and the SIGTERM
# forward on `docker stop` still reach the server itself and its graceful
# shutdown DB checkpoint still runs. Its stdout+stderr are pointed at a FIFO;
# a sidecar `tee` reads the FIFO and appends to server.log while also echoing
# to container stdout. The sidecar is recreated on every launch (it exits on
# EOF when the server stops).
#
# PID 1 is still tini (see dockerfile ENTRYPOINT) so zombies are reaped and
# signals (SIGTERM on `docker stop`) are forwarded to this script.
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
FIFO=/tmp/server-log.fifo
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

  # (Re)create the log-mirror FIFO + sidecar tee for this launch. tee reads the
  # FIFO, appends to the persistent server.log AND writes to container stdout
  # (this script's fd 1). It exits on EOF when the server stops, so we recreate
  # it each iteration.
  rm -f "$FIFO"
  mkfifo "$FIFO" 2>/dev/null
  (
    exec tee -a "$OUT"
  ) < "$FIFO" &
  TEE_PID=$!

  # Launch the server as the DIRECT child with both stdout and stderr into the
  # FIFO. Redirection order matters: `> "$FIFO" 2>&1` (stdout first, then stderr
  # follows it) — the reverse would leave stderr on the container's stdout.
  "$BIN" > "$FIFO" 2>&1 &
  SPID=$!

  wait "$SPID"
  rc=$?

  # Close the FIFO writer-side so the sidecar tee sees EOF and exits cleanly;
  # kill it only if it lingers.
  kill "$TEE_PID" 2>/dev/null
  wait "$TEE_PID" 2>/dev/null

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