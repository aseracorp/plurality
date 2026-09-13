#!/bin/bash
# Installed as /app/Plurality by the Dockerfile. Honors DEBUG:
#   DEBUG=1/yes -> /app/run.sh (durable crash capture)
#   otherwise   -> /app/Plurality.bin (stock)
if [ "${DEBUG:-0}" = "1" ] || [ "${DEBUG:-0}" = "yes" ]; then
  exec /app/run.sh
else
  exec /app/Plurality.bin
fi
