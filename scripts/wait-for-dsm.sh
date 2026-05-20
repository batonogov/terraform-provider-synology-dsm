#!/usr/bin/env bash
# Wait for Synology DSM to be ready and API accessible.
set -euo pipefail

HOST="${DSM_HOST:-http://localhost:5001}"
TIMEOUT="${DSM_TIMEOUT:-1200}" # 20 minutes default (QEMU emulation is slow)

echo "Waiting for DSM at ${HOST} (timeout: ${TIMEOUT}s)..."

end=$((SECONDS + TIMEOUT))
while [ $SECONDS -lt $end ]; do
  if curl -sf "${HOST}/webapi/entry.cgi?api=SYNO.API.Info&version=1&method=query&query=SYNO.API.Auth" 2>/dev/null | grep -q "SYNO.API.Auth"; then
    echo "DSM is ready!"
    exit 0
  fi
  echo "  Waiting... ($(( end - SECONDS ))s remaining)"
  sleep 15
done

echo "ERROR: DSM did not become ready within ${TIMEOUT}s" >&2
exit 1
