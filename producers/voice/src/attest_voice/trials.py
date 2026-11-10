"""Labelled same/different-speaker trials from your own recordings, for
`attest-voice fit-calibration`.

A manifest is JSON lines, one labelled stretch of single-speaker audio each:

    {"path": "rec/kitchen-0412.wav", "speaker": "p1", "session": "0412-kitchen", "start": 3.2, "end": 11.0}

`start`/`end` are optional (default: the whole file). `session` is one
recording occasion (one microphone, one sitting). Each speaker is enrolled
from the session where they have the most speech, and only tested on audio
from their *other* sessions, so a target trial never compares a recording
with itself. Speakers seen in a single session are enrolled (they are
impostors for everyone else) but give no target trials.
"""

from __future__ import annotations

import json
import random
from collections import defaultdict
from collections.abc import Callable, Iterable, Sequence
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .audio import SAMPLE_RATE
from .speaker import cut, voiceprint

DURATIONS = (0.5, 1.0, 1.5, 2.0, 3.0, 5.0, 8.0)


@dataclass(frozen=True)
class Segment:
    path: str
    speaker: str
    session: str
    start: float = 0.0
    end: float | None = None


def load_manifest(path: str | Path) -> list[Segment]:
    base = Path(path).parent
    out = []
    for line in Path(path).read_text().splitlines():
        if not line.strip():
            continue
        d = json.loads(line)
        p = Path(d["path"])
        out.append(Segment(str(p if p.is_absolute() else base / p), str(d["speaker"]), str(d["session"]),
                           float(d.get("start", 0.0)), None if d.get("end") is None else float(d["end"])))
    return out


def build_trials(
    segments: Iterable[Segment],
    voiced: Callable[[Segment], np.ndarray],
    embed: Callable[[np.ndarray], np.ndarray],
    *,
    enrol_seconds: float = 20.0,
    durations: Sequence[float] = DURATIONS,
    tests_per_speaker: int = 6,
    seed: int = 7,
) -> list[dict[str, object]]:
    """`voiced(seg)` returns the segment's voiced samples (16 kHz)."""
    rng = random.Random(seed)
    by: dict[str, dict[str, list[Segment]]] = defaultdict(lambda: defaultdict(list))
    for s in segments:
        by[s.speaker][s.session].append(s)

    audio: dict[Segment, np.ndarray] = {}

    def get(s: Segment) -> np.ndarray:
        if s not in audio:
            audio[s] = voiced(s)
        return audio[s]

    prints: dict[str, np.ndarray] = {}
    tests: list[tuple[str, float, np.ndarray]] = []
    for spk in sorted(by):
        sessions = by[spk]
        enrol = max(sorted(sessions), key=lambda k: sum(len(get(s)) for s in sessions[k]))
        buf, n = [], 0
        for s in sessions[enrol]:
            buf.append(get(s))
            n += len(buf[-1])
            if n >= enrol_seconds * SAMPLE_RATE:
                break
        p = voiceprint(np.concatenate(buf), embed) if n else None
        if p is None:
            continue
        prints[spk] = p
        pool = [s for k in sorted(sessions) if k != enrol for s in sessions[k]]
        rng.shuffle(pool)
        for s in pool[:tests_per_speaker]:
            v = get(s)
            for d in durations:
                k = int(d * SAMPLE_RATE)
                if len(v) < k:
                    continue
                off = rng.randrange(0, len(v) - k + 1)
                tests.append((spk, d, embed(v[off : off + k])))

    return [
        {"score": round(float(np.dot(e, p) / (np.linalg.norm(p) + 1e-9)), 5), "same": enr == spk, "seconds": d}
        for spk, d, e in tests
        for enr, p in prints.items()
    ]


def voiced_reader(decode: Callable[[str], np.ndarray], vad: Callable[[np.ndarray], list[tuple[float, float]]]):
    """`voiced` for build_trials: decode once per file, crop, keep VAD speech."""
    files: dict[str, np.ndarray] = {}

    def voiced(s: Segment) -> np.ndarray:
        if s.path not in files:
            files[s.path] = decode(s.path)
        a = files[s.path]
        a = a[int(s.start * SAMPLE_RATE) : None if s.end is None else int(s.end * SAMPLE_RATE)]
        return cut(a, vad(a))

    return voiced
