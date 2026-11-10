"""Who said which words.

Each ASR word is attributed to the one diarized speaker whose turns cover it.
A word that lies in overlapped speech, is shared between two speakers, or is
covered by nobody is attributed to nobody. Words are grouped into utterances
at pauses and speaker changes; an utterance containing any unattributed word,
or crossing a speaker change, has ``subject = None``. Downstream, a request
built from a ``None`` utterance must not be attributed to anyone.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Callable, Sequence


@dataclass(frozen=True)
class Turn:
    start: float
    end: float
    speaker: int


@dataclass(frozen=True)
class Word:
    w: str
    start: float
    end: float


@dataclass
class Utterance:
    speaker: int | None
    words: list[Word] = field(default_factory=list)

    @property
    def start(self) -> float:
        return self.words[0].start

    @property
    def end(self) -> float:
        return self.words[-1].end

    @property
    def text(self) -> str:
        return " ".join(w.w for w in self.words)

    def to_json(self, subject_of: Callable[[int], str]) -> dict:
        return {
            "subject": None if self.speaker is None else subject_of(self.speaker),
            "start": round(self.start, 3),
            "end": round(self.end, 3),
            "text": self.text,
            "words": [{"w": w.w, "start": round(w.start, 3), "end": round(w.end, 3)} for w in self.words],
        }


def _overlap(a0: float, a1: float, b0: float, b1: float) -> float:
    return max(0.0, min(a1, b1) - max(a0, b0))


CONTESTED = "contested"  # overlap or straddle: no clean boundary
UNCOVERED = "uncovered"  # outside every turn (silence edges, guarded islands)


def classify(word: Word, turns: Sequence[Turn], *, dominance: float = 0.8, slack: float = 0.05) -> int | str:
    """The single speaker covering ``word``, else CONTESTED or UNCOVERED.

    ``dominance``: share of the word's speech-covered time that must belong to
    one speaker (time where two speakers overlap counts for both, so a word
    inside an overlap never reaches it). ``slack`` widens a word with zero or
    tiny duration so it can still be located.
    """
    s, e = word.start, max(word.end, word.start + slack)
    cover: dict[int, float] = {}
    for t in turns:
        o = _overlap(s, e, t.start, t.end)
        if o > 0:
            cover[t.speaker] = cover.get(t.speaker, 0.0) + o
    if not cover:
        return UNCOVERED
    total = sum(cover.values())
    spk, best = max(cover.items(), key=lambda kv: kv[1])
    if best / total < dominance:
        return CONTESTED
    if best / (e - s) < 0.5:
        return UNCOVERED
    return spk


def speaker_of(word: Word, turns: Sequence[Turn]) -> int | None:
    c = classify(word, turns)
    return c if isinstance(c, int) else None


def utterances(words: Sequence[Word], turns: Sequence[Turn], *, max_gap: float = 0.5) -> list[Utterance]:
    """Group words into utterances at pauses longer than ``max_gap`` and at
    changes of attribution. A contested word (overlap, straddle) poisons the
    whole contiguous run it is in, before and after, because nothing marks
    where one speaker's request ends. Uncovered words were cut at silences, so
    they form their own unattributed utterance without poisoning neighbors.
    """
    out: list[Utterance] = []
    cur: Utterance | None = None
    poisoned = False  # cur contains a contested word
    last_end = -1e9
    for word in sorted(words, key=lambda w: w.start):
        c = classify(word, turns)
        contested = c == CONTESTED
        spk = c if isinstance(c, int) else None
        if cur is not None and word.start - last_end <= max_gap:
            if poisoned or contested:
                cur.speaker = None
                poisoned = True
                cur.words.append(word)
                last_end = word.end
                continue
            if spk == cur.speaker:
                cur.words.append(word)
                last_end = word.end
                continue
        cur = Utterance(speaker=spk, words=[word])
        poisoned = contested
        out.append(cur)
        last_end = word.end
    return out
