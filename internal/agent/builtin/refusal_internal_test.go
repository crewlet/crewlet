package builtin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
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
		{"no task", fmt.Errorf("x: %w", worklet.ErrNoTask), tools.RefusalNotFound},
		{"lost race", fmt.Errorf("x: %w", statelog.ErrConflict), tools.RefusalConflict},
		{"exists", fmt.Errorf("x: %w", statelog.ErrExists), tools.RefusalExists},
		{"stale", fmt.Errorf("x: %w", worklet.ErrStaleVersion), tools.RefusalStaleVersion},
		{"hand-offs", fmt.Errorf("x: %w", worklet.ErrReassignmentBudget),
			tools.RefusalReassignmentBudget},
		{"authority", fmt.Errorf("x: %w", worklet.ErrForbidden), tools.RefusalForbidden},
		// A FULL INBOX LIST asks for a different gesture, not a smaller
		// one — read_through — so it is not a content refusal either.
		{"inbox full", fmt.Errorf("x: %w", worklet.ErrInboxFull), tools.RefusalInboxFull},
		// A SECOND ANSWER THAT LOST THE RACE is refused by the write's own
		// snapshot, after the thread read passed it — typed, naming who
		// answered. It is not the node's failure and not a retry.
		{"already answered", fmt.Errorf("x: %w", &worklet.AlreadyAnsweredError{
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
		// AN ESTATE HELD CLOSED while a peer's snapshot is adopted opens
		// again on its own: a condition, not a fault.
		{"estate closed for an adoption", fmt.Errorf("x: %w", store.ErrNoEstate),
			tools.RefusalUnavailable},
		// A WRITE LOCK THE WRITERS AHEAD HELD past the wait clears when
		// they commit, and the store marks exactly that give-up: wrapped
		// beside the driver's words, the way retryTransient wraps it.
		{"store busy with other writes", fmt.Errorf("x: %w", fmt.Errorf("%w: %w",
			store.ErrBusy, errors.New("turso: error: database is locked"))),
			tools.RefusalUnavailable},
		// A BROKER THIS NODE COULD NOT REACH — a reconnect, a leader that
		// did not answer — is the queue contract's condition, marked
		// beside the client's own error the way the backend marks it.
		{"event broker unreachable", fmt.Errorf("publish x: %w", fmt.Errorf("%w: %w",
			queue.ErrUnavailable, errors.New("nats: no responders available for request"))),
			tools.RefusalUnavailable},
		// AND A QUEUE WHOSE CONNECTION IS CLOSED FOR GOOD is not live: the
		// node is stopping, which is a condition too.
		{"event queue not live", fmt.Errorf("x: %w", queue.ErrNotLive),
			tools.RefusalUnavailable},
		// THE WRITER'S OWN REFUSAL ON CONTENT, marked, which names what
		// it refused: offering a retry for it would be a retry that
		// never lands.
		{"refused on content", fmt.Errorf("worklet: field estimate is exact "+
			"to two places: %w", worklet.ErrInvalid), tools.RefusalInvalid},
		{"text past its cap", fmt.Errorf("x: %w", &worklet.TextCapError{
			Field: worklet.TextTitle, Size: 600, Limit: worklet.MaxTitle}),
			tools.RefusalInvalid},
		// A TAG THAT CANNOT BE DECLARED is what the call asked for, and its
		// sentence names the tag and the way past it.
		{"a clashing tag", fmt.Errorf("x: %w", &worklet.TagClash{Project: "OPS",
			Slug: "api", Label: "API", Other: worklet.Tag{Slug: "backend-api",
				Label: "API"}}), tools.RefusalInvalid},
		{"a full tag set", fmt.Errorf("x: %w", &worklet.TagsFull{Project: "OPS",
			Slug: "api"}), tools.RefusalInvalid},
		// EVERY OTHER REFUSAL A DOMAIN DECIDED is its class too
		// ([WriterClass]): with no arm of its own here each fell to the
		// fault, and told a caller the node had broken over a comment
		// that is not there or a remark that is not theirs.
		{"a comment that is not there", fmt.Errorf("x: %w", worklet.ErrNoComment),
			tools.RefusalNotFound},
		{"a project that is not there", fmt.Errorf("x: %w", worklet.ErrNoProject),
			tools.RefusalNotFound},
		{"a remark that is not the caller's", fmt.Errorf("x: %w", worklet.ErrNotAuthor),
			tools.RefusalForbidden},
		// AND THE DOMAIN'S CLASS IS TAKEN BEFORE A CONDITION ITS ERROR
		// ALSO CARRIES: a file filed under a project this node does not
		// hold wraps the log's unavailable beside the domain's own
		// sentinel, as the tracker writes it, and is the not-found its
		// sentinel names — the project may be on its way, and the
		// sentence says so.
		{"a project not on this node yet", fmt.Errorf("%w: %s (or it is not on this node yet: %w)",
			worklet.ErrNoProject, "ENG", statelog.ErrUnavailable), tools.RefusalNotFound},
		// A GESTURE STOPPED AT A STEP WHOSE OUTCOME IS UNKNOWN may have
		// written part of what it asked for, so it is never a refusal of
		// what was asked: it is the class an interrupted call answers,
		// the node's and not the request's — see [writeCause].
		{"a step whose outcome is unknown", fmt.Errorf("x: %w",
			worklet.ErrStepUnresolved), tools.RefusalUnavailable},
		{"a step this node cannot vouch for", fmt.Errorf("x: %w",
			worklet.ErrStepUnvouched), tools.RefusalUnavailable},
		// AN UNMARKED FAILURE IS THE NODE'S, AND A FAULT. These arrive raw
		// from the publisher and the writer's own reads — telling a person
		// to fix their input over a disk error is one lie these rows pin,
		// and telling them to come back in two seconds, for ever, is the
		// other.
		{"ledger unreadable", errors.New("statelog: read the operation " +
			"ledger: disk I/O"), tools.RefusalInternalError},
		// THE WORDS ARE NOT THE MARK: "database is locked" a store did not
		// mark busy, and "no responders" a queue did not mark unavailable,
		// stay faults — the marks are what their own packages decided, and
		// a classifier reading text would call any error naming a lock a
		// wait.
		{"re-spread unreadable", errors.New("worklet: read the re-spread " +
			"window in ENG: database is locked"), tools.RefusalInternalError},
		{"broker words with no mark", errors.New("publish x: nats: no " +
			"responders available for request"), tools.RefusalInternalError},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := writeFailure(t.Context(), Actor{}, "update_work_item", c.err)
			if !got.Failed || tools.RefusalOf(got) != c.want {
				t.Fatalf("writeFailure(%v) = failed %v class %q, want %q",
					c.err, got.Failed, tools.RefusalOf(got), c.want)
			}
			// THE ERROR IS BENEATH THE CLASS, never instead of it: a
			// caller branching on the writer's sentinel reads the same
			// cause the class was read from.
			if !errors.Is(got.Cause, c.err) {
				t.Errorf("the cause %v does not carry %v", got.Cause, c.err)
			}
			// AND ONLY A STOPPED STEP IS AN UNKNOWN: a refusal read as one
			// tells a surface the write may have landed when nothing did.
			wantUnknown := errors.Is(c.err, worklet.ErrStepUnresolved)
			if tools.UnknownOf(got) != wantUnknown {
				t.Errorf("UnknownOf(writeFailure(%v)) = %v, want %v", c.err,
					tools.UnknownOf(got), wantUnknown)
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
	named := writeFailure(t.Context(), Actor{Operation: "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b.update_work_item.00"},
		UpdateWorkItemTool, reused)
	if tools.RefusalOf(named) != tools.RefusalInvalid || !strings.Contains(named.Output, "new write") {
		t.Errorf("a reused operation the caller named answered %+v, want an invalid "+
			"refusal saying to make it as a new write", named)
	}
	if seat := writeFailure(t.Context(), Actor{TurnID: "run-1", WorkKey: "wk-1"}, UpdateWorkItemTool,
		reused); tools.RefusalOf(seat) != tools.RefusalUnavailable {
		t.Errorf("a seat's reused operation answered %+v, want the log's own class", seat)
	}
	// A REQUEST NAMED ITS OPERATION TOO — by the key its transport carries —
	// so it is the caller's to change, in its own terms: a new request under
	// a new key, never an `op_id` it has no way to leave out.
	request := writeFailure(t.Context(), Actor{WorkKey: "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b"},
		UpdateWorkItemTool, reused)
	if tools.RefusalOf(request) != tools.RefusalInvalid ||
		!strings.Contains(request.Output, "new request") ||
		strings.Contains(request.Output, "op_id") {
		t.Errorf("a request's reused operation answered %+v, want an invalid "+
			"refusal saying to send it as a new request", request)
	}
}

// A CONTENT REFUSAL IS THE WRITER'S SENTENCE. The invalid class is the one a
// caller corrects its call by — "the sentence names the argument" — so it must
// not carry the node-failure wording written for a failure the call did not
// meet, nor anything the caller never sent. A title
// filed from the dashboard at 600 bytes came back as "create_work_item did not
// land (worklet: the title on task 68c5… is 600 bytes …). The change was NOT
// made — do not report it as done." — a tool name, a Go error prefix, the id
// of a task that was never created, and an instruction meant for a model.
func TestAContentRefusalIsTheWritersSentence(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		err   error
		names []string
	}{
		{"a title", &worklet.TextCapError{Field: worklet.TextTitle, Size: 600,
			Limit: worklet.MaxTitle}, []string{"`title`", "600", "256", "title"}},
		{"a description", &worklet.TextCapError{Field: worklet.TextBody,
			Size: worklet.MaxBody + 1, Limit: worklet.MaxBody},
			[]string{"`body`", "description", fmt.Sprint(worklet.MaxBody)}},
		{"a comment", &worklet.TextCapError{Field: worklet.TextComment,
			Size: worklet.MaxCommentBody + 9, Limit: worklet.MaxCommentBody},
			[]string{"`body`", "comment", fmt.Sprint(worklet.MaxCommentBody + 9)}},
		{"any other content refusal", invalidDecision(),
			[]string{"a decision has 0 option(s)"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := writeFailure(t.Context(), Actor{}, CreateWorkItemTool, c.err)
			if tools.RefusalOf(got) != tools.RefusalInvalid {
				t.Fatalf("classed %q, want invalid", tools.RefusalOf(got))
			}
			for _, want := range c.names {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the sentence does not say %q: %s", want, got.Output)
				}
			}
			// NEITHER THE NODE-FAILURE WORDING NOR THE GO PREFIX: the
			// class is told as it stands, and "worklet:" is a word for a
			// log about where the error came from.
			for _, never := range []string{"did not land", "do not report it as done",
				"worklet:"} {
				if strings.Contains(got.Output, never) {
					t.Errorf("a content refusal carries %q: %s",
						never, got.Output)
				}
			}
		})
	}
	// THE TEXT CAP IS COMPOSED FROM ITS FIELDS, never the writer's Go error.
	got := writeFailure(t.Context(), Actor{}, CreateWorkItemTool, &worklet.TextCapError{
		Field: worklet.TextTitle, Size: 600, Limit: worklet.MaxTitle})
	for _, never := range []string{"worklet:", CreateWorkItemTool} {
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
		{fmt.Errorf("x: %w", doclet.ErrInvalid), tools.RefusalInvalid},
		{fmt.Errorf("x: %w", doclet.ErrTitleTaken), tools.RefusalExists},
		{fmt.Errorf("x: %w", doclet.ErrStaleVersion), tools.RefusalStaleVersion},
		{fmt.Errorf("x: %w", doclet.ErrConflict), tools.RefusalConflict},
		{fmt.Errorf("x: %w", doclet.ErrNotFound), tools.RefusalNotFound},
		// AND WHAT NO ARM NAMES, BY THE DOMAINS' ONE TABLE ([WriterClass]).
		{fmt.Errorf("x: %w", doclet.ErrBadCursor), tools.RefusalInvalid},
		{fmt.Errorf("x: %w", statelog.ErrExists), tools.RefusalExists},
		{&statelog.Unavailable{Reason: statelog.ReasonLogFull}, tools.RefusalUnavailable},
		{errors.New("doclet: read the container ENG: disk I/O"), tools.RefusalInternalError},
	} {
		if got := pageWriteFailure(t.Context(), "save_page", c.err, ""); !got.Failed || tools.RefusalOf(got) != c.want {
			t.Errorf("pageWriteFailure(%v) = failed %v class %q, want %q",
				c.err, got.Failed, tools.RefusalOf(got), c.want)
		}
	}
}

// EVERY CHAT SENTINEL HAS ITS OWN CLASS, on [TestPageWriteFailureClassesEachSentinel]'s
// reasoning — and the chat store wraps every refusal it decides on what was
// asked in one of them, so the unmarked remainder is this node's: a condition
// waiting clears, or a fault. Every row is shaped the way the store builds it:
// a field refusal from a payload's own validation, the rest wrapping their
// mark with the sentence after it.
//
// Mutation: drop any arm of chatClass and its row goes red.
func TestChatWriteFailureClassesEachSentinel(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		err  error
		want tools.Refusal
	}{
		{"a field refused", chatlet.ChannelPatch{}.Validate(), tools.RefusalInvalid},
		// A ROOM THAT TAKES NOTHING MORE is fixed by saying it somewhere
		// else — the argument — and is nobody's permission to ask for.
		{"an archived room", fmt.Errorf("%w: room-1 was archived", chatlet.ErrArchived),
			tools.RefusalInvalid},
		{"not this actor's", fmt.Errorf("%w: message m1 was written by ana",
			chatlet.ErrForbidden), tools.RefusalForbidden},
		// A ROOM A NEWER PEER MADE is this node's to serve once upgraded,
		// never a permission: told "forbidden", a caller stops asking for
		// something that lands the moment this node is upgraded.
		{"a room of a kind this build does not know", fmt.Errorf("%w: room-1 is "+
			"a \"guild\" room", chatlet.ErrUnknownKind), tools.RefusalUnavailable},
		{"a name somebody holds", fmt.Errorf("%w: launch", chatlet.ErrNameTaken),
			tools.RefusalExists},
		{"a conversation already open", fmt.Errorf("%w: dm-1", chatlet.ErrRoomExists),
			tools.RefusalExists},
		{"a lost race", fmt.Errorf("%w: room-1: %w", chatlet.ErrConflict,
			statelog.ErrConflict), tools.RefusalConflict},
		{"no such room", fmt.Errorf("%w: room room-1", chatlet.ErrNotFound),
			tools.RefusalNotFound},
		// THE LOG'S OWN REFUSAL is the node's condition, and anything
		// unmarked its fault: a person told to fix their input over a disk
		// error is the lie these rows pin.
		{"log refused", &statelog.Unavailable{Reason: statelog.ReasonDeferred},
			tools.RefusalUnavailable},
		{"timed out", fmt.Errorf("x: %w", context.DeadlineExceeded), tools.RefusalUnavailable},
		{"ledger unreadable", errors.New("statelog: read the operation ledger: " +
			"disk I/O"), tools.RefusalInternalError},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := chatWriteFailure(t.Context(), PostMessageTool, c.err, "")
			if !got.Failed || tools.RefusalOf(got) != c.want {
				t.Fatalf("chatWriteFailure(%v) = failed %v class %q, want %q",
					c.err, got.Failed, tools.RefusalOf(got), c.want)
			}
		})
	}
}

// AND A REFUSAL OF WHAT A CHAT CALL ASKED IS THE STORE'S OWN SENTENCE: the
// field, the rule or the room it names, without the package's Go prefix, and
// none of the wording a tool tells a model about a failure of the node, since
// nothing failed: the call was refused for what it asked.
func TestAChatRefusalIsTheStoresOwnSentence(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		got  tools.Result
		says string
	}{
		"invalid": {chatWriteFailure(t.Context(), PostMessageTool,
			chatlet.ChannelPatch{}.Validate(), ""), "`patch`: a patch sets no field"},
		"forbidden": {chatWriteFailure(t.Context(), ReactToMessageTool, fmt.Errorf("%w: "+
			"room-1 is a private room and jane is not in it", chatlet.ErrForbidden), ""),
			"room-1 is a private room and jane is not in it"},
		"a room this node cannot serve yet": {chatWriteFailure(t.Context(), PostMessageTool,
			fmt.Errorf("%w: room-1 is a \"guild\" room — upgrade this node",
				chatlet.ErrUnknownKind), ""),
			"room-1 is a \"guild\" room — upgrade this node"},
		"forbidden read": {chatReadFailure(t.Context(), ReadChannelTool, fmt.Errorf("%w: "+
			"viewer: this chat read resolved to no seat", chatlet.ErrForbidden)),
			"viewer: this chat read resolved to no seat"},
		"invalid read": {chatReadFailure(t.Context(), ReadChannelTool,
			chatlet.ChannelPatch{}.Validate()), "`patch`: a patch sets no field"},
	} {
		if !strings.Contains(c.got.Output, c.says) {
			t.Errorf("%s: the sentence does not say %q: %s", name, c.says, c.got.Output)
		}
		for _, never := range []string{"chatlet:", "did not land", "do not report it as done"} {
			if strings.Contains(c.got.Output, never) {
				t.Errorf("%s: a refusal of what was asked carries %q: %s", name, never,
					c.got.Output)
			}
		}
	}
}

// A CHAT WRITE NOBODY ANSWERED IS UNKNOWN, and nothing wrapping the answer
// turns it back. A post the router sent to a data node that never answered
// may have been said; send_dm's post after its open rewrote every node-class
// answer as "the message was NOT sent", and that class is the one an unknown
// outcome is answered in — so the mapper's unknown would reach the caller as a
// refusal saying the opposite.
func TestAChatWriteNobodyAnsweredIsUnknown(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("%w (chat.post, asked of data-1, data-2)", estate.ErrOutcomeUnknown)
	room := chatlet.Written{Channel: chatlet.Channel{ID: "dm-1"}}
	for name, got := range map[string]tools.Result{
		"a chat write": chatWriteFailure(t.Context(), PostMessageTool, err, ""),
		"a direct message's post after its open": directPostFailure(t.Context(),
			[]string{"sam"}, room, err, ""),
	} {
		if !tools.UnknownOf(got) || tools.RefusalOf(got) != tools.RefusalUnavailable {
			t.Errorf("%s nobody answered: failed %v unknown %v class %q, want an "+
				"unavailable refusal marked unknown", name, got.Failed,
				tools.UnknownOf(got), tools.RefusalOf(got))
		}
		for _, never := range []string{"nothing was written", "NOT sent", "was not said"} {
			if strings.Contains(got.Output, never) {
				t.Errorf("%s nobody answered says %q, which nobody can know: %s",
					name, never, got.Output)
			}
		}
	}
	// AND THE DIRECT MESSAGE STILL SAYS ITS CONVERSATION IS OPEN: the open is
	// a record that landed, and only the post after it is in doubt.
	if dm := directPostFailure(t.Context(), []string{"sam"}, room, err, ""); !strings.Contains(dm.Output, "is open (dm-1)") {
		t.Errorf("a direct message's unanswered post does not say its "+
			"conversation is open: %s", dm.Output)
	}
}

// THE SHARED REFUSALS ARE NEVER "NOT FOUND". A read this node could not serve
// and a backend this company does not run both say nothing about whether the
// object exists, and a surface told not-found would render it as gone. And a
// read this node could not serve is ONE OF TWO: a condition waiting clears is
// unavailable, and a store that broke is a fault.
func TestTheSharedRefusalsCarryTheirClass(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk I/O")
	behind := &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadSession}
	for name, c := range map[string]struct {
		got  tools.Result
		want tools.Refusal
	}{
		"outside a turn":        {notInATurn("update_work_item"), tools.RefusalForbidden},
		"no tracker":            {unconfigured("update_work_item"), tools.RefusalUnavailable},
		"no knowledge base":     {unconfiguredKB("save_page"), tools.RefusalUnavailable},
		"tracker read, behind":  {readFailure(t.Context(), "get_work_item", behind), tools.RefusalUnavailable},
		"page read, behind":     {pageReadFailure(t.Context(), "get_page", behind), tools.RefusalUnavailable},
		"tracker read, a fault": {readFailure(t.Context(), "get_work_item", boom), tools.RefusalInternalError},
		"page read, a fault":    {pageReadFailure(t.Context(), "get_page", boom), tools.RefusalInternalError},
		"no Chatlet":            {unconfiguredChat("post_message"), tools.RefusalUnavailable},
		"chat read, behind":     {chatReadFailure(t.Context(), "read_channel", behind), tools.RefusalUnavailable},
		"chat read, a fault":    {chatReadFailure(t.Context(), "read_channel", boom), tools.RefusalInternalError},
		// A CHAT CALLER WITH NOBODY TO ACT AS is forbidden whichever way
		// it got there: a seat outside a turn, or a request that carries
		// no person bound to a seat — whose sentence names the binding.
		"chat outside a turn": {chatActorFailure("post_message", turnctx.ErrNoSeat),
			tools.RefusalForbidden},
		"chat with nobody": {chatActorFailure("post_message",
			errors.New("builtin: this request carries nobody")), tools.RefusalForbidden},
		"bare failed": {failed("needs an `item`"), tools.RefusalInvalid},
	} {
		if !c.got.Failed || tools.RefusalOf(c.got) != c.want {
			t.Errorf("%s: failed %v class %q, want %q", name, c.got.Failed,
				tools.RefusalOf(c.got), c.want)
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
		{"no such comment", fmt.Errorf("x: %w", worklet.ErrNoComment), tools.RefusalNotFound},
		{"somebody else's question", fmt.Errorf("x: %w", worklet.ErrForbidden),
			tools.RefusalForbidden},
		{"already answered", fmt.Errorf("x: %w", worklet.ErrAlreadyAnswered),
			tools.RefusalAlreadyAnswered},
		{"not a question", fmt.Errorf("x: %w", worklet.ErrInvalid), tools.RefusalInvalid},
	} {
		got, ok := answerRefusal(c.err)
		if !ok || !got.Failed || tools.RefusalOf(got) != c.want {
			t.Errorf("%s: answerRefusal = %v failed %v class %q, want %q",
				c.name, ok, got.Failed, tools.RefusalOf(got), c.want)
		}
	}
	if _, ok := answerRefusal(errors.New("worklet: read the ask c1: disk I/O")); ok {
		t.Error("a read failure was classed as the reader's refusal of an answer")
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
	e := &worklet.ErrAmbiguousAnswer{Task: "ENG-1", Actor: "ana", Asks: []worklet.AskCandidate{
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

// invalidDecision is the tracker's refusal of a decision with no options, as
// its writer marks it: a content refusal that is no text cap.
func invalidDecision() error {
	_, err := worklet.ValidateDecision(context.Background(), decision.Decision{Question: "Which?"}, nil)
	return err
}
