"""Turning an enrollment sample into a stored voiceprint.

Before anything is stored the sample is checked: enough voiced speech (by the
VAD), not clipped, not too quiet. A refusal carries a machine-readable reason
so a client can say plainly what went wrong and what to do next. The checks
use only the samples and the VAD's intervals; no extra model.
"""

from __future__ import annotations

from typing import Protocol

import numpy as np

from .audio import SAMPLE_RATE
from .speaker import cut, voiceprint
from .store import Store

# A sample counts as clipped at or above this magnitude (full scale is 1.0).
CLIP_LEVEL = 0.98
# Refuse when more than this share of voiced samples is clipped. Natural
# speech peaks touch full scale now and then; a distorted recording sits there.
CLIP_SHARE = 0.01
# Refuse when voiced speech is quieter than this RMS (about -46 dBFS).
QUIET_RMS = 0.005
# A recording whose loudest sample stays under this is treated as silent: a
# muted or wrong microphone rather than a short recording.
SILENT_PEAK = 0.01
# Under this share of voiced time, a too-short recording is "mostly silence".
SILENCE_SHARE = 0.3


class EnrollEngine(Protocol):
    def vad(self, audio: np.ndarray) -> list[tuple[float, float]]: ...
    def embed(self, samples: np.ndarray) -> np.ndarray: ...


class Refused(ValueError):
    """The sample is not good enough to enroll from. ``reason`` is one of
    ``too_short``, ``mostly_silence``, ``too_quiet``, ``clipping``."""

    def __init__(self, reason: str, message: str, *, voiced: float, needed: float, recorded: float):
        super().__init__(message)
        self.reason = reason
        self.voiced = voiced
        self.needed = needed
        self.recorded = recorded


class TooLittleSpeech(Refused):
    def __init__(self, voiced: float, needed: float, recorded: float, reason: str = "too_short"):
        super().__init__(
            reason,
            f"Heard {voiced:.1f} s of speech; enrollment needs at least {needed:.0f} s.",
            voiced=voiced, needed=needed, recorded=recorded,
        )


def enroll(identity: str, audio: np.ndarray, engine: EnrollEngine, store: Store, *, model: str, min_seconds: float) -> float:
    """Store a voiceprint for ``identity``; returns voiced seconds used.
    Raises Refused, and then the stored print (if any) is untouched."""
    samples, voiced = check(audio, engine, min_seconds)
    vec = voiceprint(samples, engine.embed)
    assert vec is not None
    store.put(identity, vec, model, voiced)
    return voiced


def check(audio: np.ndarray, engine: EnrollEngine, min_seconds: float) -> tuple[np.ndarray, float]:
    """Voiced samples of ``audio`` and their duration, or Refused."""
    recorded = len(audio) / SAMPLE_RATE
    peak = float(np.max(np.abs(audio))) if len(audio) else 0.0
    iv = engine.vad(audio)
    voiced = sum(e - s for s, e in iv)

    def refuse(reason: str, message: str) -> Refused:
        return Refused(reason, message, voiced=voiced, needed=min_seconds, recorded=recorded)

    if peak < SILENT_PEAK:
        raise refuse("too_quiet", "The recording is silent. Is the microphone muted or the wrong one?")
    if voiced < min_seconds:
        mostly_silent = recorded > 0 and voiced / recorded < SILENCE_SHARE
        raise TooLittleSpeech(voiced, min_seconds, recorded, "mostly_silence" if mostly_silent else "too_short")
    samples = cut(audio, iv)
    mag = np.abs(samples)
    if float(np.mean(mag >= CLIP_LEVEL)) > CLIP_SHARE:
        raise refuse("clipping", "The recording is distorted: the voice is too loud for the microphone.")
    if float(np.sqrt(np.mean(samples.astype(np.float64) ** 2))) < QUIET_RMS:
        raise refuse("too_quiet", "The voice in the recording is too quiet.")
    return samples, voiced
