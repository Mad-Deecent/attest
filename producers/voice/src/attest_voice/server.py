"""``POST /enroll``: a browser uploads a recording with an enrollment ticket.

Request: raw audio body (e.g. ``audio/webm;codecs=opus`` from MediaRecorder),
``Authorization: Bearer <ticket>``. The ticket is redeemed at attest with the
producer's own credential; attest answers with the identity it enrolls. The
audio is checked before the ticket is redeemed, so a too-short recording does
not burn a single-use ticket.

Responses: 201 ``{"identityId", "voicedSeconds"}``; 401 no ticket, or attest
says the ticket is unknown, used or expired; 413 too large; 415 undecodable;
422 too little speech; 502 attest unreachable, without a redeem route, or
answering unexpectedly (logged).
Audio and embeddings are never logged.
"""

from __future__ import annotations

import json
import logging
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

from .attest_client import Attest, RedeemUnavailable
from .audio import DecodeError, decode
from .enroll import EnrollEngine, TooLittleSpeech, check
from .speaker import voiceprint
from .store import Store

log = logging.getLogger("attest_voice.server")


class EnrollService:
    def __init__(self, attest: Attest, engine: EnrollEngine, store: Store, *, model: str,
                 min_seconds: float, cors_origin: str = "", max_body: int = 20 * 1024 * 1024, max_audio_seconds: float = 120):
        self.attest = attest
        self.engine = engine
        self.store = store
        self.model = model
        self.min_seconds = min_seconds
        self.cors_origin = cors_origin
        self.max_body = max_body
        self.max_audio_seconds = max_audio_seconds
        # One model instance is not safe for concurrent use; enrollment is rare.
        self._lock = threading.Lock()

    def handle(self, ticket: str, body: bytes) -> tuple[int, dict[str, Any]]:
        if not ticket:
            return 401, {"error": "missing enrollment ticket"}
        try:
            audio = decode(body, max_seconds=self.max_audio_seconds)
        except DecodeError as e:
            return 415, {"error": str(e)}
        with self._lock:
            try:
                samples, voiced = check(audio, self.engine, self.min_seconds)
            except TooLittleSpeech as e:
                return 422, {"error": str(e), "voicedSeconds": round(e.voiced, 1), "requiredSeconds": e.needed}
            try:
                identity = self.attest.redeem_ticket(ticket)
            except RedeemUnavailable as e:
                log.error("ticket redemption failed, not because of the ticket: %s", e)
                return 502, {"error": "The voice service could not check your enrollment ticket with attest: attest "
                                      "has no ticket-redemption endpoint or answered unexpectedly. Nothing was saved. "
                                      "Ask whoever runs this system to check the voice service's log."}
            except OSError as e:
                log.error("ticket redemption failed, attest unreachable: %s", e)
                return 502, {"error": "The voice service could not reach attest to check your enrollment ticket. "
                                      "Nothing was saved. Please try again in a few minutes."}
            if identity is None:
                return 401, {"error": "enrollment ticket is invalid, used, or expired"}
            vec = voiceprint(samples, self.engine.embed)
            assert vec is not None
            self.store.put(identity, vec, self.model, voiced)
        log.info("enrolled an identity (%.1f s voiced)", voiced)
        return 201, {"identityId": identity, "voicedSeconds": round(voiced, 1)}


def make_handler(svc: EnrollService) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        server_version = "attest-voice"

        def log_message(self, fmt: str, *args: Any) -> None:  # method, path, status only
            log.info("%s %s", self.address_string(), fmt % args)

        def _cors(self) -> None:
            origin = self.headers.get("Origin")
            if svc.cors_origin and origin == svc.cors_origin:
                self.send_header("Access-Control-Allow-Origin", origin)
                self.send_header("Vary", "Origin")
                self.send_header("Access-Control-Allow-Methods", "POST, OPTIONS")
                self.send_header("Access-Control-Allow-Headers", "Authorization, Content-Type")
                self.send_header("Access-Control-Max-Age", "600")

        def _reply(self, status: int, body: dict[str, Any] | None) -> None:
            raw = json.dumps(body).encode() if body is not None else b""
            self.send_response(status)
            self._cors()
            if body is not None:
                self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_OPTIONS(self) -> None:
            if self.path != "/enroll":
                return self._reply(404, {"error": "not found"})
            origin = self.headers.get("Origin")
            if not svc.cors_origin or origin != svc.cors_origin:
                return self._reply(403, {"error": "origin not allowed"})
            self._reply(204, None)

        def do_POST(self) -> None:
            if self.path != "/enroll":
                return self._reply(404, {"error": "not found"})
            origin = self.headers.get("Origin")
            if origin is not None and origin != svc.cors_origin:
                return self._reply(403, {"error": "origin not allowed"})
            auth = self.headers.get("Authorization", "")
            ticket = auth[7:].strip() if auth.lower().startswith("bearer ") else ""
            try:
                n = int(self.headers.get("Content-Length", ""))
            except ValueError:
                return self._reply(411, {"error": "Content-Length required"})
            if n > svc.max_body:
                return self._reply(413, {"error": "recording too large"})
            body = self.rfile.read(n)
            status, reply = svc.handle(ticket, body)
            self._reply(status, reply)

    return Handler


def serve(svc: EnrollService, listen: str) -> None:
    host, _, port = listen.rpartition(":")
    httpd = ThreadingHTTPServer((host or "127.0.0.1", int(port)), make_handler(svc))
    log.info("enroll endpoint on http://%s:%s/enroll", host or "127.0.0.1", port)
    httpd.serve_forever()
