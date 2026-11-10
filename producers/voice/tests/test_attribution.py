from attest_voice.attribution import CONTESTED, UNCOVERED, Turn, Word, classify, speaker_of, utterances
from attest_voice.models import tokens_to_words

# Owner (0) speaks 0-4 s; passenger (1) 4.0-5.0 s; overlap 6.0-6.6 s.
TURNS = [Turn(0.0, 4.0, 0), Turn(4.0, 5.0, 1), Turn(5.5, 6.6, 0), Turn(6.0, 7.5, 1)]


def W(w, s, e):
    return Word(w, s, e)


def test_word_inside_one_turn():
    assert speaker_of(W("send", 1.0, 1.3), TURNS) == 0
    assert speaker_of(W("yeah", 4.2, 4.5), TURNS) == 1


def test_word_straddling_speaker_change_is_nobodys():
    assert speaker_of(W("it", 3.8, 4.2), TURNS) is None


def test_word_in_overlap_is_nobodys():
    assert speaker_of(W("now", 6.1, 6.4), TURNS) is None


def test_word_outside_turns_is_nobodys():
    assert speaker_of(W("um", 5.1, 5.4), TURNS) is None
    assert classify(W("um", 5.1, 5.4), TURNS) == UNCOVERED
    assert classify(W("now", 6.1, 6.4), TURNS) == CONTESTED


def test_zero_length_word_still_located():
    assert speaker_of(W("a", 2.0, 2.0), TURNS) == 0


def test_clean_speaker_change_splits_utterances():
    words = [W("play", 0.5, 0.8), W("jazz", 0.9, 1.2), W("yeah", 4.2, 4.5), W("send", 4.55, 4.8)]
    u = utterances(words, TURNS)
    assert [(x.speaker, x.text) for x in u] == [(0, "play jazz"), (1, "yeah send")]


def test_straddling_word_poisons_its_utterance():
    words = [W("send", 3.5, 3.8), W("it", 3.85, 4.15), W("yeah", 4.2, 4.5)]
    u = utterances(words, TURNS)
    assert len(u) == 1 and u[0].speaker is None
    assert u[0].text == "send it yeah"


def test_overlap_utterance_is_nobodys_but_later_clean_speech_is():
    words = [W("now", 6.1, 6.4), W("later", 8.5, 8.8)]
    u = utterances(words, TURNS + [Turn(8.0, 9.0, 0)])
    assert [x.speaker for x in u] == [None, 0]


def test_uncovered_word_is_its_own_unattributed_utterance():
    # 5.0-5.5 is a hole (e.g. a guarded island cut out of the owner's turn).
    turns = [Turn(0, 5.0, 0), Turn(5.5, 8, 0)]
    words = [W("play", 4.5, 4.9), W("yeah", 5.05, 5.4), W("jazz", 5.6, 6.0)]
    u = utterances(words, turns)
    assert [(x.speaker, x.text) for x in u] == [(0, "play"), (None, "yeah"), (0, "jazz")]


def test_pause_splits_same_speaker():
    words = [W("one", 0.1, 0.3), W("two", 2.0, 2.3)]
    assert len(utterances(words, TURNS)) == 2


def test_utterance_json_shape():
    u = utterances([W("hi", 1.0, 1.2)], TURNS)[0]
    j = u.to_json(lambda s: f"sess/spk{s}")
    assert j == {"subject": "sess/spk0", "start": 1.0, "end": 1.2, "text": "hi",
                 "words": [{"w": "hi", "start": 1.0, "end": 1.2}]}
    u.speaker = None
    assert u.to_json(lambda s: "x")["subject"] is None


def test_tokens_to_words():
    toks = [" Well", ",", " I", " don", "'", "t", " w", "ish", "."]
    starts = [0.48, 0.64, 0.8, 0.88, 0.96, 1.04, 1.12, 1.2, 1.3]
    durs = [0.16, 0.08, 0.08, 0.08, 0.08, 0.08, 0.08, 0.08, 0.08]
    ws = tokens_to_words(toks, starts, durs, offset=10.0, chunk_seconds=2.0)
    assert [w.w for w in ws] == ["Well,", "I", "don't", "wish."]
    assert ws[0].start == 10.48 and ws[0].end == 10.72
    assert ws[2].end == 11.12
