package assistantdomain

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/secrets"
)

// SEALING: what a conversation says is stored as ciphertext under the fleet
// keyring (package doc, "Sealed, bounded, and gone in thirty days"), never a
// second key hierarchy.
//
// The associated data ([AAD]) names conversation, record kind, record and
// column, because every envelope opens under every key the ring holds: a value
// pasted into another row must not open there. The [Writer] seals and the
// [Applier] writes the bytes through unopened, so every node writes identical
// rows; only the [Reader] opens. A key may leave the ring once
// [Reader.SealedKeys] counts nothing under it (the secret store's listing
// reports `assistant_values`); dropped sooner, a read reports [ErrUnreadable].

// RecordKind names what a sealed value belongs to in its associated data.
type RecordKind string

// The record kinds a value is sealed for.
const (
	RecordConversation RecordKind = "conversation"
	RecordTurn         RecordKind = "turn"
	RecordMessage      RecordKind = "message"
	RecordStep         RecordKind = "step"
	RecordCard         RecordKind = "card"
	RecordProposal     RecordKind = "proposal"
	RecordParked       RecordKind = "parked"
	RecordAuditRead    RecordKind = "audit_read"
	RecordFile         RecordKind = "file"
)

// The columns a value is sealed for.
const (
	ColumnTitle     = "title"
	ColumnSummary   = "summary"
	ColumnText      = "text"
	ColumnPageTitle = "page_title"
	ColumnBody      = "body"
	ColumnQuery     = "query"
	ColumnCard      = "card"
	ColumnReceipt   = "receipt"
	ColumnMessages  = "messages"
	ColumnReason    = "reason"
	ColumnName      = "name"
	// ColumnAttachments is a message's attachments, and ColumnContent a
	// file's bytes, which are sealed into the OBJECT rather than a row.
	ColumnAttachments = "attachments"
	ColumnContent     = "content"
)

// AAD is the associated data one value is sealed under: the conversation, the
// kind of record, the record and the column.
func AAD(conversation uuid.UUID, kind RecordKind, record uuid.UUID, column string) string {
	return "assistant/" + conversation.String() + "/" + string(kind) + "/" +
		record.String() + "/" + column
}

// ErrUnreadable reports a stored value this node's keyring does not open: its
// key was dropped from the ring before the value aged out, or the value was
// moved under a record it was not sealed for.
var ErrUnreadable = errors.New("assistantdomain: a stored value does not open " +
	"under this node's keyring")

// Sealer seals and opens the assistant's values under the fleet keyring.
type Sealer struct {
	cipher secrets.Cipher
}

// NewSealer builds one over the fleet keyring. A nil cipher is refused, never
// read as "seal nothing": no node runs without a keyring, so nil is a wiring
// mistake that would write conversations in the clear.
func NewSealer(cipher secrets.Cipher) (*Sealer, error) {
	if cipher == nil {
		return nil, errors.New("assistantdomain: a sealer with no keyring would " +
			"write every conversation in the clear into the log, every node's " +
			"database, every snapshot and every backup")
	}
	if _, err := cipher.Encrypt("", "assistant/sealer/probe"); err != nil {
		return nil, fmt.Errorf("assistantdomain: the keyring cannot seal: %w", err)
	}
	return &Sealer{cipher: cipher}, nil
}

// Seal seals plain under aad. An empty value stays empty, so an absent title
// or page chip costs no envelope.
func (s *Sealer) Seal(plain []byte, aad string) (string, error) {
	if len(plain) == 0 {
		return "", nil
	}
	sealed, err := s.cipher.Encrypt(string(plain), aad)
	if err != nil {
		return "", fmt.Errorf("assistantdomain: seal a value: %w", err)
	}
	return sealed, nil
}

// Open opens a value sealed under aad. The empty string opens as empty;
// anything that is not an envelope opening under aad is [ErrUnreadable].
func (s *Sealer) Open(sealed, aad string) ([]byte, error) {
	if sealed == "" {
		return nil, nil
	}
	if !secrets.IsEnvelope(sealed) {
		return nil, fmt.Errorf("%w: it is not an envelope", ErrUnreadable)
	}
	plain, err := s.cipher.Decrypt(sealed, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	return []byte(plain), nil
}

// sealString is [Sealer.Seal] for a text value.
func (s *Sealer) sealString(plain, aad string) (string, error) {
	return s.Seal([]byte(plain), aad)
}

// openString is [Sealer.Open] for a text value.
func (s *Sealer) openString(sealed, aad string) (string, error) {
	plain, err := s.Open(sealed, aad)
	return string(plain), err
}
