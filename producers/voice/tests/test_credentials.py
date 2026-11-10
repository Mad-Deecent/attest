import json
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from attest_voice.attest_client import ClientCredentials
from attest_voice.config import Config


def test_client_credentials_discovers_and_caches(tmp_path, monkeypatch):
    calls: list[dict] = []

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            assert self.path == "/realms/home/.well-known/openid-configuration"
            self._json({"token_endpoint": f"{base}/realms/home/token"})

        def do_POST(self):
            form = urllib.parse.parse_qs(self.rfile.read(int(self.headers["Content-Length"])).decode())
            calls.append({k: v[0] for k, v in form.items()})
            self._json({"access_token": f"tok-{len(calls)}", "expires_in": 300})

        def _json(self, body):
            raw = json.dumps(body).encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    httpd = ThreadingHTTPServer(("127.0.0.1", 0), H)
    base = f"http://127.0.0.1:{httpd.server_address[1]}"
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        secret = tmp_path / "secret"
        secret.write_text("s3cret\n")
        cfg = tmp_path / "c.json"
        cfg.write_text(json.dumps({"attest": {"oidc": {"issuer": f"{base}/realms/home", "clientId": "voice-mic-1",
                                                        "secretFile": str(secret)}}}))
        creds = Config.load(cfg).credentials()
        assert isinstance(creds, ClientCredentials)
        assert creds.token() == "tok-1"
        assert creds.token() == "tok-1"  # cached
        assert calls == [{"grant_type": "client_credentials", "client_id": "voice-mic-1", "client_secret": "s3cret"}]
        creds.invalidate()
        assert creds.token() == "tok-2"

        monkeypatch.setenv("VOICE_SECRET", "from-env")
        cfg.write_text(json.dumps({"attest": {"oidc": {"issuer": f"{base}/realms/home", "clientId": "x",
                                                        "secretEnv": "VOICE_SECRET"}}}))
        Config.load(cfg).credentials().token()
        assert calls[-1]["client_secret"] == "from-env"
    finally:
        httpd.shutdown()


def test_static_token_config(tmp_path):
    cfg = tmp_path / "c.json"
    cfg.write_text(json.dumps({"attest": {"token": "dev-voice-token"}}))
    assert Config.load(cfg).credentials().token() == "dev-voice-token"


def test_example_config_loads():
    from pathlib import Path

    c = Config.load(Path(__file__).parent.parent / "attest-voice.example.json")
    assert c.zone == "car" and c.guard == {"max_seconds": 1.5, "min_reference": 2.0, "min_llr": 2.0}
    assert c.load_calibration().id == "demo-eres2net-librispeech-v1"
