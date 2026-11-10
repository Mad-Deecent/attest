# attest
An evidence-driven identity assurance layer. Fuse signals, maintain identity beliefs, and map assurance bands into your existing authorization stack.

Status: early. The core runs in memory; see [docs/design/v0.md](docs/design/v0.md)
for the design and what is built.

## First run

```bash
go run ./cmd/attest
```

With no config file, attest starts unconfigured, keeps its state in
`~/.local/state/attest/attest.json` (`-data` to move it), and prints a one-time
setup code to its log. A console will walk through the rest; until it exists,
these are the calls it makes:

```bash
A=localhost:8420
# Trade the setup code for a 30-minute setup session; note the csrf it returns.
curl -c jar -X POST $A/v1/setup/session -d '{"code":"ABCD-EFGH-JKMN-PQRS"}'
CSRF=...
curl -b jar -H "X-CSRF-Token: $CSRF" -X PUT $A/v1/settings/public-url -d '{"publicUrl":"http://localhost:8420"}'
# Keycloak: an admin's username/password (or a realm-admin service client's
# clientId/clientSecret), used once and never stored.
curl -b jar -H "X-CSRF-Token: $CSRF" -X POST $A/v1/settings/provider -d '{
  "kind": "keycloak", "url": "https://idp.example.com", "realm": "home",
  "credential": {"username": "me", "password": "..."}}'
curl -b jar -H "X-CSRF-Token: $CSRF" -X POST $A/v1/setup/complete
```

Connecting Keycloak creates what is missing (realm roles `attest-admin`,
`attest-producer`, `attest-agent`, and a public enrolment client
`attest-enrol` that sends people back to `{publicUrl}/enrol/callback`), grants
you `attest-admin`, and reports what it created and what it found. For any
other OIDC provider, create a public client with PKCE yourself and connect
with `{"kind": "oidc", "issuer": "...", "clientId": "..."}`.

After setup the code is dead and settings take an `attest-admin` token:
`GET /v1/settings`, `PUT /v1/settings/public-url`, `PUT /v1/settings/policy`,
`POST /v1/settings/provider`. Changes apply at once, without a restart.
Locked out? Restart once with `-reset-setup` for a new code.

## Quickstart with a config file

For development and automation, a config file sets everything up front:

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
until a second, independent modality supports the same subject. A decision
request without a `subject` can only be granted floor permissions.

## Demo

[`demo/car/`](demo/car/) runs the whole path in one command: a voice producer
works out who said which words in a two-speaker recording, a scripted
assistant runtime turns trigger phrases into MCP tool calls, and
[`attest-gateway`](internal/gateway/README.md) asks attest before each call
reaches the MCP server. The owner's calendar request is served; the
passenger's email request is refused, with reasons.

```bash
demo/car/run.sh              # self-contained, static dev tokens
demo/car/run.sh --keycloak   # gateway and producer use OIDC client credentials
```

## Credentials from your identity provider

The quickstart uses static development tokens. To accept tokens from an OIDC
provider (Keycloak, Ory, Auth0, ...) instead, connect it as in
[First run](#first-run), or add its issuer to the config:

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
to learn which identity it is for. attest never receives audio. Set
`enrol.voiceProducer` to the producer's principal id so no other producer can
redeem tickets. Requests to `/enrol` under any host other than `publicUrl`'s
are redirected there, because sign-in cookies only work on that host.

Choices saved on the page replace only the delegations the person granted
themselves. Delegations an administrator made (with zones, ceilings, or an
expiry) are shown read-only. `policy.labels` maps permissions to the words the
page shows, e.g. `{"email.send": "Send email as you"}`.

Set `data` to a file path to keep identities, roles, delegations, and settings
across restarts; attest refuses to start if its directory is missing or not
writable. Empty keeps everything in memory. With a data file, the config's
`oidc`, `publicUrl`, and `policy` are seeds: each is applied when it first
appears or changes in the file, and changes made at runtime survive a restart.

## Development

```bash
go test ./...
```
