package estate

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// answerKind is how an operation's answer is bounded. A data node answers in
// ONE reply, refused whole past the transport's message ([server.encodeReply]),
// so every read declares which bound holds it, before its reader builds an
// answer that large: TestEveryOperationDeclaresHowLargeItsAnswerGrows.
type answerKind string

const (
	// answerRows is a page of rows read through [statelog.RowBytes] to the
	// operation's declared bound, its answer saying where it was cut the way
	// it says a page is short ([op.rowsTo]).
	answerRows answerKind = "rows"

	// answerOwnBound is a page its domain cuts by a filler of its own, at a
	// bound no larger than [statelog.AnswerBytes] ([op.ownBound]): a usage
	// page at a record's edge ([usage.Fill]), Gitlet's at a row past its
	// bytes (gitlet.MaxPageBytes). The row that crosses the bound stays, so
	// the page may pass it by that row; the reply ceiling's other half holds
	// it (a push row encodes to some 1.7 MB at its worst, past one record).
	answerOwnBound answerKind = "own_bound"

	// answerOne is one value bounded where it is written: one record under
	// its log's record cap, a count, a flag, or what a write wrote. Every
	// write's ([define]).
	answerOne answerKind = "one"

	// answerPerAsked is one small entry for each key the request names (a
	// flag per turn, a place per run), or at most the count it asks for (a
	// duty's nominations): bounded by the request, itself one message.
	answerPerAsked answerKind = "per_asked"

	// answerCapped is a list its domain caps at a named count of records,
	// each held to a named byte bound where it is written ([op.cappedAt]),
	// the two together within [statelog.AnswerBytes]: never paged.
	answerCapped answerKind = "capped"
)

// The fixed parts of a capped row, JSON-encoded: an int64 count or ordinal
// (twenty digits, a key's quotes and colon), and an RFC 3339 instant with its
// nanoseconds and a field name.
const (
	countBytes = 24
	timeBytes  = 64
)

// answerShape is an operation's declared [answerKind]: for rows or an own
// bound the bytes they are read to, for a capped list its count and the
// bytes each row is held to.
type answerShape struct {
	kind  answerKind
	bytes int
	rows  int
}

// rowsTo declares a read answering rows, read to at most bytes of them
// through [statelog.RowsOf], to which every run carries the bound
// ([statelog.WithAnswerBytes]), in-process too: a page's length must not
// depend on which node answered it. At most [statelog.AnswerBytes]; a smaller
// bound needs its reason at the declaration.
func (o op[A, R]) rowsTo(bytes int) op[A, R] {
	if bytes <= 0 || bytes > statelog.AnswerBytes {
		panic(fmt.Sprintf("estate: %s reads its rows to %d bytes, outside (0, %d]",
			o.spec.name, bytes, statelog.AnswerBytes))
	}
	return o.answers(answerShape{kind: answerRows, bytes: bytes})
}

// ownBound declares a page its domain cuts at bytes by a filler of its own
// ([answerOwnBound]).
func (o op[A, R]) ownBound(bytes int) op[A, R] {
	if bytes <= 0 || bytes > statelog.AnswerBytes {
		panic(fmt.Sprintf("estate: %s is cut at %d bytes, outside (0, %d]",
			o.spec.name, bytes, statelog.AnswerBytes))
	}
	return o.answers(answerShape{kind: answerOwnBound, bytes: bytes})
}

// oneAnswer declares a read answering one bounded value ([answerOne]).
func (o op[A, R]) oneAnswer() op[A, R] { return o.answers(answerShape{kind: answerOne}) }

// perAsked declares a read answering one small entry per key the request
// names ([answerPerAsked]).
func (o op[A, R]) perAsked() op[A, R] { return o.answers(answerShape{kind: answerPerAsked}) }

// cappedAt declares a read answering at most rows records of at most
// rowBytes each ([answerCapped]); the sweep holds their product to
// [statelog.AnswerBytes].
func (o op[A, R]) cappedAt(rows, rowBytes int) op[A, R] {
	return o.answers(answerShape{kind: answerCapped, rows: rows, bytes: rowBytes})
}

// answers sets a READ's shape once: a write's is [answerOne] by its class.
func (o op[A, R]) answers(shape answerShape) op[A, R] {
	switch {
	case o.spec.class != opRead:
		panic(fmt.Sprintf("estate: %s is a write, and a write answers what it wrote", o.spec.name))
	case o.spec.answer.kind != "":
		panic(fmt.Sprintf("estate: %s declares its answer twice", o.spec.name))
	}
	o.spec.answer = shape
	return o
}

// reading is ctx carrying the bound a read answering rows declared, to the
// reader that scans them ([statelog.RowsOf]); every other context as it is.
func (s answerShape) reading(ctx context.Context) context.Context {
	if s.kind != answerRows {
		return ctx
	}
	return statelog.WithAnswerBytes(ctx, s.bytes)
}

// valid reports a kind this build declares.
func (k answerKind) valid() bool {
	switch k {
	case answerRows, answerOwnBound, answerOne, answerPerAsked, answerCapped:
		return true
	}
	return false
}
