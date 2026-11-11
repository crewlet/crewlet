package builtin

import (
	"time"

	"github.com/crewlet/crewlet/internal/tools"
)

// EncodeAnswer is [encodeAnswer], so the cases that weigh a tool answer weigh
// the bytes a reader is actually handed rather than another encoder's.
var EncodeAnswer = encodeAnswer

// CodeStatusJudges is how many grant reads one code_status call runs at once,
// for the cases that hold a call to it.
const CodeStatusJudges = codeStatusJudges

// ReadSessionWithin is read_session over deps, bounded at within rather than
// [SessionReadWithin], for the case about a read past its bound.
func ReadSessionWithin(deps SessionDeps, within time.Duration) tools.SeatCallable {
	return &readSession{deps: deps, within: within}
}

// ListedSessionsMax and ListedProseBytes are a listing's page and the most of
// each of a listed row's words, for the cases that hold a page at its worst
// to one answer and a listed row's words to their cut.
const (
	ListedSessionsMax = listedSessionsMax
	ListedProseBytes  = listedProseBytes
)

// OperateCallDigest is [operateCallDigest], so the cases that compose an
// approved decision bind it to the call they will make.
var OperateCallDigest = operateCallDigest
