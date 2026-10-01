package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
		// A SECOND ANSWER THAT LOST THE RACE is refused by the write's own
		// snapshot, after the thread read passed it — typed, naming who
		// answered. It is not the node's failure and not a retry.
		{"already answered", fmt.Errorf("x: %w", &tracker.AlreadyAnsweredError{
			Task: "t-1", Comment: "c-1", By: "ana"}), tools.RefusalAlreadyAnswered},
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
		{"text past its cap", fmt.Errorf("x: %w", &tracker.TextCapError{
			Field: tracker.TextTitle, Size: 600, Limit: tracker.MaxTitle}),
			tools.RefusalInvalid},
		// A TAG THAT CANNOT BE DECLARED is what the call asked for, and its
		// sentence names the tag and the way past it.
		{"a clashing tag", fmt.Errorf("x: %w", &tracker.TagClash{Project: "OPS",
			Slug: "api", Label: "API", Other: tracker.Tag{Slug: "backend-api",
				Label: "API"}}), tools.RefusalInvalid},
		{"a full tag set", fmt.Errorf("x: %w", &tracker.TagsFull{Project: "OPS",
			Slug: "api"}), tools.RefusalInvalid},
		// A GESTURE STOPPED AT A STEP WHOSE OUTCOME IS UNKNOWN may have
		// written part of what it asked for, so it is never a refusal of
		// what was asked: it is the class an interrupted call answers,
		// the node's and not the request's — see [unknownOutcome].
		{"a step whose outcome is unknown", fmt.Errorf("x: %w",
			tracker.ErrStepUnresolved), tools.RefusalUnavailable},
		{"a step this node cannot vouch for", fmt.Errorf("x: %w",
			tracker.ErrStepUnvouched), tools.RefusalUnavailable},
		// AN UNMARKED FAILURE IS THE NODE'S. These arrive raw from the
		// publisher and the writer's own reads — telling a person to fix
		// their input over a disk error is the lie this row pins.
		{"ledger unreadable", errors.New("statelog: read the operation " +
			"ledger: disk I/O"), tools.RefusalUnavailable},
		{"re-spread unreadable", errors.New("tracker: read the re-spread " +
			"window in ENG: database is locked"), tools.RefusalUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := writeFailure(Actor{}, "update_work_item", c.err)
			if !got.Failed || got.Refusal != c.want {
				t.Fatalf("writeFailure(%v) = failed %v class %q, want %q",
					c.err, got.Failed, got.Refusal, c.want)
			}
		})
	}
}

// AN OPERATION THAT ALREADY WROTE TO SOMETHING ELSE IS THE CALLER'S TO CHANGE,
// and only where the caller named the operation: the call has to be made as a
// new write, which is a different request — the argument class. A seat names
// no operation, so the same refusal reaches it as the log's own.
func TestAReusedOperationIsTheCallersToChange(t *testing.T) {
	t.Parallel()
	reused := fmt.Errorf("x: %w", &statelog.Unavailable{Reason: statelog.ReasonOpReused})
	named := writeFailure(Actor{Operation: "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.update_work_item.00"},
		UpdateWorkItemTool, reused)
	if named.Refusal != tools.RefusalInvalid || !strings.Contains(named.Output, "new write") {
		t.Errorf("a reused operation the caller named answered %+v, want an invalid "+
			"refusal saying to make it as a new write", named)
	}
	if seat := writeFailure(Actor{WorkKey: "wk-1"}, UpdateWorkItemTool, reused); seat.Refusal != tools.RefusalUnavailable {
		t.Errorf("a seat's reused operation answered %+v, want the log's own class", seat)
	}
}

// A CONTENT REFUSAL READS FOR THE PERSON WHO IS SHOWN IT. The invalid class is
// the one a person's surface prints as the tool wrote it (`contract/errors.ts`
// — "the sentence names the argument"), so it must not carry the node-failure
// wording written for a model, nor anything the person never typed. A title
// filed from the dashboard at 600 bytes came back as "create_work_item did not
// land (tracker: the title on task 68c5… is 600 bytes …). The change was NOT
// made — do not report it as done." — a tool name, a Go error prefix, the id
// of a task that was never created, and an instruction meant for a model.
func TestAContentRefusalIsASentenceForAPerson(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		err   error
		names []string
	}{
		{"a title", &tracker.TextCapError{Field: tracker.TextTitle, Size: 600,
			Limit: tracker.MaxTitle}, []string{"`title`", "600", "256", "title"}},
		{"a description", &tracker.TextCapError{Field: tracker.TextBody,
			Size: tracker.MaxBody + 1, Limit: tracker.MaxBody},
			[]string{"`body`", "description", fmt.Sprint(tracker.MaxBody)}},
		{"a comment", &tracker.TextCapError{Field: tracker.TextComment,
			Size: tracker.MaxCommentBody + 9, Limit: tracker.MaxCommentBody},
			[]string{"`body`", "comment", fmt.Sprint(tracker.MaxCommentBody + 9)}},
		{"any other content refusal", tracker.Decision{Question: "Which?"}.Validate(),
			[]string{"a decision has 0 option(s)"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := writeFailure(Actor{}, CreateWorkItemTool, c.err)
			if got.Refusal != tools.RefusalInvalid {
				t.Fatalf("classed %q, want invalid", got.Refusal)
			}
			for _, want := range c.names {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the sentence does not say %q: %s", want, got.Output)
				}
			}
			// NEITHER THE NODE-FAILURE WORDING NOR THE GO PREFIX: the
			// class is printed to a person as it stands, and "tracker:" is
			// a word for a log about where the error came from.
			for _, never := range []string{"did not land", "do not report it as done",
				"tracker:"} {
				if strings.Contains(got.Output, never) {
					t.Errorf("a content refusal a person reads carries %q: %s",
						never, got.Output)
				}
			}
		})
	}
	// THE TEXT CAP IS COMPOSED FROM ITS FIELDS, never the writer's Go error.
	got := writeFailure(Actor{}, CreateWorkItemTool, &tracker.TextCapError{
		Field: tracker.TextTitle, Size: 600, Limit: tracker.MaxTitle})
	for _, never := range []string{"tracker:", CreateWorkItemTool} {
		if strings.Contains(got.Output, never) {
			t.Errorf("the text-cap sentence carries %q: %s", never, got.Output)
		}
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
