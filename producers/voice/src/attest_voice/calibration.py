"""Score -> LLR mapping.

A calibration is affine in the cosine score, ``llr = a*score + b``, fitted by
logistic regression on labeled same/different-speaker trials, plus a
duration-aware shrink so that a little speech cannot produce strong evidence.

Shrink, for ``d`` seconds of voiced audio behind the score:

* ``w(d) = clamp((d - shrink.floor) / (shrink.full - shrink.floor), 0, 1)``.
  Both signs are scaled by ``w``, so with under ``floor`` seconds nothing is
  claimed either way.
* Positive LLRs are additionally capped by ``cap(d)``, a piecewise-linear
  table (``shrink.positiveCap``, pairs of ``[seconds, maxLLR]``). The shipped
  table keeps anything under 1.5 s below ln 4 (the posterior 0.8 needed for
  attest's lowest example band with one enrolled identity).
* Negatives are scaled by ``max(w, shrink.negativeMinWeight)`` instead of
  ``w`` once ``d >= floor``: evidence *against* an identity is the fail-safe
  direction, so it is shrunk less.

Everything is finally bounded to ``[-bound, bound]``.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Sequence

import numpy as np


@dataclass(frozen=True)
class Shrink:
    floor: float = 0.5
    full: float = 4.0
    negative_min_weight: float = 0.5
    positive_cap: tuple[tuple[float, float], ...] = ((0.5, 0.0), (1.5, 1.2), (3.0, 8.0))

    def weight(self, seconds: float) -> float:
        if self.full <= self.floor:
            return 1.0 if seconds >= self.full else 0.0
        return min(1.0, max(0.0, (seconds - self.floor) / (self.full - self.floor)))

    def cap(self, seconds: float) -> float:
        xs = [p[0] for p in self.positive_cap]
        ys = [p[1] for p in self.positive_cap]
        if seconds <= xs[0]:
            return ys[0]
        if seconds >= xs[-1]:
            return math.inf
        return float(np.interp(seconds, xs, ys))


@dataclass(frozen=True)
class Calibration:
    id: str
    a: float
    b: float
    bound: float = 8.0
    shrink: Shrink = field(default_factory=Shrink)
    model: str = ""
    provenance: dict[str, Any] = field(default_factory=dict)

    def raw_llr(self, score: float) -> float:
        return self.a * score + self.b

    def llr(self, score: float, voiced_seconds: float) -> float:
        raw = self.raw_llr(score)
        if voiced_seconds < self.shrink.floor:
            return 0.0
        w = self.shrink.weight(voiced_seconds)
        if raw > 0:
            out = min(raw * w, self.shrink.cap(voiced_seconds))
        else:
            out = raw * max(w, self.shrink.negative_min_weight)
        return max(-self.bound, min(self.bound, out))

    def to_json(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "model": self.model,
            "a": self.a,
            "b": self.b,
            "bound": self.bound,
            "shrink": {
                "floor": self.shrink.floor,
                "full": self.shrink.full,
                "negativeMinWeight": self.shrink.negative_min_weight,
                "positiveCap": [list(p) for p in self.shrink.positive_cap],
            },
            "provenance": self.provenance,
        }

    @staticmethod
    def from_json(d: dict[str, Any]) -> "Calibration":
        if not d.get("id"):
            raise ValueError("calibration needs a non-empty id")
        s = d.get("shrink", {})
        default = Shrink()
        shrink = Shrink(
            floor=float(s.get("floor", default.floor)),
            full=float(s.get("full", default.full)),
            negative_min_weight=float(s.get("negativeMinWeight", default.negative_min_weight)),
            positive_cap=tuple((float(x), float(y)) for x, y in s.get("positiveCap", default.positive_cap)),
        )
        return Calibration(
            id=d["id"], a=float(d["a"]), b=float(d["b"]), bound=float(d.get("bound", 8.0)),
            shrink=shrink, model=d.get("model", ""), provenance=d.get("provenance", {}),
        )


def load(path: str | Path) -> Calibration:
    return Calibration.from_json(json.loads(Path(path).read_text()))


def fit_affine(scores: Sequence[float], same: Sequence[bool], iters: int = 50) -> tuple[float, float]:
    """Fit ``llr = a*score + b`` by class-balanced logistic regression (IRLS).

    Plain logistic regression returns ``llr + log(pi/(1-pi))`` where pi is the
    fraction of same-speaker trials. Weighting each class to equal total mass
    makes the effective prior 0.5, so the intercept is the LLR intercept.
    """
    x = np.asarray(scores, dtype=np.float64)
    y = np.asarray(same, dtype=np.float64)
    n_pos, n_neg = y.sum(), (1 - y).sum()
    if n_pos == 0 or n_neg == 0:
        raise ValueError("need both same-speaker and different-speaker trials")
    sw = np.where(y == 1, 0.5 / n_pos, 0.5 / n_neg)
    X = np.stack([x, np.ones_like(x)], axis=1)
    beta = np.zeros(2)
    for _ in range(iters):
        p = 1 / (1 + np.exp(-(X @ beta)))
        g = X.T @ (sw * (y - p))
        h = (X * (sw * p * (1 - p))[:, None]).T @ X + 1e-9 * np.eye(2)
        step = np.linalg.solve(h, g)
        beta += step
        if np.abs(step).max() < 1e-10:
            break
    return float(beta[0]), float(beta[1])


def cllr(llrs: Sequence[float], same: Sequence[bool]) -> float:
    """Log-likelihood-ratio cost in bits; 1.0 is an uninformative system."""
    l = np.asarray(llrs, dtype=np.float64)
    y = np.asarray(same, dtype=bool)
    tar = np.mean(np.log2(1 + np.exp(-l[y])))
    non = np.mean(np.log2(1 + np.exp(l[~y])))
    return float(0.5 * (tar + non))


def eer(scores: Sequence[float], same: Sequence[bool]) -> float:
    s = np.asarray(scores, dtype=np.float64)
    y = np.asarray(same, dtype=bool)
    tar, non = np.sort(s[y]), np.sort(s[~y])
    t = np.unique(s)
    frr = np.searchsorted(tar, t, side="left") / len(tar)  # targets below threshold
    far = 1 - np.searchsorted(non, t, side="left") / len(non)  # non-targets at or above
    return float(np.min(np.maximum(frr, far)))
