"""Talking to attest: evidence, retirements, enrolment-ticket redemption.

Only identity ids and LLRs leave this process; never audio or embeddings.
"""

from __future__ import annotations

import json
import os
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any, Protocol


class TokenSource(Protocol):
    def token(self) -> str: ...


@dataclass
class StaticToken:
    value: str

    def token(self) -> str:
        return self.value


class ClientCredentials:
    """OIDC client-credentials grant with a cached access token."""

    def __init__(self, issuer: str, client_id: str, secret: str, *, scope: str = "", audience: str = "", timeout: float = 10):
        self.issuer = issuer.rstrip("/")
        self.client_id = client_id
        self._secret = secret
        self.scope = scope
        self.audience = audience
        self.timeout = timeout
        self._endpoint: str | None = None
        self._token: str | None = None
        self._exp = 0.0
        self._lock = threading.Lock()

    def _token_endpoint(self) -> str:
        if self._endpoint is None:
            doc = _get_json(f"{self.issuer}/.well-known/openid-configuration", self.timeout)
            self._endpoint = doc["token_endpoint"]
        return self._endpoint

    def token(self) -> str:
        with self._lock:
            if self._token and time.monotonic() < self._exp:
                return self._token
            form = {"grant_type": "client_credentials", "client_id": self.client_id, "client_secret": self._secret}
            if self.scope:
                form["scope"] = self.scope
            if self.audience:
                form["audience"] = self.audience
            req = urllib.request.Request(
                self._token_endpoint(), data=urllib.parse.urlencode(form).encode(),
                headers={"Content-Type": "application/x-www-form-urlencoded"},
            )
            with urllib.request.urlopen(req, timeout=self.timeout) as r:
                body = json.load(r)
            self._token = body["access_token"]
            # Refresh a little early; tokens without expires_in get a minute.
            self._exp = time.monotonic() + max(5.0, float(body.get("expires_in", 60)) - 30)
            return self._token

    def invalidate(self) -> None:
        with self._lock:
            self._token = None


def read_secret(file: str = "", env: str = "") -> str:
    if file:
        with open(os.path.expanduser(file)) as f:
            return f.read().strip()
    if env and os.environ.get(env):
        return os.environ[env]
    raise ValueError("client secret: set secretFile or secretEnv (and the variable)")


def _get_json(url: str, timeout: float) -> Any:
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return json.load(r)


class AttestError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(f"attest {status}: {message}")
        self.status = status


def rfc3339(t: datetime) -> str:
    return t.astimezone(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def evidence_payload(
    subject: str, zone: str, candidates: dict[str, float], calibration: str,
    *, ttl: float, now: datetime | None = None,
) -> dict[str, Any]:
    """Evidence JSON for attest. ``observedAt`` is wall-clock send time, not
    audio time: attest orders slots and expires evidence by it."""
    now = now or datetime.now(timezone.utc)
    subj: dict[str, str] = {"key": subject}
    if zone:
        subj["zone"] = zone
    return {
        "subject": subj,
        "modality": "voice",
        "observedAt": rfc3339(now),
        "expiresAt": rfc3339(now + timedelta(seconds=ttl)),
        "candidates": [{"identityId": i, "llr": round(l, 4)} for i, l in sorted(candidates.items())],
        "calibration": calibration,
    }


class Attest:
    def __init__(self, base_url: str, creds: TokenSource, *, timeout: float = 10):
        self.base = base_url.rstrip("/")
        self.creds = creds
        self.timeout = timeout

    def _post(self, path: str, body: dict[str, Any]) -> tuple[int, Any]:
        for attempt in (0, 1):
            req = urllib.request.Request(
                self.base + path, data=json.dumps(body).encode(), method="POST",
                headers={"Content-Type": "application/json", "Authorization": f"Bearer {self.creds.token()}"},
            )
            try:
                with urllib.request.urlopen(req, timeout=self.timeout) as r:
                    raw = r.read()
                    return r.status, (json.loads(raw) if raw else None)
            except urllib.error.HTTPError as e:
                # A cached OIDC token can be revoked early: retry once fresh.
                if e.code == 401 and attempt == 0 and isinstance(self.creds, ClientCredentials):
                    self.creds.invalidate()
                    continue
                raw = e.read()
                try:
                    msg = json.loads(raw).get("error", raw.decode(errors="replace"))
                except (ValueError, AttributeError):
                    msg = raw.decode(errors="replace")
                raise AttestError(e.code, msg) from None
        raise AssertionError("unreachable")

    def post_evidence(self, payload: dict[str, Any]) -> None:
        self._post("/v1/evidence", payload)

    def retire_subject(self, key: str) -> None:
        self._post("/v1/retirements", {"subject": key})

    def redeem_ticket(self, ticket: str) -> str | None:
        """Identity id the ticket enrols, or None if attest does not know it."""
        try:
            _, body = self._post("/v1/enrolment-tickets/redeem", {"ticket": ticket})
        except AttestError as e:
            if e.status == 404:
                return None
            raise
        ident = (body or {}).get("identityId")
        return ident if isinstance(ident, str) and ident else None
