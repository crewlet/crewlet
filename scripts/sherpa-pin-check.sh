#!/bin/sh
#
# sherpa-pin-check.sh: hold the sherpa-onnx release internal/speech pins to upstream's newest.
# .github/workflows/pins.yml runs it every week: no Dependabot ecosystem reads a GitHub release asset.
#
#   scripts/sherpa-pin-check.sh    is internal/speech's SherpaVersion sherpa-onnx's latest release?
#
# The listing's answer is refused unless it is a version of three numbers before it reaches the report.
# Exit status: 0 current; 1 behind, the bump on stdout; 2 cannot tell (a listing refused or answering no
# version, or a pin newer than the listing names). 2 is never a pass.
#
# Environment (optional; scripts/sherpa-pin_test.sh points it at a local file):
#   SHERPA_PIN_LISTING   the latest release's JSON; https://api.github.com/repos/k2-fsa/sherpa-onnx/releases/latest
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
listing=${SHERPA_PIN_LISTING:-https://api.github.com/repos/k2-fsa/sherpa-onnx/releases/latest}
version='^v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*$'

pinned=$(sed -n 's/^const SherpaVersion = "\(.*\)"$/\1/p' "$root/internal/speech/native.go")
if ! printf '%s\n' "$pinned" | grep -q "$version"; then
	echo "internal/speech/native.go names no SherpaVersion of three numbers" >&2
	exit 2
fi

case "$listing" in
file://*) answer=$(curl -fsS "$listing") ;;
*) answer=$(curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 --connect-timeout 30 \
	-H 'Accept: application/vnd.github+json' "$listing") ;;
esac || {
	echo "the release listing at $listing could not be read" >&2
	exit 2
}
latest=$(printf '%s\n' "$answer" | tr ',' '\n' | sed -n 's/^[{[:space:]]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
if ! printf '%s\n' "$latest" | grep -q "$version"; then
	echo "the release listing answered no version of three numbers (${#answer} bytes)" >&2
	exit 2
fi

if [ "$latest" = "$pinned" ]; then
	echo "sherpa-onnx $pinned is the latest release"
	exit 0
fi
if [ "$(printf '%s\n%s\n' "$pinned" "$latest" | sort -V | tail -n 1)" = "$pinned" ]; then
	echo "internal/speech pins sherpa-onnx $pinned, newer than the listing's $latest: a listing that is behind is not upstream" >&2
	exit 2
fi

cat <<REPORT
sherpa-onnx $latest is out; internal/speech pins $pinned.

Bump it in one pull request, \`build(deps): bump sherpa-onnx from $pinned to $latest\`:
- pin the release's four archives in internal/speech/native.go, each by its URL, size and SHA-256 and its two
  members' SHA-256s:
  sherpa-onnx-$latest-linux-x64-shared-no-tts-lib.tar.bz2,
  sherpa-onnx-$latest-linux-aarch64-shared-cpu-lib.tar.bz2,
  sherpa-onnx-$latest-osx-arm64-shared-no-tts-lib.tar.bz2 and
  sherpa-onnx-$latest-osx-x64-shared-no-tts-lib.tar.bz2, and set SherpaVersion;
- re-read the C API's structs (internal/speech/sherpa.go) against the release's c-api.h;
- update the sherpa-onnx and onnxruntime notices under internal/speech/third_party/;
- run \`make test-speech\`, the known answer on the real library.
https://github.com/k2-fsa/sherpa-onnx/releases/tag/$latest
REPORT
exit 1
