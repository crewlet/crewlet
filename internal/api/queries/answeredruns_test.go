package queries_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
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
// Mutation: list every `executor_answered` whatever its outcome, and the
// answer the run never used is listed as the decision.
func TestARunsAnsweredQuestionIsAnAnsweredDecision(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	now := time.Now().UTC()
	asked := &decision.Decision{Question: "Which suite?", Role: decision.RoleApprover,
		Recommended: "unit", Options: []decision.Option{
			{ID: "unit", Label: "The unit suite"}, {ID: "all", Label: "Every suite"}}}
	answered := func(id string, at time.Time, e types.ExecutorAnswered) store.EventRecord {
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
		answered("taken", now.Add(-time.Hour), types.ExecutorAnswered{
			Outcome: types.AnswerResumed, AudienceHandles: []string{"ana"},
			AnsweredBy: "ana", AnsweredByKind: "human", Choice: "unit"}),
		answered("unused", now.Add(-2*time.Hour), types.ExecutorAnswered{
			Outcome: types.AnswerNotAwaiting, AudienceHandles: []string{"ana"},
			AnsweredBy: "ana", AnsweredByKind: "human", Choice: "all"}),
		answered("theirs", now.Add(-3*time.Hour), types.ExecutorAnswered{
			Outcome: types.AnswerResumed, AudienceHandles: []string{"bo"},
			AnsweredBy: "bo", AnsweredByKind: "human", Answer: "the slow one"}),
		answered("stale", now.Add(-worklet.AnsweredWindow-time.Hour), types.ExecutorAnswered{
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

// leadsBoth is a chart whose one lead leads two seats: a person, and the
// coding seat whose run asked them.
type leadsBoth struct {
	leadsChart
	also string
}

func (c leadsBoth) Leads(ctx context.Context, actor, subject string) (bool, error) {
	if actor == c.lead && subject == c.also {
		return true, nil
	}
	return c.leadsChart.Leads(ctx, actor, subject)
}

// A RUN'S ANSWERED QUESTION IS READ BY WHOEVER MAY READ BOTH THE PERSON AND
// THE RUN. Somebody else's answered decisions are their lead's or an admin's
// ([authz.ActionPersonRead]), and a run's question is the body of the seat
// whose run asked it, withheld wherever that body is (ADR-0068): the person
// who answered reads it as theirs, an admin and a lead of the run's seat read
// it in the person's list, and a lead of the person alone reads their
// decisions without that question in them, never a refusal of the whole list.
//
// Mutation: list the run's ask for any reader of the person, and bo's lead who
// does not lead swe reads swe's question; check only the seat's body, and bo
// does not read his own answer.
func TestARunsAnsweredQuestionReachesWhoMayReadTheRunAndThePerson(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	asked := &decision.Decision{Question: "Which suite?", Role: decision.RoleApprover,
		Recommended: "unit", Options: []decision.Option{
			{ID: "unit", Label: "The unit suite"}, {ID: "all", Label: "Every suite"}}}
	e := types.ExecutorAnswered{AgentHandle: "swe", TurnID: "t-1", LaunchID: "l-1",
		Question: asked.Question, Decision: asked, Outcome: types.AnswerResumed,
		AudienceHandles: []string{"bo"}, AnsweredBy: "bo", AnsweredByKind: "human",
		Choice: "unit", AskedAt: time.Now().UTC().Add(-2 * time.Hour)}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	storetest.WriteEvents(t, db.Events(), []store.EventRecord{{ID: "answered",
		Type: e.EventType(), Source: "engine", Time: time.Now().UTC().Add(-time.Hour),
		Category: "task", Actor: "SWE", Summary: "answered", Payload: raw,
		Tags: store.ExtractTags(raw)}})
	sources := func(chart authz.Chart) queries.Sources {
		s := viewerSources(t, &stubWork{})
		s.Events = fleetOf(db.Events())
		if chart != nil {
			s.Chart = chart
		}
		return s
	}
	lead := leadsChart{lead: "ana", report: "bo"}
	for _, tc := range []struct {
		name   string
		s      queries.Sources
		reader string
		grants []iam.Grant
		listed bool
	}{
		{"the person who answered", sources(nil), "bo", nil, true},
		{"a lead of the person and the run's seat", sources(leadsBoth{lead, "swe"}), "ana",
			[]iam.Grant{iam.GrantWorklet}, true},
		{"a lead of the person alone", sources(lead), "ana", []iam.Grant{iam.GrantWorklet}, false},
		{"an admin", sources(nil), "ana", []iam.Grant{iam.GrantAdmin}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := askHolding(t, tc.s, tc.reader, "answered_decisions",
				map[string]any{"handle": "bo"}, tc.grants...)
			if err != nil {
				t.Fatalf("answered_decisions of bo: %v", err)
			}
			items := got.(map[string]any)["items"].([]any)
			if listed := len(items) == 1; listed != tc.listed {
				t.Fatalf("%s read %d answered decisions of bo, want the run's question listed: %v",
					tc.reader, len(items), tc.listed)
			}
			if tc.listed && items[0].(queries.AnsweredRunAsk).Run.Question != asked.Question {
				t.Errorf("the listed ask is %+v, want swe's question", items[0])
			}
		})
	}
}
