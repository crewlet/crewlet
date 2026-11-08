package queries_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

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
// The Sessions view under a seat's Memory is typed by `contract/sessions.ts`,
// and this holds it to both answers in both directions: every member it
// declares is a key some row sends, a required one on every row, and every key
// sent is declared — over the whole list and a page of it with a page after it,
// rows whose endings carry every field (a delivery not known, a newest capture
// not kept), a transcript page that is cut and one that could not be read,
// entries of every optional key (a subagent's, which only Claude Code's archive
// has, read as the entry type itself encodes), and the bounds, and the log
// position both answers carry — and a live row a relaunch would continue, which
// alone says where, beside the head of a conversation, which the next turn
// decides, the newest fire of a schedule, which its next fire decides, a row
// whose first launch was handed a session, and one no launch continues — every
// launch the screen names words for sent by some row. And the six closed sets
// the screen names words for are the engine's: the transcript's states, the
// entries' kinds, a page's cuts, an ending's outcomes, where a launch continues
// a session and which launch.
//
// Mutation: rename a JSON key on either answer, add one, add a state or a kind
// on one side only, drop the schedule's row from the fixture, or respell the
// schedule's prefix on one side, and this names it.
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
		Failure: "the round cap ran out", WentOn: true}
	missing := sealedSession(t, store, "ana", gzipText(t, openCodeExport("fix the flake")))
	missing.EndedAtPosition = statelog.Position{Stream: stubAt.Stream, Generation: 1, Seq: 1}
	if err := backend.Delete(t.Context(), missing.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}
	// A TURN WHOSE NEWEST CAPTURE WAS NOT KEPT, and whose reply nobody can
	// say reached the party waiting.
	unkept := sealedSession(t, store, "ana", gzipText(t, openCodeExport("look again")))
	unkept.Object, unkept.LaunchSeq, unkept.CaptureSeq = objstore.Object{}, 1, 1
	unkept.EndedAtPosition = statelog.Position{Stream: stubAt.Stream, Generation: 1, Seq: 3}
	unkept.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeFailed, DeliveredTo: "chatlet",
		DeliveryUnknown: true, Unkept: &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1,
			Reason: "refused: past the seat's budget"}, Exhausted: []string{"s-ran-out"}}
	// A SCHEDULE'S NEWEST FIRE, which the schedule's next fire would continue.
	fired := sealedSession(t, store, "ana", gzipText(t, openCodeExport("check the services")))
	fired.ConversationKey = notify.ScheduleConversation(types.ScheduleScopeRole, "a-1", "health-check")
	fired.EndedAtPosition = statelog.Position{Stream: stubAt.Stream, Generation: 1, Seq: 4}
	live := executordomain.Session{Handle: "ana", Turn: uuid.NewString(), Harness: "codex"}
	// A LIVE TURN A RELAUNCH WOULD CONTINUE, which alone sends where it would,
	// on the conversation whose head its first launch was handed.
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
	var endings, unkepts []map[string]any
	for _, row := range rows {
		if ending, ok := row["ending"].(map[string]any); ok {
			endings = append(endings, ending)
			if u, ok := ending["unkept"].(map[string]any); ok {
				unkepts = append(unkepts, u)
			}
		}
	}
	holdShape(t, "SessionEnding", endings, false)
	holdShape(t, "SessionUnkept", unkepts, false)

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
}
