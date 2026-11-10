#!/usr/bin/env bash
# End-to-end: build the car conversation, start attest, enroll A and C,
# process the conversation, and ask attest what it believes.
#
#   scripts/download-models.sh && scripts/fetch-librispeech.sh && scripts/demo.sh
#
# Needs: uv, go, curl, jq. Uses port 8420 (attest.example.json).
set -euo pipefail
HERE=$(cd "$(dirname "$0")/.." && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
LS=${LIBRISPEECH:-$HOME/.cache/librispeech/LibriSpeech/dev-clean}
WORK=${WORK:-$(mktemp -d)}
GO=${GO:-go}
ATTEST=http://localhost:8420
ADMIN='Authorization: Bearer dev-admin-token'
AGENT='Authorization: Bearer dev-agent-token'
VOICE='Authorization: Bearer dev-voice-token'
ID_A=${ID_A:-$(cat /proc/sys/kernel/random/uuid)}
ID_C=${ID_C:-$(cat /proc/sys/kernel/random/uuid)}
SESSION=${SESSION:-drive-$(date +%s)}
cd "$HERE"
av() { uv run --quiet attest-voice -c "$WORK/voice.json" "$@"; }
step() { printf '\n== %s\n' "$*" >&2; }

step "building demo audio in $WORK"
uv run --quiet python scripts/make_demo_audio.py "$LS" "$WORK" 2>/dev/null
jq -c . "$WORK/truth.json"

step "starting attest"
# Refuse to talk to someone else's attest on the same port.
if curl -fs "$ATTEST/healthz" >/dev/null 2>&1; then echo "something already answers on $ATTEST; stop it first" >&2; exit 1; fi
(cd "$ROOT" && "$GO" build -o "$WORK/attest" ./cmd/attest)
(cd "$ROOT" && exec "$WORK/attest" -config attest.example.json) >"$WORK/attest.log" 2>&1 &
APID=$!
trap 'kill $APID 2>/dev/null || true' EXIT
for _ in $(seq 50); do curl -fs "$ATTEST/healthz" >/dev/null && break; sleep 0.2; done

step "identities A=$ID_A C=$ID_C"
curl -fs -X PUT "$ATTEST/v1/identities/$ID_A" -H "$ADMIN" -d '{"displayName":"A (owner)"}' >/dev/null
curl -fs -X PUT "$ATTEST/v1/identities/$ID_C" -H "$ADMIN" -d '{"displayName":"C"}' >/dev/null
curl -fs -X PUT "$ATTEST/v1/identities/$ID_A/roles" -H "$ADMIN" -d '{"roles":["resident"]}' >/dev/null
curl -fs -X POST "$ATTEST/v1/delegations" -H "$ADMIN" \
  -d "{\"identityId\":\"$ID_A\",\"agent\":\"concierge\",\"permissions\":[\"*\"],\"grantedBy\":\"$ID_A\"}" >/dev/null

cat >"$WORK/voice.json" <<EOF
{"attest": {"url": "$ATTEST", "token": "dev-voice-token"},
 "zone": "car", "ttlSeconds": 300, "threads": 4, "dataDir": "$WORK/voiceprints"}
EOF

step "enrolling A and C from audio not in the conversation"
av enroll --identity "$ID_A" "$WORK/enroll-A.wav"
av enroll --identity "$ID_C" "$WORK/enroll-C.wav"

step "processing the conversation (session $SESSION)"
av run "$WORK/conversation.wav" --session "$SESSION" --keep-subjects --scores >"$WORK/stream.jsonl" 2>"$WORK/run.err"
jq -c 'if .type=="utterance" then del(.words) else . end' "$WORK/stream.jsonl"
tail -1 "$WORK/run.err"

for key in $(jq -r 'select(.type=="evidence") | .subject' "$WORK/stream.jsonl" | sort -u); do
  step "belief for $key"
  curl -s "$ATTEST/v1/subjects/$(jq -rn --arg k "$key" '$k|@uri')/belief" -H "$AGENT" | jq -c .
  curl -s -X POST "$ATTEST/v1/decisions" -H "$AGENT" -d "{\"subject\":\"$key\",\"permission\":\"music.control\"}" | jq -c '{permission:"music.control",granted,identityId,reasons}'
done

step "same audio as a normal session: the producer retires its subjects at the end"
av run "$WORK/conversation.wav" --session "$SESSION-2" >"$WORK/stream2.jsonl" 2>/dev/null
jq -c 'select(.type=="retired")' "$WORK/stream2.jsonl"
for key in $(jq -r 'select(.type=="retired") | .subject' "$WORK/stream2.jsonl"); do
  printf '%s belief -> HTTP %s\n' "$key" "$(curl -s -o /dev/null -w '%{http_code}' "$ATTEST/v1/subjects/$(jq -rn --arg k "$key" '$k|@uri')/belief" -H "$AGENT")"
done

step "forget A"
av forget --identity "$ID_A"
echo "work dir: $WORK" >&2
