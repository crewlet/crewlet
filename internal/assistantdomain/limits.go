package assistantdomain

import "github.com/crewlet/crewlet/internal/assistant/wire"

// THE BOUNDS ON WHAT IS STORED. They are this package's because the writer is
// where a value is refused before it is sealed and published, and the
// orchestrator (internal/assistant) reads them from here rather than stating
// its own: a bound written twice is one the two sides disagree about, and the
// side that lost would be a record the log refuses `record_too_large` after a
// turn already showed it.
const (
	// TitleMaxBytes bounds a conversation's title.
	TitleMaxBytes = 256

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

	// DeliveredPerConversation bounds the files deep-work runs deliver into
	// one conversation: ten runs' worth of wire.DeliveredFilesPerRun, so
	// what runs hand back is bounded the same way.
	DeliveredPerConversation = 10 * wire.DeliveredFilesPerRun

	// MaxListLimit is the most conversations one listing returns, and
	// DefaultListLimit what a listing that names none takes.
	MaxListLimit     = 50
	DefaultListLimit = 20
)

// FilesPerConversation is how many files of origin one conversation keeps at
// most, and zero for an origin this build does not know.
func FilesPerConversation(origin wire.FileOrigin) int {
	switch origin {
	case wire.FileAttached:
		return AttachmentsPerConversation
	case wire.FileDelivered:
		return DeliveredPerConversation
	}
	return 0
}

// fileMediaTypes is what a file of origin may be: what the person may attach,
// or what a run may deliver.
func fileMediaTypes(origin wire.FileOrigin) []string {
	if origin == wire.FileAttached {
		return wire.AttachmentMediaTypes
	}
	return wire.FileMediaTypes
}
