#!/bin/bash
# Plurality server launcher — durable crash capture when DEBUG=1.
#
# Dockerfile builds the real binary as /app/Plurality.bin and installs a tiny
# shim at /app/Plurality that honors DEBUG. This file is the DEBUG path:
# when DEBUG=1, run.sh tees all output to /app/data/server.log and records
# the exact exit code / signal + last stderr lines to /app/data/crash.log on
# death. DEBUG unset: shim just execs /app/Plurality.bin directly (stock).
set -u
LOG=/app/data/server.log
CRASH=/app/data/crash.log
mkdir -p /app/data
echo "=== run.sh $(date -u +%FT%T) pid=$$ DEBUG=${DEBUG:-0} ===" >> "$LOG"
(
  exec /app/Plurality.bin
) 2>&1 | tee -a "$LOG"
rc="${PIPESTATUS[0]}"
{
  echo "== run.sh: server exited rc=$rc at $(date -u +%FT%T) =="
  echo "-- uptime $(awk '{printf "%.1fs", $1}' /proc/uptime) --"
  echo "-- last stderr tail --"
  tail -n 40 "$LOG" 2>/dev/null | tail -n 40
  echo "-- end --"
} >> "$CRASH" 2>&1
echo "run.sh: exited rc=$rc" >> "$LOG"
exit $rc
