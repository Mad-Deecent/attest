#!/usr/bin/env bash
# Download the sherpa-onnx models into $MODELS (default ~/.cache/sherpa-onnx-models).
set -euo pipefail
MODELS=${MODELS:-$HOME/.cache/sherpa-onnx-models}
B=https://github.com/k2-fsa/sherpa-onnx/releases/download
mkdir -p "$MODELS"
cd "$MODELS"
get() { [ -e "$2" ] || { echo "fetching $2" >&2; curl -fsSL "$B/$1" -o "$2.part" && mv "$2.part" "$2"; }; }
untar() { [ -d "$2" ] || { echo "fetching $2" >&2; curl -fsSL "$B/$1/$2.tar.bz2" | tar xj; }; }

get asr-models/silero_vad.onnx silero_vad.onnx
get speaker-recongition-models/3dspeaker_speech_eres2net_sv_en_voxceleb_16k.onnx 3dspeaker_speech_eres2net_sv_en_voxceleb_16k.onnx
untar speaker-segmentation-models sherpa-onnx-pyannote-segmentation-3-0
untar asr-models sherpa-onnx-nemo-parakeet_tdt_transducer_110m-en-36000-int8
du -sh "$MODELS"
