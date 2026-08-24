// Package whsec is the Standard Webhooks signing-secret format, and the one
// place that decides what a valid one is.
//
// A secret is `whsec_` followed by STANDARD base64 (padded) over a 32-byte
// key, and the HMAC key is those DECODED bytes rather than the printable
// form. GitLab's own API says the same thing — "Must be in whsec_<base64>
// format encoding a 32-byte key" — and 19.1 is the first release that
// accepts one.
//
// # Why this is a package rather than a helper beside its caller
//
// Three places need the rule and none of them may disagree: config refuses a
// literal that is not one, the webhook edge refuses a resolved value that is
// not one, and the engine refuses to start an integration whose ${VAR}
// resolved to something else. Written twice, it drifted the first time —
// the validator required 32 bytes and the verifier took a payload of any
// length, so a ${VAR} holding a 16-byte key was refused as a literal and
// silently accepted as a reference, verifying every delivery against a
// weaker key than the operator believed they had configured.
//
// Config sits at the bottom of the engine's import graph and cannot reach
// the vendor package, so the rule lives here, where all three can.
//
// # Getting the encoding wrong is silent
//
// A URL-safe payload usually fails a standard decode. An earlier verifier
// fell back to keying on the printable string verbatim, so a hand-written
// secret "still worked" — while GitLab kept keying on the decoded bytes.
// Every delivery was then refused with a signature mismatch and nothing
// anywhere named the encoding. That fallback is gone: a secret this package
// cannot decode is a secret nothing can verify with, and saying so is the
// only useful answer.
package whsec

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// Prefix marks a Standard Webhooks signing secret.
const Prefix = "whsec_"

// KeyBytes is the key length the signature scheme is specified around.
//
// 32 bytes is the HMAC-SHA256 block-equivalent strength the signature gets
// its security from: more is not stronger, and less is the only way to get
// this wrong.
const KeyBytes = 32

// Key decodes a signing secret to its raw HMAC key, reporting whether the
// secret is one the vendor could have produced.
//
// Three-valued in effect rather than two: a caller distinguishes "no secret
// configured" (the empty string, which is not this function's business) from
// "a secret that cannot be a key", and the second must never be treated as
// the first — an unusable secret that fell through to "unconfigured" would
// have a route answer 503 forever while the operator's config looks set.
func Key(secret string) ([]byte, bool) {
	payload, ok := strings.CutPrefix(secret, Prefix)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(raw) != KeyBytes {
		return nil, false
	}
	return raw, true
}

// Valid reports whether a secret is a usable signing key.
func Valid(secret string) bool {
	_, ok := Key(secret)
	return ok
}

// Mint generates a signing secret.
func Mint() (string, error) {
	raw := make([]byte, KeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("whsec: generate a signing secret: %w", err)
	}
	return Prefix + base64.StdEncoding.EncodeToString(raw), nil
}
