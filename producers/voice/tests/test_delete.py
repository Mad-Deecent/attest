"""Deleting a voice from the browser: ``DELETE /enroll`` with a deletion ticket,
and ``GET /enroll`` for what a recording needs."""

import http.client
import json
import threading
from http.server import ThreadingHTTPServer

import pytest

from attest_voice.attest_client import Attest, StaticToken
from attest_voice.server import EnrollService, make_handler
from attest_voice.store import Store

from .fakes import FakeAttest, FakeEngine
from .test_enroll import ORIGIN, post, speech, webm_opus


@pytest.fixture
def env(tmp_path):
    fake = FakeAttest(tickets={"enroll-ana": "id-ana"}, deletions={"delete-ana": "id-ana", "delete-ana-2": "id-ana"})
    store = Store(tmp_path / "data")
    svc = EnrollService(Attest(fake.url, StaticToken("producer-token")), FakeEngine(), store,
                        model="fake", min_seconds=12, cors_origin=ORIGIN)
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(svc))
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    yield fake, store, svc, httpd.server_address[1]
    httpd.shutdown()
    fake.close()


def call(port, method: str, headers: dict):
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    c.request(method, "/enroll", headers=headers)
    r = c.getresponse()
    raw = r.read()
    return r.status, dict(r.getheaders()), (json.loads(raw) if raw else None)


def enroll_ana(port):
    st, _, body = post(port, webm_opus(speech(18, 48000)), {"Authorization": "Bearer enroll-ana"})
    assert st == 201, body


def test_requirements_say_the_configured_minimum(env):
    _, _, _, port = env
    st, h, body = call(port, "GET", {"Origin": ORIGIN})
    assert st == 200 and body == {"minVoicedSeconds": 12, "maxSeconds": 120}
    assert h["Access-Control-Allow-Origin"] == ORIGIN


def test_preflight_allows_delete_for_the_page_only(env):
    _, _, _, port = env
    st, h, _ = call(port, "OPTIONS", {"Origin": ORIGIN, "Access-Control-Request-Method": "DELETE"})
    assert st == 204 and "DELETE" in h["Access-Control-Allow-Methods"]
    assert "Authorization" in h["Access-Control-Allow-Headers"]
    st, h, _ = call(port, "OPTIONS", {"Origin": "https://evil.example", "Access-Control-Request-Method": "DELETE"})
    assert st == 403 and "Access-Control-Allow-Origin" not in h


def test_delete_removes_the_print_after_attest_lets_go(env):
    fake, store, _, port = env
    enroll_ana(port)
    assert store.get("id-ana") is not None
    st, h, body = call(port, "DELETE", {"Authorization": "Bearer delete-ana", "Origin": ORIGIN})
    assert st == 200, body
    assert body == {"identityId": "id-ana", "deleted": True, "held": True}
    assert h["Access-Control-Allow-Origin"] == ORIGIN
    assert store.get("id-ana") is None
    path, req, auth = fake.requests[-1]
    assert path == "/v1/voice-deletion-tickets/redeem" and req == {"ticket": "delete-ana"} and auth == "Bearer producer-token"

    # Spent: the same ticket again is refused. A fresh one with nothing
    # left to delete is not an error.
    st, _, body = call(port, "DELETE", {"Authorization": "Bearer delete-ana"})
    assert st == 401 and "invalid, used, or expired" in body["error"]
    st, _, body = call(port, "DELETE", {"Authorization": "Bearer delete-ana-2"})
    assert st == 200 and body["held"] is False


def test_tickets_only_work_for_their_purpose(env):
    fake, store, _, port = env
    # An enrollment ticket cannot delete...
    st, _, _ = call(port, "DELETE", {"Authorization": "Bearer enroll-ana"})
    assert st == 401
    enroll_ana(port)
    # ...and a deletion ticket cannot enroll.
    st, _, _ = post(port, webm_opus(speech(18, 48000)), {"Authorization": "Bearer delete-ana"})
    assert st == 401
    assert store.get("id-ana") is not None and "delete-ana" not in fake.tickets


def test_no_ticket_or_another_origin(env):
    _, store, _, port = env
    enroll_ana(port)
    assert call(port, "DELETE", {})[0] == 401
    assert call(port, "DELETE", {"Authorization": "Bearer delete-ana", "Origin": "https://evil.example"})[0] == 403
    assert store.get("id-ana") is not None


@pytest.mark.parametrize("reply", [(404, b"404 page not found\n"), (500, b'{"error": "could not save state; delete nothing"}')])
def test_attest_problems_delete_nothing(env, reply, caplog):
    fake, store, _, port = env
    enroll_ana(port)
    fake.delete_reply = reply  # an attest without the route, or one that could not save
    with caplog.at_level("ERROR", logger="attest_voice.server"):
        st, _, body = call(port, "DELETE", {"Authorization": "Bearer delete-ana"})
    assert st == 502 and "Nothing was deleted" in body["error"]
    assert store.get("id-ana") is not None
    assert any("deletion ticket redemption failed" in r.getMessage() for r in caplog.records)


def test_attest_unreachable_deletes_nothing(tmp_path):
    store = Store(tmp_path)
    store.put("id-ana", speech(1)[:2], "fake", 20)
    svc = EnrollService(Attest("http://127.0.0.1:9", StaticToken("t"), timeout=2), FakeEngine(), store,
                        model="fake", min_seconds=15)
    st, body = svc.delete("delete-ana")
    assert st == 502 and "could not reach attest" in body["error"]
    assert store.get("id-ana") is not None


def test_a_print_that_cannot_be_deleted_says_so(env, caplog):
    _, store, svc, port = env
    enroll_ana(port)

    def fail(identity):
        raise OSError("disk full")

    svc.store.delete = fail
    with caplog.at_level("ERROR", logger="attest_voice.server"):
        st, _, body = call(port, "DELETE", {"Authorization": "Bearer delete-ana"})
    assert st == 500 and body["deleted"] is False
    assert "attest no longer lists your voice" in body["error"] and "try again" in body["error"]
    assert any("could not delete a voiceprint" in r.getMessage() for r in caplog.records)
