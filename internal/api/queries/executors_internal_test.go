package queries

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY KEY AN EXECUTOR'S ROW CARRIES IS CLASSIFIED, in both directions, and
// what a run was asked, asked back and answered is its seat's BODY (ADR-0033).
//
// The row is the board everybody reads, so a key added to [runRow] reaches
// everybody's overview the moment it is added — unless the table says what it
// is. A key the row writes that [runKeys] does not name fails here, as does a
// name the table keeps for a key the row no longer writes (a classification of
// nothing, which reads as a decision somebody made). And the four the owner's
// decision lists as a run's body, and the entry the job was launched on, which
// is the company's configuration, are held to it by name, because the
// classification itself is what a careless edit would move.
//
// Mutation: add a key to runRow, or drop a row of runKeys, and this goes red;
// classify `task_description` or `provider_key` as status and it goes red too.
func TestEveryExecutorRowKeyIsClassified(t *testing.T) {
	t.Parallel()
	// A RUN THAT ASKED WITH A CHOICE, about a call it holds, because
	// `decision`, `held_call` and `held_arguments` are the keys the row
	// leaves out rather than sends empty (absent, never null).
	row := runRow(sandbox.PendingRun{Decision: &decision.Decision{}, HeldCall: &sandbox.HeldCall{}},
		sandbox.BridgeLog{}, runOpened{})
	for key := range row {
		shown, ok := runKeys[key]
		if !ok {
			t.Errorf("the run row carries %q and runKeys does not classify it — "+
				"say whether it is status, body, the question or the tracker's", key)
			continue
		}
		if shown < runStatus || shown > runConfig {
			t.Errorf("%q is classified %d, which is no class", key, shown)
		}
	}
	for key := range runKeys {
		if _, ok := row[key]; !ok {
			t.Errorf("runKeys classifies %q and the run row does not carry it", key)
		}
	}
	for key, want := range map[string]runShown{
		"task_description": runBody,
		"reply":            runBody,
		"bridge_calls":     runBody,
		"question":         runQuestion,
		"held_call":        runQuestion,
		"held_arguments":   runBody,
		"work_item":        runTracker,
		"provider_key":     runConfig,
	} {
		if got := runKeys[key]; got != want {
			t.Errorf("%q is classified %d, want %d", key, got, want)
		}
	}
}

// A READER WITH NO REACH IS SHOWN STATUS AND NOTHING ELSE, and a reader with
// every reach is shown the whole row: the mask withholds by the table and by
// nothing else.
//
// Mutation: drop the default arm's delete in serialiseRun and add an
// unclassified key, and the first half goes red.
func TestAMaskedRunCarriesOnlyItsStatus(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe",
		AudienceHandles: []string{"ana"}, Decision: &decision.Decision{},
		HeldCall: &sandbox.HeldCall{ID: "held-1", Server: "tracker", Tool: "tr_close_issue"}}
	status := serialiseRun(run, sandbox.BridgeLog{}, livestate.Reach{}, runOpened{})
	for key := range status {
		if runKeys[key] != runStatus {
			t.Errorf("a reader with no reach was shown %q, which is not status", key)
		}
	}
	if status["body_withheld"] != true {
		t.Errorf("body_withheld = %v for a reader shown no body", status["body_withheld"])
	}
	whole := serialiseRun(run, sandbox.BridgeLog{}, livestate.Reach{Body: true, Tracker: true, Config: true}, runOpened{})
	if got, want := slices.Sorted(maps.Keys(whole)), slices.Sorted(maps.Keys(runKeys)); !slices.Equal(got, want) {
		t.Errorf("a reader with every reach was shown %v, want every key %v", got, want)
	}
	if whole["body_withheld"] != false {
		t.Errorf("body_withheld = %v for a reader shown the body", whole["body_withheld"])
	}
}

// WHAT BECAME OF AN ANSWER IS ON THE RUN'S ROW: the answer recorded on it (by
// its deliveries, who gave it and when) and the answers its question let go
// of. A person who answered reads their own answer's fate there, by the id
// answer_executor gave them, and the first recorded answer is shown as the one
// that won. Ids and names only, never what anybody wrote.
//
// WHO GAVE IT, AS ITS SEAL SAYS: the answerer is sealed with the answer and
// kept nowhere in the clear, so a node whose keyring does not open the seal
// names nobody rather than guessing.
//
// Mutation: name the answerer whatever the seal says, and the unopened row
// names somebody.
func TestARunRowSaysWhatBecameOfItsAnswers(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 28, 0, 35, 6, 0, time.UTC)
	seal := answerSealOf(t, "the fleet's key material")
	run := sandbox.PendingRun{TurnID: "t1", LaunchID: "launch-t1", Status: sandbox.StatusAnswered,
		DeclinedAnswers: []string{"a0"}}
	answer, err := seal.Seal(run, sandbox.AnswerWords{Text: "use main", Via: types.AnswerViaOperator,
		By: "miles", ByKind: "human"}, sandbox.RecordedAnswer{RecordedAt: at, EventIDs: []string{"a1"}})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	run.Answer = &answer
	sources := Sources{RunAnswers: seal}
	row := runRow(run, sandbox.BridgeLog{}, sources.opened(run))
	if got := row["answer_ids"]; !slices.Equal(got.([]string), []string{"a1"}) {
		t.Errorf("answer_ids = %v, want the recorded answer's delivery", got)
	}
	if row["answered_by"] != "miles" || row["answered_at"] != isoOrEmpty(at) {
		t.Errorf("answered_by %v at %v, want miles at %s", row["answered_by"], row["answered_at"], at)
	}
	if got := row["declined_answers"]; !slices.Equal(got.([]string), []string{"a0"}) {
		t.Errorf("declined_answers = %v, want the answer the question let go of", got)
	}
	for key, value := range row {
		if s, ok := value.(string); ok && s == "use main" {
			t.Errorf("%s carries what the answer said", key)
		}
	}
	elsewhere := Sources{RunAnswers: answerSealOf(t, "another fleet's key material")}
	if by := elsewhere.answeredBy(run); by != "" {
		t.Errorf("a seal this node cannot open names %q as the answerer, want nobody", by)
	}
	if by := (Sources{}).answeredBy(run); by != "" {
		t.Errorf("a node with no seal names %q as the answerer, want nobody", by)
	}
	unanswered := sandbox.PendingRun{TurnID: "t2", Status: sandbox.StatusAwaiting}
	empty := runRow(unanswered, sandbox.BridgeLog{}, sources.opened(unanswered))
	if got := empty["answer_ids"]; got == nil || len(got.([]string)) != 0 || empty["answered_by"] != "" {
		t.Errorf("a run with no recorded answer shows %v by %v, want empty", got, empty["answered_by"])
	}
}

// A RUN ROW SAYS ITS ANSWER IS A REFUSAL (ADR-0114). A reply too large for its
// run is recorded as the question's outcome in the answer's place, and the row
// names it as an answer is named — its deliveries, its sender and when — so a
// reader tells the two apart by `answer_refused` alone: how many messages the
// reply was and its two figures, never its words, and null on a row whose
// answer is one.
//
// Mutation: leave the key off the row, and a refusal reads on every board as
// an answer its run carries on with.
func TestARunRowSaysItsAnswerIsARefusal(t *testing.T) {
	t.Parallel()
	seal := answerSealOf(t, "the fleet's key material")
	run := sandbox.PendingRun{TurnID: "t1", LaunchID: "launch-t1", Status: sandbox.StatusAnswered}
	refusal := sandbox.AnswerRefusal{EventIDs: []string{"a1", "a2"}, Bytes: 600 << 10, MaxBytes: 512 << 10}
	answer, err := seal.Seal(run, sandbox.AnswerWords{Via: types.AnswerViaChat, By: "miles", ByKind: "human",
		Refusal: &refusal}, sandbox.RecordedAnswer{Via: types.AnswerViaChat, EventIDs: refusal.EventIDs,
		RecordedAt: time.Date(2026, 10, 28, 0, 35, 6, 0, time.UTC)})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	run.Answer, run.AnswerRefusal = &answer, &refusal
	sources := Sources{RunAnswers: seal}
	row := runRow(run, sandbox.BridgeLog{}, sources.opened(run))
	got, ok := row["answer_refused"].(map[string]any)
	if !ok || got["messages"] != 2 || got["bytes"] != 600<<10 || got["max_bytes"] != 512<<10 {
		t.Errorf("answer_refused = %v, want two messages at 600 KiB past 512 KiB", row["answer_refused"])
	}
	if ids := row["answer_ids"]; !slices.Equal(ids.([]string), refusal.EventIDs) || row["answered_by"] != "miles" {
		t.Errorf("the refused reply is named as %v by %v, want its deliveries and its sender", ids,
			row["answered_by"])
	}
	plain := sandbox.PendingRun{TurnID: "t2", LaunchID: "launch-t2", Status: sandbox.StatusAnswered}
	words, err := seal.Seal(plain, sandbox.AnswerWords{Text: "use main", Via: types.AnswerViaChat},
		sandbox.RecordedAnswer{Via: types.AnswerViaChat, EventIDs: []string{"b1"}})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	plain.Answer = &words
	if got := runRow(plain, sandbox.BridgeLog{}, sources.opened(plain))["answer_refused"]; got != nil {
		t.Errorf("a row answered with words carries answer_refused %v, want null", got)
	}
}

// answerSealOf is a fleet keyring's answer seal, under key material of its own.
func answerSealOf(t *testing.T, material string) *sandbox.AnswerSeal {
	t.Helper()
	seal, err := sandbox.NewAnswerSeal(recordseal.OneKey("answers", material))
	if err != nil {
		t.Fatalf("NewAnswerSeal: %v", err)
	}
	return seal
}
