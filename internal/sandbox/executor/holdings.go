package executor

import (
	"context"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHICH OF A ROUND'S CALLS A KEPT SESSION HOLDS, READ BY THE RUNNER OF ITS
// HARNESS.
//
// A relaunch continuing a cut-off kept capture asks the sandbox which of its
// round's calls the session holds ([sandbox.Coordinator.HoldingsOf]), and the
// sandbox asks the runner whose harness wrote the session
// ([Runner.ReadHoldings]): each harness files its calls in records of its own
// shape, so reading them is each harness's own ([holdingsReader]), and a
// runner whose harness has no reading reads none. Claude Code's is
// claudeholdings.go's.
//
// IN ORDER, NOTHING OPENED BEFORE THE LAST: the harness's reading; the ask and
// the archive held to its own rule; the writer's release held to its floor;
// the slots; and only then the archive opened, and read. A reading refused at
// any of the first four holds no slot and opens no store connection.
//
// THE SEATS' ONE TRANSCRIPT SLOT, INSIDE THE PROCESS'S TWO, AND NO POOL OF ITS
// OWN ([SeatTranscriptReadsAtOnce]). A reading holds at its worst what one
// read of a transcript can, on the order of a hundred mebibytes
// ([TranscriptReadsAtOnce]), and a pool beside the process's two would double
// what reads can hold at their worst. So a relaunch's reading takes turns with
// the seats' reads, and never takes the slot a person's read is kept: it
// waits for its slots inside its caller's wait, and a reading that waits past
// it is no reading, which tells the session every call after its save point,
// never wrong.
//
// THE WRITER'S FLOOR IS THE READING'S OWN ([ClaudeCode.holdingsFloor]), never
// a carry floor ([claudeCarriedWriterFloor], [claudeCarryFloor]): those say
// what a box can continue, which is its harness's resume to do, where a
// reading decides from the shape of what a release recorded, and is held to
// the oldest release those shapes were measured on. A release it cannot read,
// or none recorded, is below it ([sandbox.ValidRelease]): every release check
// of the package reads an unreadable release as a current one
// ([releaseAtLeast]), which here would read a writer nobody measured.

// holdingsReader is a harness's reading of its own sessions for which of a
// round's calls they hold ([Runner.ReadHoldings]).
type holdingsReader interface {
	// holdingsFloor is the oldest release of the harness whose sessions the
	// reading reads: the oldest its rules were measured on.
	holdingsFloor() string

	// checkHoldings holds ask, and the archive as its row describes it, to
	// what the reading answers, before anything is opened.
	checkHoldings(archive sandbox.SessionArchive, ask sandbox.HoldingsAsk) error

	// readHoldings reads body, archive's stream, within bounds, for the calls
	// ask marks asked, deciding nothing before body has reached io.EOF, and
	// checks ctx between the bounded steps of its work.
	readHoldings(ctx context.Context, body io.Reader, archive sandbox.SessionArchive,
		ask sandbox.HoldingsAsk, bounds scrubBounds) (sandbox.SessionHoldings, error)
}

// ReadHoldings reads what archive, a kept session of this runner's harness,
// holds of the calls ask marks asked ([sandbox.HoldingsReader]), or says why it
// reads none. See the file comment: only where the harness has a reading; of
// an ask and an archive that reading answers; of a session a release at or
// after the reading's floor wrote; inside the seats' slot and one of the
// process's; the archive opened only then, through open, at most once, and
// closed before the slots are given back; read within the bounds a kept
// session is scrubbed within ([boundsFor]).
//
// Whether this runner restores archive at all is not asked again: a reading's
// caller asks [Runner.Restores] first, through [sandbox.ContinuityOf].
func (r *Runner) ReadHoldings(ctx context.Context, archive sandbox.SessionArchive,
	open func(context.Context) (io.ReadCloser, error), ask sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {

	h, ok := r.harness.(holdingsReader)
	if !ok {
		return sandbox.SessionHoldings{}, fmt.Errorf("executor: this build reads no session %s wrote for "+
			"the calls it holds", r.harness.Name())
	}
	if err := h.checkHoldings(archive, ask); err != nil {
		return sandbox.SessionHoldings{}, err
	}
	if floor := h.holdingsFloor(); !sandbox.ValidRelease(archive.Version) || !releaseAtLeast(archive.Version, floor) {
		return sandbox.SessionHoldings{}, fmt.Errorf("executor: the session's writer, %s %.64q, is not one "+
			"a reading reads: its records are read as %s %s and later write them, the oldest they were "+
			"measured on", r.harness.Name(), archive.Version, r.harness.Name(), floor)
	}
	release, err := takeSlots(ctx, ReadBySeat)
	if err != nil {
		return sandbox.SessionHoldings{}, err
	}
	// DEFERRED BEFORE THE ARCHIVE IS OPENED, SO IT RUNS AFTER IT IS CLOSED, as
	// a person's read gives its slot back ([readTranscript]).
	defer release()
	body, err := open(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		// A READING ITS CALLER ABANDONED stops as abandoned, whatever the
		// store made of the open stopping under it.
		return sandbox.SessionHoldings{}, ctx.Err()
	case err != nil:
		return sandbox.SessionHoldings{}, fmt.Errorf("executor: opening the session's archive: %w", err)
	}
	defer func() { _ = body.Close() }()
	return h.readHoldings(ctx, body, archive, ask, boundsFor(executorkeep.SessionMaxBytes))
}

var _ sandbox.HoldingsReader = (*Runner)(nil)

// holdingsError is why a reading read nothing: the reading's own words, whole,
// and what gave it, where something did, cut ([firstLine]). What gives it can
// quote what a box wrote, a line's bytes in a decode's error or a member's
// name a mebibyte long, and the error is a log line's detail; so a member's
// name rides the reading's own words only as [memberNamed] cuts it.
type holdingsError struct {
	why   string
	cause error
}

// Error is the reading's words, and its cause's first line, redacted and cut.
func (e *holdingsError) Error() string {
	if e.cause == nil {
		return "executor: " + e.why
	}
	return "executor: " + e.why + ": " + firstLine(e.cause.Error(), transcriptDetailLimit)
}

// Unwrap is what gave it.
func (e *holdingsError) Unwrap() error { return e.cause }
