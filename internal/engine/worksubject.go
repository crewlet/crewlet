package engine

import (
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Which work item a turn is on.
//
// A turn is charged to ONE work item or to none, and this file is the whole of
// the rule that decides which. The rules are tried in order and the
// first that names an item wins:
//
//  1. TRIGGER — the event that woke the turn names the item: a native tracker
//     wake, or a Jira, GitHub or GitLab webhook about one issue.
//  2. ASKED_BY — the turn answers a colleague's ask, and inherits the item the
//     ASKING turn was on. Help given on a task is work on that task.
//  3. RESUME — the turn re-enters a parked coding run, whose row recorded the
//     item it was on when it parked.
//  4. SOLE_WRITE — nothing above named an item, and the turn's own writes
//     committed to exactly one. Resolved AT COMPLETION ONLY, because only then
//     is "exactly one" a fact rather than a guess; see [completedWorkItem].
//
// Otherwise the turn is on nothing, which is the ordinary case for a chat wake
// and is recorded as an absent `work_item` rather than a null one. A turn is
// NEVER SPLIT between items: one that touches three tasks is charged to the one
// it was woken for, or — woken for none — to none, because a split would have
// to invent a ratio nothing in the turn states.
//
// Rules 1 to 3 are all read off something that EXISTED BEFORE THE TURN RAN — a
// trigger, an ask, a row — which is why they are resolved at dispatch and
// stamped on every event the turn publishes from its first. Rule 4 is the only
// one that depends on what the turn did.

// workItemNamer is what a notification source declares when its wakes can be
// about one work item: the item a wake's metadata names, if it names one.
//
// DECLARED HERE, by the one caller, and implemented by each source's own
// prompt — the package that already owns what that source's metadata keys
// mean. A source that does not implement it (chat, a pager) names no item, and
// that is the honest answer rather than a gap.
type workItemNamer interface {
	WorkItem(metadata map[string]string) (types.WorkItem, bool)
}

// workItemNamers are the sources whose wakes can name an item, by source.
//
// A FIXED SET rather than the node's routed prompts, because the question is
// about the EVENT, not about this node's configuration: a wake already on a
// seat's inbox was routed by whichever node took the webhook, and an
// integration disconnected since then does not change which issue it was
// about. Each is a stateless value type, so holding them here costs nothing.
var workItemNamers = func() map[string]workItemNamer {
	namers := map[string]workItemNamer{}
	for _, source := range []interface {
		workItemNamer
		Source() string
	}{tracker.Prompt{}, jira.Prompt{}, github.Prompt{}, gitlab.Prompt{}} {
		namers[source.Source()] = source
	}
	return namers
}()

// workItemOf resolves rules 1 and 2 for one dispatch: the item the trigger
// names, or the one the asking turn was on.
//
// OFF THE PARTITION'S FIRST EVENT, the same event [Engine.describeTurn] takes
// the trigger from — a coalesced partition is one conversation, and the first
// constituent is the one whose thread the turn is answering. Reading the item
// from a different event than the trigger would let a turn's own record
// disagree with itself about what woke it.
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
		// A COPY, because the event is shared with every other reader
		// of this delivery and the turn owns its own value.
		item := *ask.WorkItem
		return &item, types.BasisAskedBy
	}
	return nil, ""
}

// resumedWorkItem is rule 3: the item a parked run recorded at launch.
//
// A run recorded by a build that predates the field carries none, and resumes
// on nothing — which is what that turn's first half was charged to as well.
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
// A SEGMENT THAT PARKED IS NOT CHARGED BY RULE 4, because it has not finished
// writing: the resumed segment may write a second item, and a sole write
// concluded at the park would then have named an item the whole turn was not
// on alone. The segment that finishes the turn decides, over everything the
// turn wrote: the half before a park travels on the suspended conversation
// (execstate.State.Written) and seeds the resumed segment's set, so "exactly
// one" is judged over the turn rather than over its last segment.
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
