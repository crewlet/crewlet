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
	// active is the keyring key the cipher seals under.
	active string
}

// NewSealer builds one over the fleet keyring. A nil cipher is refused: it
// would write production commands in the clear into every copy and backup.
func NewSealer(cipher secrets.Cipher) (*Sealer, error) {
	if cipher == nil {
		return nil, errors.New("accessdomain: a sealer with no keyring would write every " +
			"command in the clear into the log, every node's database and every backup")
	}
	probe, err := cipher.Encrypt("", "access/sealer/probe")
	if err != nil {
		return nil, fmt.Errorf("accessdomain: the keyring cannot seal: %w", err)
	}
	active, ok := secrets.EnvelopeKeyID(probe)
	if !ok {
		return nil, errors.New("accessdomain: the keyring sealed a value that names no key")
	}
	return &Sealer{cipher: cipher, active: active}, nil
}

// SealedCommand is a command's text sealed to its record, and the keyring key
// it is sealed under: what crosses between nodes in place of the text.
type SealedCommand struct {
	Sealed string
	Key    string
}

// SealCommand seals the text of seat handle's command id to its record.
func (s *Sealer) SealCommand(handle, commandID, text string) (SealedCommand, error) {
	sealed, key, err := s.Seal(text, AAD(handle, commandID))
	if err != nil {
		return SealedCommand{}, err
	}
	return SealedCommand{Sealed: sealed, Key: key}, nil
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
