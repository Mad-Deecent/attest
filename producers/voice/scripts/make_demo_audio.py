"""Build the two-speaker 'car' demo from LibriSpeech (CC BY 4.0).

Speakers are from the calibration's held-out split. A (owner) and C are
enrolled from chapters that do not appear in the conversation; B (passenger)
is not enrolled. The conversation:

    A  ~6 s request
    B  a one/two-word interjection (<= ~1 s), cut on ASR word boundaries
    A  ~6 s
    A  a few seconds, with B starting ~1 s before A finishes (overlap)
    B  ~8 s

    uv run python scripts/make_demo_audio.py ~/.cache/librispeech/LibriSpeech/dev-clean OUT_DIR
"""

from __future__ import annotations

import json
import sys
import wave
from pathlib import Path

import numpy as np

from attest_voice.audio import SAMPLE_RATE, decode
from attest_voice.models import Models
from attest_voice.speaker import cut

A, A_ENROLL, A_CONV = "652", "129742", "130726"
B, B_CONV = "5536", "43358"
C, C_ENROLL = "2035", "147960"


def write(path: Path, x: np.ndarray) -> None:
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SAMPLE_RATE)
        w.writeframes((np.clip(x, -1, 1) * 32767).astype(np.int16).tobytes())


def files(root: Path, spk: str, ch: str) -> list[Path]:
    return sorted((root / spk / ch).glob("*.flac"))


def trimmed(m: Models, f: Path) -> np.ndarray:
    a = decode(f)
    iv = m.vad(a)
    return a[int(iv[0][0] * SAMPLE_RATE) : int(iv[-1][1] * SAMPLE_RATE)]


def utt(m: Models, root: Path, spk: str, ch: str, lo: float, hi: float, skip: int = 0) -> np.ndarray:
    hits = [x for x in (trimmed(m, f) for f in files(root, spk, ch)) if lo <= len(x) / SAMPLE_RATE <= hi]
    return hits[skip]


def enrollment(m: Models, root: Path, spk: str, ch: str, seconds: float = 25) -> np.ndarray:
    out, n = [], 0.0
    for f in files(root, spk, ch):
        a = decode(f)
        out.append(a)
        n += sum(e - s for s, e in m.vad(a))
        if n >= seconds:
            break
    return np.concatenate(out)


def main(root_s: str, out_s: str) -> None:
    root, out = Path(root_s).expanduser(), Path(out_s)
    out.mkdir(parents=True, exist_ok=True)
    m = Models("~/.cache/sherpa-onnx-models", threads=4)

    write(out / "enroll-A.wav", enrollment(m, root, A, A_ENROLL))
    write(out / "enroll-C.wav", enrollment(m, root, C, C_ENROLL))

    a1 = utt(m, root, A, A_CONV, 5, 7.5)
    a2 = utt(m, root, A, A_CONV, 5, 7.5, skip=1)
    a3 = utt(m, root, A, A_CONV, 3, 5)
    b_long = utt(m, root, B, B_CONV, 7, 10)
    # The interjection: the first words (>= 0.7 s) of another B utterance.
    src = utt(m, root, B, B_CONV, 3, 10, skip=1)
    words = m.transcribe(src, 0.0)
    k = next(i for i, w in enumerate(words) if w.end - words[0].start >= 0.7)
    mutter = src[int(max(0, words[0].start - 0.05) * SAMPLE_RATE) : int((words[k].end + 0.05) * SAMPLE_RATE)]
    mutter_text = " ".join(w.w for w in words[: k + 1])

    gap = lambda s: np.zeros(int(s * SAMPLE_RATE), np.float32)  # noqa: E731
    plan = [("A", a1), (None, gap(0.35)), ("B", mutter), (None, gap(0.35)), ("A", a2), (None, gap(0.6)), ("A", a3)]
    conv = np.concatenate([x for _, x in plan])
    truth, t = [], 0.0
    for who, x in plan:
        if who:
            truth.append({"speaker": who, "start": round(t, 2), "end": round(t + len(x) / SAMPLE_RATE, 2)})
        t += len(x) / SAMPLE_RATE
    # B starts 1 s before A's last utterance ends: overlapped speech.
    start = len(conv) - SAMPLE_RATE
    conv = np.concatenate([conv, np.zeros(len(b_long) - SAMPLE_RATE, np.float32)])
    conv[start:] += b_long
    truth.append({"speaker": "B", "start": round(start / SAMPLE_RATE, 2), "end": round(len(conv) / SAMPLE_RATE, 2)})
    conv = np.concatenate([conv, gap(0.5)])
    write(out / "conversation.wav", conv)
    (out / "truth.json").write_text(json.dumps({
        "speakers": {"A": f"LibriSpeech {A} (enrolled from chapter {A_ENROLL}; conversation uses {A_CONV})",
                     "B": f"LibriSpeech {B} (not enrolled)", "C": f"LibriSpeech {C} (enrolled, absent)"},
        "mutterText": mutter_text,
        "mutterSeconds": round(len(mutter) / SAMPLE_RATE, 2),
        "mutterVoicedSeconds": round(sum(e - s for s, e in m.vad(mutter)), 2),
        "turns": truth,
    }, indent=1))
    print(json.dumps(json.loads((out / "truth.json").read_text())), file=sys.stderr)


if __name__ == "__main__":
    main(*sys.argv[1:])
