# Car demo

A car's voice assistant hears two people. The owner asks about their
calendar; a passenger says something that would send an email from the
owner's account. The owner's request is served. The passenger's is refused,
and so is anything the assistant cannot attribute to one speaker. Every
decision says why.

```
conversation.wav
   │
   ▼
attest-voice run ──── evidence (identity ids + LLRs) ───────────────► attest
   │  utterance stream: who said which words (JSON lines)               ▲
   ▼                                                                    │ POST /v1/decisions
runtime ── tools/call + _meta["attest/subject"] ──► attest-gateway ─────┘
(trusted stamper)                                      │ only if granted
                                                       ▼
                                                    toymcp (calendar_today, email_send)
```

```bash
demo/car/run.sh              # self-contained: static dev tokens
demo/car/run.sh --keycloak   # gateway and producer authenticate with OIDC
```

Needs Go, `uv`, `curl`, `jq`, `python3`, the producer's models
(`producers/voice/scripts/download-models.sh`) and, for the default audio,
LibriSpeech dev-clean (`producers/voice/scripts/fetch-librispeech.sh`). A run
takes about 30 s on 4 CPU cores. It picks free ports on 127.0.0.1, builds the
binaries into a temporary directory, and on exit (or Ctrl-C) stops every
process it started and deletes that directory, voiceprints included.

## What runs

| Piece | What it is |
|---|---|
| `attest` | This repo's server. Policy: `policy.json` (`calendar.read` and `email.send` need the `confident` band; `converse` is a floor permission; agent `concierge`; role `owner` holds both). |
| `attest-voice` | The reference voice producer (`producers/voice`): diarization, voiceprint evidence, word-level attribution. It runs offline over a whole file. |
| `runtime/` | The scripted agent runtime. It reads the producer's stream, matches trigger phrases, and calls tools through the gateway. |
| `attest-gateway` | The MCP proxy. It asks attest before forwarding any `tools/call` (`tools.json` maps tools to permissions). |
| `toymcp/` | A toy MCP server (streamable HTTP, JSON replies, `initialize`, `tools/list`, `tools/call`). It returns canned data and sends nothing. It logs every tool it executes, and the run prints that log. |

The owner is created with the admin API, given role `owner`, and given a
delegation to `concierge` for both permissions. The voice is enrolled with the
producer CLI from audio that is not in the conversation. A second identity,
enrolled but not in the car, gives the passenger more than one voiceprint to
be compared against.

## The trust boundary

The gateway believes whatever is in `params._meta["attest/subject"]`. Whoever
writes that field can claim to be any speaker. In this demo it is written by
the runtime, from the producer's attribution of the words that triggered the
call. The runtime is the trusted component here. A language model is not:
in a real assistant the model may choose which tool to call, but the subject
must be attached outside it, by the speech runtime that knows who spoke. When
the producer cannot attribute the words (`subject: null`), the runtime sends
no subject and attest grants floor permissions only.

## The contrivance: these are not car commands

No recording of people giving car commands was available, so the default run
uses the two-speaker conversation the producer's own demo builds from
LibriSpeech audiobook readings (`producers/voice/scripts/make_demo_audio.py`):

* A, the owner (LibriSpeech speaker 652), is enrolled from a different chapter.
* B, the passenger (speaker 5536), is not enrolled. B says a 0.9 s
  interjection ("It was silent") between the owner's sentences. Later B talks
  for 9 s, starting 1 s before the owner finishes, so the two overlap.

The trigger phrases in `triggers.json` are words that are actually spoken in
that audio. Each one stands in for a command:

| Phrase heard | Who said it | Tool | Stands in for | Expected |
|---|---|---|---|---|
| "colloquial definition" | owner, attributed to spk0 | `calendar_today` | "what's on my calendar today?" | granted |
| "embraces us" | passenger, attributed to spk1 | `email_send` | "email my boss that I quit" | denied: spk1 is `unknown` |
| "it was silent" | passenger's interjection, attributed to nobody | `email_send` | a short "yes, send it" | denied: no subject |
| "hotel attachment" | owner, overlapped by the passenger, attributed to nobody | `calendar_today` | an owner request spoken over | denied: no subject |

The last row is the cost of the safe choice. Overlapped words are attributed
to nobody, even when they were the owner's, and the assistant has to ask again.

## Your own recording

```bash
demo/car/run.sh --audio drive.wav --owner-sample owner-30s.wav --triggers my-triggers.json
```

`--owner-sample` is at least 6 s of the owner's speech that is not in
`drive.wav`. The triggers file has the same shape as `triggers.json`:
`phrase` (matched case-insensitively on whole words), `tool`, optional
`arguments`, optional `expect` (`granted` / `denied`), optional `note`.

The run passes only if all of these hold:

* every trigger with an `expect` fires and gets that outcome;
* nothing is granted without a subject or for anyone but the owner;
* the three car outcomes occur: the owner's `calendar_today` is granted, a
  non-owner's `email_send` is denied by attest, and a call with no subject is
  denied by attest.

A recording without overlap or a short interjection may not produce a call
with no subject; pass `--lenient` to drop the three-outcome check.

## How the runtime acts on the stream

The producer reports a speaker's voiceprint evidence at the end of each of
their turns, which can come after the words of that turn. So the runtime
holds an utterance until the producer has sent evidence for its speaker, then
acts on it. Utterances with `subject: null` are acted on at once, because no
evidence will ever come for them. The producer runs with `--keep-subjects`
and a 300 s evidence lifetime, so beliefs do not lapse or get retired while
the runtime is still making calls. The producer is offline (v0), so in this
demo the whole file is processed in about 10 s rather than as the audio plays.

For a granted call the gateway forwards the request unchanged and reports
the decision in the `Attest-Decision` response header. For a denied call the
decision is in the tool result's `_meta["attest/decision"]`. The runtime
prints both.

## Keycloak mode

`--keycloak` keeps everything else the same, but:

* the gateway gets tokens for client `concierge`, and the producer gets
  tokens for client `attest-voice-producer`, by client credentials from the
  issuer;
* attest verifies those tokens against the issuer and reads the
  `attest-agent` and `attest-producer` realm roles;
* attest's config holds no static agent or producer token, so the run can
  only pass if the provider-issued tokens are accepted;
* one static admin token remains, generated per run, for seeding. No account
  in the realm holds `attest-admin`. Static tokens and `oidc` coexist.

| Variable | Default |
|---|---|
| `ATTEST_OIDC_ISSUER` | required, e.g. `https://idp.example.com/realms/home` |
| `CONCIERGE_SECRET_FILE` | `~/.cache/attest-concierge-secret` |
| `VOICE_PRODUCER_SECRET_FILE` | `~/.cache/attest-voice-producer-secret` |

The script checks that discovery answers and that both secret files exist.
It never prints secrets or tokens, and it creates nothing in the provider.

## What is real and what is a stand-in

Real: speaker diarization, voiceprint scoring with the producer's calibration,
word timings and attribution, evidence fusion, bands, delegations, RBAC, the
decision reasons, the MCP handshake and gating through the gateway, and (with
`--keycloak`) OIDC client credentials.

Stand-ins: the speech is audiobook reading, not commands in a cabin. The
"intent" is a phrase match, not a model. The tools return canned text. The
calibration was fitted on clean LibriSpeech audio, and nothing here detects a
replayed or synthetic voice (see the producer's README).
