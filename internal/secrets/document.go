package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// EnvelopeKey is the single field a sealed document is wrapped in.
//
// The WHOLE document is sealed as one opaque blob rather than field by field.
// Per-field sealing looks tidier and leaks the shape: an operator's org chart,
// their role names, which integrations they run and how many seats they have
// are all structure, and structure is what a config document mostly is.
const EnvelopeKey = "__encrypted__"

// ErrUnsealedWithKey reports a payload that is NOT sealed, read by a caller
// that holds a keyring.
//
// THE SEAL IS WHAT AUTHENTICATES A DOCUMENT, not only what hides it: the
// company document a peer applies arrives through the coordination store,
// and anything that can write to the broker can write that bucket. Sealed
// under the fleet's keyring (AES-GCM), a forged body fails to open; a
// plaintext one would open as whatever its author wrote. So a node holding a
// keyring — which is every node — reads only sealed payloads, and a plaintext
// one is refused rather than applied, adopted or shown.
var ErrUnsealedWithKey = errors.New("secrets: this document is stored unsealed, " +
	"and a node holding a keyring reads only sealed ones — a plaintext payload is " +
	"one anything that can write the store or the coordination bucket could have " +
	"authored")

// ErrUnknownEnvelope reports a payload wrapped as a sealed document (a JSON
// object whose one member is [EnvelopeKey]) whose seal is not in the form this
// build reads: a token that does not begin with [EnvelopePrefix], or that member
// holding something other than a token.
//
// NOT [ErrUnsealedWithKey]: a write may replace a payload with no seal at all,
// having read nothing from it, but one wrapped as a sealed document is most
// likely a NEWER build's ([EnvelopePrefix] carries the format's version), so
// this build can neither read it nor call it plaintext; replacing it discards
// what a newer peer wrote. Leave it; the remedy is the build that wrote it.
// A token in THIS build's form that does not parse is damaged, so [ErrDecrypt].
var ErrUnknownEnvelope = errors.New("secrets: this document is sealed in a " +
	"form this build does not read")

// Sealed reports whether a stored payload is a sealed envelope in the form this
// build reads ([CheckSealed] says what it is when it is not).
//
// Structural rather than a guess at the content: the envelope is a
// single-field object, so a plaintext document that happens to contain the
// string cannot be mistaken for one.
func Sealed(payload []byte) bool { return CheckSealed(payload) == nil }

// CheckSealed reports whether a stored payload is sealed in the form this
// build reads, without opening it: nil when it is, [ErrUnsealedWithKey] when
// it is not wrapped as a sealed document at all, [ErrUnknownEnvelope] when it
// is wrapped in a form this build does not read, and [ErrDecrypt] when it is
// in this build's form and damaged, which no key opens.
//
// THE THREE ANSWERS [Open] gives before it decrypts, through the same reading
// ([sealOf]), for a caller that has to tell them apart without a cipher in
// hand: a rotation sweep finds what to re-seal by the key id alone, and has to
// refuse a revision it cannot read for the reason it cannot.
func CheckSealed(payload []byte) error {
	token, err := sealOf(payload)
	switch {
	case err != nil:
		return err
	case !IsEnvelope(token):
		return ErrDecrypt
	}
	return nil
}

// sealOf is THE reading of a stored payload's seal, which [Open] and
// [CheckSealed] both go through: the token a sealed document holds when it is
// in this build's form ([EnvelopePrefix]), [ErrUnsealedWithKey] when the
// payload is not wrapped as a sealed document, and [ErrUnknownEnvelope] when it
// is wrapped and its seal is in another form. The token may still be damaged;
// what decides that is the parse ([IsEnvelope], or the cipher's own).
//
// WRAPPED MEANS THE SHAPE, whatever the member holds: one JSON object whose one
// member is [EnvelopeKey]. No company document has that shape, and a successor
// that sealed into it some other way (an object rather than a string, another
// prefix) has written a sealed document, which this build must not read as an
// unsealed one ([ErrUnknownEnvelope] says why).
func sealOf(payload []byte) (string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope) != 1 {
		return "", ErrUnsealedWithKey
	}
	raw, present := envelope[EnvelopeKey]
	if !present {
		return "", ErrUnsealedWithKey
	}
	var token string
	if json.Unmarshal(raw, &token) != nil || !strings.HasPrefix(token, EnvelopePrefix) {
		return "", ErrUnknownEnvelope
	}
	return token, nil
}

// EnvelopeKeyIDOf reports which keyring key sealed a stored payload.
//
// FALSE FOR ANYTHING BUT A SEAL THIS BUILD READS, which is the same question
// asked of a payload rather than of a bare envelope string: a plaintext
// document, and one sealed in a form this build does not read, have no key id
// it could name. A rotation sweep has to tell "sealed under a stale key" from
// those two, and only the first has a key to rotate; [CheckSealed] says which
// of the others a payload is. Reading the id without decrypting is what makes
// a dry run safe to print.
func EnvelopeKeyIDOf(payload []byte) (string, bool) {
	token, err := sealOf(payload)
	if err != nil {
		return "", false
	}
	return EnvelopeKeyID(token)
}

// Seal wraps a document as a sealed envelope.
//
// A NIL CIPHER IS REFUSED ([ErrNoKeyring]). It used to store the document as
// it was, for a caller holding no keyring — which no node is: every node's
// Tier A carries one and the engine refuses to start without it. So a nil
// here is a caller wired without the node's keyring, and what it wrote was a
// revision every node refuses to apply; the /config surface built that way
// answered 201 to writes the fleet never ran.
func Seal(cipher Cipher, document []byte) ([]byte, error) {
	if cipher == nil {
		return nil, fmt.Errorf("secrets: seal document: %w", ErrNoKeyring)
	}
	// Idempotent: re-sealing an already-sealed document would nest one
	// envelope inside another, and the outer one would open to something
	// no config parser has ever seen.
	if Sealed(document) {
		return document, nil
	}
	token, err := cipher.Encrypt(string(document), AADForDocument)
	if err != nil {
		return nil, fmt.Errorf("secrets: seal document: %w", err)
	}
	sealed, err := json.Marshal(map[string]string{EnvelopeKey: token})
	if err != nil {
		return nil, fmt.Errorf("secrets: seal document: %w", err)
	}
	return sealed, nil
}

// Open unwraps a stored payload, returning the document.
//
// ONLY A SEALED PAYLOAD OPENS, and only under a keyring. An unsealed one is
// refused ([ErrUnsealedWithKey]): the seal authenticates a document a peer
// published, and anything that reaches the coordination store could write one
// without it. A seal in a form this build does not read is [ErrUnknownEnvelope].
//
// A NIL CIPHER IS REFUSED ([ErrNoKeyring]) whatever the payload is: every node
// holds a keyring. Answering an empty document instead would boot a caller onto
// an empty company, which reads as an operator who has configured nothing.
func Open(cipher Cipher, payload []byte) ([]byte, error) {
	if cipher == nil {
		return nil, fmt.Errorf("secrets: open document: %w", ErrNoKeyring)
	}
	token, err := sealOf(payload)
	if err != nil {
		return nil, err
	}
	document, err := cipher.Decrypt(token, AADForDocument)
	if err != nil {
		return nil, fmt.Errorf("secrets: open document: %w", err)
	}
	return []byte(document), nil
}
