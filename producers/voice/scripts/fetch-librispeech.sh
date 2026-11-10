#!/usr/bin/env bash
# LibriSpeech dev-clean (CC BY 4.0, ~340 MB) into $LIBRISPEECH_DIR
# (default ~/.cache/librispeech). Used for calibration trials and the demo.
set -euo pipefail
DIR=${LIBRISPEECH_DIR:-$HOME/.cache/librispeech}
mkdir -p "$DIR"
cd "$DIR"
[ -d LibriSpeech/dev-clean ] && { echo "already present: $DIR/LibriSpeech/dev-clean"; exit 0; }
curl -fSL https://www.openslr.org/resources/12/dev-clean.tar.gz | tar xz
echo "$DIR/LibriSpeech/dev-clean"
