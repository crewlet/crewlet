package engine

import (
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Which work item a turn is on (ADR-0022): ONE item or none, never split (a
// split would invent a ratio), by the first of these rules that names one:
//
//  1. TRIGGER: the wake names it — a Worklet wake, a code wake on a change
//     implementing one (ADR-0097), a Jira, GitHub or GitLab issue webhook.
//  2. ASKED_BY: an answer to a colleague's ask inherits the asker's item.
//  3. RESUME: the parked executor's row recorded it.
//  4. SOLE_WRITE: the turn's writes committed to exactly one; at completion
//     only, when "exactly one" is a fact; see [completedWorkItem].
//
// Rules 1-3 read what existed before the turn ran, so they resolve at dispatch
// and stamp every event; a turn on nothing leaves `work_item` absent, not null.

// workItemNamer is what a notification source declares when its wakes can be
// about one work item: the item a wake's metadata names, if any. Each source's
// own prompt implements it, as the owner of its metadata keys; a source without
// it (chat, a pager) names no item.
type workItemNamer interface {
	WorkItem(metadata map[string]string) (types.WorkItem, bool)
}

// workItemNamers are the sources whose wakes can name an item, by source: a
// fixed set rather than this node's routed prompts, because a wake was routed
// by whichever node took the webhook, and an integration disconnected since
// does not change which issue it was about.
var workItemNamers = func() map[string]workItemNamer {
	namers := map[string]workItemNamer{}
	for _, source := range []interface {
		workItemNamer
		Source() string
	}{worklet.Prompt{}, gitlet.Prompt{}, jira.Prompt{}, github.Prompt{}, gitlab.Prompt{}} {
		namers[source.Source()] = source
	}
	return namers
}()

// workItemOf resolves rules 1 and 2 for one dispatch, off the partition's
// first event: the one [Engine.describeTurn] takes the trigger from, so a
// turn's record cannot disagree with itself about what woke it.
func workItemOf(req Request) (*types.WorkItem, types.WorkItemBasis) {
	for _, ev := range req.Events {
		if ev == nil {
			continue
		}
		return workItemOfEvent(ev)
	}
	return nil, ""
}

// workItemOfEvent is the item one wake names, and the rule that names it.
//
// OFF THE TYPED PAYLOAD, never the envelope's free-form bag, for the reason
// the delivery obligation reads it that way (see [ReplyFor]).
func workItemOfEvent(ev *events.Event) (*types.WorkItem, types.WorkItemBasis) {
	if n, ok := events.DataAs[*types.ExternalNotification](ev); ok {
		namer, known := workItemNamers[n.NotificationSource]
		if !known {
			return nil, ""
		}
		item, named := namer.WorkItem(n.Metadata)
		if !named || item.ID == "" {
			return nil, ""
		}
		return &item, types.BasisTrigger
	}
	if ask, ok := events.DataAs[*types.A2ARequest](ev); ok {
		if ask.WorkItem == nil || ask.WorkItem.ID == "" {
			return nil, ""
		}
		// A COPY: the event is shared with every reader of this delivery.
		item := *ask.WorkItem
		return &item, types.BasisAskedBy
	}
	return nil, ""
}

// resumedWorkItem is rule 3: the item a parked run recorded at launch.
//
// A run launched by a turn charged to no item resumes on nothing, as its
// first half was.
func resumedWorkItem(run sandbox.PendingRun) (*types.WorkItem, types.WorkItemBasis) {
	if run.WorkItem == nil || run.WorkItem.ID == "" {
		return nil, ""
	}
	item := *run.WorkItem
	return &item, types.BasisResume
}

// completedWorkItem is the item a turn's completion is charged to: the one it
// was on from dispatch, or — rule 4 — the one item its writes committed to.
//
// A parked segment concludes no sole write: the resumed segment may write a
// second item. The writes before a park ride execstate.State.Written into the
// resumed segment's set, so "exactly one" is judged over the whole turn.
func completedWorkItem(item *types.WorkItem, basis types.WorkItemBasis,
	written *turnctx.Written, suspended bool,
) (*types.WorkItem, types.WorkItemBasis) {
	if item != nil {
		return item, basis
	}
	if suspended {
		return nil, ""
	}
	sole, ok := written.Sole()
	if !ok {
		return nil, ""
	}
	return &sole, types.BasisSoleWrite
}
