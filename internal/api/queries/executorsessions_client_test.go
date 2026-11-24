package queries_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/executorread"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SESSIONS SCREEN READS WHAT THE ANSWERS SEND.
//
// `contract/sessions.ts` types the Sessions view; every member it declares is
// a key some row sends (a required one on every row) and every key sent is
// declared. Fixtures cover a page with a page after it, every ending and
// optional entry key, a cut and an unreadable transcript, a trimmed session,
// and each launch kind the screen words beside a row in progress. The closed
// sets and the words the screen says as the engine does are the engine's.
//
// Mutation: rename a JSON key on either answer, add one, add a state or a kind
// on one side only, drop the schedule's row or the row in progress, or respell
// the schedule's prefix or the live words on one side, and this names it.
func TestTheSessionsScreenReadsWhatTheAnswersSend(t *testing.T) {
	t.Parallel()
	store, backend := sessionObjectStore(t)
	texts := []string{strings.Repeat("a long brief ", 2<<10)}
	for i := 1; i <= executor.TranscriptMaxEntries; i++ {
		texts = append(texts, fmt.Sprintf("message %d", i))
	}
	// THE CONVERSATION'S HEAD, the newest of its turns that left a session — a
	// session its turn went on past, stopped once its box was gone.
	read := sealedSession(t, store, "ana", gzipText(t, openCodeExport(texts...)))
	read.EndedAtPosition = statelog.Position{Stream: stubAt.Stream, Generation: 1, Seq: 2}
	read.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, Decision: "done",
		Delivered: true, DeliveredTo: "chatlet", Answer: "answered", CompletedWork: "Filed ENG-7.",
		BlockedOn: "the review", StoppedBy: "ana.diaz", StopReason: "wrong branch",
		Failure: "the round cap ran out", WentOn: true,
		NotContinued: []executorkeep.NotContinued{
			{Loss: executorkeep.LossTooLarge, Instead: executorkeep.InsteadOlder, LostTurn: "t-lost",
				OlderTurn: "t-older", Detail: "the session is past its bound"},
			{Loss: executorkeep.LossCaptureLate, Instead: executorkeep.InsteadOlder,
				Older: &executorkeep.CapturePlace{LaunchSeq: 1, CaptureSeq: 1}}}}
	missing := sealedSession(t, store, "ana", gzipText(t, openCodeExport("fix the flake")))
	missing.EndedAtPosition = statelog.Position{Stream: stubAt.Stream, Generation: 1, Seq: 1}
	if err := backend.Delete(t.Context(), missing.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}
	// A TURN WHOSE NEWEST CAPTURE, ITS LAST WORD, WAS NOT KEPT, and whose
	// reply nobody can say reached the party waiting.
	unkept := sealedSession(t, store, "ana", gzipText(t, openCodeExport("look again")))
	unkept.Object, unkept.LaunchSeq, unkept.CaptureSeq = objstore.Object{}, 1, 1
	unkept.EndedAtPosition = statelog.Position{Stream: stubAt.Stream, Generation: 1, Seq: 3}
	unkept.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeFailed, DeliveredTo: "chatlet",
		DeliveryUnknown: true, Unkept: &executorkeep.Unkept{Loss: executorkeep.LossStoreRefused, LaunchSeq: 2, CaptureSeq: 1,
			Reason: "refused: past the seat's budget"}, WentOn: true,
		LastWord:      &executorkeep.CapturePlace{LaunchSeq: 2, CaptureSeq: 1},
		SpentSessions: []executorkeep.SpentSession{{Session: "s-ran-out", Loss: executorkeep.LossContextExhausted}}}
	// A SCHEDULE'S NEWEST FIRE, which the schedule's next fire would continue.
	fired := sealedSession(t, store, "ana", gzipText(t, openCodeExport("check the services")))
	fired.ConversationKey = notify.ScheduleConversation(types.ScheduleScopeRole, "a-1", "health-check")
	fired.EndedAtPosition = statelog.Position{Stream: stubAt.Stream, Generation: 1, Seq: 4}
	// ITS SESSION TRIMMED past the bound, which alone sends the trim's note.
	fired.Trimmed = true
	live := executordomain.Session{Handle: "ana", Turn: uuid.NewString(), Harness: "custom"}
	// A LIVE TURN A RELAUNCH WOULD CONTINUE, which alone sends where it would
	// and is in progress, on the conversation whose head its first launch was
	// handed.
	continued, continuedRun := liveSession(executor.ClaudeCodeName)
	continued.ConversationKey, continued.BaseTurn = read.ConversationKey, read.Turn
	reads := &stubSessions{rows: []executordomain.Session{read, missing, unkept, fired, live, continued}}
	sources := sessionSources(t, reads, store)
	sources.SessionRuns = &stubRuns{runs: []sandbox.PendingRun{continuedRun}}
	sources.RelaunchTargets = targetOf(claudeTarget())
	r := queries.NewRegistry()
	queries.Register(r, sources)

	listed, err := r.Answer(asSeat(t, "ana"), "executor_sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	list := asMap(t, listed)
	// A PAGE WITH ONE AFTER IT, which alone sends the cursor to it.
	paged, err := r.Answer(asSeat(t, "ana"), "executor_sessions", map[string]any{"limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	holdShape(t, "ExecutorSessionsAnswer", []map[string]any{list, asMap(t, paged)}, false)
	rows := rowsOf(t, list["sessions"])
	holdShape(t, "ExecutorSessionRow", rows, false)
	for _, by := range sandbox.ContinuedBys {
		if !slices.ContainsFunc(rows, func(row map[string]any) bool { return row["continued_by"] == string(by) }) {
			t.Errorf("no row of the page says `continued_by: %s`, so the screen's words for it are held to "+
				"nothing sent", by)
		}
	}
	// A ROW IN PROGRESS AND ONE THAT IS NOT, so the screen's reading of
	// `in_progress` is held to both.
	for _, progress := range []bool{true, false} {
		if !slices.ContainsFunc(rows, func(row map[string]any) bool { return row["in_progress"] == progress }) {
			t.Errorf("no row of the page says `in_progress: %v`, so the screen's reading of it is held to "+
				"nothing sent", progress)
		}
	}
	var endings, unkepts, verdicts, spent []map[string]any
	for _, row := range rows {
		if ending, ok := row["ending"].(map[string]any); ok {
			endings = append(endings, ending)
			if u, ok := ending["unkept"].(map[string]any); ok {
				unkepts = append(unkepts, u)
			}
			if said, ok := ending["not_continued"]; ok {
				verdicts = append(verdicts, rowsOf(t, said)...)
			}
			if sessions, ok := ending["spent_sessions"]; ok {
				spent = append(spent, rowsOf(t, sessions)...)
			}
		}
	}
	holdShape(t, "SessionEnding", endings, false)
	holdShape(t, "SessionUnkept", unkepts, false)
	holdShape(t, "SessionNotContinued", verdicts, false)
	holdShape(t, "SessionSpent", spent, false)

	var answers, entries []map[string]any
	for _, turn := range []string{read.Turn, missing.Turn} {
		got, err := r.Answer(asSeat(t, "ana"), "executor_session", map[string]any{"turn_id": turn})
		if err != nil {
			t.Fatal(err)
		}
		answer := asMap(t, got)
		answers = append(answers, answer)
		entries = append(entries, rowsOf(t, answer["entries"])...)
	}
	// A SUBAGENT'S ENTRY, which only a Claude Code archive holds: the entry
	// type as the answer encodes it.
	entries = append(entries, asMap(t, executor.TranscriptEntry{Kind: executor.EntryModel,
		Thread: "agent-a1", Tool: "Task", Failed: true, Text: "found it", Bytes: 8}))
	holdShape(t, "ExecutorSessionAnswer", answers, false)
	holdShape(t, "TranscriptEntry", entries, false)
	bounds, ok := answers[0]["bounds"].(map[string]any)
	if !ok {
		t.Fatalf("the answer's bounds are %v", answers[0]["bounds"])
	}
	holdShape(t, "TranscriptBounds", []map[string]any{bounds}, false)
	// THE POSITION BOTH ANSWERS CARRY is the engine's statelog.Position, and
	// the dashboard declares it once, for every answer that carries one.
	var positions []map[string]any
	for _, answer := range append([]map[string]any{list}, answers...) {
		positions = append(positions, asMap(t, answer["position"]))
	}
	holdShape(t, "LogPosition", positions, false)

	for name, engine := range map[string][]string{
		"TranscriptState":     stringsOf(executorread.TranscriptStates),
		"TranscriptEntryKind": stringsOf(executor.EntryKinds),
		"TranscriptCut":       stringsOf(executor.TranscriptCuts),
		"SessionOutcome":      stringsOf(executorkeep.Outcomes),
		"SessionContinuesIn":  stringsOf(sandbox.ContinuesIns),
		"SessionContinuedBy":  stringsOf(sandbox.ContinuedBys),
		"SessionLoss":         stringsOf(executorkeep.Losses),
		"SessionInstead":      stringsOf(executorkeep.Insteads),
	} {
		got, err := clientsource.Union(clientsource.Tree(t), name)
		if err != nil {
			t.Fatalf("%v — this gate cannot run without the client's declaration", err)
		}
		slices.Sort(got)
		if want := slices.Sorted(slices.Values(engine)); !slices.Equal(got, want) {
			t.Errorf("the dashboard's %s is %v; the engine sends %v", name, got, want)
		}
	}
	// AND A SCHEDULE'S KEY BY THE ENGINE'S OWN PREFIX, which a row prints
	// decoded: under another, a fire's conversation would print escaped.
	prefix, err := clientsource.Scalar(clientsource.Tree(t), "SCHEDULE_CONVERSATION_PREFIX")
	if err != nil {
		t.Fatalf("%v — this gate cannot run without the client's declaration", err)
	}
	if prefix != notify.SchedulePrefix {
		t.Errorf("the dashboard knows a schedule's conversation by %q; the engine keys one %q", prefix,
			notify.SchedulePrefix)
	}
	// AND A SESSION IN PROGRESS KEPT AGAIN BY THE ENGINE'S OWN WORDS, which the
	// screen's banners say of a transcript a newer capture has passed: under
	// others, the screen and the engine's refusal of the same page would tell a
	// reader two things.
	kept, err := clientsource.Scalar(clientsource.Tree(t), "LIVE_KEPT")
	if err != nil {
		t.Fatalf("%v — this gate cannot run without the client's declaration", err)
	}
	if kept != executorread.LiveKept {
		t.Errorf("the dashboard tells a reader a live session was kept since as %q; the engine says %q", kept,
			executorread.LiveKept)
	}
	// AND EVERY LOSS IN THE ENGINE'S OWN CLAUSE, which the screen's sentence of a
	// launch's verdict is built from: under others, a launch card and the
	// session's note would say one loss two ways.
	words, err := clientsource.Literal(clientsource.Tree(t), "SESSION_LOSS_WORDS")
	if err != nil {
		t.Fatalf("%v — this gate cannot run without the client's declaration", err)
	}
	for _, loss := range executorkeep.Losses {
		said := clientsource.Scalars(words, string(loss))
		if len(said) != 1 || said[0] != loss.Words() {
			t.Errorf("the dashboard words %s as %q; the engine says %q", loss, said, loss.Words())
		}
	}
	if unnamed, err := clientsource.Scalar(clientsource.Tree(t), "SESSION_UNNAMED_LOSS"); err != nil ||
		unnamed != executorkeep.UnnamedLossWords {
		t.Errorf("the dashboard words a loss it does not name %q (%v); the engine says %q", unnamed, err,
			executorkeep.UnnamedLossWords)
	}
}

// THE DASHBOARD NEVER ABANDONS A READ THE ENGINE MAY STILL ANSWER: a transcript
// page's query waits at least as long as a person's read of a session can take
// to answer at its bounds ([executor.TranscriptAnsweredWithin]), the slowest
// read of what harnesses write behind one slot's wait; a read past it fails
// closed. A screen that gave up first showed a session a harness wrote as
// failed, and its frame's wait ended the read the engine was making.
//
// Mutation: wait QUERY_TIMEOUT_MS for a transcript, or raise the engine's
// figure past the dashboard's, and this goes red.
func TestTheDashboardWaitsPastAPersonsReadOfASession(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Scalar(clientsource.Tree(t), "TRANSCRIPT_QUERY_TIMEOUT_MS")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := strconv.ParseInt(strings.ReplaceAll(body, "_", ""), 10, 64)
	if err != nil {
		t.Fatalf("TRANSCRIPT_QUERY_TIMEOUT_MS = %q is not a number: %v", body, err)
	}
	if wait := time.Duration(ms) * time.Millisecond; wait < executor.TranscriptAnsweredWithin {
		t.Errorf("the dashboard waits %s for a transcript page the engine may take %s to answer", wait,
			executor.TranscriptAnsweredWithin)
	}
}
