package accessdomain

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/secrets"
)

// SEALING: a command's text is stored as ciphertext under the fleet keyring,
// bound to the one record it belongs to. The writer seals before it publishes,
// the applier writes the bytes through (so every node's rows are identical
// whatever keys its ring holds), and only an administrator's audit read opens.

// AAD is the associated data a command's text is sealed under, so a value moved
// to another record does not open.
func AAD(handle, commandID string) string {
	return "access/" + handle + "/command/" + commandID + "/command"
}

// Sealer seals and opens command text under the fleet keyring.
type Sealer struct {
	cipher secrets.Cipher
}

// NewSealer builds one over the fleet keyring. A nil cipher is refused: it
// would write production commands in the clear into every copy and backup.
func NewSealer(cipher secrets.Cipher) (*Sealer, error) {
	if cipher == nil {
		return nil, errors.New("accessdomain: a sealer with no keyring would write every " +
			"command in the clear into the log, every node's database and every backup")
	}
	if _, err := cipher.Encrypt("", "access/sealer/probe"); err != nil {
		return nil, fmt.Errorf("accessdomain: the keyring cannot seal: %w", err)
	}
	return &Sealer{cipher: cipher}, nil
}

// Seal seals plain under aad, answering the envelope and the key it names.
func (s *Sealer) Seal(plain, aad string) (sealed, key string, err error) {
	sealed, err = s.cipher.Encrypt(plain, aad)
	if err != nil {
		return "", "", fmt.Errorf("accessdomain: seal a command: %w", err)
	}
	key, ok := secrets.EnvelopeKeyID(sealed)
	if !ok {
		return "", "", errors.New("accessdomain: the keyring sealed a value that names no key")
	}
	return sealed, key, nil
}

// Open opens a value sealed under aad; anything that does not is [ErrUnreadable].
func (s *Sealer) Open(sealed, aad string) (string, error) {
	if !secrets.IsEnvelope(sealed) {
		return "", fmt.Errorf("%w: it is not an envelope", ErrUnreadable)
	}
	plain, err := s.cipher.Decrypt(sealed, aad)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	return plain, nil
}
