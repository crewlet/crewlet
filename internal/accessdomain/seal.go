package accessdomain

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/secrets"
)

// SEALING: a command's text, and what a verdict decided with its giver's
// reason and findings, are stored as ciphertext under the fleet keyring, bound
// to the one record they belong to. The writer seals before it publishes, the
// applier writes the bytes through (so every node's rows are identical whatever
// keys its ring holds), and only an administrator's audit read opens.

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

// The places a verdict's two texts are sealed to: what it decided, and its
// giver's reason and findings.
const (
	verdictSubjectPart = "subject"
	verdictWordsPart   = "words"
)

// verdictAAD is the associated data one of a verdict's texts is sealed under:
// its record and its place in it, so neither opens moved to another record or
// swapped for the other.
func verdictAAD(handle, verdictID, part string) string {
	return "access/" + handle + "/verdict/" + verdictID + "/" + part
}

// SealedVerdict is a verdict's two texts sealed to its record, and the keyring
// key both are sealed under: what crosses between nodes in place of the text.
type SealedVerdict struct {
	Subject string
	Words   string
	Key     string
}

// verdictWords is a giver's reason and findings as one sealed value.
type verdictWords struct {
	Reason   string   `json:"reason,omitempty"`
	Findings []string `json:"findings,omitempty"`
}

// SealVerdict seals what seat handle's verdict verdictID decided, and its
// giver's reason and findings, to the record, under one key.
func (s *Sealer) SealVerdict(handle, verdictID, subject, reason string,
	findings []string) (SealedVerdict, error) {

	words, err := json.Marshal(verdictWords{Reason: reason, Findings: findings})
	if err != nil {
		return SealedVerdict{}, fmt.Errorf("accessdomain: encode a verdict's words: %w", err)
	}
	sealedSubject, key, err := s.Seal(subject, verdictAAD(handle, verdictID, verdictSubjectPart))
	if err != nil {
		return SealedVerdict{}, err
	}
	sealedWords, wordsKey, err := s.Seal(string(words), verdictAAD(handle, verdictID, verdictWordsPart))
	if err != nil {
		return SealedVerdict{}, err
	}
	if wordsKey != key {
		return SealedVerdict{}, errors.New("accessdomain: the keyring moved its active key between " +
			"a verdict's two texts; seal it again")
	}
	return SealedVerdict{Subject: sealedSubject, Words: sealedWords, Key: key}, nil
}

// openVerdict opens a verdict's two texts sealed to seat handle's verdictID.
func (s *Sealer) openVerdict(handle, verdictID, subjectSealed, wordsSealed string) (
	subject, reason string, findings []string, err error) {

	subject, err = s.Open(subjectSealed, verdictAAD(handle, verdictID, verdictSubjectPart))
	if err != nil {
		return "", "", nil, err
	}
	raw, err := s.Open(wordsSealed, verdictAAD(handle, verdictID, verdictWordsPart))
	if err != nil {
		return "", "", nil, err
	}
	var words verdictWords
	if err := json.Unmarshal([]byte(raw), &words); err != nil {
		return "", "", nil, fmt.Errorf("%w: a verdict's words are not the record's: %w", ErrUnreadable, err)
	}
	return subject, words.Reason, words.Findings, nil
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
