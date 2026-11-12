"""Test doubles: a model-free engine and an in-process fake attest."""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import numpy as np

from attest_voice.attribution import Turn, Word
from attest_voice.audio import SAMPLE_RATE


class FakeEngine:
    """'Speech' is any sample with |x| > 0.01. The embedding encodes the
    sign of the mean sample, so audio of +amplitude and -amplitude are two
    'speakers'."""

    def __init__(self, turns: list[Turn] | None = None, words: list[Word] | None = None):
        self.turns = turns or []
        self.words = words or []

    def vad(self, audio: np.ndarray) -> list[tuple[float, float]]:
        mask = np.abs(audio) > 0.01
        out, start = [], None
        for i, m in enumerate(np.append(mask, False)):
            if m and start is None:
                start = i
            elif not m and start is not None:
                out.append((start / SAMPLE_RATE, i / SAMPLE_RATE))
                start = None
        return out

    def embed(self, samples: np.ndarray) -> np.ndarray:
        v = np.array([1.0, 0.0]) if samples.mean() >= 0 else np.array([0.0, 1.0])
        return v.astype(np.float32)

    def diarize(self, audio: np.ndarray) -> list[Turn]:
        return self.turns

    def transcribe(self, samples: np.ndarray, offset: float) -> list[Word]:
        end = offset + len(samples) / SAMPLE_RATE
        return [w for w in self.words if offset <= w.start < end]


class FakeAttest:
    """Records requests; redeems enrollment tickets from ``tickets`` and voice
    deletion tickets from ``deletions``. ``redeem_reply`` = (status, raw body)
    and ``delete_reply`` override the two redeem routes' answers."""

    def __init__(self, tickets: dict[str, str] | None = None, token: str = "producer-token",
                 deletions: dict[str, str] | None = None):
        self.tickets = dict(tickets or {})
        self.deletions = dict(deletions or {})
        self.token = token
        self.redeem_reply: tuple[int, bytes] | None = None
        self.delete_reply: tuple[int, bytes] | None = None
        self.requests: list[tuple[str, dict[str, Any], str]] = []
        fake = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a: Any) -> None:
                pass

            def do_POST(self) -> None:
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                auth = self.headers.get("Authorization", "")
                fake.requests.append((self.path, body, auth))
                if auth != f"Bearer {fake.token}":
                    return self._send(401, {"error": "unauthenticated"})
                override = {"/v1/enrollment-tickets/redeem": fake.redeem_reply,
                            "/v1/voice-deletion-tickets/redeem": fake.delete_reply}.get(self.path)
                if override:
                    code, raw = override
                    self.send_response(code)
                    self.send_header("Content-Length", str(len(raw)))
                    self.end_headers()
                    self.wfile.write(raw)
                    return
                if self.path == "/v1/enrollment-tickets/redeem":
                    ident = fake.tickets.pop(body.get("ticket", ""), None)
                    return self._send(200, {"identityId": ident}) if ident else self._send(404, {"error": "unknown ticket"})
                if self.path == "/v1/voice-deletion-tickets/redeem":
                    ident = fake.deletions.pop(body.get("ticket", ""), None)
                    return self._send(200, {"identityId": ident, "hadVoice": True}) if ident else self._send(404, {"error": "unknown ticket"})
                if self.path == "/v1/evidence":
                    return self._send(202, None)
                if self.path == "/v1/retirements":
                    return self._send(204, None)
                self._send(404, {"error": "not found"})

            def _send(self, code: int, body: Any) -> None:
                raw = json.dumps(body).encode() if body is not None else b""
                self.send_response(code)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    def close(self) -> None:
        self.httpd.shutdown()
