package queries_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A CODING RUN'S QUESTION A PERSON ANSWERED IS ONE OF THEIR ANSWERED DECISIONS
// (ADR-0068), read back from the record of the answer, beside a work item's:
// what was asked with its options, the option chosen and, where nothing else
// was written, its label as the answer's words, who answered and when. Only an
// answer the run took, only within the window, and only one they gave: a run's
// question can be put to a whole team, and a teammate's answer is theirs.
//
// Mutation: list every `sandbox_run_answered` whatever its outcome, and the
// answer the run never used is listed as the decision.
func TestARunsAnsweredQuestionIsAnAnsweredDecision(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	now := time.Now().UTC()
	asked := &decision.Decision{Question: "Which suite?", Role: decision.RoleApprover,
		Recommended: "unit", Options: []decision.Option{
			{ID: "unit", Label: "The unit suite"}, {ID: "all", Label: "Every suite"}}}
	answered := func(id string, at time.Time, e types.SandboxRunAnswered) store.EventRecord {
		e.AgentHandle, e.TurnID, e.LaunchID = "swe", "t-"+id, "l-"+id
		e.Question, e.Decision, e.AskedAt = asked.Question, asked, at.Add(-time.Hour)
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		return store.EventRecord{ID: id, Type: e.EventType(), Source: "engine", Time: at,
			Category: "task", Actor: "SWE", Summary: "answered", Payload: raw,
			// TAGGED AS THE NODE'S WRITER TAGS IT, which is what the read
			// selects a person's answers by.
			Tags: store.ExtractTags(raw)}
	}
	recs := []store.EventRecord{
		answered("taken", now.Add(-time.Hour), types.SandboxRunAnswered{
			Outcome: types.AnswerResumed, AudienceHandles: []string{"ana"},
			AnsweredBy: "ana", AnsweredByKind: "human", Choice: "unit"}),
		answered("unused", now.Add(-2*time.Hour), types.SandboxRunAnswered{
			Outcome: types.AnswerNotAwaiting, AudienceHandles: []string{"ana"},
			AnsweredBy: "ana", AnsweredByKind: "human", Choice: "all"}),
		answered("theirs", now.Add(-3*time.Hour), types.SandboxRunAnswered{
			Outcome: types.AnswerResumed, AudienceHandles: []string{"bo"},
			AnsweredBy: "bo", AnsweredByKind: "human", Answer: "the slow one"}),
		answered("stale", now.Add(-worklet.AnsweredWindow-time.Hour), types.SandboxRunAnswered{
			Outcome: types.AnswerResumed, AudienceHandles: []string{"ana"},
			AnsweredBy: "ana", AnsweredByKind: "human", Choice: "all"}),
	}
	storetest.WriteEvents(t, db.Events(), recs)
	s := viewerSources(t, &stubWork{})
	s.Events = fleetOf(db.Events())

	got, err := askHolding(t, s, "ana", "answered_decisions", nil, iam.GrantWorklet)
	if err != nil {
		t.Fatalf("answered_decisions: %v", err)
	}
	items := got.(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("listed %d answered decisions, want the one run question ana answered: %+v",
			len(items), items)
	}
	item, ok := items[0].(queries.AnsweredRunAsk)
	if !ok {
		t.Fatalf("the item is %T, want a run's answered ask", items[0])
	}
	if item.Run.TurnID != "t-taken" || item.Run.Question != asked.Question ||
		item.Run.Decision == nil || item.Answer.Choice != "unit" ||
		item.Answer.Body != "The unit suite" || item.Answer.By != "ana" {
		t.Errorf("the answered run ask is %+v, want the question, its options, the choice "+
			"and its label as the answer, by ana", item)
	}
}
