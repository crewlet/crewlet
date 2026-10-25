package assistantdomain

import "time"

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

	// MaxListLimit is the most conversations one listing returns, and
	// DefaultListLimit what a listing that names none takes.
	MaxListLimit     = 50
	DefaultListLimit = 20
)

// The retention a conversation is kept for after it was last used (ADR-0049).
const (
	// DefaultRetention is what `assistant.retention_days` takes unset: the
	// turn detail's and the trash's thirty days.
	DefaultRetention = 30 * 24 * time.Hour

	// MinRetention and MaxRetention bound the setting: a day, the trash
	// reaper's own floor, and a year.
	MinRetention = 24 * time.Hour
	MaxRetention = 365 * 24 * time.Hour
)
