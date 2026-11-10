"""Voiceprints on local disk, keyed by identity id.

One JSON file, mode 0600, in a 0700 directory, replaced atomically. A
voiceprint records the embedding model it came from; scoring skips prints
from a different model rather than comparing incompatible vectors.
"""

from __future__ import annotations

import json
import os
import threading
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

import numpy as np


@dataclass(frozen=True)
class Voiceprint:
    identity: str
    embedding: np.ndarray
    model: str
    voiced_seconds: float
    enrolled_at: str


class Store:
    def __init__(self, data_dir: str | Path):
        self.dir = Path(data_dir).expanduser()
        self.path = self.dir / "voiceprints.json"
        self._lock = threading.Lock()

    def _read(self) -> dict:
        try:
            return json.loads(self.path.read_text())
        except FileNotFoundError:
            return {}

    def _write(self, data: dict) -> None:
        self.dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        os.chmod(self.dir, 0o700)
        tmp = self.path.with_suffix(".tmp")
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            json.dump(data, f)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self.path)

    def put(self, identity: str, embedding: np.ndarray, model: str, voiced_seconds: float) -> None:
        with self._lock:
            data = self._read()
            data[identity] = {
                "embedding": [float(x) for x in embedding],
                "model": model,
                "voicedSeconds": round(voiced_seconds, 2),
                "enrolledAt": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            }
            self._write(data)

    def delete(self, identity: str) -> bool:
        with self._lock:
            data = self._read()
            if identity not in data:
                return False
            del data[identity]
            self._write(data)
            return True

    def all(self) -> list[Voiceprint]:
        with self._lock:
            data = self._read()
        return [
            Voiceprint(i, np.asarray(v["embedding"], dtype=np.float32), v["model"], v["voicedSeconds"], v["enrolledAt"])
            for i, v in sorted(data.items())
        ]
