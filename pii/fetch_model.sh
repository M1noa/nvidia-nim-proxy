#!/bin/sh
# fetch the rampart ner bundle (model + vocab) into pii/model/.
# cc by 4.0, see NOTICE. needs: curl or python3 with huggingface_hub.
set -e
cd "$(dirname "$0")"
REV="b1993e4e68b082835b80ffc65acc03325ea2e501"
BASE="https://huggingface.co/nationaldesignstudio/rampart/resolve/$REV"
mkdir -p model
if [ ! -f model/model_q4.onnx ]; then
  echo "fetching model_q4.onnx (14.7mb)..."
  curl -sL -o model/model_q4.onnx "$BASE/onnx/model_q4.onnx"
fi
if [ ! -f model/vocab.txt ]; then
  echo "fetching vocab.txt..."
  curl -sL -o model/vocab.txt "$BASE/vocab.txt"
fi
ls -la model/
