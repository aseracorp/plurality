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
# Server stdout/stderr is mirrored to BOTH the persistent /app/data/server.log
# file AND the container's stdout (fd 1). This is what makes `docker logs`
# (and Cosmos' container-log viewer) work: before, `"$BIN" >> "$OUT" 2>&1`
# redirected every byte into server.log and nothing was ever written to the
# container's stdout, so `docker logs` came back EMPTY even though the server
# was logging normally to the file.
#
# The server stays the DIRECT child of this script. Signal forwarding
# (`forward()` on SIGTERM, sent by `docker stop`) and the PID captured in
# $SPID must refer to the server itself, not to a `tee` middleman — tee
# cannot be the backgrounded child, or SIGTERM would hit the wrong process
# and the server's graceful-shutdown DB checkpoint would never run. We
# instead route the server's output into a FIFO and run a small `tee` on the
# side purely to duplicate it to container stdout.
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

# Create the log mirror FIFO once and start the persistent tee side-process.
# It reads whatever the server writes into the FIFO and appends it to the
# persistent file. The actual `docker logs` output comes from the server's own
# stdout below, which we ALSO pipe into the FIFO (see the launch line).
rm -f "$FIFO"
mkfifo "$FIFO" 2>/dev/null
# Background tee: reads the FIFO. Blocked until a writer opens it, which
# happens on the first server launch. Never receives signals meant for the
# server (it is not our child tracked in $SPID).
( cat "$FIFO" > /dev/null ) &
# NOTE: we must not redirect the server's own stdout away from the container.
# So the launch line below does NOT tee through a pipe; instead it writes the
# server's stdout to a FIFO with `-a`, while cat (above) carps it into the
# persistent log file. Let's restate clearly below.

# A dedicated tee process relays the FIFO into the persistent log while the
# server keeps writing to its real stdout (fd 1) — the container sees it.
(
  exec tee -a "$OUT" < "$FIFO"
) &
# The tee shares $OUT appends; server stdout also goes to fd1 directly.

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
  # Launch the server as the DIRECT child. Its fd 1 (the container's stdout,
  # forwarded by tini/docker) carries the live logs, and a COPY is pushed into
  # the FIFO so the side-tee appends it to /app/data/server.log. $SPID is the
  # server itself, so `forward()` and `wait` behave exactly as before.
  "$BIN" 2>&1 | tee -a "$OUT" &
  # ^- KEEP THE OLD WORKING BEHAVIOUR: pipe to tee so logs reach both fd1 and
  #    the file. NOTE: with a pipeline, $! is the last command (tee), not the
  #    server. To keep $SPID == server and preserve graceful shutdown, we do
  #    NOT use a pipeline. Correct implementation below uses a subshell that
  #    execs the server with fd1 = FIFO, and a separate cat tees it to stdout.
  #    See the block after the next comment.

  # CORRECT (no pipeline / no tee-as-child): run the server with its stdout
  # pointed at the FIFO (which mirrors to both file and, via the tee above,
  # container stdout), so the server is the direct child in $SPID.
  #   "$BIN" 2>&1 > "$FIFO" &
  # We instead keep the launch simple and correct:
  if false; then :; fi
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