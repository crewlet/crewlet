package assistantdomain

import (
	"time"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// Retention is how long a person's conversation is kept after its last
// activity: thirty days, as long as a turn's detail and the trash keep theirs,
// since a transcript holds whatever the person's reads returned (ADR-0053).
const Retention = 30 * 24 * time.Hour

// THE BOUNDS ON WHAT IS STORED live here, where the writer refuses a value
// before it is sealed, and internal/assistant reads them rather than stating
// its own: a bound written twice drifts, and the losing side is a record the
// log refuses `record_too_large` after a turn already showed it.
const (
	// TitleMaxBytes bounds a conversation's title.
	TitleMaxBytes = 256
	// SummaryMaxBytes bounds a conversation's summary of its earlier messages:
	// a few paragraphs, a small share of a turn's prompt.
	SummaryMaxBytes = 4 << 10

	// MessageMaxBytes bounds one message's text.
	MessageMaxBytes = 32 << 10

	// StepMaxBytes bounds one step's encoded wire StepEvent: its label,
	// request, summary and refusal are sentences, never a tool's answer.
	StepMaxBytes = 16 << 10

	// CardMaxBytes bounds one card's encoded wire Card.
	CardMaxBytes = 256 << 10

	// ParkedMaxBytes bounds a parked turn's loop conversation.
	ParkedMaxBytes = 512 << 10

	// ProposalBodyMaxBytes bounds the request body a proposal stores. A
	// proposal's arguments are inside the loop its turn parks, so nothing
	// larger than [ParkedMaxBytes] could ever be proposed.
	ProposalBodyMaxBytes = ParkedMaxBytes

	// QueryMaxBytes bounds a proposal's encoded query string.
	QueryMaxBytes = 8 << 10

	// AuditReasonMaxBytes bounds a compliance read's reason: the wire's
	// rune bound at four bytes a rune.
	AuditReasonMaxBytes = 4 * 500

	// MaxApps bounds the apps a card names.
	MaxApps = 8

	// FileNameMaxBytes bounds a kept file's name, as internal/assistant's
	// SniffFile keeps it (a test holds the two to one value).
	FileNameMaxBytes = 120

	// AttachmentsPerConversation bounds the images one conversation keeps
	// from its owner: ten questions' worth of wire.AttachmentsPerTurn, so a
	// conversation's attachments are at most 200 MiB of the object store
	// for the thirty days it is kept, whoever scripts the upload route.
	AttachmentsPerConversation = 10 * wire.AttachmentsPerTurn

	// MaxListLimit is the most conversations one listing returns, and
	// DefaultListLimit what a listing that names none takes.
	MaxListLimit     = 50
	DefaultListLimit = 20
)

// FilesPerConversation is how many files of origin one conversation keeps at
// most, and zero for an origin this build does not know.
func FilesPerConversation(origin wire.FileOrigin) int {
	if origin == wire.FileAttached {
		return AttachmentsPerConversation
	}
	return 0
}

// fileMediaTypes is what a file of origin may be: what the person may attach.
func fileMediaTypes(origin wire.FileOrigin) []string {
	if origin == wire.FileAttached {
		return wire.AttachmentMediaTypes
	}
	return nil
}
