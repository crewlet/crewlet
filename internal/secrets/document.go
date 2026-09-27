package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
)

// EnvelopeKey is the single field a sealed document is wrapped in.
//
// The WHOLE document is sealed as one opaque blob rather than field by field.
// Per-field sealing looks tidier and leaks the shape: which integrations an
// operator runs, what their settings are, and how much of the surface they
// have configured are all structure, and structure is what a config document
// mostly is.
//
// # What this argument no longer covers, and why
//
// It used to say "their org chart, their role names and how many seats they
// have" too, and that half left with the chart. The chart is a LOG now — one
// record per object, arbitrated at the broker on a subject that IS a unit key
// or a seat handle — so its shape is on the wire whatever this package does,
// and sealing it as a document would have put every writer back on one lock
// and undone the reason the domain exists.
//
// The trade it makes instead is stated where it is made, in
// [internal/chart]'s own sealing: the STRUCTURE is plaintext, a secret-tagged
// VALUE is never plaintext, and a person's own fields ride under a key that
// can be deleted. What is left here is the settings document, for which this
// argument holds unchanged.
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
//
// IT STATES THE FACT AND NAMES NO REMEDY, because the remedy depends on which
// document it is and only the caller knows that. This node's ACTIVE revision
// is sealed by `crewlet config seal`, which seals the active revision and
// nothing else; a SUPERSEDED revision is sealed in place by nothing, and comes
// back only by importing its document again; a body a PEER published is fixed
// on that peer; a STAGED CHART is fixed by re-running the import against a
// running node. It used to name `crewlet config seal` for all of them, so
// every reader but the first carried a remedy that did nothing.
var ErrUnsealedWithKey = errors.New("secrets: this document is stored unsealed, " +
	"and a node holding a keyring reads only sealed ones — a plaintext payload is " +
	"one anything that can write the store or the coordination bucket could have " +
	"authored")

// Sealed reports whether a stored payload is a sealed envelope.
//
// Structural rather than a guess at the content: the envelope is a
// single-field object, so a plaintext document that happens to contain the
// string cannot be mistaken for one.
func Sealed(payload []byte) bool {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return false
	}
	if len(envelope) != 1 {
		return false
	}
	raw, present := envelope[EnvelopeKey]
	if !present {
		return false
	}
	var token string
	return json.Unmarshal(raw, &token) == nil && IsEnvelope(token)
}

// EnvelopeKeyIDOf reports which keyring key sealed a stored payload.
//
// FALSE FOR A PLAINTEXT DOCUMENT, which is the same question asked of a
// payload rather than of a bare envelope string: a rotation sweep has to tell
// "sealed under a stale key" from "not sealed at all", and the two need
// opposite commands (`config rekey` and `config seal`). Reading the id
// without decrypting is what makes a dry run safe to print.
func EnvelopeKeyIDOf(payload []byte) (string, bool) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", false
	}
	raw, present := envelope[EnvelopeKey]
	if len(envelope) != 1 || !present {
		return "", false
	}
	var token string
	if err := json.Unmarshal(raw, &token); err != nil {
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
// ONLY A SEALED PAYLOAD OPENS, and only under a keyring. A payload that is not
// sealed is refused with [ErrUnsealedWithKey]: the seal is what authenticates
// a document a peer published, and one without it could have been written by
// anything that reaches the coordination store. It used to come back
// verbatim, so a node holding a keyring applied a forged plaintext document as
// readily as its own.
//
// A NIL CIPHER IS REFUSED ([ErrNoKeyring]) whatever the payload is. It used to
// read a plaintext payload verbatim — the same forged document, for any caller
// wired without the node's keyring — and to refuse a sealed one with an error
// of its own; neither is a posture a node can be in, since every node holds a
// keyring and the engine refuses to start without one. Refusing, rather than
// answering an empty document, is what keeps a caller built that way from
// booting onto an empty company, which reads on every surface as an operator
// who has configured nothing.
//
// The one reader that must take a plaintext payload with a keyring in hand is
// the migration that seals it, and it says so by calling [OpenToReseal].
func Open(cipher Cipher, payload []byte) ([]byte, error) {
	if cipher == nil {
		return nil, fmt.Errorf("secrets: open document: %w", ErrNoKeyring)
	}
	if !Sealed(payload) {
		return nil, ErrUnsealedWithKey
	}
	return openSealed(cipher, payload)
}

// OpenToReseal opens a payload a caller is about to write back SEALED —
// sealed or not.
//
// THE MIGRATION'S READ, and nothing else's. A store written before the
// keyring was required can hold plaintext revisions, and the commands that
// rewrite them sealed (`crewlet config seal`, and `crewlet config scrub`,
// whose erasure must reach a plaintext revision as surely as a sealed one)
// have to be able to read what they are replacing. Every other reader goes
// through [Open], which refuses a plaintext payload; a caller that used this
// to READ a document would be accepting exactly the unauthenticated body
// [ErrUnsealedWithKey] exists to refuse, which is why it takes the keyring it
// will seal with and refuses to run without one.
func OpenToReseal(cipher Cipher, payload []byte) ([]byte, error) {
	if cipher == nil {
		return nil, fmt.Errorf("secrets: a payload is opened to be re-sealed "+
			"only by a caller holding the keyring it will seal under: %w", ErrNoKeyring)
	}
	if !Sealed(payload) {
		return payload, nil
	}
	return openSealed(cipher, payload)
}

// openSealed decrypts an envelope [Sealed] has already recognised.
func openSealed(cipher Cipher, payload []byte) ([]byte, error) {
	var envelope map[string]string
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("secrets: open document: %w", err)
	}
	document, err := cipher.Decrypt(envelope[EnvelopeKey], AADForDocument)
	if err != nil {
		return nil, fmt.Errorf("secrets: open document: %w", err)
	}
	return []byte(document), nil
}
