package queries_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// THE SESSIONS SCREEN READS WHAT THE ANSWERS SEND.
//
// The Sessions view under a seat's Memory is typed by `contract/sessions.ts`,
// and this holds it to both answers in both directions: every member it
// declares is a key some row sends, a required one on every row, and every key
// sent is declared — over the whole list and a page of it with a page after it,
// rows whose endings carry every field, a transcript page that is cut and one
// that could not be read, entries of every optional key (a subagent's, which
// only Claude Code's archive has, read as the entry type itself encodes), and
// the bounds. And the four closed sets the screen names words for are the
// engine's: the transcript's states, the entries' kinds, a page's cuts and an
// ending's outcomes.
//
// Mutation: rename a JSON key on either answer, add one, or add a state or a
// kind on one side only, and this names it.
func TestTheSessionsScreenReadsWhatTheAnswersSend(t *testing.T) {
	t.Parallel()
	store, backend := sessionObjectStore(t)
	texts := []string{strings.Repeat("a long brief ", 2<<10)}
	for i := 1; i <= executor.TranscriptMaxEntries; i++ {
		texts = append(texts, fmt.Sprintf("message %d", i))
	}
	read := sealedSession(t, store, "ana", gzipText(t, openCodeExport(texts...)))
	read.Ending = &executordomain.Ending{Outcome: executordomain.OutcomeStopped, Decision: "done",
		Delivered: true, DeliveredTo: "chatlet", Answer: "answered", CompletedWork: "Filed ENG-7.",
		BlockedOn: "the review", StoppedBy: "ana.diaz", StopReason: "wrong branch",
		Failure: "the round cap ran out"}
	missing := sealedSession(t, store, "ana", gzipText(t, openCodeExport("fix the flake")))
	if err := backend.Delete(t.Context(), missing.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}
	live := executordomain.Session{Handle: "ana", Turn: uuid.NewString(), Harness: "codex"}
	reads := &stubSessions{rows: []executordomain.Session{read, missing, live}}
	r := queries.NewRegistry()
	queries.Register(r, sessionSources(t, reads, store))

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
	var endings []map[string]any
	for _, row := range rows {
		if ending, ok := row["ending"].(map[string]any); ok {
			endings = append(endings, ending)
		}
	}
	holdShape(t, "SessionEnding", endings, false)

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

	for name, engine := range map[string][]string{
		"TranscriptState":     stringsOf(queries.TranscriptStates),
		"TranscriptEntryKind": stringsOf(executor.EntryKinds),
		"TranscriptCut":       stringsOf(executor.TranscriptCuts),
		"SessionOutcome":      stringsOf(executordomain.Outcomes),
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
}
