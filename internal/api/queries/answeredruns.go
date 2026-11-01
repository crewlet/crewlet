package queries

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A CODING RUN'S QUESTION, ONCE ANSWERED, IS ONE OF A PERSON'S ANSWERED
// DECISIONS (ADR-0068).
//
// A run asks in the shape a work item's decision is asked, and the person who
// answered reads it back beside the work item asks they answered: what was
// asked, what they chose or said, who answered and when. The run's own row
// forgets its question once the run moves on, so the one account that outlives
// it is the record of the answer, `sandbox_run_answered`, which carries the
// question, its decision and the answer, and which every data node's event log
// keeps for the window this read covers ([worklet.AnsweredWindow]). Read from
// the FLEET's event log, never one node's ([FleetEvents]).
//
// ONLY ANSWERS THE RUN TOOK (`resumed`): an answer the run could not use was
// not the decision, and the question it was given to waited for another.

// AnsweredRunAsk is one coding run's question a person answered, as
// `answered_decisions` lists it beside a work item's ([worklet.AnsweredAsk]):
// the same `at` and `answer`, and the run's ask where a work item's carries
// its `ask`, so the Inbox draws both with one pane.
type AnsweredRunAsk struct {
	At     time.Time         `json:"at"`
	Run    RunAsked          `json:"run"`
	Answer worklet.AskAnswer `json:"answer"`
}

// RunAsked is a run's question as its answer's record states it.
type RunAsked struct {
	TurnID          string             `json:"turn_id"`
	LaunchID        string             `json:"launch_id"`
	AgentHandle     string             `json:"agent_handle"`
	Question        string             `json:"question"`
	QuestionContext string             `json:"question_context"`
	Decision        *decision.Decision `json:"decision,omitempty"`
	AskedAt         time.Time          `json:"asked_at"`
	WorkItem        *types.WorkItem    `json:"work_item,omitempty"`
}

// answeredRuns is the run questions handle answered within the window, shown
// to this reader as they see a run's question on the run's board
// ([serialiseRun]): whoever reads the seat's trail, and the person who
// answered it. Newest first, at most [worklet.MaxAnswered], with whether the
// window held more.
//
// SELECTED BY WHO ANSWERED, in the log's own read ([store.ListQuery.AnsweredBy],
// a range seek on schema/0053's index) and over the window alone, and only
// those read whole: a listing carries no payload, and the question and its
// answer are in it. Read through the tags instead, every answer every person
// gave in the window was paged through to keep one person's. By who answered
// rather than by whom it was put to, because a run's question can be put to a
// whole team, and one teammate's answer is that teammate's decision, not
// everybody's.
func (s Sources) answeredRuns(ctx context.Context, handle string, since time.Time,
) ([]AnsweredRunAsk, bool, error) {
	if s.Events == nil || handle == "" {
		return nil, false, nil
	}
	var (
		out    []AnsweredRunAsk
		before *store.Cursor
	)
	reach := s.runReach(ctx)
	for {
		listing, _, err := s.Events.List(ctx, store.ListQuery{
			Type: types.SandboxRunAnswered{}.EventType(), AnsweredBy: handle,
			Since: since, Limit: MaxEventPage, Before: before,
		})
		if err != nil {
			return nil, false, err
		}
		for _, rec := range listing.Rows {
			whole, _, err := s.Events.ByID(ctx, rec.ID)
			if errors.Is(err, store.ErrNotFound) {
				// SWEPT SINCE THE LISTING: nothing left to show.
				continue
			}
			if err != nil {
				return nil, false, err
			}
			item, ok := answeredRunOf(whole)
			if !ok {
				continue
			}
			if r := reach(item.run); !r.Body && r.Record != handle {
				continue
			}
			if len(out) == worklet.MaxAnswered {
				return out, true, nil
			}
			out = append(out, item.AnsweredRunAsk)
		}
		if !listing.More || len(listing.Rows) == 0 {
			return out, false, nil
		}
		last := listing.Rows[len(listing.Rows)-1]
		before = &store.Cursor{Time: last.Time, ID: last.ID}
	}
}

// answeredRunItem is one record read, with the seat its reach is decided by.
type answeredRunItem struct {
	AnsweredRunAsk
	run sandbox.PendingRun
}

// answeredRunOf reads one `sandbox_run_answered` record as an answered run
// ask, or false for one that is not: an answer the run did not take, or a
// record from before answers carried their question.
func answeredRunOf(rec store.EventRecord) (answeredRunItem, bool) {
	var e types.SandboxRunAnswered
	if json.Unmarshal(rec.Payload, &e) != nil || e.Outcome != types.AnswerResumed ||
		e.Question == "" {
		return answeredRunItem{}, false
	}
	body := e.Answer
	if body == "" && e.Decision != nil {
		// THE OPTION'S LABEL WHERE NOTHING ELSE WAS WRITTEN, as a work
		// item's answer reads ([worklet.AskAnswer.Body]).
		if option, ok := e.Decision.Option(e.Choice); ok {
			body = option.Label
		}
	}
	return answeredRunItem{
		AnsweredRunAsk: AnsweredRunAsk{
			At: rec.Time.UTC(),
			Run: RunAsked{TurnID: e.TurnID, LaunchID: e.LaunchID, AgentHandle: e.AgentHandle,
				Question: e.Question, QuestionContext: e.QuestionContext, Decision: e.Decision,
				AskedAt: e.AskedAt.UTC(), WorkItem: e.WorkItem},
			Answer: worklet.AskAnswer{By: e.AnsweredBy, ByKind: worklet.AuthorKind(e.AnsweredByKind),
				At: rec.Time.UTC(), Choice: e.Choice, Body: body},
		},
		// THE SEAT THE RUN WAS, which is all [Sources.runReach] decides
		// a run's reach by.
		run: sandbox.PendingRun{AgentHandle: e.AgentHandle},
	}, true
}
