import math

import numpy as np
import pytest

from attest_voice.calibration import Calibration, Shrink, cllr, eer, fit_affine

CAL = Calibration(id="t", a=15.0, b=-5.5)


def posterior(llr: float, enrolled: int = 1, prior_unknown: float = 0.5) -> float:
    """attest's fusion for one candidate (internal/perception/fusion.go)."""
    prior = (1 - prior_unknown) / enrolled
    w = prior * math.exp(llr)
    return w / (w + prior_unknown + (enrolled - 1) * prior)


def test_fit_recovers_known_llr_regardless_of_target_fraction():
    # Equal-variance Gaussians: the true LLR is affine in the score.
    rng = np.random.default_rng(0)
    mu1, mu0, sd = 0.5, 0.3, 0.1
    a_true = (mu1 - mu0) / sd**2
    b_true = (mu0**2 - mu1**2) / (2 * sd**2)
    for n_same in (500, 5000):  # target fraction ~9% and 50%
        s = np.concatenate([rng.normal(mu1, sd, n_same), rng.normal(mu0, sd, 5000)])
        y = np.concatenate([np.ones(n_same, bool), np.zeros(5000, bool)])
        a, b = fit_affine(s, y)
        assert a == pytest.approx(a_true, rel=0.1)
        # The decision point (llr = 0) is where the prior offset would show.
        assert -b / a == pytest.approx(-b_true / a_true, abs=0.01)


def test_fit_needs_both_classes():
    with pytest.raises(ValueError):
        fit_affine([0.1, 0.2], [True, True])


def test_long_speech_is_unshrunk_but_bounded():
    assert CAL.llr(0.6, 10) == pytest.approx(15 * 0.6 - 5.5)
    assert CAL.llr(0.99, 10) == 8.0
    assert CAL.llr(-0.5, 10) == -8.0


def test_too_little_speech_says_nothing():
    for s in (-1.0, 0.0, 0.5, 1.0):
        assert CAL.llr(s, 0.3) == 0.0


@pytest.mark.parametrize("secs", [0.5, 0.8, 1.0, 1.2, 1.49])
def test_short_speech_cannot_reach_lowest_band(secs):
    # With one enrolled identity, posterior 0.8 needs llr >= ln 4. Even a
    # perfect score from under 1.5 s must stay below it.
    llr = CAL.llr(1.0, secs)
    assert llr < math.log(4)
    assert posterior(llr) < 0.8


def test_positive_llr_grows_with_duration():
    vals = [CAL.llr(0.8, d) for d in (0.6, 1.0, 1.5, 2.0, 3.0, 5.0)]
    assert vals == sorted(vals)
    assert vals[-1] == pytest.approx(15 * 0.8 - 5.5)


def test_negatives_shrink_less_than_positives():
    secs = 1.0
    w = CAL.shrink.weight(secs)
    neg = CAL.llr(0.1, secs)
    assert neg < 0
    assert abs(neg) >= abs(CAL.raw_llr(0.1)) * max(w, 0.5) - 1e-9


def test_json_roundtrip():
    c = Calibration(id="x", a=1, b=2, shrink=Shrink(positive_cap=((1, 0), (2, 3))), model="m")
    assert Calibration.from_json(c.to_json()) == c
    with pytest.raises(ValueError):
        Calibration.from_json({"id": "", "a": 1, "b": 0})


def test_metrics():
    s = [0.9, 0.8, 0.2, 0.1]
    y = [True, True, False, False]
    assert eer(s, y) == 0.0
    assert cllr([0.0] * 4, y) == pytest.approx(1.0)
