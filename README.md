# attest
An evidence-driven identity assurance layer. Fuse signals, maintain identity beliefs, and map assurance bands into your existing authorization stack.

Status: early. The core runs in memory; see [docs/design/v0.md](docs/design/v0.md)
for the design and what is built.

## Quickstart

```bash
go run ./cmd/attest -config attest.example.json
```

```bash
A=localhost:8420
# Enrol an identity, give it a role, and let the agent act for it.
curl -X PUT $A/v1/identities/ana -H 'Authorization: Bearer dev-admin-token' -d '{"displayName":"Ana"}'
curl -X PUT $A/v1/identities/ana/roles -H 'Authorization: Bearer dev-admin-token' -d '{"roles":["resident"]}'
curl -X POST $A/v1/delegations -H 'Authorization: Bearer dev-admin-token' \
  -d '{"identityId":"ana","agent":"concierge","permissions":["*"],"grantedBy":"ana"}'

# A camera reports a face match as a log-likelihood ratio.
curl -X POST $A/v1/evidence -H 'Authorization: Bearer dev-face-token' -d '{
  "subject": {"key": "track-1", "zone": "lobby"}, "modality": "face",
  "observedAt": "'$(date -u +%FT%TZ)'", "expiresAt": "'$(date -u -d +5min +%FT%TZ)'",
  "candidates": [{"identityId": "ana", "llr": 6}], "calibration": "v1"}'

# The agent asks whether it may act for whoever track-1 is.
curl -X POST $A/v1/decisions -H 'Authorization: Bearer dev-agent-token' \
  -d '{"subject":"track-1","permission":"music.control"}'
```

`music.control` is granted. `door.unlock` is denied with `modalities 1 < min 2`
until a second, independent modality supports the same subject.

## Credentials from your identity provider

The quickstart uses static development tokens. To accept tokens from an OIDC
provider (Keycloak, Ory, Auth0, ...) instead, add its issuer to the config:

```json
"oidc": { "issuer": "https://idp.example.com/realms/home" }
```

A token opens the endpoints its roles allow: `attest-admin`, `attest-producer`,
`attest-agent`, read from `realm_access.roles`. Set `rolesClaim` and `roles` if
your provider names them differently, and `audience` to require one. An agent
is known to policy by the client the token was issued to.

## Enrolment

People enrol themselves at `/enrol`: they sign in through the identity
provider, can record their voice, and choose what each agent may do for them.
It needs a public OIDC client (authorization code with PKCE) whose redirect URI
is `{publicUrl}/enrol/callback`:

```json
"publicUrl": "http://localhost:8420",
"oidc": { "issuer": "https://idp.example.com/realms/home", "clientId": "attest-enrol" },
"enrol": { "voiceUrl": "https://voice.example.com/enrol" }
```

Signing in links an identity to the provider account and adopts its roles that
`policy.roles` defines, refreshed on every sign-in. The voice recording goes
from the browser straight to the voice producer at `enrol.voiceUrl`, carrying a
single-use ticket; the producer redeems it at `POST /v1/enrolment-tickets/redeem`
to learn which identity it is for. attest never receives audio.

Set `data` to a file path to keep identities, roles, and delegations across
restarts. Empty keeps everything in memory.

## Development

```bash
go test ./...
```
