"""``POST /enroll``: a browser uploads a recording with an enrollment ticket.

Request: ``Authorization: Bearer <ticket>`` and either a raw audio body (e.g.
``audio/webm;codecs=opus`` or ``audio/mp4`` from MediaRecorder) or
``multipart/form-data`` with one or more ``take`` parts, each a separately
recorded file. Takes are decoded one by one and joined, so a person who did not
say enough can record more and send everything again with a fresh ticket;
nothing is kept between requests. The audio is checked before the ticket is
redeemed at attest (with the producer's own credential), so a refused
recording does not burn a single-use ticket, and a stored voiceprint is only
replaced by one that passed every check.

``GET /enroll`` tells a page what a recording needs: ``{"minVoicedSeconds",
"maxSeconds"}``, so it does not have to guess the configured minimum.

``DELETE /enroll`` with ``Authorization: Bearer <voice deletion ticket>``
deletes the person's voiceprint. The ticket is redeemed at attest first
(``POST /v1/voice-deletion-tickets/redeem``); attest forgets the enrollment
date and saves that before it answers, and only then is the print deleted, so
attest never lists a voice that is gone. 200 ``{"identityId", "deleted":
true, "held"}`` (``held`` says whether there was a print to delete; deleting
twice is not an error); 401 no ticket, or attest says it is unknown, used,
expired, or not a deletion ticket; 502 attest unreachable or answering
unexpectedly (nothing deleted, attest unchanged); 500 attest has forgotten the
date but the print could not be deleted here (logged; the page asks the
person to try again with a fresh ticket).

Responses to ``POST``: 201 ``{"identityId", "voicedSeconds", "recordedSeconds",
"replaced", "previousVoicedSeconds"?}``; 401 no ticket, or attest says the
ticket is unknown, used or expired; 413 too large or too long; 415 undecodable; 422
refused, ``{"error", "reason", "voicedSeconds", "requiredSeconds",
"recordedSeconds"}`` where reason is ``too_short``, ``mostly_silence``,
``too_quiet`` or ``clipping``; 502 attest unreachable, without a redeem route,
or answering unexpectedly (logged).
The recording is decoded in memory and discarded; only the voiceprint (a
numeric summary), its model, voiced seconds and date are stored. Audio and
embeddings are never logged.
"""

from __future__ import annotations

import json
import logging
import threading
from email.message import Message
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

from .attest_client import Attest, RedeemUnavailable
from .audio import SAMPLE_RATE, DecodeError, TooLong, decode_takes
from .enroll import EnrollEngine, Refused, check
from .speaker import voiceprint
from .store import Store

log = logging.getLogger("attest_voice.server")


MAX_TAKES = 16


def takes(content_type: str, body: bytes) -> list[bytes]:
    """The recordings in a request body: the ``take`` parts of a
    multipart/form-data body, else the whole body as one take."""
    if not content_type.lower().startswith("multipart/form-data"):
        return [body]
    m = Message()
    m["Content-Type"] = content_type
    boundary = m.get_param("boundary")
    if not isinstance(boundary, str) or not boundary:
        raise DecodeError("multipart body without a boundary")
    delim = b"--" + boundary.encode("latin-1")
    out: list[bytes] = []
    for section in body.split(delim)[1:]:
        if section.startswith(b"--"):
            break
        head, sep, data = section.removeprefix(b"\r\n").partition(b"\r\n\r\n")
        if not sep:
            raise DecodeError("malformed multipart body")
        h = Message()
        for line in head.decode("latin-1").split("\r\n"):
            name, _, value = line.partition(":")
            h[name.strip()] = value.strip()
        if h.get_param("name", header="content-disposition") == "take":
            out.append(data.removesuffix(b"\r\n"))
            if len(out) > MAX_TAKES:
                raise DecodeError(f"more than {MAX_TAKES} recordings")
    if not out:
        raise DecodeError("no recording in the request")
    return out


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

    def requirements(self) -> dict[str, Any]:
        return {"minVoicedSeconds": self.min_seconds, "maxSeconds": self.max_audio_seconds}

    def delete(self, ticket: str) -> tuple[int, dict[str, Any]]:
        """Delete the voiceprint a deletion ticket names. Holds the enrollment
        lock, so it cannot interleave with an enrollment storing a print."""
        if not ticket:
            return 401, {"error": "missing voice deletion ticket"}
        with self._lock:
            try:
                identity = self.attest.redeem_deletion_ticket(ticket)
            except RedeemUnavailable as e:
                log.error("deletion ticket redemption failed, not because of the ticket: %s", e)
                return 502, {"error": "The voice service could not check your deletion ticket with attest. Nothing was "
                                      "deleted. Ask whoever runs this system to check the voice service's log."}
            except OSError as e:
                log.error("deletion ticket redemption failed, attest unreachable: %s", e)
                return 502, {"error": "The voice service could not reach attest to check your deletion ticket. "
                                      "Nothing was deleted. Please try again in a few minutes."}
            if identity is None:
                return 401, {"error": "deletion ticket is invalid, used, or expired"}
            try:
                held = self.store.delete(identity)
            except OSError as e:
                # attest has already forgotten the date: say so, and let the
                # person ask again. A retry redeems a fresh ticket and deletes.
                log.error("could not delete a voiceprint after attest released it: %s", e)
                return 500, {"error": "attest no longer lists your voice, but the voice service could not delete it. "
                                      "Please try again.", "deleted": False}
        log.info("deleted an identity's voiceprint" if held else "deletion asked for an identity with no voiceprint")
        return 200, {"identityId": identity, "deleted": True, "held": held}

    def handle(self, ticket: str, body: bytes, content_type: str = "") -> tuple[int, dict[str, Any]]:
        if not ticket:
            return 401, {"error": "missing enrollment ticket"}
        try:
            audio = decode_takes(takes(content_type, body), max_seconds=self.max_audio_seconds)
        except TooLong as e:
            return 413, {"error": str(e)}
        except DecodeError as e:
            return 415, {"error": str(e)}
        with self._lock:
            try:
                samples, voiced = check(audio, self.engine, self.min_seconds)
            except Refused as e:
                return 422, {"error": str(e), "reason": e.reason, "voicedSeconds": round(e.voiced, 1),
                             "requiredSeconds": e.needed, "recordedSeconds": round(e.recorded, 1)}
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
            previous = self.store.get(identity)
            self.store.put(identity, vec, self.model, voiced)
        log.info("enrolled an identity (%.1f s voiced%s)", voiced, ", replacing an earlier one" if previous else "")
        reply: dict[str, Any] = {"identityId": identity, "voicedSeconds": round(voiced, 1),
                                 "recordedSeconds": round(len(audio) / SAMPLE_RATE, 1), "replaced": previous is not None}
        if previous is not None:
            reply["previousVoicedSeconds"] = round(previous.voiced_seconds, 1)
        return 201, reply


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
                self.send_header("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
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

        def _ticket(self) -> str | None:
            """The bearer ticket, or None after refusing another origin."""
            origin = self.headers.get("Origin")
            if origin is not None and origin != svc.cors_origin:
                self._reply(403, {"error": "origin not allowed"})
                return None
            auth = self.headers.get("Authorization", "")
            return auth[7:].strip() if auth.lower().startswith("bearer ") else ""

        def do_GET(self) -> None:
            if self.path != "/enroll":
                return self._reply(404, {"error": "not found"})
            self._reply(200, svc.requirements())

        def do_DELETE(self) -> None:
            if self.path != "/enroll":
                return self._reply(404, {"error": "not found"})
            ticket = self._ticket()
            if ticket is None:
                return
            self._reply(*svc.delete(ticket))

        def do_POST(self) -> None:
            if self.path != "/enroll":
                return self._reply(404, {"error": "not found"})
            ticket = self._ticket()
            if ticket is None:
                return
            try:
                n = int(self.headers.get("Content-Length", ""))
            except ValueError:
                return self._reply(411, {"error": "Content-Length required"})
            if n > svc.max_body:
                return self._reply(413, {"error": "recording too large"})
            body = self.rfile.read(n)
            status, reply = svc.handle(ticket, body, self.headers.get("Content-Type", ""))
            self._reply(status, reply)

    return Handler


def serve(svc: EnrollService, listen: str) -> None:
    host, _, port = listen.rpartition(":")
    httpd = ThreadingHTTPServer((host or "127.0.0.1", int(port)), make_handler(svc))
    log.info("enroll endpoint on http://%s:%s/enroll", host or "127.0.0.1", port)
    httpd.serve_forever()
