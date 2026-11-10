"""One session, offline: audio -> diarization + voiceprint evidence + an
attributed transcript.

v0 reads a whole file. The steps are ordered by audio time and emit events as
if live (evidence for a speaker is re-sent after each of their turns, with
everything they have said so far), so a streaming front end can replace the
file reader and the offline diarizer without changing what downstream sees.
"""

from __future__ import annotations

import json
import math
import sys
import time
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Callable, Protocol, Sequence

import numpy as np

from .attest_client import Attest, evidence_payload
from .attribution import Turn, Word, utterances
from .audio import SAMPLE_RATE
from .calibration import Calibration
from .guard import guard_short_islands
from .speaker import Accumulator, cut, intersect, overlaps, score_all, subtract
from .store import Voiceprint


class Engine(Protocol):
    def vad(self, audio: np.ndarray) -> list[tuple[float, float]]: ...
    def diarize(self, audio: np.ndarray) -> list[Turn]: ...
    def embed(self, samples: np.ndarray) -> np.ndarray: ...
    def transcribe(self, samples: np.ndarray, offset: float) -> list[Word]: ...


def subject_key(session: str, speaker: int) -> str:
    return f"{session}/spk{speaker}"


@dataclass
class Session:
    session: str
    zone: str
    ttl: float
    cal: Calibration
    prints: Sequence[Voiceprint]
    attest: Attest | None = None
    emit: Callable[[dict[str, Any]], None] = lambda e: print(json.dumps(e), flush=True)
    callback_url: str = ""
    include_scores: bool = False
    guard: dict[str, float] = field(default_factory=dict)  # guard_short_islands kwargs
    timings: dict[str, float] = field(default_factory=dict)

    def run(self, audio: np.ndarray, engine: Engine) -> list[str]:
        """Process a session; returns the subject keys used."""
        t0 = time.perf_counter()
        voiced = engine.vad(audio)
        t1 = time.perf_counter()
        turns, checks = guard_short_islands(audio, voiced, engine.diarize(audio), engine.embed, self.cal, **self.guard)
        for c in checks:
            ev: dict[str, Any] = {
                "type": "island", "start": round(c.start, 3), "end": round(c.end, 3),
                "diarizedAs": subject_key(self.session, c.speaker) if c.speaker is not None else None,
                "referenceSeconds": c.reference, "kept": c.kept,
            }
            if self.include_scores:
                ev["score"] = None if c.score is None else round(c.score, 4)
                ev["rawLLR"] = None if c.llr is None else round(c.llr, 3)
            self.emit(ev)
        t2 = time.perf_counter()
        words: list[Word] = []
        for s, e in voiced:
            words += engine.transcribe(audio[int(s * SAMPLE_RATE) : int(e * SAMPLE_RATE)], s)
        t3 = time.perf_counter()
        utts = utterances(words, turns)

        # Speaker audio for voiceprinting: voiced, theirs, and not overlapped.
        shared = overlaps(turns)
        events: list[tuple[float, int, Callable[[], None]]] = []
        accs: dict[int, Accumulator] = {}
        for turn in turns:
            own = subtract(intersect([(turn.start, turn.end)], voiced), shared)
            acc = accs.setdefault(turn.speaker, Accumulator(engine.embed))
            events.append((turn.end, 0, self._evidence_step(acc, turn.speaker, cut(audio, own))))
        for u in utts:
            events.append((u.end, 1, self._utterance_step(u)))
        events.sort(key=lambda x: (x[0], x[1]))
        for _, _, step in events:
            step()
        t4 = time.perf_counter()
        self.timings = {
            "audioSeconds": len(audio) / SAMPLE_RATE, "vad": t1 - t0, "diarization": t2 - t1,
            "asr": t3 - t2, "embeddingAndSend": t4 - t3, "total": t4 - t0,
        }
        return [subject_key(self.session, s) for s in sorted(accs)]

    def _evidence_step(self, acc: Accumulator, speaker: int, samples: np.ndarray) -> Callable[[], None]:
        def step() -> None:
            if len(samples) == 0 and acc.voiced_seconds == 0:
                return
            acc.add(samples)
            secs = acc.voiced_seconds
            scored = score_all(acc.embedding(), secs, self.prints, self.cal)
            key = subject_key(self.session, speaker)
            payload = evidence_payload(key, self.zone, {i: l for i, (_, l) in scored.items()}, self.cal.id, ttl=self.ttl)
            if self.attest is not None:
                self.attest.post_evidence(payload)
            event: dict[str, Any] = {
                "type": "evidence", "subject": key, "voicedSeconds": round(secs, 2),
                "calibration": self.cal.id, "candidates": payload["candidates"], "sent": self.attest is not None,
            }
            if self.include_scores:
                event["scores"] = {i: (None if math.isnan(s) else round(s, 4)) for i, (s, _) in scored.items()}
            self.emit(event)
        return step

    def _utterance_step(self, u: Any) -> Callable[[], None]:
        def step() -> None:
            body = u.to_json(lambda spk: subject_key(self.session, spk))
            self.emit({"type": "utterance", **body})
            if self.callback_url:
                _post_json(self.callback_url, body)
        return step

    def retire(self, keys: Sequence[str]) -> None:
        if self.attest is None:
            return
        for k in keys:
            self.attest.retire_subject(k)
            self.emit({"type": "retired", "subject": k})


def _post_json(url: str, body: dict[str, Any]) -> None:
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST", headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5).close()
    except OSError as e:  # the transcript on stdout is authoritative
        print(f"callback failed: {e}", file=sys.stderr)
