"""Speaker embeddings over voiced audio, and scoring against voiceprints.

Enrollment and session scoring use the same procedure: voiced audio is
concatenated, cut into fixed windows, each window embedded and L2-normalized,
and the embedding is the duration-weighted mean, renormalized. During a
session complete windows are cached, so re-scoring a growing speaker costs one
window, not the whole history.
"""

from __future__ import annotations

from typing import Callable, Iterable, Sequence

import numpy as np

from .attribution import Turn
from .audio import SAMPLE_RATE
from .calibration import Calibration
from .store import Voiceprint

Interval = tuple[float, float]
Embed = Callable[[np.ndarray], np.ndarray]

WINDOW_SECONDS = 10.0
MIN_WINDOW_SECONDS = 0.5


def merge(iv: Iterable[Interval]) -> list[Interval]:
    out: list[list[float]] = []
    for s, e in sorted(iv):
        if e <= s:
            continue
        if out and s <= out[-1][1]:
            out[-1][1] = max(out[-1][1], e)
        else:
            out.append([s, e])
    return [(s, e) for s, e in out]


def intersect(a: Sequence[Interval], b: Sequence[Interval]) -> list[Interval]:
    a, b = merge(a), merge(b)
    out, i, j = [], 0, 0
    while i < len(a) and j < len(b):
        s, e = max(a[i][0], b[j][0]), min(a[i][1], b[j][1])
        if e > s:
            out.append((s, e))
        if a[i][1] < b[j][1]:
            i += 1
        else:
            j += 1
    return out


def subtract(a: Sequence[Interval], b: Sequence[Interval]) -> list[Interval]:
    out: list[Interval] = []
    b = merge(b)
    for s, e in merge(a):
        cur = s
        for bs, be in b:
            if be <= cur or bs >= e:
                continue
            if bs > cur:
                out.append((cur, bs))
            cur = max(cur, be)
        if cur < e:
            out.append((cur, e))
    return out


def overlaps(turns: Sequence[Turn]) -> list[Interval]:
    """Time where two or more speakers are active."""
    out: list[Interval] = []
    for i, a in enumerate(turns):
        for b in turns[i + 1 :]:
            if a.speaker != b.speaker:
                s, e = max(a.start, b.start), min(a.end, b.end)
                if e > s:
                    out.append((s, e))
    return merge(out)


def seconds(iv: Sequence[Interval]) -> float:
    return sum(e - s for s, e in iv)


def cut(audio: np.ndarray, iv: Sequence[Interval]) -> np.ndarray:
    parts = [audio[int(s * SAMPLE_RATE) : int(e * SAMPLE_RATE)] for s, e in iv]
    return np.concatenate(parts) if parts else np.zeros(0, dtype=np.float32)


class Accumulator:
    """Running embedding of one speaker's voiced audio."""

    def __init__(self, embed: Embed, window: float = WINDOW_SECONDS):
        self.embed = embed
        self.win = int(window * SAMPLE_RATE)
        self.done: list[tuple[np.ndarray, int]] = []  # (embedding, samples)
        self.tail = np.zeros(0, dtype=np.float32)

    @property
    def voiced_seconds(self) -> float:
        return (sum(n for _, n in self.done) + len(self.tail)) / SAMPLE_RATE

    def add(self, samples: np.ndarray) -> None:
        buf = np.concatenate([self.tail, samples])
        while len(buf) >= self.win:
            self.done.append((self.embed(buf[: self.win]), self.win))
            buf = buf[self.win :]
        self.tail = buf

    def embedding(self) -> np.ndarray | None:
        parts = list(self.done)
        # A short tail only adds noise once there is a full window.
        if len(self.tail) >= MIN_WINDOW_SECONDS * SAMPLE_RATE or (not parts and len(self.tail) > 0):
            parts.append((self.embed(self.tail), len(self.tail)))
        if not parts:
            return None
        v = sum(e * n for e, n in parts)
        return v / (np.linalg.norm(v) + 1e-9)


def voiceprint(samples: np.ndarray, embed: Embed) -> np.ndarray | None:
    acc = Accumulator(embed)
    acc.add(samples)
    return acc.embedding()


def score_all(
    emb: np.ndarray | None, voiced_seconds: float, prints: Sequence[Voiceprint], cal: Calibration,
) -> dict[str, tuple[float, float]]:
    """identity -> (cosine score, LLR). Every enrolled identity gets an LLR,
    because a negative one is evidence too. Prints from another embedding
    model are skipped."""
    out: dict[str, tuple[float, float]] = {}
    for p in prints:
        if cal.model and p.model != cal.model:
            continue
        if emb is None:
            out[p.identity] = (float("nan"), 0.0)
            continue
        s = float(np.dot(emb, p.embedding) / (np.linalg.norm(p.embedding) + 1e-9))
        out[p.identity] = (s, cal.llr(s, voiced_seconds))
    return out
