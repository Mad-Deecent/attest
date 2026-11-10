"""attest-voice: enrol | forget | run | serve | fit-calibration"""

from __future__ import annotations

import argparse
import json
import logging
import sys
from collections import defaultdict
from pathlib import Path

from .config import Config


def _models(cfg: Config):  # type: ignore[no-untyped-def]
    from .models import Models

    return Models(cfg.models_dir, threads=cfg.threads, diarization=cfg.diarization)


def cmd_enrol(cfg: Config, a: argparse.Namespace) -> int:
    from .audio import decode
    from .enrol import TooLittleSpeech, enrol
    from .models import EMBEDDING_MODEL
    from .store import Store

    try:
        voiced = enrol(a.identity, decode(a.audio), _models(cfg), Store(cfg.data_dir),
                       model=EMBEDDING_MODEL, min_seconds=cfg.min_enrol_seconds)
    except TooLittleSpeech as e:
        print(e, file=sys.stderr)
        return 2
    print(json.dumps({"identityId": a.identity, "voicedSeconds": round(voiced, 1)}))
    return 0


def cmd_forget(cfg: Config, a: argparse.Namespace) -> int:
    from .store import Store

    ok = Store(cfg.data_dir).delete(a.identity)
    print(json.dumps({"identityId": a.identity, "deleted": ok}))
    return 0 if ok else 1


def cmd_run(cfg: Config, a: argparse.Namespace) -> int:
    from .audio import decode
    from .pipeline import Session
    from .store import Store

    cal = cfg.load_calibration()
    prints = Store(cfg.data_dir).all()
    if a.speakers:
        cfg.diarization = type(cfg.diarization)(**{**cfg.diarization.__dict__, "num_speakers": a.speakers})
    sess = Session(
        session=a.session, zone=cfg.zone, ttl=cfg.ttl_seconds, cal=cal, prints=prints,
        attest=None if a.dry_run else cfg.attest(), callback_url=cfg.callback_url, include_scores=a.scores,
        guard=cfg.guard,
    )
    keys = sess.run(decode(a.audio), _models(cfg))
    if not a.keep_subjects:
        sess.retire(keys)
    t = sess.timings
    print(json.dumps({"type": "timing", **{k: round(v, 3) for k, v in t.items()},
                      "realtimeFactor": round(t["total"] / t["audioSeconds"], 4)}), file=sys.stderr)
    return 0


def cmd_serve(cfg: Config, a: argparse.Namespace) -> int:
    from .models import EMBEDDING_MODEL
    from .server import EnrolService, serve
    from .store import Store

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(message)s")
    svc = EnrolService(cfg.attest(), _models(cfg), Store(cfg.data_dir), model=EMBEDDING_MODEL,
                       min_seconds=cfg.min_enrol_seconds, cors_origin=cfg.cors_origin, max_body=cfg.max_body_bytes)
    serve(svc, a.listen or cfg.listen)
    return 0


def cmd_fit(cfg: Config, a: argparse.Namespace) -> int:
    from .calibration import Calibration, Shrink, cllr, eer, fit_affine

    trials = [json.loads(l) for l in Path(a.trials).read_text().splitlines() if l.strip()]
    use = [t for t in trials if t.get("seconds", 1e9) >= a.min_seconds]
    s, y = [t["score"] for t in use], [bool(t["same"]) for t in use]
    ca, cb = fit_affine(s, y)
    cal = Calibration(
        id=a.id, a=ca, b=cb, bound=a.bound, shrink=Shrink(), model=a.model,
        provenance={"trials": a.trials_name or Path(a.trials).name, "minSeconds": a.min_seconds,
                    "same": sum(y), "different": len(y) - sum(y), "note": a.note},
    )
    report: dict[str, object] = {
        "a": ca, "b": cb, "eer": eer(s, y), "cllr": cllr([cal.raw_llr(x) for x in s], y),
        "sameScoreMean": sum(x for x, t in zip(s, y) if t) / max(1, sum(y)),
        "differentScoreMean": sum(x for x, t in zip(s, y) if not t) / max(1, len(y) - sum(y)),
    }
    # How the calibration behaves on shorter test audio, by duration bucket:
    # the evidence behind the duration shrink.
    buckets: dict[float, list[dict]] = defaultdict(list)
    for t in trials:
        if "seconds" in t:
            buckets[t["seconds"]].append(t)
    per = {}
    for secs, ts in sorted(buckets.items()):
        bs, by = [t["score"] for t in ts], [bool(t["same"]) for t in ts]
        if any(by) and not all(by):
            ba, bb = fit_affine(bs, by)
            raw = [cal.raw_llr(x) for x in bs]
            shr = [cal.llr(x, secs) for x in bs]
            per[str(secs)] = {"n": len(ts), "eer": round(eer(bs, by), 4), "fitA": round(ba, 3), "fitB": round(bb, 3),
                              "cllrUnshrunk": round(cllr(raw, by), 4), "cllrShrunk": round(cllr(shr, by), 4),
                              "maxShrunkLLR": round(max(shr), 3)}
    report["byDuration"] = per
    if a.eval:
        ev = [json.loads(l) for l in Path(a.eval).read_text().splitlines() if l.strip()]
        ey = [bool(t["same"]) for t in ev]
        long = [(t["score"], bool(t["same"])) for t in ev if t.get("seconds", 1e9) >= a.min_seconds]
        report["heldout"] = {
            "trials": a.eval_name or Path(a.eval).name, "n": len(ev),
            "eerAtFitDurations": round(eer([x for x, _ in long], [z for _, z in long]), 4),
            "cllrAtFitDurations": round(cllr([cal.raw_llr(x) for x, _ in long], [z for _, z in long]), 4),
            "cllrAllDurationsShrunk": round(cllr([cal.llr(t["score"], t.get("seconds", 1e9)) for t in ev], ey), 4),
            "maxShrunkLLRUnder1.5s": round(max((cal.llr(t["score"], t["seconds"]) for t in ev if t.get("seconds", 1e9) < 1.5), default=0), 3),
        }
    cal = Calibration(**{**cal.__dict__, "provenance": {**cal.provenance, "fit": report}})
    Path(a.out).write_text(json.dumps(cal.to_json(), indent=2) + "\n")
    print(json.dumps(report, indent=2))
    return 0


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="attest-voice", description=__doc__)
    p.add_argument("-c", "--config", help="config JSON (see attest-voice.example.json)")
    sub = p.add_subparsers(dest="cmd", required=True)

    e = sub.add_parser("enrol", help="store a voiceprint from an audio file (operator path)")
    e.add_argument("--identity", required=True)
    e.add_argument("audio")
    e.set_defaults(fn=cmd_enrol)

    f = sub.add_parser("forget", help="delete an identity's voiceprint")
    f.add_argument("--identity", required=True)
    f.set_defaults(fn=cmd_forget)

    r = sub.add_parser("run", help="process one session's audio and report evidence to attest")
    r.add_argument("audio")
    r.add_argument("--session", required=True)
    r.add_argument("--speakers", type=int, default=0, help="known number of speakers (default: cluster threshold)")
    r.add_argument("--keep-subjects", action="store_true", help="do not retire subjects at session end")
    r.add_argument("--dry-run", action="store_true", help="do not contact attest")
    r.add_argument("--scores", action="store_true", help="include raw cosine scores in evidence events")
    r.set_defaults(fn=cmd_run)

    s = sub.add_parser("serve", help="run the browser enrolment endpoint")
    s.add_argument("--listen", default="")
    s.set_defaults(fn=cmd_serve)

    c = sub.add_parser("fit-calibration", help="fit a score->LLR calibration from labelled trials")
    c.add_argument("trials", help='JSON lines: {"score": float, "same": bool, "seconds": float}')
    c.add_argument("--id", required=True, help="calibration id sent to attest")
    c.add_argument("--model", default="", help="embedding model the scores came from")
    c.add_argument("--min-seconds", type=float, default=2.0, help="fit only on trials with at least this much test speech")
    c.add_argument("--bound", type=float, default=8.0)
    c.add_argument("--trials-name", default="")
    c.add_argument("--note", default="")
    c.add_argument("--eval", default="", help="held-out trials (same format) to report metrics on")
    c.add_argument("--eval-name", default="")
    c.add_argument("-o", "--out", required=True)
    c.set_defaults(fn=cmd_fit)

    a = p.parse_args(argv)
    return int(a.fn(Config.load(a.config), a))


if __name__ == "__main__":
    sys.exit(main())
