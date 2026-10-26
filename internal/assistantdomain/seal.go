package assistantdomain

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/secrets"
)

// SEALING: what a conversation says is stored as ciphertext, bound to the one
// record it belongs to.
//
// # Why every value is sealed
//
// The replicated estate is not encrypted at rest, and it rides every snapshot,
// every catch-up and every backup of every data node. What a person read
// through the assistant (a message, a card's rows, a proposal's body, a parked
// turn) is a copy of data they were entitled to see, so in the clear it would
// be a second way to read it for anybody holding a node's disk or a backup.
// Every such value is sealed under the FLEET KEYRING (internal/secrets'
// AES-256-GCM), the one every node already holds for the identity estate's
// sealed values: one envelope format and one rotation story, never a second
// key hierarchy.
//
// # Why the associated data names the record
//
// Under one keyring every envelope opens under every key id the ring holds, so
// a value lifted out of one row and pasted into another would open there. The
// associated data is `assistant/<conversation>/<kind>/<record>/<column>`, so a
// value opens only as the one column of the one record of the one conversation
// it was sealed for ([AAD]): moved to another conversation, another record or
// another column, it does not open at all.
//
// # The writer seals and the applier never opens
//
// Every value is sealed by the [Writer] before it is published and written
// through by the [Applier] as the bytes the record carries, so every node
// writes identical rows whatever keys its ring holds, and the identity claim
// holds across them. Only the [Reader] opens a value, on the node serving the
// person's request.
//
// # Rotation
//
// A value sealed under a key opens for as long as the ring holds that key, and
// leaves with its conversation. So a key may be retired from the ring once no
// value is sealed under it any more, which [Reader.SealedKeys] counts and the
// secret store's listing reports (`assistant_values`); a key dropped sooner
// leaves the values still sealed under it unreadable, which a read reports
// ([ErrUnreadable]) rather than hides.

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

// NewSealer builds one over the fleet keyring.
//
// A NIL CIPHER IS REFUSED rather than read as "seal nothing": a sealer that
// passed values through would write what people asked and were shown in the
// clear into the log, every node's database, every snapshot and every backup,
// and no node runs without a keyring, so a nil here is a wiring mistake.
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

// Seal seals plain under aad. AN EMPTY VALUE STAYS EMPTY, so an absent title
// or page chip costs no envelope; every other value is an envelope.
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
