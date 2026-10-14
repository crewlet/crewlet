package builtin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/learning"
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
		if got := pageWriteFailure("save_page", c.err, ""); !got.Failed || got.Refusal != c.want {
			t.Errorf("pageWriteFailure(%v) = failed %v class %q, want %q",
				c.err, got.Failed, got.Refusal, c.want)
		}
	}
}

// A WRITE NOBODY ANSWERED IS UNKNOWN, NEVER "NOT MADE".
//
// The router sends a write carrying no key of its caller's to one data node
// and no further, because a repeat would be a second write — so one that node
// never answered may have landed. Falling through to each mapper's remainder,
// it was told "the change was NOT made" / "nothing was written": a caller that
// believed it wrote the comment or said the message a second time.
//
// AND NOTHING THAT WRAPS A MAPPER'S ANSWER MAY TURN IT BACK. send_dm's post
// after its open rewrote every node-class answer as "the message was NOT
// sent", and that class is the one an unknown outcome is answered in — so the
// mapper's unknown reached the caller as a refusal saying the opposite. The
// direct message's own answer is held here beside the mappers for that
// reason; save_page's rename, the other wrapper, is held through the tool in
// TestASaveWhoseRenameNobodyAnsweredIsUnknown.
//
// A TRACKER WRITE IS ASKED OF EVERY DATA NODE under its operation, and when
// none of them answered, the router says so as the same unknown — each one
// that stayed silent may have run it. Its mapper fell through to "the change
// was NOT made" as well, which a seat acts on by doing it again by other
// means.
func TestAnUnansweredWriteIsAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("%w (pages.comment, asked of data-1)", estate.ErrOutcomeUnknown)
	tracked := fmt.Errorf("%w (tracker.update_task, asked of data-1, data-2: "+
		"data-1: no answer; data-2: no answer)", estate.ErrOutcomeUnknown)
	for name, got := range map[string]tools.Result{
		"a page write":    pageWriteFailure("comment_on_page", err, ""),
		"a chat write":    chatWriteFailure(PostMessageTool, err, ""),
		"a tracker write": writeFailure(Actor{}, UpdateWorkItemTool, tracked),
		"a direct message's post after its open": directPostFailure([]string{"sam"},
			chat.Written{Channel: chat.Channel{ID: "dm-1"}}, err, ""),
	} {
		if !got.Failed || !got.Unknown || got.Refusal != tools.RefusalUnavailable {
			t.Errorf("%s nobody answered: failed %v unknown %v class %q, want an "+
				"unavailable refusal marked unknown", name, got.Failed, got.Unknown, got.Refusal)
		}
		for _, never := range []string{"NOT made", "nothing was written", "NOT sent",
			"was not said"} {
			if strings.Contains(got.Output, never) {
				t.Errorf("%s nobody answered says %q, which nobody can know: %s",
					name, never, got.Output)
			}
		}
		if !strings.Contains(got.Output, "unknown") {
			t.Errorf("%s nobody answered does not say its outcome is unknown: %s",
				name, got.Output)
		}
	}
	// AND A TRACKER WRITE SAYS HOW THIS CALLER SETTLES IT: the same
	// operation again, which the ledger answers with what landed — for an
	// operator, the op_id it brings back.
	operator := writeFailure(Actor{Operation: "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.update_work_item.00"},
		UpdateWorkItemTool, tracked)
	if !strings.Contains(operator.Output, "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.update_work_item.00") {
		t.Errorf("an operator's unanswered tracker write does not name the op_id that "+
			"settles it: %s", operator.Output)
	}
	// AND THE DIRECT MESSAGE STILL SAYS ITS CONVERSATION IS OPEN: the open is
	// a record that landed, and only the post after it is in doubt.
	dm := directPostFailure([]string{"sam"}, chat.Written{Channel: chat.Channel{ID: "dm-1"}}, err, "")
	if !strings.Contains(dm.Output, "is open (dm-1)") {
		t.Errorf("a direct message's unanswered post does not say its "+
			"conversation is open: %s", dm.Output)
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
		"no native chat":    {unconfiguredChat("post_message"), tools.RefusalUnavailable},
		"chat read":         {chatReadFailure("read_channel", boom), tools.RefusalUnavailable},
		// A CHAT CALLER WITH NOBODY TO ACT AS is forbidden whichever way
		// it got there: a seat outside a turn, or a credential bound to
		// no seat — whose sentence names the binding to add.
		"chat outside a turn": {chatActorFailure("post_message", turnctx.ErrNoSeat),
			tools.RefusalForbidden},
		"chat unbound token": {chatActorFailure("post_message",
			errors.New("the token \"ci\" is bound to no seat")), tools.RefusalForbidden},
		"tracker read": {readFailure("get_work_item", boom), tools.RefusalUnavailable},
		"page read":    {pageReadFailure("get_page", boom), tools.RefusalUnavailable},
		"bare failed":  {failed("needs an `item`"), tools.RefusalInvalid},
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

// EVERY CHAT SENTINEL HAS ITS OWN CLASS, on [TestWriteFailureClassesEachSentinel]'s
// reasoning — and the chat store wraps every refusal it decides on what was
// asked in one of them, so the unmarked remainder is this node's condition.
// Every row is shaped the way the store builds it: a field refusal from a
// payload's own validation, the rest wrapping their mark with the sentence
// after it.
func TestChatWriteFailureClassesEachSentinel(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		err  error
		want tools.Refusal
	}{
		{"a field refused", chat.ChannelPatch{}.Validate(), tools.RefusalInvalid},
		// A ROOM THAT TAKES NOTHING MORE is fixed by saying it somewhere
		// else — the argument — and is nobody's permission to ask for.
		{"an archived room", fmt.Errorf("%w: room-1 was archived", chat.ErrArchived),
			tools.RefusalInvalid},
		{"not this actor's", fmt.Errorf("%w: message m1 was written by ana",
			chat.ErrForbidden), tools.RefusalForbidden},
		// A ROOM A NEWER PEER MADE is this node's to serve once upgraded,
		// never a permission: told "forbidden", a caller stops asking for
		// something that lands the moment this node is upgraded.
		{"a room of a kind this build does not know", fmt.Errorf("%w: room-1 is "+
			"a \"guild\" room", chat.ErrUnknownKind), tools.RefusalUnavailable},
		{"a name somebody holds", fmt.Errorf("%w: launch", chat.ErrNameTaken),
			tools.RefusalExists},
		{"a conversation already open", fmt.Errorf("%w: dm-1", chat.ErrRoomExists),
			tools.RefusalExists},
		{"a lost race", fmt.Errorf("%w: room-1: %w", chat.ErrConflict,
			statelog.ErrConflict), tools.RefusalConflict},
		{"no such room", fmt.Errorf("%w: room room-1", chat.ErrNotFound),
			tools.RefusalNotFound},
		// THE LOG'S OWN REFUSAL AND ANYTHING UNMARKED are the node's: a
		// person told to fix their input over a disk error is the lie this
		// row pins.
		{"log refused", &statelog.Unavailable{Reason: statelog.ReasonDeferred},
			tools.RefusalUnavailable},
		{"timed out", fmt.Errorf("x: %w", context.DeadlineExceeded), tools.RefusalUnavailable},
		{"ledger unreadable", errors.New("statelog: read the operation ledger: " +
			"disk I/O"), tools.RefusalUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := chatWriteFailure(PostMessageTool, c.err, "")
			if !got.Failed || got.Refusal != c.want {
				t.Fatalf("chatWriteFailure(%v) = failed %v class %q, want %q",
					c.err, got.Failed, got.Refusal, c.want)
			}
		})
	}
}

// AND THE TWO CLASSES A PERSON IS SHOWN AS WRITTEN READ FOR THAT PERSON: the
// store's own sentence, without the package's Go prefix, and nothing written
// for a model about a failure the person could not see.
func TestAChatRefusalAPersonReadsIsTheirSentence(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		got  tools.Result
		says string
	}{
		"invalid": {chatWriteFailure(PostMessageTool, chat.ChannelPatch{}.Validate(), ""),
			"`patch`: a patch sets no field"},
		"forbidden": {chatWriteFailure(ReactToMessageTool, fmt.Errorf("%w: room-1 "+
			"is a private room and jane is not in it", chat.ErrForbidden), ""),
			"room-1 is a private room and jane is not in it"},
		"a room this node cannot serve yet": {chatWriteFailure(PostMessageTool,
			fmt.Errorf("%w: room-1 is a \"guild\" room — Upgrade this node",
				chat.ErrUnknownKind), ""),
			"room-1 is a \"guild\" room — Upgrade this node"},
		"forbidden read": {chatReadFailure(ReadChannelTool, fmt.Errorf("%w: viewer: "+
			"this chat read resolved to no seat", chat.ErrForbidden)),
			"viewer: this chat read resolved to no seat"},
		"invalid read": {chatReadFailure(ReadChannelTool, chat.ChannelPatch{}.Validate()),
			"`patch`: a patch sets no field"},
	} {
		if !strings.Contains(c.got.Output, c.says) {
			t.Errorf("%s: the sentence does not say %q: %s", name, c.says, c.got.Output)
		}
		for _, never := range []string{"chat:", "did not land", "do not report it as done"} {
			if strings.Contains(c.got.Output, never) {
				t.Errorf("%s: a refusal a person reads carries %q: %s", name, never,
					c.got.Output)
			}
		}
	}
}

// A NOTE LIST CUT TO A LIMIT SAYS HOW MANY IT LEFT OFF. The filter picks up to
// eight; the default shows every one of them, and a smaller limit is a
// narrowing the answer admits to rather than a list that reads as complete.
func TestRefreshMemorySaysWhatALimitLeftOff(t *testing.T) {
	t.Parallel()
	entries := make([]learning.DiaryEntry, noteLimit)
	for i := range entries {
		entries[i] = learning.DiaryEntry{Kind: "long", Content: "note " + strconv.Itoa(i)}
	}
	all := renderHintedNotes(entries, "deploys", noteLimit)
	if strings.Count(all.Output, "- [long]") != noteLimit || strings.Contains(all.Output, "more that bear") {
		t.Fatalf("the default did not show every pick:\n%s", all.Output)
	}
	some := renderHintedNotes(entries, "deploys", 3)
	if strings.Count(some.Output, "- [long]") != 3 || !strings.Contains(some.Output, "+5 more that bear on this") {
		t.Fatalf("a narrowed list did not say what it left off:\n%s", some.Output)
	}
}

// AN AMBIGUOUS ANSWER'S CANDIDATES ARE WHOLE OR BY REFERENCE. They were the
// first hundred and twenty characters of each, unmarked, so two questions that
// opened alike read as one question asked twice — and the refusal exists to
// tell them apart.
func TestAmbiguousAnswerCandidatesAreWholeOrReferenced(t *testing.T) {
	t.Parallel()
	opening := strings.Repeat("About the release plan for next week, ", 4)
	e := &tracker.ErrAmbiguousAnswer{Task: "ENG-1", Actor: "ana", Asks: []tracker.AskCandidate{
		{Comment: "c-1", Author: "bo", Body: opening + "should we ship on Friday?"},
		{Comment: "c-2", Author: "cy", Body: opening + "who signs off the migration?"},
		{Comment: "c-3", Author: "di", Question: "Pick a region", Body: "context"},
		{Comment: "c-4", Author: "ed", Body: strings.Repeat("long ", 4000)},
	}}
	got := ambiguousText(e)
	for _, want := range []string{"should we ship on Friday?", "who signs off the migration?",
		"Pick a region", `get_work_item(item="ENG-1", comment="c-4")`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "long long") || strings.Contains(got, "…") && !strings.Contains(got, "… and more") {
		t.Errorf("a candidate was cut rather than carried whole or referenced:\n%s", got)
	}
}
