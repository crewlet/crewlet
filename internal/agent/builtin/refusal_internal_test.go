package builtin

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY WRITE SENTINEL HAS ITS OWN CLASS, because the class is what a person's
// surface acts on and the sentence is not: a stale version is re-read and
// offered again, a lost race is retried, a spent hand-off budget is never
// retried, an authority refusal names whom to ask, and a log that refused the
// append is this node's condition. Each row is wrapped the way the writer
// wraps it, so a mapping that matched the sentinel only when bare would fail.
func TestWriteFailureClassesEachSentinel(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		err  error
		want tools.Refusal
	}{
		{"no task", fmt.Errorf("x: %w", tracker.ErrNoTask), tools.RefusalNotFound},
		{"lost race", fmt.Errorf("x: %w", statelog.ErrConflict), tools.RefusalConflict},
		{"exists", fmt.Errorf("x: %w", statelog.ErrExists), tools.RefusalExists},
		{"stale", fmt.Errorf("x: %w", tracker.ErrStaleVersion), tools.RefusalStaleVersion},
		{"hand-offs", fmt.Errorf("x: %w", tracker.ErrReassignmentBudget),
			tools.RefusalReassignmentBudget},
		{"authority", fmt.Errorf("x: %w", tracker.ErrForbidden), tools.RefusalForbidden},
		// A FULL INBOX LIST asks for a different gesture, not a smaller
		// one — read_through — so it is not a content refusal either.
		{"inbox full", fmt.Errorf("x: %w", tracker.ErrInboxFull), tools.RefusalInboxFull},
		// A maintenance or sealed fleet has no publisher and an evicted,
		// deferred or full-log node refuses the append: all of them
		// arrive as a statelog refusal.
		{"log refused", &statelog.Unavailable{Reason: statelog.ReasonDeferred},
			tools.RefusalUnavailable},
		{"not on this node", fmt.Errorf("x: %w", statelog.ErrUnavailable),
			tools.RefusalUnavailable},
		{"lease unreadable", fmt.Errorf("x: %w", coord.ErrUnavailable),
			tools.RefusalUnavailable},
		{"timed out", fmt.Errorf("x: %w", context.DeadlineExceeded), tools.RefusalUnavailable},
		// THE WRITER'S OWN REFUSAL ON CONTENT, marked, which names what
		// it refused: offering a retry for it would be a retry that
		// never lands.
		{"refused on content", fmt.Errorf("tracker: field estimate is exact "+
			"to two places: %w", tracker.ErrInvalid), tools.RefusalInvalid},
		// AN UNMARKED FAILURE IS THE NODE'S. These arrive raw from the
		// publisher and the writer's own reads — telling a person to fix
		// their input over a disk error is the lie this row pins.
		{"ledger unreadable", errors.New("statelog: read the operation " +
			"ledger: disk I/O"), tools.RefusalUnavailable},
		{"re-spread unreadable", errors.New("tracker: read the re-spread " +
			"window in ENG: database is locked"), tools.RefusalUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := writeFailure("update_work_item", c.err)
			if !got.Failed || got.Refusal != c.want {
				t.Fatalf("writeFailure(%v) = failed %v class %q, want %q",
					c.err, got.Failed, got.Refusal, c.want)
			}
		})
	}
}

// THE KNOWLEDGE BASE'S WRITER CLASSES ITS OWN REFUSALS, so here the unmarked
// remainder is this node's failure rather than the caller's.
func TestPageWriteFailureClassesEachSentinel(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err  error
		want tools.Refusal
	}{
		{fmt.Errorf("x: %w", pages.ErrInvalid), tools.RefusalInvalid},
		{fmt.Errorf("x: %w", pages.ErrReserved), tools.RefusalForbidden},
		{fmt.Errorf("x: %w", pages.ErrTitleTaken), tools.RefusalExists},
		{fmt.Errorf("x: %w", pages.ErrStaleVersion), tools.RefusalStaleVersion},
		{fmt.Errorf("x: %w", pages.ErrConflict), tools.RefusalConflict},
		{fmt.Errorf("x: %w", pages.ErrNotFound), tools.RefusalNotFound},
		{errors.New("pages: read the container ENG: disk I/O"), tools.RefusalUnavailable},
	} {
		if got := pageWriteFailure("save_page", c.err); !got.Failed || got.Refusal != c.want {
			t.Errorf("pageWriteFailure(%v) = failed %v class %q, want %q",
				c.err, got.Failed, got.Refusal, c.want)
		}
	}
}

// THE SHARED REFUSALS ARE NEVER "NOT FOUND". A read this node could not serve
// and a backend this company does not run both say nothing about whether the
// object exists, and a surface told not-found would render it as gone.
func TestTheSharedRefusalsCarryTheirClass(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk I/O")
	for name, c := range map[string]struct {
		got  tools.Result
		want tools.Refusal
	}{
		"outside a turn":    {notInATurn("update_work_item"), tools.RefusalForbidden},
		"no tracker":        {unconfigured("update_work_item"), tools.RefusalUnavailable},
		"no knowledge base": {unconfiguredKB("save_page"), tools.RefusalUnavailable},
		"tracker read":      {readFailure("get_work_item", boom), tools.RefusalUnavailable},
		"page read":         {pageReadFailure("get_page", boom), tools.RefusalUnavailable},
		"bare failed":       {failed("needs an `item`"), tools.RefusalInvalid},
	} {
		if !c.got.Failed || c.got.Refusal != c.want {
			t.Errorf("%s: failed %v class %q, want %q", name, c.got.Failed,
				c.got.Refusal, c.want)
		}
	}
}

// AN `answers` THE READER REFUSES IS NOT A READ FAILURE. Each of these used to
// reach the model as "could not read the tracker — try again", a retry that
// refused identically; each has its own class because each asks a different
// thing of the caller, and a failure the reader did NOT write stays the read
// failure it is.
func TestAnAnswerTheReaderRefusesCarriesItsClass(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		err  error
		want tools.Refusal
	}{
		{"no such comment", fmt.Errorf("x: %w", tracker.ErrNoComment), tools.RefusalNotFound},
		{"somebody else's question", fmt.Errorf("x: %w", tracker.ErrForbidden),
			tools.RefusalForbidden},
		{"already answered", fmt.Errorf("x: %w", tracker.ErrAlreadyAnswered),
			tools.RefusalAlreadyAnswered},
		{"not a question", fmt.Errorf("x: %w", tracker.ErrInvalid), tools.RefusalInvalid},
	} {
		got, ok := answerRefusal(c.err)
		if !ok || !got.Failed || got.Refusal != c.want {
			t.Errorf("%s: answerRefusal = %v failed %v class %q, want %q",
				c.name, ok, got.Failed, got.Refusal, c.want)
		}
	}
	if _, ok := answerRefusal(errors.New("tracker: read the ask c1: disk I/O")); ok {
		t.Error("a read failure was classed as the reader's refusal of an answer")
	}
}
