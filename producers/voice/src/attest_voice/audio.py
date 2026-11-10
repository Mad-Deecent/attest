"""Decode any container/codec PyAV knows (wav, flac, webm/opus, ogg, mp4)
to 16 kHz mono float32. PyAV bundles its own FFmpeg libraries, so no system
ffmpeg is needed."""

from __future__ import annotations

import io
from pathlib import Path

import av
import numpy as np

SAMPLE_RATE = 16000


class DecodeError(ValueError):
    pass


def decode(src: str | Path | bytes, *, max_seconds: float = 3600) -> np.ndarray:
    f = io.BytesIO(src) if isinstance(src, (bytes, bytearray)) else str(src)
    try:
        with av.open(f) as c:
            stream = next((s for s in c.streams if s.type == "audio"), None)
            if stream is None:
                raise DecodeError("no audio stream")
            rs = av.AudioResampler(format="flt", layout="mono", rate=SAMPLE_RATE)
            chunks: list[np.ndarray] = []
            n = 0
            limit = int(max_seconds * SAMPLE_RATE)
            for frame in c.decode(stream):
                for out in rs.resample(frame):
                    a = out.to_ndarray().reshape(-1)
                    chunks.append(a)
                    n += a.size
                if n > limit:
                    raise DecodeError(f"audio longer than {max_seconds:.0f} s")
            for out in rs.resample(None):
                chunks.append(out.to_ndarray().reshape(-1))
    except av.FFmpegError as e:
        raise DecodeError(f"cannot decode audio: {e}") from None
    if not chunks:
        raise DecodeError("no audio samples")
    return np.ascontiguousarray(np.concatenate(chunks), dtype=np.float32)
