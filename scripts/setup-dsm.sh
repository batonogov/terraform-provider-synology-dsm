#!/usr/bin/env bash
# Complete the initial DSM setup wizard via API.
# Sets admin password so that write APIs become available.
set -euo pipefail

HOST="${DSM_HOST:-http://localhost:5001}"
USERNAME="${DSM_USERNAME:-admin}"
PASSWORD="${DSM_PASSWORD:-TestAcc123!}"
TIMEOUT="${DSM_SETUP_TIMEOUT:-300}"

echo "Setting up DSM at ${HOST} (timeout: ${TIMEOUT}s)..."

# --- Helpers ---

api_call() {
    local method="$1" endpoint="$2"
    shift 2
    curl -sf -X "${method}" "${HOST}/webapi/entry.cgi${endpoint}" "$@" 2>/dev/null
}

api_get() {
    api_call GET "$@"
}

# --- Step 1: Wait for DSM API ---

end=$((SECONDS + TIMEOUT))
while [ $SECONDS -lt $end ]; do
    if api_get "?api=SYNO.API.Info&version=1&method=query&query=SYNO.API.Auth" 2>/dev/null | grep -q "SYNO.API.Auth"; then
        echo "DSM API is ready"
        break
    fi
    echo "  Waiting for API... ($(( end - SECONDS ))s remaining)"
    sleep 10
done

if [ $SECONDS -ge $end ]; then
    echo "ERROR: DSM API not available within ${TIMEOUT}s" >&2
    exit 1
fi

# --- Step 2: Login with empty password ---

echo "Logging in with empty password..."
LOGIN_RESP=$(api_get "?api=SYNO.API.Auth&version=7&method=login&account=${USERNAME}&passwd=&format=sid&enable_syno_token=yes")

if echo "$LOGIN_RESP" | grep -q '"success":true'; then
    SID=$(echo "$LOGIN_RESP" | grep -o '"sid":"[^"]*"' | cut -d'"' -f4)
    TOKEN=$(echo "$LOGIN_RESP" | grep -o '"synotoken":"[^"]*"' | cut -d'"' -f4)
    echo "Logged in (SID: ${SID:0:8}...)"
else
    echo "ERROR: Login failed" >&2
    echo "$LOGIN_RESP"
    exit 1
fi

# Helper to make authenticated API calls
auth_get() {
    local api="$1" version="$2" method="$3"
    shift 3
    curl -sf "${HOST}/webapi/entry.cgi?api=${api}&version=${version}&method=${method}&_sid=${SID}&SynoToken=${TOKEN}$@" 2>/dev/null
}

# --- Step 3: Discover initialization APIs ---

echo "Probing initialization APIs..."

# Try SYNO.Core.DSM.Initialization
INIT_RESP=$(auth_get "SYNO.Core.DSM.Initialization" "1" "get" 2>/dev/null || echo '{"success":false}')
echo "Initialization status: ${INIT_RESP}" | head -c 200
echo

# --- Step 4: Complete setup ---

echo "Completing initial setup..."

# Try to set admin password via initialization API
# DSM 7 wizard calls this to complete the initial setup
SETUP_RESP=$(curl -sf -X POST "${HOST}/webapi/entry.cgi" \
    -d "api=SYNO.Core.DSM.Initialization" \
    -d "version=1" \
    -d "method=set" \
    -d "_sid=${SID}" \
    -d "SynoToken=${TOKEN}" \
    -d "username=${USERNAME}" \
    -d "password=${PASSWORD}" \
    -d "description=" \
    2>/dev/null || echo '{"success":false}')

echo "Setup response: ${SETUP_RESP}" | head -c 300

if echo "$SETUP_RESP" | grep -q '"success":true'; then
    echo ""
    echo "Setup completed successfully!"
elif echo "$SETUP_RESP" | grep -q '"success":false'; then
    echo ""
    echo "Standard initialization API failed, trying alternative approaches..."

    # Try SYNO.Core.User password change for admin
    PASS_RESP=$(curl -sf -X POST "${HOST}/webapi/entry.cgi" \
        -d "api=SYNO.Core.User" \
        -d "version=1" \
        -d "method=update" \
        -d "_sid=${SID}" \
        -d "SynoToken=${TOKEN}" \
        -d "name=${USERNAME}" \
        -d "password=${PASSWORD}" \
        2>/dev/null || echo '{"success":false}')

    echo "Password update response: ${PASS_RESP}" | head -c 300

    if echo "$PASS_RESP" | grep -q '"success":true'; then
        echo ""
        echo "Password set via user update!"
    else
        # Try without explicit username (admin implied)
        PASS_RESP2=$(curl -sf -X POST "${HOST}/webapi/entry.cgi" \
            -d "api=SYNO.Core.DSM.Initialization" \
            -d "version=1" \
            -d "method=set" \
            -d "_sid=${SID}" \
            -d "SynoToken=${TOKEN}" \
            -d "admin_pwd=${PASSWORD}" \
            -d "confirm_pwd=${PASSWORD}" \
            2>/dev/null || echo '{"success":false}')

        echo "Alt init response: ${PASS_RESP2}" | head -c 300

        if echo "$PASS_RESP2" | grep -q '"success":true'; then
            echo ""
            echo "Setup completed via alternative API!"
        else
            echo ""
            echo "ERROR: Could not complete setup automatically." >&2
            echo "The DSM first-login wizard may need to be completed manually." >&2
            echo "Try accessing ${HOST} in a web browser." >&2

            # Logout
            auth_get "SYNO.API.Auth" "7" "logout" >/dev/null 2>&1 || true
            exit 1
        fi
    fi
fi

# --- Step 5: Wait for DSM to reinitialize ---

echo "Waiting for DSM to reinitialize (up to 120s)..."
sleep 5

end=$((SECONDS + 120))
while [ $SECONDS -lt $end ]; do
    if LOGIN_NEW=$(curl -sf "${HOST}/webapi/entry.cgi?api=SYNO.API.Auth&version=7&method=login&account=${USERNAME}&passwd=${PASSWORD}&format=sid&enable_syno_token=yes" 2>/dev/null); then
        if echo "$LOGIN_NEW" | grep -q '"success":true'; then
            NEW_SID=$(echo "$LOGIN_NEW" | grep -o '"sid":"[^"]*"' | cut -d'"' -f4)
            echo "Successfully logged in with new password (SID: ${NEW_SID:0:8}...)"

            # Verify write APIs work
            VERIFY=$(curl -sf "${HOST}/webapi/entry.cgi?api=SYNO.Core.Group&version=1&method=list&offset=0&limit=1&_sid=${NEW_SID}" 2>/dev/null || echo '{"success":false}')
            if echo "$VERIFY" | grep -q '"success":true'; then
                echo "Write APIs verified - DSM is fully operational!"

                # Logout
                curl -sf "${HOST}/webapi/entry.cgi?api=SYNO.API.Auth&version=7&method=logout&_sid=${NEW_SID}" >/dev/null 2>&1 || true
                exit 0
            else
                echo "Write APIs still blocked, waiting..."
            fi
        fi
    fi
    echo "  Waiting for reinitialize... ($(( end - SECONDS ))s remaining)"
    sleep 10
done

echo "ERROR: DSM did not reinitialize within 120s" >&2
exit 1
