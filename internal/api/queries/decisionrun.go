package queries

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A CODING RUN PARKED ON A QUESTION, AS THE PERSON IT IS PUT TO READS IT.
//
// `decisions` is a member question — it is what is waiting on the person
// reading — and the run row it used to carry was the sandbox board's whole
// record ([serialiseRun]): the bridge's tool log with every argument and
// output, the pushed branch, the coding CLI's session and command ids, the
// trace, the node that owns the run, the brief the agent was working from.
// That is what the machine PROCESSED (ADR-0031), and a teammate answering one
// question needs none of it: what was asked, about which task, by which agent,
// of whom, since when, how long the run keeps its place, and whether they can
// answer it in a thread. So the member reads [DecisionRun], whose every field
// is classified in decisionrun_internal_test.go; the board's row rides beside
// it as `run_detail` to an admin key only, which reads the board anyway.

// DecisionRun is a coding run parked on a question put to the reader.
type DecisionRun struct {
	// TurnID is the run's address: what `answer_run` is called with. An id
	// and nothing more — what it addresses (the trace, the turn) is an
	// admin's to open.
	TurnID string `json:"turn_id"`

	// Question is what the coding agent asked, in its words.
	Question string `json:"question"`

	// Task is the work item the run is charged to, null on a run charged to
	// none — see [DecisionTask].
	Task *DecisionTask `json:"task"`

	// Agent is the seat whose run it is.
	Agent string `json:"agent_handle"`

	// Asked is who the question is put to, resolved against the chart when
	// the run parked — always a list, empty where it resolved to nobody —
	// and AskedFallback whether that is the seat's lead chain because the
	// agent named nobody the chart has. The agent's own free label for its
	// audience is not served: it is the agent's word, which the resolution
	// already answered.
	Asked         []string `json:"asked"`
	AskedFallback bool     `json:"asked_fallback,omitempty"`

	// AskedAt is when the question was put — the anchor every answer is
	// measured against ([sandbox.PendingRun.AskedAt]).
	AskedAt time.Time `json:"asked_at"`

	// KeepsPlaceUntil is when the engine reclaims the run's held box — after
	// it an answer restarts the work from the question on a fresh one — and
	// NULL once nothing is held: the box was reclaimed, or never kept
	// (a zero pause TTL). COMPUTED HERE from the row, by the reaper's own
	// rule ([sandbox.PendingRun.HeldUntil]), and never by a screen from a
	// pause stamp and a TTL on its own clock: the box goes when the engine
	// says, and a browser counting down from what it was sent would be a
	// second clock with a second opinion.
	KeepsPlaceUntil *time.Time `json:"keeps_place_until"`

	// AnswerableInChat is whether a reply in a thread can reach the run, and
	// ChatSurface the surface that thread is on — `slack`, `mattermost`, or
	// the tracker or vendor whose thread it was asked from — empty when no
	// thread can. See [answerableInChat].
	AnswerableInChat bool   `json:"answerable_in_chat"`
	ChatSurface      string `json:"chat_surface,omitempty"`
}

// DecisionTask is the work item a parked run is charged to.
type DecisionTask struct {
	ID  string `json:"id"`
	Key string `json:"key"`

	// Title is the item's title as the tracker holds it now, read beside
	// the run — absent for an item this node's tracker does not hold (a
	// vendor tracker's), or when it could not be read: the run's row keeps
	// the item's key, never its title, because a title is renamed.
	Title string `json:"title,omitempty"`
}

// decisionRunOf is a parked run as a person reads it. titles names the
// native item each run is charged to, by run — see [Sources.runTitles].
func decisionRunOf(run sandbox.PendingRun, titles map[string]tracker.TurnPlace) DecisionRun {
	out := DecisionRun{
		TurnID:           run.TurnID,
		Question:         run.Question,
		Agent:            run.AgentHandle,
		Asked:            nonNilStrings(run.AudienceHandles),
		AskedFallback:    run.AudienceFallback,
		AskedAt:          run.AskedAt.UTC(),
		AnswerableInChat: answerableInChat(run.ConversationKey),
		ChatSurface:      notify.SourceOf(run.ConversationKey),
	}
	out.KeepsPlaceUntil = keepsPlaceUntil(run)
	if item := run.WorkItem; item != nil {
		out.Task = &DecisionTask{ID: item.ID, Key: item.Key}
		// THE TRACKER'S TITLE ONLY FOR THE ITEM THE ROW NAMES: the turn's
		// charged task and the row's item are one item by ADR-0022, and a
		// place that names another would put one task's title on another's
		// key.
		if place, ok := titles[run.TurnID]; ok && place.TaskID == item.ID {
			out.Task.Title = place.Title
		}
	}
	return out
}

// keepsPlaceUntil is when the engine reclaims a parked run's held box, or nil
// once nothing is held — [sandbox.PendingRun.HeldUntil], the reaper's own
// deadline, which the member's decision and the admin's board row both serve.
func keepsPlaceUntil(run sandbox.PendingRun) *time.Time {
	until, held := run.HeldUntil()
	if !held {
		return nil
	}
	until = until.UTC()
	return &until
}

// runTitles names the native task each run is charged to, by run — best
// effort, because the question is the decision and a tracker that could not
// be read leaves the cards naming their keys alone rather than failing the
// list of what waits on a person.
func (s Sources) runTitles(ctx context.Context, runs []sandbox.PendingRun) map[string]tracker.TurnPlace {
	if s.Work == nil {
		return nil
	}
	var ids []string
	for _, run := range runs {
		if run.WorkItem != nil {
			ids = append(ids, run.TurnID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	places, err := s.Work.TurnPlaces(ctx, ids, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		log.WarnContext(ctx, "decisions_run_titles_unread", "runs", len(ids), "error", err.Error())
		return nil
	}
	return places
}
