#!/bin/sh
#
# sherpa-pin_test.sh: the own suite of scripts/sherpa-pin-check.sh, against stand-in release listings under file://,
# so it reads no network. Each case names the exit status and what the output must say.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
pinned=$(sed -n 's/^const SherpaVersion = "\(.*\)"$/\1/p' "$root/internal/speech/native.go")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
failed=0

# check NAME WANT-STATUS WANT-TEXT JSON: run the check against JSON as the listing.
check() {
	printf '%s' "$4" >"$work/$1.json"
	status=0
	SHERPA_PIN_LISTING="file://$work/$1.json" "$root/scripts/sherpa-pin-check.sh" >"$work/$1.out" 2>&1 || status=$?
	if [ "$status" -ne "$2" ] || ! grep -q -- "$3" "$work/$1.out"; then
		echo "FAIL $1: exit $status, want $2 and \"$3\" in:" >&2
		cat "$work/$1.out" >&2
		failed=1
	else
		echo "ok   $1"
	fi
}

check current 0 "is the latest release" "{\"url\":\"x\",\"tag_name\":\"$pinned\",\"name\":\"$pinned\"}"
check behind 1 "sherpa-onnx v99.0.0 is out" '{"tag_name":"v99.0.0","assets":[]}'
check behind-names-assets 1 "sherpa-onnx-v99.0.0-linux-aarch64-shared-cpu-lib.tar.bz2" '{"tag_name":"v99.0.0"}'
check pretty 1 "v99.1.2 is out" '{
  "tag_name": "v99.1.2",
  "draft": false
}'
check listing-behind 2 "newer than the listing" '{"tag_name":"v0.0.1"}'
check no-version 2 "no version of three numbers" '{"message":"Not Found"}'
check not-a-version 2 "no version of three numbers" '{"tag_name":"asr-models"}'
check empty 2 "no version of three numbers" ''
status=0
SHERPA_PIN_LISTING="file://$work/missing.json" "$root/scripts/sherpa-pin-check.sh" >"$work/missing.out" 2>&1 || status=$?
if [ "$status" -ne 2 ] || ! grep -q "could not be read" "$work/missing.out"; then
	echo "FAIL refused: exit $status, want 2 and \"could not be read\"" >&2
	failed=1
else
	echo "ok   refused"
fi
exit "$failed"
