#!/usr/bin/env bash
# End-to-end smoke test for crate-html.
#
# Builds the binaries, starts crated in an isolated XDG home on a non-default
# port (so it won't collide with a running daemon), pushes every fixture in
# testdata/sites/, verifies each is reachable via HTTP, exercises ls/rm/auth/
# traversal-block, and cleans up.
#
# Exit 0 = all checks passed. Exit 1 = something failed; the daemon's stderr
# log is dumped before exit so you can see what happened.
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${SMOKE_PORT:-17777}"
TOKEN="smoketoken"

# --- build ------------------------------------------------------------------
echo "==> build"
go build -o bin/crate ./cmd/crate
go build -o bin/crated ./cmd/crated

# --- isolate XDG home -------------------------------------------------------
SMOKE_HOME="$(mktemp -d)"
export XDG_CONFIG_HOME="$SMOKE_HOME/config"
export XDG_DATA_HOME="$SMOKE_HOME/data"
export XDG_STATE_HOME="$SMOKE_HOME/state"
mkdir -p "$XDG_CONFIG_HOME/crate"

cat > "$XDG_CONFIG_HOME/crate/config.yaml" <<EOF
port: $PORT
listen_addr: 127.0.0.1:$PORT
base_url: http://localhost:$PORT
token: $TOKEN
EOF

# --- start crated -----------------------------------------------------------
echo "==> start crated on :$PORT"
./bin/crated > "$SMOKE_HOME/crated.log" 2>&1 &
CRATED_PID=$!

dump_log_and_exit() {
  local rc=$?
  echo
  echo "--- crated.log ---"
  cat "$SMOKE_HOME/crated.log" 2>/dev/null || true
  echo "------------------"
  kill "$CRATED_PID" 2>/dev/null || true
  wait "$CRATED_PID" 2>/dev/null || true
  rm -rf "$SMOKE_HOME"
  exit "$rc"
}
trap dump_log_and_exit EXIT

# Wait up to 2s for the daemon to come up.
for _ in $(seq 1 20); do
  if ./bin/crate status >/dev/null 2>&1; then break; fi
  sleep 0.1
done
./bin/crate status >/dev/null

PASS=0
FAIL=0
check() {
  if eval "$2"; then
    echo "  ✓ $1"
    PASS=$((PASS+1))
  else
    echo "  ✗ $1"
    FAIL=$((FAIL+1))
  fi
}

http_code() {
  curl -sS -o /dev/null -w "%{http_code}" "$1"
}

# --- push every fixture, verify HTTP -----------------------------------------
echo "==> push fixtures"
for dir in testdata/sites/*/; do
  [ -d "$dir" ] || continue
  name=$(basename "$dir")
  # Skip the README directory at testdata/sites/
  [ -f "$dir/index.html" ] || continue

  ./bin/crate push "$dir" "$name" >/dev/null
  code=$(http_code "http://localhost:$PORT/$name/")
  check "$name serves (got $code)" "[ \"$code\" = 200 ]"
done

# --- built-in cratesplainer (no disk site for it) ----------------------------
echo "==> built-in"
check "cratesplainer (built-in) serves" \
  "[ \"\$(http_code http://localhost:$PORT/cratesplainer/)\" = 200 ]"
check "cratesplainer/style.css serves" \
  "[ \"\$(http_code http://localhost:$PORT/cratesplainer/style.css)\" = 200 ]"

# --- ls / rm round-trip ------------------------------------------------------
echo "==> ls + rm"
check "ls shows welcome" "./bin/crate ls | grep -q '^welcome'"
./bin/crate rm welcome >/dev/null
check "rm welcome removes it" "! ./bin/crate ls | grep -q '^welcome'"

# --- security checks ---------------------------------------------------------
echo "==> security"
check "path traversal blocked" \
  "[ \"\$(http_code 'http://localhost:$PORT/overview/../../etc/passwd')\" = 404 ]"
check "missing token on PUT yields 401" \
  "[ \"\$(curl -sS -o /dev/null -w '%{http_code}' -X PUT --data-binary @/dev/null http://localhost:$PORT/api/sites/x)\" = 401 ]"
check "wrong token on PUT yields 401" \
  "[ \"\$(curl -sS -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer wrong' -X PUT --data-binary @/dev/null http://localhost:$PORT/api/sites/x)\" = 401 ]"

# --- status ------------------------------------------------------------------
echo "==> status"
check "GET /api/status returns 200" \
  "[ \"\$(http_code http://localhost:$PORT/api/status)\" = 200 ]"

# --- summary -----------------------------------------------------------------
echo
echo "==> $PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
  exit 1
fi
trap - EXIT
kill "$CRATED_PID" 2>/dev/null || true
wait "$CRATED_PID" 2>/dev/null || true
rm -rf "$SMOKE_HOME"
echo "PASS"
