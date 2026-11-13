package atlasdomain

import "time"

// THE BOUNDS ON WHAT IS READ AND STORED, held by the writer before a record is
// published so the log never refuses one as too large after the fact. A claim
// is one fact of a closed vocabulary in a typed value, so its record is a few
// hundred bytes; nothing here is a document.
const (
	// MaxPageSize is the most rows a history page answers, DefaultPageSize
	// what a page that names none takes.
	MaxPageSize     = 200
	DefaultPageSize = 50

	// MaxLiveClaims bounds one read of every live claim: forty times the
	// reference company's twenty thousand, past which the read refuses
	// rather than answering a slice of the atlas as if it were all of it.
	MaxLiveClaims = 800_000

	// MaxTurnBytes bounds the run an agent's claim names: a run id is a
	// UUID, and nothing longer names a turn.
	MaxTurnBytes = 64

	// MaxPurges bounds one read of the purge markers, which only an
	// administrator adds, one value or entry at a time.
	MaxPurges = 10_000
)

// ExpiryBucket is the span one expiry removes: an hour of records, so one
// record's apply stays well inside the framework's row budget.
const ExpiryBucket = time.Hour
