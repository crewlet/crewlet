package opkey

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
)

// Canonical is the ONE encoding of a request that something is bound to: its
// path, its query encoded with sorted keys, and its decoded body re-encoded,
// each followed by a NUL. The same fields in another order or spacing are the
// same request, and any change to the object, the query or the arguments is
// another one.
//
// # One encoding for every binding (ADR-0008)
//
// Two things bind themselves to a request. [Digest] steps an operation key by
// it, so a key sent with another request is another operation; and the
// Dashboard Assistant binds an approval to the exact request its preview
// showed (ADR-0051), so a request that moved after the person approved it is
// refused rather than executed. Encoded twice, the two would agree only until
// one of them was edited, and the second is a security property. asks is the
// decoded body, nil for a route that takes none; an error is a body that
// decoded and will not encode, which is the caller's type declared wrongly.
func Canonical(path string, query url.Values, asks any) ([]byte, error) {
	body, err := json.Marshal(asks)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for _, part := range [][]byte{[]byte(path), []byte(query.Encode()), body} {
		b.Write(part)
		b.WriteByte(0)
	}
	return b.Bytes(), nil
}

// RequestDigest is the full SHA-256 of a request's method and its [Canonical]
// encoding, as 64 lowercase hex digits under a domain of its own, so it never
// equals an operation's digest of the same request.
//
// FULL LENGTH, unlike [Digest]'s sixteen digits: [Digest] only tells an
// accidental change of request apart, while this binds an approval to what was
// approved, and a shortened binding is one a party choosing the request could
// search for a collision with.
func RequestDigest(method, path string, query url.Values, asks any) (string, error) {
	canon, err := Canonical(path, query, asks)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(requestDomain), []byte(method)} {
		_, _ = h.Write(part)
		_, _ = h.Write([]byte{0})
	}
	_, _ = h.Write(canon)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RequestDigestOf is [RequestDigest] over a request as it travels: its body
// raw, decoded here with encoding/json into a plain value (numbers as float64,
// never json.Number) before it is encoded again.
//
// THE ONE DECODE, so the party that binds a request and the party that checks
// it can never decode a body two ways: the assistant's dispatcher and the
// request guard both hand this the bytes they hold, and an empty body is the
// absent one.
func RequestDigestOf(method string, u *url.URL, raw []byte) (string, error) {
	var asks any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &asks); err != nil {
			return "", fmt.Errorf("opkey: the body is not one JSON value: %w", err)
		}
	}
	return RequestDigest(method, u.Path, u.Query(), asks)
}

// requestDomain separates [RequestDigest] from every other digest of a
// request.
const requestDomain = "crewlet.opkey.request.v1"
