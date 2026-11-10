from datetime import datetime, timedelta, timezone

import numpy as np
import pytest

from attest_voice.attest_client import Attest, StaticToken, evidence_payload
from attest_voice.attribution import Turn, Word
from attest_voice.audio import SAMPLE_RATE
from attest_voice.calibration import Calibration
from attest_voice.pipeline import Session
from attest_voice.store import Voiceprint

from .fakes import FakeAttest, FakeEngine

CAL = Calibration(id="cal-test", a=10.0, b=-5.0, model="fake")


def test_evidence_payload_shape():
    now = datetime(2026, 11, 9, 12, 0, 0, tzinfo=timezone.utc)
    p = evidence_payload("s1/spk0", "car", {"b": -3.21234, "a": 2.5}, "cal-1", ttl=60, now=now)
    assert p == {
        "subject": {"key": "s1/spk0", "zone": "car"},
        "modality": "voice",
        "observedAt": "2026-11-09T12:00:00.000Z",
        "expiresAt": "2026-11-09T12:01:00.000Z",
        "candidates": [{"identityId": "a", "llr": 2.5}, {"identityId": "b", "llr": -3.2123}],
        "calibration": "cal-1",
    }
    assert "zone" not in evidence_payload("k", "", {}, "c", ttl=1)["subject"]
    assert evidence_payload("k", "", {}, "c", ttl=1)["candidates"] == []


def tone(seconds: float, sign: float) -> np.ndarray:
    return np.full(int(seconds * SAMPLE_RATE), 0.1 * sign, dtype=np.float32)


@pytest.fixture
def attest():
    fake = FakeAttest()
    yield fake
    fake.close()


def test_session_end_to_end_with_fakes(attest):
    # Owner (+) talks 6 s; the passenger (-) mutters 0.6 s; the owner says a
    # short "ok" (0.5 s) and then 2 s more. The diarizer, as real ones do,
    # merged the mutter into the owner's turn.
    audio = np.concatenate([tone(6, +1), tone(0.3, 0), tone(0.6, -1), tone(0.3, 0), tone(0.5, +1),
                            tone(0.3, 0), tone(2, +1)])
    turns = [Turn(0, 10.0, 0)]
    words = [Word("call", 1.0, 1.4), Word("mum", 1.5, 1.9), Word("yeah", 6.35, 6.6), Word("send", 6.62, 6.85),
             Word("ok", 7.25, 7.65), Word("thanks", 8.0, 8.4)]
    prints = [
        Voiceprint("owner", np.array([1.0, 0.0], np.float32), "fake", 20, ""),
        Voiceprint("other", np.array([0.0, 1.0], np.float32), "fake", 20, ""),
        Voiceprint("stale", np.array([1.0, 0.0], np.float32), "another-model", 20, ""),
    ]
    events: list[dict] = []
    s = Session("sess", "car", 60, CAL, prints, attest=Attest(attest.url, StaticToken("producer-token")),
                emit=events.append)
    keys = s.run(audio, FakeEngine(turns, words))
    s.retire(keys)

    islands = [(e["start"], e["kept"]) for e in events if e["type"] == "island"]
    assert islands == [(6.3, False), (7.2, True)]
    utts = [e for e in events if e["type"] == "utterance"]
    assert [(u["subject"], u["text"]) for u in utts] == [
        ("sess/spk0", "call mum"), (None, "yeah send"), ("sess/spk0", "ok thanks")]

    posted = [b for p, b, _ in attest.requests if p == "/v1/evidence"]
    assert [b["subject"]["key"] for b in posted] == ["sess/spk0"] * len(posted) and posted
    for b in posted:
        assert b["modality"] == "voice" and b["calibration"] == "cal-test" and b["subject"]["zone"] == "car"
        # Every enrolled identity of this model, never the other model's print.
        assert sorted(c["identityId"] for c in b["candidates"]) == ["other", "owner"]
        t0 = datetime.fromisoformat(b["observedAt"].replace("Z", "+00:00"))
        t1 = datetime.fromisoformat(b["expiresAt"].replace("Z", "+00:00"))
        assert t1 - t0 == timedelta(seconds=60)
    llr = lambda b, i: next(c["llr"] for c in b["candidates"] if c["identityId"] == i)  # noqa: E731
    # The mutter was cut out, so the owner's print stays clean.
    assert llr(posted[-1], "owner") == pytest.approx(5.0) and llr(posted[-1], "other") < -2
    ev = [e for e in events if e["type"] == "evidence"]
    assert ev[-1]["voicedSeconds"] == pytest.approx(8.5, abs=0.05)

    retired = [b["subject"] for p, b, _ in attest.requests if p == "/v1/retirements"]
    assert retired == ["sess/spk0"]
    assert all(a == "Bearer producer-token" for _, _, a in attest.requests)


def test_short_speaker_gets_weak_evidence(attest):
    # A speaker diarized on their own with only 0.8 s of speech.
    audio = np.concatenate([tone(6, +1), tone(0.3, 0), tone(0.8, -1)])
    turns = [Turn(0, 6.0, 0), Turn(6.3, 7.1, 1)]
    prints = [Voiceprint("other", np.array([0.0, 1.0], np.float32), "fake", 20, "")]
    events: list[dict] = []
    s = Session("s", "", 60, CAL, prints, attest=None, emit=events.append, guard={"max_seconds": 0.0})
    s.run(audio, FakeEngine(turns, []))
    ev = {e["subject"]: e for e in events if e["type"] == "evidence"}
    # A perfect match from 0.8 s cannot exceed the 1.5 s cap.
    assert 0 < ev["s/spk1"]["candidates"][0]["llr"] < 0.5


def test_no_enrolled_identities_sends_empty_candidates(attest):
    events: list[dict] = []
    s = Session("s", "", 60, CAL, [], attest=Attest(attest.url, StaticToken("producer-token")), emit=events.append)
    s.run(tone(3, +1), FakeEngine([Turn(0, 3, 0)], []))
    (path, body, _), = attest.requests
    assert path == "/v1/evidence" and body["candidates"] == []
