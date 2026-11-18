#!/usr/bin/env bash
# Builds librespot.aar (+ librespot-sources.jar) from ./mobilebind with gomobile.
# minSdk 24 (Android 7.0), ARM only; symbols stripped to keep the AAR small.
#
# Requires gomobile/gobind on PATH and an Android SDK + NDK. Override the
# defaults with ANDROID_HOME / ANDROID_NDK_HOME.
set -euo pipefail

cd "$(dirname "$0")"

export ANDROID_HOME="${ANDROID_HOME:-$HOME/Library/Android/sdk}"
if [ -z "${ANDROID_NDK_HOME:-}" ]; then
  ANDROID_NDK_HOME="$(ls -d "$ANDROID_HOME"/ndk/* | sort -V | tail -n1)"
  export ANDROID_NDK_HOME
fi

gomobile bind \
  -androidapi 24 \
  -target android/arm64,android/arm \
  -ldflags="-s -w" \
  -o librespot.aar \
  ./mobilebind
