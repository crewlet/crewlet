package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
)

// EnvelopeKey is the single field a sealed document is wrapped in.
//
// The WHOLE document is sealed as one opaque blob rather than field by field.
// Per-field sealing looks tidier and leaks the shape: an operator's org chart,
// their role names, which integrations they run and how many seats they have
// are all structure, and structure is what a config document mostly is.
const EnvelopeKey = "__encrypted__"

// ErrSealedWithoutKey reports a sealed document and no keyring to open it.
//
// Distinct from a decrypt failure on purpose: the fix is different. A missing
// keyring is a deployment that lost its root of trust — the bootstrap config
// no longer names the key that sealed this — where a decrypt failure is a
// document that does not belong to the key it was offered.
var ErrSealedWithoutKey = errors.New("secrets: document is sealed and no keyring is configured")

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

// Seal wraps a document as a sealed envelope.
//
// A nil cipher stores the document as it is. That is the documented opt-out —
// a deployment with no keyring in Tier A stores plaintext — and it is not the
// same as a failure: refusing here would make the first run of an
// unconfigured-keyring deployment impossible rather than merely unsealed.
func Seal(cipher Cipher, document []byte) ([]byte, error) {
	if cipher == nil {
		return document, nil
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
// A payload that is not sealed comes back verbatim, so a store written before
// a keyring was configured keeps reading. A payload that IS sealed with no
// cipher to open it is an error rather than an empty document: silently
// returning nothing would boot the node onto an empty company, which reads on
// every surface as an operator who has configured nothing.
func Open(cipher Cipher, payload []byte) ([]byte, error) {
	if !Sealed(payload) {
		return payload, nil
	}
	if cipher == nil {
		return nil, ErrSealedWithoutKey
	}
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
