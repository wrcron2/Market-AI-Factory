#!/bin/bash
# Market-AI-Factory smoke driver — build + launch backend and frontend on
# scratch ports (9180/9100) with a throwaway DB, verify endpoints and routes,
# screenshot the Products grid, clean up.
#
# Usage:  .claude/skills/run-market-ai-factory/smoke.sh [workdir]
# Exit 0 = all checks passed. Screenshot: $WORK/factory.png
set -u
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
WORK="${1:-$(mktemp -d)}"
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
PASS=0; FAIL=0
BACKEND_PID=""; VITE_PID=""

check() {
  if [ "$2" = "$3" ]; then echo "PASS  $1 ($3)"; PASS=$((PASS+1));
  else echo "FAIL  $1 (expected $2, got $3)"; FAIL=$((FAIL+1)); fi
}
cleanup() {
  [ -n "$BACKEND_PID" ] && kill "$BACKEND_PID" 2>/dev/null
  [ -n "$VITE_PID" ] && kill "$VITE_PID" 2>/dev/null
}
trap cleanup EXIT

echo "== workdir: $WORK"

# ── Backend ──────────────────────────────────────────────────────────────────
(cd "$ROOT/backend" && go build -o "$WORK/factory-server" ./cmd/server) || { echo "FAIL backend build"; exit 1; }
echo "PASS  backend build"
FACTORY_PORT=9180 FACTORY_DB_DSN="$WORK/factory.db" "$WORK/factory-server" > "$WORK/backend.log" 2>&1 &
BACKEND_PID=$!
sleep 2
check "backend /api/health"   200 "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:9180/api/health)"
check "backend /api/products" 200 "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:9180/api/products)"
check "products empty-DB body" '{"products":[]}' "$(curl -s http://localhost:9180/api/products | tr -d ' \n')"

# ── Frontend (vite on scratch port, proxy target overridden is not needed:
#    grid renders without backend data; API checks above cover the backend) ──
(cd "$ROOT/frontend" && npx vite --port 9100 --strictPort > "$WORK/vite.log" 2>&1) &
VITE_PID=$!
sleep 3
check "frontend /products"     200 "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:9100/products)"
check "frontend deep product"  200 "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:9100/products/some-name)"
check "frontend /pipeline"     200 "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:9100/pipeline)"

# ── Screenshot ───────────────────────────────────────────────────────────────
if [ -x "$CHROME" ]; then
  "$CHROME" --headless --disable-gpu --window-size=1440,900 \
    --virtual-time-budget=6000 --screenshot="$WORK/factory.png" \
    http://localhost:9100/products > /dev/null 2>&1
  [ -s "$WORK/factory.png" ] && { echo "PASS  screenshot → $WORK/factory.png"; PASS=$((PASS+1)); } \
                             || { echo "FAIL  screenshot empty"; FAIL=$((FAIL+1)); }
else
  echo "SKIP  screenshot (Chrome not found)"
fi

echo "== $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
