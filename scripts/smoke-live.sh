#!/usr/bin/env bash
# Live smoke test against the real Command Code API.
# Requires CC_LIVE_KEY (a user_... key). Never prints the key.
set -euo pipefail

: "${CC_LIVE_KEY:?CC_LIVE_KEY must be set (user_... key; never committed)}"
PORT="${CC_SMOKE_PORT:-3199}"

here="$(cd "$(dirname "$0")/.." && pwd)"
bin="$(mktemp -t cc2api-smoke)"
go build -o "$bin" "$here" || { echo "BUILD FAILED"; exit 1; }

cfg="$(mktemp -t cc2api-cfg).json"
cat > "$cfg" <<EOF
{"port": $PORT, "host": "127.0.0.1", "logLevel": "info"}
EOF

"$bin" -config "$cfg" &
proxy_pid=$!
trap 'kill "$proxy_pid" 2>/dev/null || true; rm -f "$bin" "$cfg"' EXIT
for _ in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$PORT/health" >/dev/null && break
  sleep 0.1
done

base="http://127.0.0.1:$PORT"
fail=0

echo "== /health =="
curl -sf "$base/health" && echo

echo "== /v1/models (live catalog) =="
curl -sf "$base/v1/models" -H "Authorization: Bearer $CC_LIVE_KEY" | head -c 300; echo

echo "== chat completions, non-streaming =="
resp=$(curl -s "$base/v1/chat/completions" \
  -H "Authorization: Bearer $CC_LIVE_KEY" -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"Reply with exactly: OK"}],"max_tokens":100}')
echo "$resp" | head -c 400; echo
echo "$resp" | grep -q '"finish_reason"' || { echo "FAIL: no finish_reason"; fail=1; }

echo "== chat completions, streaming =="
stream=$(curl -sN "$base/v1/chat/completions" \
  -H "Authorization: Bearer $CC_LIVE_KEY" -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"Reply with exactly: OK"}],"stream":true,"max_tokens":100}')
echo "$stream" | tail -3
echo "$stream" | grep -q '\[DONE\]' || { echo "FAIL: stream missing [DONE]"; fail=1; }

echo "== anthropic messages =="
resp=$(curl -s "$base/v1/messages" \
  -H "x-api-key: $CC_LIVE_KEY" -H "Content-Type: application/json" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"deepseek/deepseek-v4-flash","max_tokens":100,"messages":[{"role":"user","content":"Reply with exactly: OK"}]}')
echo "$resp" | head -c 400; echo
echo "$resp" | grep -q '"stop_reason"' || { echo "FAIL: no stop_reason"; fail=1; }

echo "== responses API =="
resp=$(curl -s "$base/v1/responses" \
  -H "Authorization: Bearer $CC_LIVE_KEY" -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","input":"Reply with exactly: OK","max_output_tokens":100}')
echo "$resp" | head -c 400; echo
echo "$resp" | grep -q '"status"' || { echo "FAIL: responses missing status"; fail=1; }

if [ "$fail" = 0 ]; then echo "SMOKE OK"; else echo "SMOKE FAILED"; exit 1; fi
