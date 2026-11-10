"""Generate calibration trials from a labeled manifest of your own audio
(see attest_voice/trials.py for the format), for `attest-voice fit-calibration`.

    uv run python scripts/manifest_trials.py manifest.jsonl trials.jsonl [--durations 1,2,3,5,10] [--tests-per-speaker 6]

Scores carry no speaker labels or audio; the trials file is safe to keep
next to the calibration it produces.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections import Counter

from attest_voice.audio import decode
from attest_voice.models import Models
from attest_voice.trials import DURATIONS, build_trials, load_manifest, voiced_reader


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("manifest")
    p.add_argument("out")
    p.add_argument("--durations", default=",".join(f"{d:g}" for d in DURATIONS), help="test crop lengths, seconds")
    p.add_argument("--tests-per-speaker", type=int, default=6, help="test segments per speaker")
    p.add_argument("--models", default="~/.cache/sherpa-onnx-models")
    a = p.parse_args()
    m = Models(a.models, threads=4)
    rows = build_trials(load_manifest(a.manifest), voiced_reader(decode, m.vad), m.embed,
                        durations=[float(x) for x in a.durations.split(",")], tests_per_speaker=a.tests_per_speaker)
    with open(a.out, "w") as f:
        for r in rows:
            f.write(json.dumps(r) + "\n")
    same = Counter(r["seconds"] for r in rows if r["same"])
    diff = Counter(r["seconds"] for r in rows if not r["same"])
    for d in sorted(diff):
        print(f"{d:>4}s: {same[d]} same / {diff[d]} different", file=sys.stderr)


if __name__ == "__main__":
    main()
