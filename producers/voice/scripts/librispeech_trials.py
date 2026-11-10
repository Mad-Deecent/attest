"""Generate labelled same/different-speaker trials from LibriSpeech.

Speakers are split deterministically: every 4th speaker id (sorted) is held
out for evaluation and for the demo; the rest are used to fit. For each
speaker an enrolment print is built from >= ENROL_SECONDS of voiced audio
from one chapter; test segments are voiced crops of fixed durations taken
from utterances not used for enrolment (another chapter when there is one).
Every test segment is scored against every enrolment in its split.

    uv run python scripts/librispeech_trials.py ~/.cache/librispeech/LibriSpeech/dev-clean out/
"""

from __future__ import annotations

import json
import random
import sys
from pathlib import Path

import numpy as np

from attest_voice.audio import SAMPLE_RATE, decode
from attest_voice.models import Models
from attest_voice.speaker import cut, voiceprint

ENROL_SECONDS = 20.0
DURATIONS = [0.5, 1.0, 1.5, 2.0, 3.0, 5.0, 8.0]
TESTS_PER_SPEAKER = 6


def utterances(spk_dir: Path) -> dict[str, list[Path]]:
    return {c.name: sorted(c.glob("*.flac")) for c in sorted(spk_dir.iterdir()) if c.is_dir()}


def main(root: str, out: str, models_dir: str = "~/.cache/sherpa-onnx-models") -> None:
    rng = random.Random(7)
    m = Models(models_dir, threads=4)
    outdir = Path(out)
    outdir.mkdir(parents=True, exist_ok=True)
    speakers = sorted((p for p in Path(root).iterdir() if p.is_dir()), key=lambda p: int(p.name))
    split = {s.name: ("heldout" if i % 4 == 0 else "fit") for i, s in enumerate(speakers)}

    prints: dict[str, np.ndarray] = {}
    tests: list[tuple[str, float, np.ndarray]] = []
    for s in speakers:
        chapters = utterances(s)
        names = list(chapters)
        enrol_ch = names[0]
        buf, secs, used = [], 0.0, set()
        for f in chapters[enrol_ch]:
            a = decode(f)
            v = cut(a, m.vad(a))
            buf.append(v)
            used.add(f)
            secs += len(v) / SAMPLE_RATE
            if secs >= ENROL_SECONDS:
                break
        prints[s.name] = voiceprint(np.concatenate(buf), m.embed)  # type: ignore[assignment]
        pool = [f for c in names[1:] for f in chapters[c]] or [f for f in chapters[enrol_ch] if f not in used]
        rng.shuffle(pool)
        for f in pool[:TESTS_PER_SPEAKER]:
            a = decode(f)
            v = cut(a, m.vad(a))
            for d in DURATIONS:
                n = int(d * SAMPLE_RATE)
                if len(v) < n:
                    continue
                off = rng.randrange(0, len(v) - n + 1)
                tests.append((s.name, d, m.embed(v[off : off + n])))
        print(f"speaker {s.name} ({split[s.name]}): enrol {secs:.1f}s, chapters {len(names)}", file=sys.stderr)

    files = {k: open(outdir / f"trials-{k}.jsonl", "w") for k in ("fit", "heldout")}
    for spk, d, e in tests:
        for enr, p in prints.items():
            if split[enr] != split[spk]:
                continue
            row = {"score": round(float(np.dot(e, p)), 5), "same": enr == spk, "seconds": d}
            files[split[spk]].write(json.dumps(row) + "\n")
    for f in files.values():
        f.close()
    (outdir / "split.json").write_text(json.dumps(split, indent=1))


if __name__ == "__main__":
    main(*sys.argv[1:])
