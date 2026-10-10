#!/usr/bin/env bash
# Phase 2 acceptance client: one-shot SSE prompt with per-event timestamps,
# then a follow-up on the same session, then 10 concurrent clients.
#
# Usage: bash examples/sse-client.sh [base_url]
set -u
BASE="${1:-http://127.0.0.1:8080}"

stamp() { date +%H:%M:%S.%3N; }

echo "== healthz =="
curl -s "$BASE/healthz"; echo; echo

echo "== 1) one-shot prompt (SSE, timestamps per event) =="
# Capture the stream, prefixing each SSE line with a timestamp so incremental
# delivery is visible. session event carries the id we reuse below.
STREAM=$(mktemp)
curl -sN -X POST "$BASE/v1/sessions" \
  -H 'Content-Type: application/json' \
  -d '{"message":"run `echo lyon-sse-ok` in the shell, then reply with just DONE"}' \
  | while IFS= read -r line; do
      printf '%s  %s\n' "$(stamp)" "$line"
      printf '%s\n' "$line" >> "$STREAM"
    done

SID=$(grep -o '"session_id":"[^"]*"' "$STREAM" | head -1 | cut -d'"' -f4)
echo
echo "session_id=$SID"
[ -n "$SID" ] || { echo "FAIL: no session id"; exit 1; }

echo
echo "== 2) follow-up on the same session =="
curl -sN -X POST "$BASE/v1/sessions/$SID/messages" \
  -H 'Content-Type: application/json' \
  -d '{"message":"what did you run in the previous turn? answer in one short line."}' \
  | grep -E '^(event|data): ' | tail -6

echo
echo "== 3) stats =="
curl -s "$BASE/v1/sessions/$SID/stats"; echo

echo
echo "== 4) 10 concurrent independent streams =="
for i in $(seq 1 10); do
  (
    START=$(stamp)
    EVENTS=$(curl -sN -X POST "$BASE/v1/sessions" \
      -H 'Content-Type: application/json' \
      -d "{\"message\":\"reply with only the number $i\"}" \
      | grep -c '^event: ')
    echo "client $i: first=$START events=$EVENTS"
  ) &
done
wait

echo
echo "== 5) delete session =="
curl -s -o /dev/null -w 'HTTP %{http_code}\n' -X DELETE "$BASE/v1/sessions/$SID"
rm -f "$STREAM"
echo "done."
