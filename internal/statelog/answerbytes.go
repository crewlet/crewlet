package statelog

import (
	"context"
	"encoding/json"

	"github.com/crewlet/crewlet/internal/queue"
)

// AnswerBytes is the most the rows of one read's answer hold ([RowBytes]).
//
// HALF OF [queue.MaxPayloadBytes], as a chat page and eventfan's answers are: a
// node holding no data reads the answer through the estate router as ONE reply,
// refused whole past the transport's message, and the other half is for what
// rides beside the rows, the envelope and its seal.
const AnswerBytes = queue.MaxPayloadBytes / 2

// RowBytes holds the rows a read takes to a byte bound, where it scans them,
// so no read builds an answer its reply would refuse. It stops BEFORE the row
// that would take what is held past the bound, and keeps the first row
// whatever its size, so every page moves its cursor on. [RowBytes.Cut] says
// the read stopped short: its answer says more lies past its last row the way
// that read already says a page is short (a cursor, `more`, `truncated`).
//
// The zero value keeps one row: a bound left unset never reads everything.
type RowBytes struct {
	bound int
	held  int
	rows  int
	cut   bool
}

// NewRowBytes is a read's hold on its rows, to at most bound bytes.
func NewRowBytes(bound int) *RowBytes { return &RowBytes{bound: bound} }

// answerBytesKey carries a smaller bound than [AnswerBytes] to a read.
type answerBytesKey struct{}

// WithAnswerBytes is ctx carrying the bound a read's rows are held to
// ([RowsOf]), for an operation that declared one smaller than [AnswerBytes].
// On the context, as [WithSpent] is, because it is the asking surface's to
// set and every reader between it and the scan only passes it on.
func WithAnswerBytes(ctx context.Context, bound int) context.Context {
	return context.WithValue(ctx, answerBytesKey{}, bound)
}

// RowsOf is a read's hold on its rows: to the bound ctx carries, or to
// [AnswerBytes] where it carries none, so no reader reads unbounded.
func RowsOf(ctx context.Context) *RowBytes {
	if bound, ok := ctx.Value(answerBytesKey{}).(int); ok && bound > 0 && bound < AnswerBytes {
		return NewRowBytes(bound)
	}
	return NewRowBytes(AnswerBytes)
}

// Take reports whether a row of n bytes is kept: the first always is, and a
// later one only while what is held stays within the bound. Once a row is
// refused every later one is too, so a read that skips rather than stops
// still answers a prefix its cursor can resume after.
func (b *RowBytes) Take(n int) bool {
	if b.cut {
		return false
	}
	if b.rows > 0 && b.held+n > b.bound {
		b.cut = true
		return false
	}
	b.held += n
	b.rows++
	return true
}

// TakeEncoded is [RowBytes.Take] of a row measured as the wire encodes it,
// for a row read column by column rather than as one stored document. A row
// that cannot be encoded is counted past the bound: the reply would refuse it
// anyway.
func (b *RowBytes) TakeEncoded(row any) bool {
	raw, err := json.Marshal(row)
	if err != nil {
		return b.Take(b.bound + 1)
	}
	return b.Take(len(raw))
}

// Cut reports whether a row was refused: the answer is short of what its
// query matched.
func (b *RowBytes) Cut() bool { return b.cut }

// Held is the bytes of the rows kept.
func (b *RowBytes) Held() int { return b.held }
