"""Generate calibration trials from a labelled manifest of your own audio
(see attest_voice/trials.py for the format), for `attest-voice fit-calibration`.

    uv run python scripts/manifest_trials.py manifest.jsonl trials.jsonl

Scores carry no speaker labels or audio; the trials file is safe to keep
next to the calibration it produces.
"""

from __future__ import annotations

import json
import sys
from collections import Counter

from attest_voice.audio import decode
from attest_voice.models import Models
from attest_voice.trials import build_trials, load_manifest, voiced_reader


def main(manifest: str, out: str, models_dir: str = "~/.cache/sherpa-onnx-models") -> None:
    m = Models(models_dir, threads=4)
    segs = load_manifest(manifest)
    rows = build_trials(segs, voiced_reader(decode, m.vad), m.embed)
    with open(out, "w") as f:
        for r in rows:
            f.write(json.dumps(r) + "\n")
    same = Counter(r["seconds"] for r in rows if r["same"])
    diff = Counter(r["seconds"] for r in rows if not r["same"])
    for d in sorted(diff):
        print(f"{d:>4}s: {same[d]} same / {diff[d]} different", file=sys.stderr)


if __name__ == "__main__":
    main(*sys.argv[1:])
