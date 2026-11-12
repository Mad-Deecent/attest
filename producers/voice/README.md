# attest-voice

Reference voice producer for attest. It turns audio into speaker-identity
evidence: who spoke when (diarization), which enrolled identity each speaker
is (a calibrated log-likelihood ratio), and which speaker said which words
(so an agent can tell the owner's "send it" from a passenger's).

Voiceprints live only here. attest receives identity ids and LLRs, never
audio or embeddings.

```
audio ─► VAD ─► diarization ─► short-island guard ─┬─► per-speaker embedding ─► cosine vs voiceprints ─► calibrated LLR ─► POST /v1/evidence
                                                   └─► ASR word timings ─► word→speaker attribution ─► utterance stream (stdout / callback)
```

**v0 is offline**: it processes a whole file (a completed session). Steps run
in audio-time order and evidence is re-sent after each of a speaker's turns,
as a live producer would, so a streaming front end can replace the file
reader and offline diarizer without changing what attest or downstream see.
Streaming is not implemented.

## Stack

CPU-only, no torch: [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx)
with these models (fetched by `scripts/download-models.sh`, ~150 MB, into
`~/.cache/sherpa-onnx-models`; never committed):

| Job | Model | License (upstream) |
|---|---|---|
| VAD | Silero VAD | MIT |
| Diarization segmentation | pyannote segmentation-3.0 (ONNX) | MIT |
| Speaker embedding (diarization and identity) | 3D-Speaker ERes2Net, VoxCeleb, 192-d | Apache-2.0 |
| ASR with token timestamps | NVIDIA Parakeet TDT 110M, int8 | CC-BY-4.0 |

The embedding model was chosen by measurement: on cross-chapter LibriSpeech
pairs, the WeSpeaker ResNet34 / ResNet34-LM / CAM++ exports gave non-target
cosine means of 0.5–0.6 and worse EER without cohort mean subtraction;
ERes2Net gave a non-target mean near 0.1 and the best EER with plain cosine.
Audio decoding uses PyAV (bundled FFmpeg libraries; no system ffmpeg).

## Use

```bash
uv sync
scripts/download-models.sh
cp attest-voice.example.json voice.json   # edit
uv run attest-voice -c voice.json enroll --identity <uuid> sample.wav
uv run attest-voice -c voice.json run session.wav --session drive-42
uv run attest-voice -c voice.json forget --identity <uuid>
uv run attest-voice -c voice.json serve          # browser enrollment endpoint
uv run attest-voice fit-calibration trials.jsonl --id my-cal-v1 --model <embedding> -o my-cal.json
```

`run` flags: `--speakers N` (known speaker count), `--keep-subjects` (do not
retire at the end), `--dry-run` (no attest), `--scores` (add raw cosine
scores to evidence events, for calibration work).

### Configuration

See `attest-voice.example.json`. `attest.token` is a static bearer (dev). For
OIDC client credentials use instead:

```json
"attest": {"url": "https://attest.example", "oidc": {
  "issuer": "https://idp.example/realms/home", "clientId": "voice-mic-1",
  "secretFile": "/run/secrets/voice", "secretEnv": "", "scope": "", "audience": ""}}
```

The token endpoint is discovered from the issuer, the token cached until 30 s
before expiry, and refreshed once on a 401.

## What it sends and emits

**Subjects.** One per diarized speaker per session: `{session}/spk{N}`, zone
from config. Evidence for a speaker is re-sent after each of their turns with
everything they have said so far (it replaces the previous evidence in
attest's slot); `observedAt` is wall-clock send time and `expiresAt` is
`observedAt + ttlSeconds` (default 60). At session end the subjects are
retired (`POST /v1/retirements {"subject": key}`). The `/` in the key is
fine for `GET /v1/subjects/{key}/belief` when URL-encoded (`%2F`); verified.

**Candidates.** Every enrolled identity gets an LLR, including negative ones,
bounded to ±8 (inside attest's default `maxLLR` 10). With no voiceprints the
evidence has no candidates ("heard someone, matched nobody").
Voiceprints from a different embedding model than the calibration's are
skipped.

**Stream** (JSON lines on stdout):

```json
{"type":"utterance","subject":"drive-42/spk0","start":0.12,"end":6.28,"text":"...","words":[{"w":"San","start":0.122,"end":0.362}, ...]}
{"type":"evidence","subject":"drive-42/spk0","voicedSeconds":6.3,"calibration":"demo-eres2net-librispeech-v1","candidates":[...],"sent":true}
{"type":"island","start":6.67,"end":7.62,"diarizedAs":"drive-42/spk0","referenceSeconds":6.3,"kept":false}
{"type":"retired","subject":"drive-42/spk0"}
```

If `callbackUrl` is set, each utterance object (without `type`) is also
POSTed there. A downstream gateway tags a tool call with the `subject` of the
utterance(s) that produced the request and asks attest about that subject.
**`subject: null` means the words cannot be attributed to anyone**; a request
built from them must not be acted on for anyone (re-ask). `demo/car/` at the
repo root shows the whole path: a runtime stamps tool calls with the subject
and `attest-gateway` asks attest before each call reaches an MCP server.

### Word attribution

Words come from ASR token timestamps (start + duration; 80 ms resolution).
Each word belongs to the one speaker covering ≥80% of its speech-covered time
and ≥50% of its span. Otherwise it is *contested* (overlap, or straddling a
speaker change) or *uncovered* (outside every turn). Words group into
utterances at pauses > 0.5 s and at attribution changes. A contested word
nulls its whole contiguous run, before and after, because nothing marks where
one speaker's request ends. Uncovered words (cut at silences, e.g. by the
guard below) become their own null utterance without poisoning neighbors.

### Short-island guard

Diarization fails exactly where it matters: in the demo below it clusters a
0.9 s passenger interjection into the owner's turn. So every pause-delimited
VAD island shorter than 1.5 s is re-scored against its diarized speaker's
*earlier* speech (≤ 30 s of it; only the past, so it works on a stream) and
must reach a raw calibrated LLR of 2. Islands that fail, or have under 2 s of
earlier reference, are cut out of the speaker's turns: their words come out
`subject: null` and their audio does not feed the speaker's voiceprint.
Cost: a genuine short owner reply can also come out null (e.g. "yes" as the
speaker's first words). Tunable under `guard` in the config.

## Calibration

`llr = a·cosine + b`, fitted by class-balanced logistic regression (so the
target-trial fraction does not leak into `b`), then shrunk by duration and
bounded. The shipped calibration
`src/attest_voice/calibrations/demo-eres2net-librispeech-v1.json` is a
**demo calibration**. It was fitted on real trials, but on clean read
audiobook speech, not on car or far-field audio:

* LibriSpeech dev-clean (CC BY 4.0), 40 speakers split deterministically:
  30 to fit, 10 held out (every 4th speaker id). Enrollment ≥ 20 s voiced
  from one chapter; test segments are voiced crops of 0.5–8 s from other
  chapters. `scripts/librispeech_trials.py` generates the trials.
* Fit on test crops ≥ 3 s: 260 same-speaker / 7,540 different-speaker
  trials. a = 15.13, b = −5.50.
* Fit split (≥ 3 s): EER 5.4%, Cllr 0.157. Mean cosine: same 0.744, different 0.111.
* Held-out speakers (≥ 3 s, 10 speakers): EER 0.09%, Cllr 0.094. The
  fit-split EER is worse than the held-out one, so a few speakers are hard
  for this model. With 10–30 speakers these are rough numbers.

The full fit report, including per-duration results, is stored in the file
under `provenance.fit`. Before relying on a calibration anywhere real,
collect trials from your own microphones, cabin and population, then refit
with `fit-calibration` and a new id.

### Calibrating to your own audio

1. Label stretches of single-speaker audio from the microphones you will
   deploy into a manifest (`attest_voice/trials.py` documents the format).
   Each speaker needs at least two *sessions* (separate recordings): one
   to enroll from, the others to test on.
2. `uv run python scripts/manifest_trials.py manifest.jsonl trials.jsonl`
3. `uv run attest-voice fit-calibration trials.jsonl --id <neutral-id> --model 3dspeaker_speech_eres2net_sv_en_voxceleb_16k -o cal.json`,
   then set `"calibration": "cal.json"` in the config.

The trials file holds only scores, so it and the calibration can be kept
without the audio. Pitfalls that make trials look easier than deployment:

* **Same recording on both sides.** A target trial against audio from the
  enrollment session shares room, microphone and day, and scores far higher
  than a real later attempt. The script never does this.
* **Bleed.** In a house with several microphones the same utterance lands
  in more than one recording at once. Treat simultaneous recordings as one
  session.
* **TV, radio and music.** VAD fires on them. A broadcast voice labeled as
  a household speaker poisons both classes. Label by ear, not by VAD.
* **Speaker labels from cameras.** A face in view is not the person
  talking. Use such labels only for single-voice audio, and check some.
  If same-speaker trials do not score clearly above different-speaker
  ones (EER near 50%, a slope near zero or negative), the labels or the
  audio carry no voice identity and the fit must not be used.
* **Labels from this producer's diarization.** Clusters are made with the
  same embeddings at a fixed threshold, so they confirm the model's own
  view of who is the same person. Use them to find audio, not to label it.

**Duration shrink.** For a score backed by `d` seconds of voiced audio:

* `d < 0.5 s`: LLR 0, in both directions.
* Otherwise `w = clamp((d − 0.5)/3.5, 0, 1)`.
  * A positive LLR becomes `min(llr·w, cap(d))`. `cap` is piecewise linear
    through (0.5 s, 0), (1.5 s, 1.2), (3 s, 8) and unbounded after that.
  * A negative LLR becomes `llr·max(w, 0.5)`. Evidence against an identity
    is the fail-safe direction, so it is shrunk less.

The cap keeps anything under 1.5 s below ln 4 ≈ 1.39. With one enrolled
identity and attest's `priorUnknown` 0.5, ln 4 is what the 0.8 posterior of
the lowest example band needs. The unit tests check this against attest's
fusion formula.

The shrink is a policy choice, not a calibration improvement, and it has a
cost. On the clean LibriSpeech trials the per-duration fits are close to the
global fit even at 1 s (a ≈ 16.8, b ≈ −4.9). Shrinking therefore makes
short-segment Cllr worse: at 1 s it goes from 0.25 to 0.53. It is kept
because short real-world segments are less trustworthy than clean crops:
they can be contaminated by diarization errors, noise, or someone else's
voice.

## Browser enrollment (`serve`)

`POST /enroll`, header `Authorization: Bearer <enrollment ticket>`, body
either the raw recording (`MediaRecorder` output: `audio/webm;codecs=opus` in
Chrome and Firefox, `audio/mp4` with AAC in Safari, or any format PyAV
decodes) or `multipart/form-data` with one or more `take` parts (at most 16),
each a separately recorded file.

Takes let a person add to what they already said without starting over. The
page keeps its takes in memory and, on each save, sends *all* of them with a
fresh ticket; the producer decodes each, joins them with a short silence and
judges the whole. Nothing is kept between requests, so there is no partial
enrollment to clean up and a ticket still names one identity, is redeemed by
one producer and is spent once.

1. The takes are decoded (at most 120 s of audio in all) and voice activity
   measured. The sample is refused with **422** *before* the ticket is touched,
   so a retry can reuse it:

   | `reason` | When |
   |---|---|
   | `too_short` | less than `enroll.minVoicedSeconds` (default 15 s) of speech |
   | `mostly_silence` | as above, and under 30% of the recording is speech |
   | `too_quiet` | the loudest sample is under 1% of full scale (a muted or wrong microphone), or the speech is quieter than about -46 dBFS |
   | `clipping` | more than 1% of the speech samples are at full scale |

   The body is `{"error", "reason", "voicedSeconds", "requiredSeconds",
   "recordedSeconds"}`; `error` is for logs, a client words its own message
   from `reason` and the numbers.
2. The ticket is redeemed at attest:
   `POST {attest}/v1/enrollment-tickets/redeem {"ticket": "..."}` with the
   producer's own credential. 200 `{"identityId"}` → stored; attest's 404
   with a JSON `error` body (unknown, used, or expired ticket) → **401**.
3. **201** `{"identityId", "voicedSeconds", "recordedSeconds", "replaced"}`,
   plus `previousVoicedSeconds` when it replaced an earlier voiceprint. An
   earlier voiceprint is only ever replaced by one that passed every check.

Other replies: 415 undecodable, 413 too large (`enroll.maxBodyBytes`, default
20 MiB; a minute of browser audio is well under 1 MiB) or longer than 120 s,
**502** when attest is unreachable or answers anything else: no redeem route
(a plain-text 404), 403 (attest's `enroll.voiceProducer` names another
producer), 5xx, or a 200 without an identity. The 502 body says which of the
two it was, nothing is stored, and the cause is logged.
CORS allows only `enroll.corsOrigin` (methods `GET`, `POST`, `DELETE`):
preflight is answered for it, and requests carrying any other `Origin` get 403.

`GET /enroll` answers `{"minVoicedSeconds", "maxSeconds"}`, so the page's
meter and its disabled Finish button use this producer's configured minimum
rather than a guess.

### Deleting a voice

`DELETE /enroll`, header `Authorization: Bearer <voice deletion ticket>`. The
page gets the ticket from attest (`POST /enroll/voice-deletion-ticket`); it is
single-use, lasts five minutes, and is good for deletion only: attest refuses
it at the enrollment redeem route, and refuses an enrollment ticket here.

1. The ticket is redeemed at attest:
   `POST {attest}/v1/voice-deletion-tickets/redeem {"ticket": "..."}` with the
   producer's own credential. attest clears the identity's enrollment date and
   saves that *before* it answers 200 `{"identityId", "hadVoice"}`; if it
   cannot save, it puts the date back and answers 500, and nothing is deleted.
2. The voiceprint is deleted, under the same lock as enrollment, so a
   recording being stored cannot interleave with it. (Asking for a deletion
   ticket also voids the person's unredeemed enrollment ticket.)
3. **200** `{"identityId", "deleted": true, "held"}`. `held` is false when
   there was no print; deleting twice is not an error.

Other replies: **401** no ticket, or attest says it is unknown, used, expired,
or not a deletion ticket; **502** attest unreachable or answering anything
else (nothing deleted, attest unchanged); **500** `{"error", "deleted":
false}` when attest has already forgotten the date but the print could not be
deleted here (logged). That last case is the one partial failure: attest no
longer lists a voice the producer still holds. The page says so and offers to
try again; a retry gets a fresh ticket and deletes. attest never lists a voice
the producer has deleted.

```js
const form = new FormData();
for (const take of takes) form.append("take", take);   // Blobs from MediaRecorder
await fetch("http://127.0.0.1:8431/enroll", {method: "POST", body: form,
  headers: {"Authorization": `Bearer ${ticket}`}});
```

The recording is decoded in memory and discarded. Voiceprints (the embedding,
its model, voiced seconds and date) are kept in `{dataDir}/voiceprints.json`:
a 0700 directory and a 0600 file, replaced atomically, keyed by identity id.
`forget --identity` deletes one from the command line; it does not tell attest,
so attest's enrollment date stays until the person enrolls or deletes again. Audio and embeddings are never logged or
transmitted. `serve` warns at startup when `enroll.minVoicedSeconds` is under
15 s: prints from less speech score unreliably on real room audio.

## Reproduce the end-to-end demo

```bash
scripts/download-models.sh
scripts/fetch-librispeech.sh          # dev-clean, ~340 MB, ~/.cache/librispeech
GO=~/sdk/go/bin/go scripts/demo.sh    # builds and starts attest from this repo on :8420
```

`scripts/make_demo_audio.py` builds a 27 s "car" conversation from
LibriSpeech held-out speakers:

* A (652, male, the owner) is enrolled from a chapter that is not in the
  conversation.
* B (5536, male, the passenger) is not enrolled.
* C (2035, female) is enrolled but absent.

The script order is: A 6.3 s, then B's 0.9 s interjection ("It was silent"),
then A twice, the second time with B starting 1 s before A finishes, then B
for 9 s. Recorded result (2026-11-09):

| | |
|---|---|
| Diarization | Merged B's interjection into A's turn: `[0.03–13.97 spk0] [14.51–18.46 spk0] [17.6–26.76 spk1]` |
| Guard | Interjection vs A's earlier 6.3 s: cosine −0.04, raw LLR −6.1 → cut; its words `subject: null` |
| A's words | 0.1–6.3 s and 8.0–13.9 s attributed to `spk0` |
| Overlap | The contiguous run 14.7–20.5 s (A's last sentence + B's start) → `null` |
| B's later words | Attributed to `spk1` |
| spk0 evidence | A **+8.0** (cosine 0.92), C −5.2 |
| spk1 evidence | A **−4.1** (cosine 0.09), C −3.9 |
| attest, spk0 | `resolved`, leading A, 0.9993; `music.control` granted |
| attest, spk1 | `leading: null`, unknown 0.982; `music.control` denied |
| Retirement | After a normal run (no `--keep-subjects`) both subjects' beliefs return 404 |

When this was recorded, attest reported spk1's state as `ambiguous`: both
candidates were tiny (0.0098 and 0.0082) and within `ambiguityMargin` of each
other, and fusion checked that before comparing the leader with the unknown
mass. Fusion now checks the unknown mass first, so spk1 is `unknown`. The
outcome is the same (no leader, denied).

Runtime on this machine's CPU (4 vCPU, Xeon Platinum 8260, 4 threads):
about 19 s per minute of audio (RTF 0.32 on 120 s). Diarization is ~65% of
that and ASR ~20%. Model load time is included.

## Tests

```bash
uv run pytest
```

The tests need no models. They cover:

* LLR fitting (prior-offset correctness), the shrink against attest's
  fusion math, and bounds.
* Word attribution: overlap, straddle, uncovered words, pauses.
* Evidence payloads and a whole session with a fake engine against a fake
  attest, including the guard and retirement.
* Enrollment: webm/opus and mp4/AAC (plain and fragmented) decoding, encoded
  by PyAV; takes that add up across requests; each refusal reason, without
  redeeming the ticket; a refusal never replacing a stored voiceprint; the
  recording not being kept; unknown tickets, CORS, file modes.
* OIDC client credentials and token caching.
* Trials from a manifest: target trials only across sessions.

## Known limitations

* **Presentation / replay.** Nothing here detects a recording, a
  loudspeaker, or synthetic speech. A replayed or cloned voice of the owner
  will score as the owner. Do not use voice alone for anything that matters;
  attest's `minModalities` exists for this.
* **Calibration validity.** It was fitted on clean read English
  (LibriSpeech) with 30 speakers. Cabin noise, other microphones, children,
  other languages, and emotional or whispered speech all shift the score
  distributions, so the LLRs are then not what they claim. Refit per
  deployment.
* **Diarization on short turns.** The guard catches short islands that
  are separated by pauses. It cannot catch an interjection with no pause
  around it. That case is only caught when it is a contested word, i.e.
  when diarization marks an overlap or speaker change there. A missed speaker change inside continuous speech is
  attributed to the surrounding speaker.
* The cluster threshold (0.5) was not tuned. With a known number of
  speakers, pass `--speakers`.
* ASR timestamps have 80 ms resolution, and words cut at a VAD boundary can
  be truncated or hallucinated (the demo's "because").
* Offline only (see top). Evidence for a long session is sent in a burst at
  processing time, not as the audio happens.
