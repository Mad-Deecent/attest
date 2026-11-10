import json

import numpy as np

from attest_voice.audio import SAMPLE_RATE
from attest_voice.trials import Segment, build_trials, load_manifest, voiced_reader


def tone(speaker: int, seconds: float) -> np.ndarray:
    return np.full(int(seconds * SAMPLE_RATE), 0.1 * (speaker + 1), dtype=np.float32)


def embed(samples: np.ndarray) -> np.ndarray:
    # One axis per 'speaker' amplitude; a mixed window points between them.
    v = np.zeros(4, dtype=np.float32)
    for i in range(4):
        v[i] = np.sum(np.isclose(samples, 0.1 * (i + 1)))
    return v / (np.linalg.norm(v) + 1e-9)


def segs() -> list[Segment]:
    return [
        Segment("a1", "A", "s1"), Segment("a1b", "A", "s1"), Segment("a2", "A", "s2"),
        Segment("b1", "B", "s1"), Segment("b3", "B", "s3"),
        Segment("c1", "C", "s1"),  # single session: enrolled, never tested
    ]


def voiced(s: Segment) -> np.ndarray:
    return tone("ABC".index(s.speaker), 10 if s.path != "a1b" else 12)


def test_targets_come_only_from_other_sessions():
    calls: list[int] = []

    def counting(x: np.ndarray) -> np.ndarray:
        calls.append(len(x))
        return embed(x)

    rows = build_trials(segs(), voiced, counting, enrol_seconds=20, durations=(1.0, 3.0))
    same = [r for r in rows if r["same"]]
    # A: tested on s2 only, B: on s3 only; C has no other session.
    assert len(same) == 4
    assert all(r["score"] > 0.99 for r in same)
    diff = [r for r in rows if not r["same"]]
    assert len(diff) == 4 * 2  # each test crop against the two other prints
    assert all(abs(r["score"]) < 1e-6 for r in diff)
    assert {r["seconds"] for r in rows} == {1.0, 3.0}
    assert set(rows[0]) == {"score", "same", "seconds"}


def test_enrols_from_the_session_with_most_speech():
    s = [Segment("x", "A", "short"), Segment("y", "A", "long"), Segment("z", "B", "other")]

    def v(seg: Segment) -> np.ndarray:
        return tone("AB".index(seg.speaker), {"x": 4, "y": 30, "z": 10}[seg.path])

    rows = build_trials(s, v, embed, durations=(3.0,))
    # A enrolled from 'long', so the 4 s 'short' segment is A's only test.
    assert sum(r["same"] for r in rows) == 1


def test_manifest_and_reader(tmp_path):
    (tmp_path / "m.jsonl").write_text(
        json.dumps({"path": "r.wav", "speaker": 1, "session": "k", "start": 1, "end": 3}) + "\n\n"
    )
    [seg] = load_manifest(tmp_path / "m.jsonl")
    assert seg == Segment(str(tmp_path / "r.wav"), "1", "k", 1.0, 3.0)
    audio = np.arange(5 * SAMPLE_RATE, dtype=np.float32)
    read = voiced_reader(lambda p: audio, lambda a: [(0.0, 1.0)])
    out = read(seg)
    assert len(out) == SAMPLE_RATE and out[0] == SAMPLE_RATE
