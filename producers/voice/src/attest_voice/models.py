"""sherpa-onnx models: VAD, speaker embedding, offline diarization, ASR.

sherpa_onnx is imported on first use so that everything else (and the unit
tests) runs without it or the model files. File names match
``scripts/download-models.sh``.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .attribution import Turn, Word
from .audio import SAMPLE_RATE

VAD_FILE = "silero_vad.onnx"
EMBEDDING_FILE = "3dspeaker_speech_eres2net_sv_en_voxceleb_16k.onnx"
SEGMENTATION_DIR = "sherpa-onnx-pyannote-segmentation-3-0"
ASR_DIR = "sherpa-onnx-nemo-parakeet_tdt_transducer_110m-en-36000-int8"

# Embedding id recorded with voiceprints and calibrations: a print or a
# calibration from another model is not comparable.
EMBEDDING_MODEL = EMBEDDING_FILE.removesuffix(".onnx")


@dataclass(frozen=True)
class DiarizationParams:
    num_speakers: int = -1  # -1: decide by cluster_threshold
    cluster_threshold: float = 0.5
    min_duration_on: float = 0.2
    min_duration_off: float = 0.3


class Models:
    def __init__(self, models_dir: str | Path, *, threads: int = 2, diarization: DiarizationParams = DiarizationParams()):
        self.dir = Path(models_dir).expanduser()
        self.threads = threads
        self.diar_params = diarization
        self._vad_cfg: Any = None
        self._emb: Any = None
        self._diar: Any = None
        self._asr: Any = None

    def _path(self, *parts: str) -> str:
        p = self.dir.joinpath(*parts)
        if not p.exists():
            raise FileNotFoundError(f"model file missing: {p} (run scripts/download-models.sh)")
        return str(p)

    # -- VAD -----------------------------------------------------------------

    def vad(self, audio: np.ndarray) -> list[tuple[float, float]]:
        """Voiced intervals in seconds."""
        import sherpa_onnx as so

        if self._vad_cfg is None:
            cfg = so.VadModelConfig()
            cfg.silero_vad.model = self._path(VAD_FILE)
            cfg.silero_vad.threshold = 0.5
            cfg.silero_vad.min_silence_duration = 0.25
            cfg.silero_vad.min_speech_duration = 0.15
            cfg.silero_vad.max_speech_duration = 20
            cfg.sample_rate = SAMPLE_RATE
            cfg.num_threads = 1
            self._vad_cfg = cfg
        vad = so.VoiceActivityDetector(self._vad_cfg, buffer_size_in_seconds=max(30, len(audio) / SAMPLE_RATE + 1))
        win = self._vad_cfg.silero_vad.window_size
        out: list[tuple[float, float]] = []

        def drain() -> None:
            while not vad.empty():
                seg = vad.front
                out.append((seg.start / SAMPLE_RATE, (seg.start + len(seg.samples)) / SAMPLE_RATE))
                vad.pop()

        for i in range(0, len(audio), win):
            vad.accept_waveform(audio[i : i + win])
            drain()
        vad.flush()
        drain()
        return out

    # -- speaker embedding ---------------------------------------------------

    def _extractor(self) -> Any:
        import sherpa_onnx as so

        if self._emb is None:
            cfg = so.SpeakerEmbeddingExtractorConfig(model=self._path(EMBEDDING_FILE), num_threads=self.threads)
            self._emb = so.SpeakerEmbeddingExtractor(cfg)
        return self._emb

    def embed(self, samples: np.ndarray) -> np.ndarray:
        """L2-normalized embedding of one stretch of speech."""
        ex = self._extractor()
        st = ex.create_stream()
        st.accept_waveform(SAMPLE_RATE, samples)
        st.input_finished()
        v = np.asarray(ex.compute(st), dtype=np.float32)
        return v / (np.linalg.norm(v) + 1e-9)

    # -- diarization ---------------------------------------------------------

    def diarize(self, audio: np.ndarray) -> list[Turn]:
        import sherpa_onnx as so

        if self._diar is None:
            p = self.diar_params
            cfg = so.OfflineSpeakerDiarizationConfig(
                segmentation=so.OfflineSpeakerSegmentationModelConfig(
                    pyannote=so.OfflineSpeakerSegmentationPyannoteModelConfig(model=self._path(SEGMENTATION_DIR, "model.onnx")),
                    num_threads=self.threads,
                ),
                embedding=so.SpeakerEmbeddingExtractorConfig(model=self._path(EMBEDDING_FILE), num_threads=self.threads),
                clustering=so.FastClusteringConfig(num_clusters=p.num_speakers, threshold=p.cluster_threshold),
                min_duration_on=p.min_duration_on,
                min_duration_off=p.min_duration_off,
            )
            if not cfg.validate():
                raise RuntimeError("invalid diarization config")
            self._diar = so.OfflineSpeakerDiarization(cfg)
        res = self._diar.process(audio).sort_by_start_time()
        return [Turn(float(s.start), float(s.end), int(s.speaker)) for s in res]

    # -- ASR -----------------------------------------------------------------

    def _recognizer(self) -> Any:
        import sherpa_onnx as so

        if self._asr is None:
            d = ASR_DIR
            self._asr = so.OfflineRecognizer.from_transducer(
                encoder=self._path(d, "encoder.int8.onnx"), decoder=self._path(d, "decoder.int8.onnx"),
                joiner=self._path(d, "joiner.int8.onnx"), tokens=self._path(d, "tokens.txt"),
                num_threads=self.threads, model_type="nemo_transducer",
            )
        return self._asr

    def transcribe(self, samples: np.ndarray, offset: float) -> list[Word]:
        """Words with absolute times (``offset`` + position in ``samples``)."""
        rec = self._recognizer()
        st = rec.create_stream()
        st.accept_waveform(SAMPLE_RATE, samples)
        rec.decode_stream(st)
        r = st.result
        durations = list(r.durations) if len(r.durations) == len(r.tokens) else []
        return tokens_to_words(list(r.tokens), list(r.timestamps), durations, offset, len(samples) / SAMPLE_RATE)


def tokens_to_words(
    tokens: list[str], starts: list[float], durations: list[float], offset: float, chunk_seconds: float,
) -> list[Word]:
    """Merge BPE tokens (a leading space starts a word) into timed words.
    Punctuation-only tokens join the previous word. Without per-token
    durations a word ends where the next token starts."""
    words: list[list[Any]] = []  # [text, start, end]
    for i, (tok, t) in enumerate(zip(tokens, starts)):
        if durations:
            end = t + durations[i]
        else:
            end = starts[i + 1] if i + 1 < len(starts) else min(chunk_seconds, t + 0.2)
        piece = tok.strip()
        if not piece:
            continue
        new_word = tok.startswith(" ") or not words
        if new_word and not piece.isalnum() and not any(c.isalnum() for c in piece) and words:
            new_word = False  # " ," style punctuation
        if new_word:
            words.append([piece, t, end])
        else:
            words[-1][0] += piece
            words[-1][2] = max(words[-1][2], end)
    return [Word(w, round(offset + s, 3), round(offset + e, 3)) for w, s, e in words]
