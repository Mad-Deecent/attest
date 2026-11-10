#!/usr/bin/env bash
# The car demo, end to end: a voice producer, attest, attest-gateway, a toy
# MCP server, and a scripted agent runtime. See README.md in this directory.
#
#   demo/car/run.sh                 static dev tokens, self-contained
#   demo/car/run.sh --keycloak      gateway and producer use OIDC client credentials
#   demo/car/run.sh --audio my.wav --owner-sample me.wav --triggers my-triggers.json
#
# Needs: go, uv, curl, jq, python3; the producer's models and, for the default
# audio, LibriSpeech dev-clean (producers/voice/scripts/download-models.sh and
# fetch-librispeech.sh).
set -euo pipefail

usage() {
  sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
  cat <<'EOF'
Options:
  --keycloak             authenticate the gateway and the producer to attest with
                         OIDC client-credentials tokens (see README for env vars)
  --audio WAV            your own recording instead of the LibriSpeech conversation
  --owner-sample WAV     enrolment audio of the owner (required with --audio)
  --triggers JSON        trigger phrases (required with --audio)
  --lenient              only check trigger expectations, not the three car outcomes
  --keep                 keep the work directory (holds voiceprints: delete it after)
EOF
}

MODE=static AUDIO= OWNER_SAMPLE= TRIGGERS= LENIENT= KEEP=
while [ $# -gt 0 ]; do
  case "$1" in
    --keycloak) MODE=keycloak ;;
    --audio) AUDIO=$(realpath "$2"); shift ;;
    --owner-sample) OWNER_SAMPLE=$(realpath "$2"); shift ;;
    --triggers) TRIGGERS=$(realpath "$2"); shift ;;
    --lenient) LENIENT=-lenient ;;
    --keep) KEEP=1 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done
if [ -n "$AUDIO" ] && { [ -z "$OWNER_SAMPLE" ] || [ -z "$TRIGGERS" ]; }; then
  echo "--audio needs --owner-sample and --triggers" >&2; exit 2
fi

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
VOICE="$ROOT/producers/voice"
GO=${GO:-$(command -v go || echo "$HOME/sdk/go/bin/go")}
LIBRISPEECH=${LIBRISPEECH:-$HOME/.cache/librispeech/LibriSpeech/dev-clean}
MODELS=${MODELS:-$HOME/.cache/sherpa-onnx-models}
ISSUER=${ATTEST_OIDC_ISSUER:-http://keycloak.ethel.internal:8080/realms/ethel}
CONCIERGE_SECRET_FILE=${CONCIERGE_SECRET_FILE:-$HOME/.cache/attest-concierge-secret}
VOICE_PRODUCER_SECRET_FILE=${VOICE_PRODUCER_SECRET_FILE:-$HOME/.cache/attest-voice-producer-secret}
TRIGGERS=${TRIGGERS:-$HERE/triggers.json}

for tool in "$GO" uv curl jq python3; do
  command -v "$tool" >/dev/null || { echo "missing: $tool" >&2; exit 2; }
done
[ -d "$MODELS" ] || { echo "no models in $MODELS: run producers/voice/scripts/download-models.sh" >&2; exit 2; }
if [ -z "$AUDIO" ] && [ ! -d "$LIBRISPEECH" ]; then
  echo "no LibriSpeech at $LIBRISPEECH: run producers/voice/scripts/fetch-librispeech.sh" >&2; exit 2
fi

WORK=$(mktemp -d -t attest-car-XXXXXX)
PIDS=()
cleanup() {
  local pid
  for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
  if [ -n "$KEEP" ]; then echo "work directory kept: $WORK (it holds voiceprints)" >&2; else rm -rf "$WORK"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

step() { printf '\n== %s\n' "$*"; }
# Three distinct free ports: all bound at once, so none is handed out twice.
free_ports() {
  python3 -c 'import socket; s = [socket.socket() for _ in range(3)]
for x in s: x.bind(("127.0.0.1", 0))
print(*(x.getsockname()[1] for x in s))'
}
secret() { python3 -c 'import secrets; print(secrets.token_urlsafe(24))'; }
uuid() { python3 -c 'import uuid; print(uuid.uuid4())'; }
wait_for() { # url status
  for _ in $(seq 100); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' "$1" || true)" = "$2" ] && return 0
    sleep 0.1
  done
  echo "nothing answered at $1; logs in $WORK" >&2; KEEP=1; return 1
}

if [ "$MODE" = keycloak ]; then
  step "checking Keycloak ($ISSUER)"
  for f in "$CONCIERGE_SECRET_FILE" "$VOICE_PRODUCER_SECRET_FILE"; do
    [ -r "$f" ] && [ -s "$f" ] || { echo "secret file $f is missing, unreadable, or empty" >&2; exit 2; }
  done
  CONCIERGE_SECRET_FILE=$(realpath "$CONCIERGE_SECRET_FILE")
  VOICE_PRODUCER_SECRET_FILE=$(realpath "$VOICE_PRODUCER_SECRET_FILE")
  curl -fsS -m 10 "$ISSUER/.well-known/openid-configuration" >/dev/null ||
    { echo "Keycloak discovery failed at $ISSUER" >&2; exit 2; }
  echo "discovery ok; secret files present (contents not shown)"
fi

read -r AP GP MP < <(free_ports)
ATTEST=http://127.0.0.1:$AP
ADMIN_TOKEN=$(secret)
step "building into $WORK (mode: $MODE; attest :$AP, gateway :$GP, toy MCP :$MP)"
mkdir -p "$WORK/bin" "$WORK/audio"
(cd "$ROOT" && "$GO" build -o "$WORK/bin/" ./cmd/attest ./cmd/attest-gateway ./demo/car/toymcp ./demo/car/runtime)

# attest: one static admin token for seeding (no Keycloak account holds
# attest-admin). In static mode the agent and producer get static tokens too;
# in Keycloak mode they have none, so only provider-issued tokens work.
FUSION='{"priorUnknown":0.5,"ambiguityMargin":0.2,"minAssociation":0.9,"maxLLR":10,"modalities":{"voice":{"group":"microphone"}}}'
if [ "$MODE" = static ]; then
  AGENT_TOKEN=$(secret) PRODUCER_TOKEN=$(secret)
  jq -n --arg listen "127.0.0.1:$AP" --arg admin "$ADMIN_TOKEN" --arg agent "$AGENT_TOKEN" --arg producer "$PRODUCER_TOKEN" \
    --argjson fusion "$FUSION" --slurpfile policy "$HERE/policy.json" \
    '{listen: $listen, fusion: $fusion, policy: $policy[0], tokens: {
        ($admin): {kind: "admin", id: "demo-admin"},
        ($agent): {kind: "agent", id: "concierge"},
        ($producer): {kind: "producer", id: "voice-car-1"}}}' >"$WORK/attest.json"
  ATTEST_FOR_GATEWAY=$(jq -n --arg url "$ATTEST" --arg t "$AGENT_TOKEN" '{url: $url, token: $t}')
  ATTEST_FOR_VOICE=$(jq -n --arg url "$ATTEST" --arg t "$PRODUCER_TOKEN" '{url: $url, token: $t}')
else
  jq -n --arg listen "127.0.0.1:$AP" --arg admin "$ADMIN_TOKEN" --arg issuer "$ISSUER" \
    --argjson fusion "$FUSION" --slurpfile policy "$HERE/policy.json" \
    '{listen: $listen, fusion: $fusion, policy: $policy[0], oidc: {issuer: $issuer},
      tokens: {($admin): {kind: "admin", id: "demo-admin"}}}' >"$WORK/attest.json"
  ATTEST_FOR_GATEWAY=$(jq -n --arg url "$ATTEST" --arg issuer "$ISSUER" --arg f "$CONCIERGE_SECRET_FILE" \
    '{url: $url, oidc: {issuer: $issuer, clientId: "concierge", clientSecretFile: $f}}')
  ATTEST_FOR_VOICE=$(jq -n --arg url "$ATTEST" --arg issuer "$ISSUER" --arg f "$VOICE_PRODUCER_SECRET_FILE" \
    '{url: $url, oidc: {issuer: $issuer, clientId: "attest-voice-producer", secretFile: $f}}')
fi
jq -n --arg listen "127.0.0.1:$GP" --arg up "http://127.0.0.1:$MP/mcp" --argjson attest "$ATTEST_FOR_GATEWAY" \
  --slurpfile tools "$HERE/tools.json" '{listen: $listen, upstream: $up, attest: $attest, tools: $tools[0]}' >"$WORK/gateway.json"
jq -n --argjson attest "$ATTEST_FOR_VOICE" --arg data "$WORK/voiceprints" --arg models "$MODELS" \
  '{attest: $attest, zone: "car", ttlSeconds: 300, threads: 4, dataDir: $data, modelsDir: $models}' >"$WORK/voice.json"
chmod 600 "$WORK"/*.json
echo "attest credential kinds: static $(jq -c '[.tokens[].kind]' "$WORK/attest.json")$(jq -r 'if .oidc then ", plus OIDC tokens from " + .oidc.issuer else "" end' "$WORK/attest.json")"

step "starting the toy MCP server, attest, and attest-gateway"
"$WORK/bin/toymcp" -listen "127.0.0.1:$MP" >"$WORK/toymcp.log" 2>&1 & PIDS+=($!)
"$WORK/bin/attest" -config "$WORK/attest.json" >"$WORK/attest.log" 2>&1 & PIDS+=($!)
"$WORK/bin/attest-gateway" -config "$WORK/gateway.json" >"$WORK/gateway.log" 2>&1 & PIDS+=($!)
wait_for "$ATTEST/healthz" 204
wait_for "http://127.0.0.1:$MP/mcp" 405
wait_for "http://127.0.0.1:$GP/mcp" 405 # a GET passes through the gateway

admin() { curl -fsS -X "$1" "$ATTEST$2" -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' -d "$3" >/dev/null; }
OWNER=$(uuid) OTHER=$(uuid)
step "seeding attest with the admin API: owner $OWNER"
admin PUT "/v1/identities/$OWNER" '{"displayName":"Owner"}'
admin PUT "/v1/identities/$OWNER/roles" '{"roles":["owner"]}'
admin POST /v1/delegations "{\"identityId\":\"$OWNER\",\"agent\":\"concierge\",\"permissions\":[\"calendar.read\",\"email.send\"],\"grantedBy\":\"demo-admin\"}"
echo "owner: role owner; delegation to concierge for calendar.read, email.send"
NAMES=$(jq -nc --arg o "$OWNER" '{($o): "owner"}')

av() { (cd "$VOICE" && uv run --quiet attest-voice -c "$WORK/voice.json" "$@"); }
if [ -z "$AUDIO" ]; then
  step "building the conversation from LibriSpeech (see README: these are not car commands)"
  (cd "$VOICE" && uv run --quiet python scripts/make_demo_audio.py "$LIBRISPEECH" "$WORK/audio") 2>"$WORK/audio.log"
  jq -c '{mutterText, turns}' "$WORK/audio/truth.json"
  AUDIO="$WORK/audio/conversation.wav" OWNER_SAMPLE="$WORK/audio/enrol-A.wav"
  # A second household member, enrolled but not in the car, so the passenger
  # is compared against more than one voiceprint.
  admin PUT "/v1/identities/$OTHER" '{"displayName":"Household member (not in the car)"}'
  NAMES=$(jq -nc --arg o "$OWNER" --arg c "$OTHER" '{($o): "owner", ($c): "household member"}')
  step "enrolling voices with the producer CLI (audio not in the conversation)"
  av enrol --identity "$OWNER" "$OWNER_SAMPLE"
  av enrol --identity "$OTHER" "$WORK/audio/enrol-C.wav"
else
  step "enrolling the owner's voice with the producer CLI"
  av enrol --identity "$OWNER" "$OWNER_SAMPLE"
fi

SESSION=car-$(date +%s)
step "session $SESSION: producer stream -> runtime -> gateway -> attest / toy MCP"
set +e
av run "$AUDIO" --session "$SESSION" --keep-subjects 2>"$WORK/producer.log" |
  "$WORK/bin/runtime" -gateway "http://127.0.0.1:$GP/mcp" -triggers "$TRIGGERS" -owner "$OWNER" -names "$NAMES" $LENIENT
STATUS=(${PIPESTATUS[@]})
set -e

step "what the toy MCP server executed"
grep -o 'executed .*' "$WORK/toymcp.log" || echo "(nothing)"
step "gateway log (no arguments are logged)"
sed -n 's/.*gateway: //p' "$WORK/gateway.log"

if [ "${STATUS[0]}" != 0 ]; then
  echo "the producer failed (exit ${STATUS[0]}):" >&2; tail -5 "$WORK/producer.log" >&2; exit 1
fi
exit "${STATUS[1]}"
