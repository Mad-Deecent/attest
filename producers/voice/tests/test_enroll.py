import http.client
import io
import json
import os
import stat
import threading
from http.server import ThreadingHTTPServer

import av
import numpy as np
import pytest

from attest_voice.attest_client import Attest, StaticToken
from attest_voice.audio import SAMPLE_RATE, decode
from attest_voice.enroll import TooLittleSpeech, enroll
from attest_voice.server import EnrollService, make_handler
from attest_voice.store import Store

from .fakes import FakeAttest, FakeEngine

ORIGIN = "https://app.example"


def speech(seconds: float, rate: int = SAMPLE_RATE) -> np.ndarray:
    t = np.arange(int(seconds * rate)) / rate
    return (0.3 * np.sin(2 * np.pi * 220 * t)).astype(np.float32) + 0.05


def webm_opus(samples: np.ndarray, rate: int = 48000) -> bytes:
    """What a browser's MediaRecorder sends."""
    buf = io.BytesIO()
    with av.open(buf, "w", format="webm") as c:
        s = c.add_stream("libopus", rate=rate, layout="mono")
        frame = av.AudioFrame.from_ndarray(samples.reshape(1, -1), format="flt", layout="mono")
        frame.sample_rate = rate
        for p in s.encode(frame):
            c.mux(p)
        for p in s.encode(None):
            c.mux(p)
    return buf.getvalue()


@pytest.fixture
def env(tmp_path):
    fake = FakeAttest(tickets={"good": "id-ana", "good2": "id-bo"})
    store = Store(tmp_path / "data")
    svc = EnrollService(Attest(fake.url, StaticToken("producer-token")), FakeEngine(), store,
                       model="fake", min_seconds=6, cors_origin=ORIGIN)
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(svc))
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    yield fake, store, httpd.server_address[1]
    httpd.shutdown()
    fake.close()


def post(port, body: bytes, headers: dict):
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    c.request("POST", "/enroll", body=body, headers=headers)
    r = c.getresponse()
    raw = r.read()
    return r.status, dict(r.getheaders()), (json.loads(raw) if raw else None)


def test_webm_opus_decodes_to_16k_mono():
    a = decode(webm_opus(speech(2, 48000)))
    assert abs(len(a) / SAMPLE_RATE - 2) < 0.1


def test_browser_enrollment_stores_print_and_redeems_ticket(env):
    fake, store, port = env
    st, h, body = post(port, webm_opus(speech(8, 48000)),
                       {"Authorization": "Bearer good", "Content-Type": "audio/webm;codecs=opus", "Origin": ORIGIN})
    assert st == 201, body
    assert body["identityId"] == "id-ana" and body["voicedSeconds"] >= 7
    assert h["Access-Control-Allow-Origin"] == ORIGIN
    (path, req, auth), = fake.requests
    assert path == "/v1/enrollment-tickets/redeem" and req == {"ticket": "good"} and auth == "Bearer producer-token"
    assert [p.identity for p in store.all()] == ["id-ana"]
    assert stat.S_IMODE(os.stat(store.path).st_mode) == 0o600
    assert stat.S_IMODE(os.stat(store.dir).st_mode) == 0o700


def test_short_recording_rejected_without_burning_ticket(env):
    fake, store, port = env
    st, _, body = post(port, webm_opus(speech(2, 48000)), {"Authorization": "Bearer good"})
    assert st == 422
    assert "needs at least 6 s" in body["error"]
    assert fake.requests == [] and store.all() == []


def test_unknown_ticket(env):
    fake, store, port = env
    st, _, body = post(port, webm_opus(speech(8, 48000)), {"Authorization": "Bearer nope"})
    assert st == 401 and store.all() == []


def test_missing_ticket_and_garbage_audio(env):
    _, _, port = env
    assert post(port, b"x", {})[0] == 401
    assert post(port, b"not audio at all", {"Authorization": "Bearer good"})[0] == 415


def test_cors_only_for_configured_origin(env):
    _, _, port = env
    c = http.client.HTTPConnection("127.0.0.1", port)
    c.request("OPTIONS", "/enroll", headers={"Origin": ORIGIN, "Access-Control-Request-Method": "POST"})
    r = c.getresponse(); r.read()
    assert r.status == 204 and r.getheader("Access-Control-Allow-Origin") == ORIGIN
    assert "Authorization" in r.getheader("Access-Control-Allow-Headers")
    c.request("OPTIONS", "/enroll", headers={"Origin": "https://evil.example"})
    r = c.getresponse(); r.read()
    assert r.status == 403 and r.getheader("Access-Control-Allow-Origin") is None
    st, h, _ = post(port, b"x", {"Authorization": "Bearer good", "Origin": "https://evil.example"})
    assert st == 403 and "Access-Control-Allow-Origin" not in h


def test_cli_path_enroll_and_forget(tmp_path):
    store = Store(tmp_path)
    with pytest.raises(TooLittleSpeech):
        enroll("x", speech(3), FakeEngine(), store, model="fake", min_seconds=6)
    assert enroll("x", speech(7), FakeEngine(), store, model="fake", min_seconds=6) == pytest.approx(7, abs=0.3)
    assert store.delete("x") is True
    assert store.delete("x") is False
    assert store.all() == []


@pytest.mark.parametrize("reply", [
    (404, b"404 page not found\n"),                   # an attest without the redeem route
    (404, b"<html>Not Found</html>"),                 # something else on that port
    (403, b'{"error": "only the configured voice producer may redeem enrollment tickets"}'),
    (500, b'{"error": "could not save state"}'),
    (200, b"{}"),                                     # a 200 that names nobody
    (200, b"not json"),
])
def test_attest_problems_are_502_not_a_bad_ticket(env, reply, caplog):
    fake, store, port = env
    fake.redeem_reply = reply
    with caplog.at_level("ERROR", logger="attest_voice.server"):
        st, _, body = post(port, webm_opus(speech(8, 48000)), {"Authorization": "Bearer good"})
    assert st == 502, body
    assert "attest" in body["error"] and "Nothing was saved" in body["error"]
    assert store.all() == []
    assert any("ticket redemption failed" in r.getMessage() for r in caplog.records)


def test_attest_unreachable_is_502(tmp_path):
    svc = EnrollService(Attest("http://127.0.0.1:9", StaticToken("t"), timeout=2), FakeEngine(), Store(tmp_path),
                       model="fake", min_seconds=6)
    st, body = svc.handle("good", webm_opus(speech(8, 48000)))
    assert st == 502 and "could not reach attest" in body["error"]


def test_invalid_ticket_is_attests_json_404(env):
    fake, store, port = env
    fake.redeem_reply = (404, b'{"error": "unknown, used, or expired ticket"}')
    st, _, body = post(port, webm_opus(speech(8, 48000)), {"Authorization": "Bearer good"})
    assert st == 401 and "invalid, used, or expired" in body["error"]
