"""Short-island guard.

Diarization is least reliable exactly where it matters most: a short
interjection between two turns of someone else ("yeah, send it") tends to be
clustered into the surrounding speaker. So every pause-delimited stretch of
speech (a VAD island) shorter than ``max_seconds`` is re-checked: its
embedding is scored against the attributed speaker's *earlier* audio (only the
past, so the rule works unchanged on a stream) and must reach ``min_llr`` on
the raw calibrated scale. Islands that fail, or that have too little earlier
reference to be checked, are cut out of the speaker's turns: their words are
attributed to nobody and their audio does not feed the speaker's voiceprint.

Cost: a genuine short reply by the owner ("yes") can fail and come out
unattributed. The safe direction; the agent should re-ask.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Callable, Sequence

import numpy as np

from .attribution import Turn
from .calibration import Calibration
from .speaker import Interval, cut, intersect, overlaps, subtract, voiceprint

REFERENCE_SECONDS = 30.0  # most recent earlier speech used as the reference


@dataclass(frozen=True)
class IslandCheck:
    start: float
    end: float
    speaker: int | None
    voiced: float
    reference: float  # seconds of earlier reference speech
    score: float | None
    llr: float | None
    kept: bool


def _owner(iv: Interval, turns: Sequence[Turn]) -> int | None:
    cover: dict[int, float] = {}
    for t in turns:
        o = min(iv[1], t.end) - max(iv[0], t.start)
        if o > 0:
            cover[t.speaker] = cover.get(t.speaker, 0.0) + o
    if not cover:
        return None
    spk, best = max(cover.items(), key=lambda kv: kv[1])
    return spk if best / sum(cover.values()) >= 0.8 else None


def guard_short_islands(
    audio: np.ndarray, islands: Sequence[Interval], turns: Sequence[Turn], embed: Callable[[np.ndarray], np.ndarray],
    cal: Calibration, *, max_seconds: float = 1.5, min_reference: float = 2.0, min_llr: float = 2.0,
) -> tuple[list[Turn], list[IslandCheck]]:
    shared = overlaps(turns)
    rejected: list[Interval] = []
    checks: list[IslandCheck] = []
    for iv in sorted(islands):
        dur = iv[1] - iv[0]
        if dur >= max_seconds:
            continue
        spk = _owner(iv, turns)
        if spk is None:
            continue  # already nobody's
        own = [(t.start, t.end) for t in turns if t.speaker == spk]
        ref_iv = subtract(intersect(own, [x for x in islands if x[1] <= iv[0]]), shared + rejected)
        # Keep only the most recent REFERENCE_SECONDS.
        kept_ref: list[Interval] = []
        total = 0.0
        for s, e in reversed(ref_iv):
            take = min(e - s, REFERENCE_SECONDS - total)
            if take <= 0:
                break
            kept_ref.insert(0, (e - take, e))
            total += take
        score = llr = None
        ok = False
        if total >= min_reference:
            ref = voiceprint(cut(audio, kept_ref), embed)
            mine = voiceprint(cut(audio, [iv]), embed)
            if ref is not None and mine is not None:
                score = float(np.dot(ref, mine))
                llr = cal.raw_llr(score)
                ok = llr >= min_llr
        if not ok:
            rejected.append(iv)
        checks.append(IslandCheck(iv[0], iv[1], spk, dur, round(total, 2), score, llr, ok))
    if not rejected:
        return list(turns), checks
    out: list[Turn] = []
    for t in turns:
        for s, e in subtract([(t.start, t.end)], rejected):
            out.append(Turn(s, e, t.speaker))
    return sorted(out, key=lambda t: t.start), checks

