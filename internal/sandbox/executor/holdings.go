package executor

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// Which of a round's calls a kept session holds, read by the runner of the
// harness that wrote it: a relaunch continuing a cut-off kept capture asks
// [sandbox.Coordinator.HoldingsOf], which asks [Runner.ReadHoldings]. Records
// are each harness's own shape, so reading them is too ([holdingsReader];
// Claude Code's is claudeholdings.go's); a harness without one reads none.
//
// A POOL OF ITS OWN, ONE SLOT ([HoldingsReadsAtOnce]), outside the seats'
// slot and the process's two transcript reads, taken inside its caller's
// wait; a reading that waits past it is no reading, which tells the session
// every call after its save point: never wrong.

// holdingsReader is a harness's reading of its own sessions for which of a
// round's calls they hold ([Runner.ReadHoldings]).
type holdingsReader interface {
	// holdingsFloor is the oldest release of the harness whose sessions the
	// reading reads: the oldest its rules were measured on. It is the
	// reading's own, never a carry floor ([claudeCarryFloor],
	// [claudeCarriedWriterFloor]), which says what a box's resume can
	// continue, not which record shapes were measured. The row's writer is
	// held to it before anything opens, and each record again by the release
	// it names, since a continued session holds several releases' records
	// ([ClaudeCode.readHoldings], [writerBelowFloor]).
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
// reads none. In order, opening nothing before the last: the harness's
// reading; ask and archive held to it; the writer held to its floor
// ([readsRelease]); the readings' slot ([HoldingsReadsAtOnce]); then archive
// opened once, through open, closed before the slot goes back, and read
// within a kept session's scrub bounds ([scrubPlainMax]). A refusal before the
// slot holds none and opens no store connection.
//
// Whether this runner restores archive is not asked again: the caller asked
// [Runner.Restores] first, through [sandbox.ContinuityOf].
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
	if floor := h.holdingsFloor(); !readsRelease(archive.Version, floor) {
		return sandbox.SessionHoldings{}, fmt.Errorf("executor: the session's writer, %s %.64q, is not one "+
			"a reading reads: its records are read as %s %s and later write them, the oldest they were "+
			"measured on", r.harness.Name(), archive.Version, r.harness.Name(), floor)
	}
	release, err := takeSlot(ctx, holdingsReads)
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
	return h.readHoldings(ctx, body, archive, ask, plainBounds(scrubPlainMax))
}

var _ sandbox.HoldingsReader = (*Runner)(nil)

// HoldingsReadsAtOnce is the most readings ([Runner.ReadHoldings]) this
// process runs at once: a pool apart from the transcript reads'.
//
// ONE: a reading is a relaunch's of a cut-off kept capture, rare, and holds
// at its worst 43 MiB (Claude Code's model of a thread at both its bounds;
// OpenCode's at most 2 MiB, Qwen Code's 10 measured and 12 by its bounds,
// Gemini CLI's 2 whatever a line's length and 4 at its 2^14 ids), so one adds
// that to the two transcript reads' [TranscriptReadHeldBytes] each
// ([TranscriptReadsAtOnce]): 107 MiB live, 160.5 MiB with the heap's room. A
// second waits behind the first (the slowest 21.4 to 22.6 s) inside its
// caller's wait, or is no reading, which tells. No seat's read queues ahead
// of a reading (internal/sandbox's holdingsReadWait).
const HoldingsReadsAtOnce = 1

// holdingsReads holds a slot for each reading this process runs, at most
// [HoldingsReadsAtOnce].
var holdingsReads = make(chan struct{}, HoldingsReadsAtOnce)

// readsRelease reports whether release, the writer a session's row records,
// is one a reading reads: valid ([sandbox.ValidRelease]), every number readable
// ([releaseNumbers]), and at or after floor.
//
// AN UNREADABLE RELEASE IS BELOW FLOOR, not current as [releaseAtLeast] takes
// it: "", a two-number release, or one with a number past an int
// ("0.0.99999999999999999999", which ValidRelease accepts) would have the
// reading read a writer nobody measured.
func readsRelease(release, floor string) bool {
	_, readable := releaseNumbers(release)
	return sandbox.ValidRelease(release) && readable && releaseAtLeast(release, floor)
}

// releaseSetFloor is a reading's floor ([holdingsReader.holdingsFloor]) for
// set, the releases it reads compared whole: the oldest by its numbers, never
// the set's first member, and "" where none reads ([releaseNumbers]). A
// reading's check holds the writer to the set itself first, so the floor
// never refuses a writer the set reads.
func releaseSetFloor(set []string) string {
	floor, least := "", [3]int{}
	for _, release := range set {
		numbers, ok := releaseNumbers(release)
		if ok && (floor == "" || slices.Compare(numbers[:], least[:]) < 0) {
			floor, least = release, numbers
		}
	}
	return floor
}

// holdingsError is why a reading read nothing: its own words whole, and its
// cause cut ([firstLine]). A cause can quote what a box wrote (a line's bytes, a
// member's name a mebibyte long) into a log line's detail, so a member's name
// rides the reading's own words only as [memberNamed] cuts it.
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
