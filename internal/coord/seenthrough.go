package coord

import "context"

// SeenMarks are how far into each conversation a seat's completed turns read
// (ADR-0120): one mark per (seat, conversation), the log position of the newest
// record a turn there was woken for. Kept in the completion ledger's bucket and
// aged with it, since a mark only matters to a redelivery the ledger outlives.
type SeenMarks interface {
	// SeenThrough is the mark scope holds for conversation and its revision;
	// empty and zero where it holds none.
	SeenThrough(ctx context.Context, scope, conversation string) (string, uint64, error)
	// MarkSeenThrough writes mark over the one read at revision (zero for none),
	// false where it moved since.
	MarkSeenThrough(ctx context.Context, scope, conversation, mark string, revision uint64) (bool, error)
}
