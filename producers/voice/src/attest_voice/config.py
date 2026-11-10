"""Producer configuration (JSON). See ``attest-voice.example.json``."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from importlib import resources
from pathlib import Path
from typing import Any

from . import calibration
from .attest_client import Attest, ClientCredentials, StaticToken, TokenSource, read_secret
from .models import DiarizationParams

DEFAULT_CALIBRATION = "demo-eres2net-librispeech-v1"


@dataclass
class Config:
    attest_url: str = "http://localhost:8420"
    token: str = ""
    oidc: dict[str, Any] = field(default_factory=dict)
    zone: str = ""
    ttl_seconds: float = 60.0
    data_dir: str = "~/.local/share/attest-voice"
    models_dir: str = "~/.cache/sherpa-onnx-models"
    calibration: str = ""  # path; empty = the bundled demo calibration
    threads: int = 2
    callback_url: str = ""
    diarization: DiarizationParams = field(default_factory=DiarizationParams)
    min_enrol_seconds: float = 6.0
    listen: str = "127.0.0.1:8431"
    cors_origin: str = ""
    max_body_bytes: int = 20 * 1024 * 1024
    guard: dict[str, float] = field(default_factory=dict)  # guard_short_islands kwargs

    @staticmethod
    def load(path: str | Path | None) -> "Config":
        if path is None:
            return Config()
        d = json.loads(Path(path).expanduser().read_text())
        a = d.get("attest", {})
        diar = d.get("diarization", {})
        enrol = d.get("enrol", {})
        base = Config()
        return Config(
            attest_url=a.get("url", base.attest_url),
            token=a.get("token", ""),
            oidc=a.get("oidc", {}),
            zone=d.get("zone", ""),
            ttl_seconds=float(d.get("ttlSeconds", base.ttl_seconds)),
            data_dir=d.get("dataDir", base.data_dir),
            models_dir=d.get("modelsDir", base.models_dir),
            calibration=d.get("calibration", ""),
            threads=int(d.get("threads", base.threads)),
            callback_url=d.get("callbackUrl", ""),
            diarization=DiarizationParams(
                num_speakers=int(diar.get("numSpeakers", -1)),
                cluster_threshold=float(diar.get("clusterThreshold", 0.5)),
                min_duration_on=float(diar.get("minDurationOn", 0.2)),
                min_duration_off=float(diar.get("minDurationOff", 0.3)),
            ),
            min_enrol_seconds=float(enrol.get("minVoicedSeconds", base.min_enrol_seconds)),
            listen=enrol.get("listen", base.listen),
            cors_origin=enrol.get("corsOrigin", ""),
            max_body_bytes=int(enrol.get("maxBodyBytes", base.max_body_bytes)),
            guard={k: float(d.get("guard", {})[j]) for j, k in
                   (("maxSeconds", "max_seconds"), ("minReferenceSeconds", "min_reference"), ("minLLR", "min_llr"))
                   if j in d.get("guard", {})},
        )

    def load_calibration(self) -> calibration.Calibration:
        if self.calibration:
            return calibration.load(Path(self.calibration).expanduser())
        ref = resources.files("attest_voice").joinpath(f"calibrations/{DEFAULT_CALIBRATION}.json")
        return calibration.Calibration.from_json(json.loads(ref.read_text()))

    def credentials(self) -> TokenSource:
        if self.oidc:
            o = self.oidc
            secret = read_secret(o.get("secretFile", ""), o.get("secretEnv", ""))
            return ClientCredentials(o["issuer"], o["clientId"], secret, scope=o.get("scope", ""), audience=o.get("audience", ""))
        if self.token:
            return StaticToken(self.token)
        raise ValueError("config: attest.token or attest.oidc is required")

    def attest(self) -> Attest:
        return Attest(self.attest_url, self.credentials())
