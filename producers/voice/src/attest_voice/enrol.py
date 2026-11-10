"""Turning an enrolment sample into a stored voiceprint."""

from __future__ import annotations

from typing import Protocol

import numpy as np

from .speaker import cut, voiceprint
from .store import Store


class EnrolEngine(Protocol):
    def vad(self, audio: np.ndarray) -> list[tuple[float, float]]: ...
    def embed(self, samples: np.ndarray) -> np.ndarray: ...


class TooLittleSpeech(ValueError):
    def __init__(self, voiced: float, needed: float):
        super().__init__(
            f"Heard {voiced:.1f} s of speech; enrolment needs at least {needed:.0f} s. "
            "Please record again and keep talking naturally until the timer ends."
        )
        self.voiced = voiced
        self.needed = needed


def enrol(identity: str, audio: np.ndarray, engine: EnrolEngine, store: Store, *, model: str, min_seconds: float) -> float:
    """Store a voiceprint for ``identity``; returns voiced seconds used."""
    samples, voiced = check(audio, engine, min_seconds)
    vec = voiceprint(samples, engine.embed)
    assert vec is not None
    store.put(identity, vec, model, voiced)
    return voiced


def check(audio: np.ndarray, engine: EnrolEngine, min_seconds: float) -> tuple[np.ndarray, float]:
    """Voiced samples of ``audio``, or TooLittleSpeech."""
    iv = engine.vad(audio)
    voiced = sum(e - s for s, e in iv)
    if voiced < min_seconds:
        raise TooLittleSpeech(voiced, min_seconds)
    return cut(audio, iv), voiced
