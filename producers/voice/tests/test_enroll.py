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
from attest_voice.enroll import Refused, TooLittleSpeech, enroll
from attest_voice.server import EnrollService, make_handler, takes
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


def mp4_aac(samples: np.ndarray, rate: int = 48000, fragmented: bool = False) -> bytes:
    """What Safari's MediaRecorder sends (audio/mp4, AAC), plain or fragmented."""
    buf = io.BytesIO()
    opts = {"movflags": "frag_keyframe+empty_moov+default_base_moof"} if fragmented else {}
    with av.open(buf, "w", format="mp4", options=opts) as c:
        s = c.add_stream("aac", rate=rate, layout="mono")
        n = s.codec_context.frame_size or 1024
        for i in range(0, len(samples), n):
            chunk = samples[i : i + n]
            frame = av.AudioFrame.from_ndarray(chunk.reshape(1, -1).astype(np.float32), format="fltp", layout="mono")
            frame.sample_rate = rate
            frame.pts = i
            for p in s.encode(frame):
                c.mux(p)
        for p in s.encode(None):
            c.mux(p)
    return buf.getvalue()


def multipart(*parts: bytes, name: str = "take", boundary: str = "----attestTestBoundary7MA4YWxk") -> tuple[bytes, str]:
    """A body like the browser's FormData with one file per take."""
    out = b""
    for i, p in enumerate(parts):
        out += (f"--{boundary}\r\nContent-Disposition: form-data; name=\"{name}\"; filename=\"take{i}\"\r\n"
                f"Content-Type: audio/webm\r\n\r\n").encode() + p + b"\r\n"
    out += f"--{boundary}--\r\n".encode()
    return out, f"multipart/form-data; boundary={boundary}"


@pytest.fixture
def env(tmp_path):
    fake = FakeAttest(tickets={"good": "id-ana", "good2": "id-bo"})
    store = Store(tmp_path / "data")
    svc = EnrollService(Attest(fake.url, StaticToken("producer-token")), FakeEngine(), store,
                       model="fake", min_seconds=15, cors_origin=ORIGIN)
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
    st, h, body = post(port, webm_opus(speech(18, 48000)),
                       {"Authorization": "Bearer good", "Content-Type": "audio/webm;codecs=opus", "Origin": ORIGIN})
    assert st == 201, body
    assert body["identityId"] == "id-ana" and body["voicedSeconds"] >= 16
    assert body["replaced"] is False and "previousVoicedSeconds" not in body
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
    assert body["reason"] == "too_short" and body["requiredSeconds"] == 15
    assert 1 < body["voicedSeconds"] < 2.5 and body["recordedSeconds"] == pytest.approx(2, abs=0.1)
    assert fake.requests == [] and store.all() == []


def test_unknown_ticket(env):
    fake, store, port = env
    st, _, body = post(port, webm_opus(speech(18, 48000)), {"Authorization": "Bearer nope"})
    assert st == 401 and store.all() == []


def test_missing_ticket_and_garbage_audio(env):
    _, _, port = env
    assert post(port, b"x", {})[0] == 401
    assert post(port, b"not audio at all", {"Authorization": "Bearer good"})[0] == 415
    body, ctype = multipart(b"not audio")
    assert post(port, body, {"Authorization": "Bearer good", "Content-Type": ctype})[0] == 415
    body, ctype = multipart(webm_opus(speech(18, 48000)), name="other")
    assert post(port, body, {"Authorization": "Bearer good", "Content-Type": ctype})[0] == 415
    assert post(port, b"--x--", {"Authorization": "Bearer good", "Content-Type": "multipart/form-data"})[0] == 415


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
        st, _, body = post(port, webm_opus(speech(18, 48000)), {"Authorization": "Bearer good"})
    assert st == 502, body
    assert "attest" in body["error"] and "Nothing was saved" in body["error"]
    assert store.all() == []
    assert any("ticket redemption failed" in r.getMessage() for r in caplog.records)


def test_attest_unreachable_is_502(tmp_path):
    svc = EnrollService(Attest("http://127.0.0.1:9", StaticToken("t"), timeout=2), FakeEngine(), Store(tmp_path),
                       model="fake", min_seconds=6)
    st, body = svc.handle("good", webm_opus(speech(18, 48000)))
    assert st == 502 and "could not reach attest" in body["error"]


def test_invalid_ticket_is_attests_json_404(env):
    fake, store, port = env
    fake.redeem_reply = (404, b'{"error": "unknown, used, or expired ticket"}')
    st, _, body = post(port, webm_opus(speech(18, 48000)), {"Authorization": "Bearer good"})
    assert st == 401 and "invalid, used, or expired" in body["error"]


@pytest.mark.parametrize("fragmented", [False, True])
def test_safari_mp4_aac_enrolls(env, fragmented):
    fake, store, port = env
    st, _, body = post(port, mp4_aac(speech(18, 48000), fragmented=fragmented),
                       {"Authorization": "Bearer good", "Content-Type": "audio/mp4", "Origin": ORIGIN})
    assert st == 201, body
    assert body["identityId"] == "id-ana" and body["voicedSeconds"] >= 16
    assert [p.identity for p in store.all()] == ["id-ana"]


def test_takes_add_up_and_a_refusal_keeps_the_ticket(env):
    """The page's "keep going": a short take is refused without redeeming the
    ticket; the same takes plus another, sent again, enroll."""
    fake, store, port = env
    first = webm_opus(speech(9, 48000))
    body, ctype = multipart(first)
    st, _, reply = post(port, body, {"Authorization": "Bearer good", "Content-Type": ctype, "Origin": ORIGIN})
    assert st == 422 and reply["reason"] == "too_short" and 7 < reply["voicedSeconds"] < 9.5
    assert fake.requests == [] and store.all() == []
    body, ctype = multipart(first, mp4_aac(speech(9, 48000)))
    st, _, reply = post(port, body, {"Authorization": "Bearer good", "Content-Type": ctype, "Origin": ORIGIN})
    assert st == 201, reply
    assert reply["voicedSeconds"] >= 15 and reply["recordedSeconds"] == pytest.approx(18.3, abs=0.2)
    assert [r[1] for r in fake.requests] == [{"ticket": "good"}]
    assert [p.identity for p in store.all()] == ["id-ana"]


def test_total_length_across_takes_is_capped(tmp_path):
    svc = EnrollService(Attest("http://127.0.0.1:9", StaticToken("t")), FakeEngine(), Store(tmp_path),
                       model="fake", min_seconds=15, max_audio_seconds=30)
    body, ctype = multipart(*[webm_opus(speech(12, 48000))] * 3)
    st, reply = svc.handle("good", body, ctype)
    assert st == 413 and "longer than 30 s in all" in reply["error"]


def test_too_many_takes_refused():
    body, ctype = multipart(*[b"x"] * 17)
    with pytest.raises(ValueError, match="more than 16"):
        takes(ctype, body)


def test_refusal_never_replaces_a_stored_print(env):
    fake, store, port = env
    assert post(port, webm_opus(speech(40, 48000)), {"Authorization": "Bearer good"})[0] == 201
    before = store.get("id-ana")
    assert before is not None and before.voiced_seconds > 35
    fake.tickets["again"] = "id-ana"
    # Too short, then distorted: both refused before the ticket is redeemed.
    for audio in (speech(5), np.clip(3 * speech(20), -1, 1)):
        st, _, reply = post(port, wav(audio), {"Authorization": "Bearer again"})
        assert st == 422, reply
        assert _same(store.get("id-ana"), before)
    assert "again" in fake.tickets
    # A good, shorter re-recording replaces it, and says so.
    st, _, reply = post(port, webm_opus(speech(18, 48000)), {"Authorization": "Bearer again"})
    assert st == 201 and reply["replaced"] is True and reply["previousVoicedSeconds"] > 35
    assert store.get("id-ana").voiced_seconds < 20


def _same(a, b) -> bool:
    return a is not None and a.enrolled_at == b.enrolled_at and a.voiced_seconds == b.voiced_seconds \
        and np.array_equal(a.embedding, b.embedding)


def wav(samples: np.ndarray) -> bytes:
    """16 kHz float WAV: no lossy codec to smear clipping or level."""
    buf = io.BytesIO()
    with av.open(buf, "w", format="wav") as c:
        s = c.add_stream("pcm_f32le", rate=SAMPLE_RATE, layout="mono")
        frame = av.AudioFrame.from_ndarray(samples.reshape(1, -1).astype(np.float32), format="flt", layout="mono")
        frame.sample_rate = SAMPLE_RATE
        for p in s.encode(frame):
            c.mux(p)
        for p in s.encode(None):
            c.mux(p)
    return buf.getvalue()


class AllVoiced(FakeEngine):
    """A VAD that calls everything speech, to test level checks in isolation."""

    def vad(self, audio):
        return [(0.0, len(audio) / SAMPLE_RATE)]


@pytest.mark.parametrize("audio, engine, reason", [
    (np.zeros(20 * SAMPLE_RATE, dtype=np.float32), FakeEngine(), "too_quiet"),         # muted microphone
    (speech(20) * 0.0135, AllVoiced(), "too_quiet"),                                   # audible, but faint
    (np.clip(3 * speech(20), -1, 1), FakeEngine(), "clipping"),                         # distorted
    (np.concatenate([speech(5), np.zeros(25 * SAMPLE_RATE, np.float32)]), FakeEngine(), "mostly_silence"),
    (speech(10), FakeEngine(), "too_short"),
])
def test_quality_refusals(tmp_path, audio, engine, reason):
    store = Store(tmp_path)
    with pytest.raises(Refused) as e:
        enroll("x", audio, engine, store, model="fake", min_seconds=15)
    assert e.value.reason == reason
    assert e.value.needed == 15 and e.value.recorded == pytest.approx(len(audio) / SAMPLE_RATE)
    assert store.all() == []


def test_normal_speech_is_not_refused_for_level(tmp_path):
    # Loud but unclipped, and quiet but clear, both enroll.
    for audio in (speech(20) * 2.5, speech(20) * 0.05):
        assert enroll("x", audio, AllVoiced(), Store(tmp_path), model="fake", min_seconds=15) == pytest.approx(20)


def test_the_recording_is_not_kept(env, tmp_path):
    fake, store, port = env
    assert post(port, webm_opus(speech(18, 48000)), {"Authorization": "Bearer good"})[0] == 201
    assert sorted(p.name for p in store.dir.iterdir()) == ["voiceprints.json"]
    saved = json.loads(store.path.read_text())["id-ana"]
    assert sorted(saved) == ["embedding", "enrolledAt", "model", "voicedSeconds"]
