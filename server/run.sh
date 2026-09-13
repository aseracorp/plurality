#!/bin/bash
# Plurality server launcher with optional crash telemetry (DEBUG=1).
#
# Container orchestrators (e.g. Cosmos) often override the Dockerfile CMD with
# just `/app/Plurality`, killing the stock `tee` capture — so crashes become
# invisible. This wrapper restores durable capture and records the exit reason.
#
#   DEBUG=1 /app/run.sh   -> capture stdout/stderr + exit code to /app/data
#   /app/Plurality        -> default, stock behavior
#
set -u
LOG=/app/data/server.log
CRASH=/app/data/crash.log
mkdir -p /app/data

echo "=== run.sh starting $(date -u +%FT%T) pid=$$ DEBUG=${DEBUG:-0} ===" >> "$LOG"

# Stock path (DEBUG unset): plain exec, stock behavior.
if [ "${DEBUG:-0}" != "1" ] && [ "${DEBUG:-0}" != "yes" ]; then
  exec /app/Plurality
fi

# DEBUG path: capture everything and record the exit code.
(
  exec /app/Plurality
) 2>&1 | tee -a "$LOG"
rc="${PIPESTATUS[0]}"
{
  echo "=== run.sh: server exited rc=$rc at $(date -u +%FT%T) ==="
  echo "  -- uptime $(awk '{printf "%.1fs", $1}' /proc/uptime) --"
  echo "  -- last stderr tail --"
  tail -n 30 "$LOG" 2>/dev/null | tail -n 30
  echo "  -- end --"
} >> "$CRASH" 2>&1
echo "run.sh: server exited rc=$rc; re-spawning in 2s..." >> "$LOG"
sleep 2
exit $rc
