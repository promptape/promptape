#!/usr/bin/env bash
# End-to-end test: record through the official SDKs, kill the "API", replay.
#
#   PYTHON=python3 ./e2e/run.sh
#
# Needs: Go, and a Python with `openai` and `anthropic` installed.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PYTHON="${PYTHON:-python3}"
WORK="$(mktemp -d)"
UP_PORT="${UP_PORT:-14010}"
PX_PORT="${PX_PORT:-14011}"
BIN="$WORK/promptape"
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$WORK"; }
trap cleanup EXIT

wait_for() { for _ in $(seq 1 100); do curl -fsS "$1" >/dev/null 2>&1 && return 0; sleep 0.1; done; echo "timed out waiting for $1" >&2; return 1; }
hits() { curl -fsS "http://127.0.0.1:$UP_PORT/hits" | "$PYTHON" -c 'import json,sys; print(json.load(sys.stdin)["hits"])'; }

echo "› building"
(cd "$ROOT" && go build -o "$BIN" ./cmd/promptape)

echo "› starting fake upstream API on :$UP_PORT"
"$PYTHON" "$ROOT/e2e/fake_upstream.py" "$UP_PORT" & PIDS+=($!)
wait_for "http://127.0.0.1:$UP_PORT/hits"

start_proxy() {
  "$BIN" serve --mode "$1" --dir "$WORK/tapes" --port "$PX_PORT" \
    --openai-upstream "http://127.0.0.1:$UP_PORT" --anthropic-upstream "http://127.0.0.1:$UP_PORT" \
    2>"$WORK/proxy-$1.log" & PX=$!
  PIDS+=($PX)
  wait_for "http://127.0.0.1:$PX_PORT/_promptape/health"
}
stop_proxy() { kill -INT "$PX"; wait "$PX" 2>/dev/null || true; }

echo "› run 1: record"
start_proxy record
PROMPTAPE_URL="http://127.0.0.1:$PX_PORT" "$PYTHON" "$ROOT/e2e/client.py" > "$WORK/run1.json"
stop_proxy
RECORDED_HITS="$(hits)"
echo "  upstream calls: $RECORDED_HITS"

echo "› killing the upstream API (replay must not need it)"
kill "${PIDS[0]}"; wait "${PIDS[0]}" 2>/dev/null || true

echo "› run 2: replay"
start_proxy replay
PROMPTAPE_URL="http://127.0.0.1:$PX_PORT" "$PYTHON" "$ROOT/e2e/client.py" > "$WORK/run2.json"
STATS="$(curl -fsS "http://127.0.0.1:$PX_PORT/_promptape/stats")"
stop_proxy

fail() { echo "✗ $*" >&2; echo "--- proxy logs ---" >&2; cat "$WORK"/proxy-*.log >&2; exit 1; }

diff -u "$WORK/run1.json" "$WORK/run2.json" || fail "replayed SDK results differ from recorded ones"
"$PYTHON" - "$WORK/run1.json" "$STATS" <<'PY' || fail "assertions failed"
import json, sys
run = json.load(open(sys.argv[1]))
stats = json.loads(sys.argv[2])
assert run["openai_chat"]["text"].startswith("Recorded answer"), run["openai_chat"]
assert run["openai_stream"]["usage"] == 1500, run["openai_stream"]
assert run["anthropic_stream"]["deltas"] > 3 and run["anthropic_stream"]["stop"] == "end_turn", run["anthropic_stream"]
assert run["anthropic_repeat"][0] != run["anthropic_repeat"][1], "repeated calls should get distinct recordings"
assert stats["replayed"] == 7 and stats["misses"] == 0, stats
assert stats["saved_usd"] > 0, stats
print(f"  replayed {stats['replayed']} calls, saved ${stats['saved_usd']:.4f}, {stats['saved_tokens']} tokens")
PY

if grep -rqE 'e2e-dummy-key|Authorization|x-api-key' "$WORK/tapes"; then fail "credentials leaked into tapes"; fi

echo "› report"
"$BIN" report --dir "$WORK/tapes"

echo "✓ e2e passed: official SDKs got identical results with the API offline"
