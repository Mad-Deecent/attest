# attest-gateway

An MCP proxy that asks attest before forwarding `tools/call`. It sits between
an agent and an MCP server (streamable HTTP) and refuses calls that the person
who caused them is not entitled to.

    go build ./cmd/attest-gateway && ./attest-gateway -config gateway.json

See `gateway.example.json` at the repo root.

## Config

- `listen`: address to serve on (default `:8430`).
- `upstream`: the MCP endpoint URL. Every request path is mapped to it.
- `attest.url`: base URL of attest.
- `attest.token`: static agent token (development), or
- `attest.oidc`: `{issuer, clientId, clientSecretFile | clientSecretEnv}`;
  client-credentials grant against the issuer's discovered token endpoint,
  cached and refreshed shortly before expiry.
- `tools`: MCP tool name to `{permission, resource?}`. A tool that is not
  listed is denied.

## The `_meta` convention

The upstream speech component must stamp each `tools/call` with the speaker's
subject key:

    {"method":"tools/call","params":{"name":"unlock_door","arguments":{},
      "_meta":{"attest/subject":"spk-ana"}}}

A missing or empty subject is evaluated as `anonymous`, so only floor
permissions pass. The gateway trusts this field: whoever can write it can
claim any speaker, so it must be set by a component the model cannot
influence, not by the model itself.

## Behaviour

- Granted: the request is forwarded unmodified.
- Denied: upstream is not contacted. The reply is a JSON-RPC result with
  `isError: true`, text `Not permitted: <reasons>`, and the decision under
  `_meta["attest/decision"]`.
- attest unreachable or not 200: denied as "identity service unavailable".
- Everything else (other methods, GET/SSE, DELETE, headers) passes through,
  streaming included.
- A JSON-RPC batch containing `tools/call` is rejected (400). Bodies with
  duplicate or case-colliding object keys, or compressed bodies, are rejected
  because the gateway and upstream could read them differently.
- One log line per gated call: tool, subject, granted, identity. No arguments.
- Decisions are not cached.
