package builtin

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/commentanchor"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/textcut"
)

// WHO A COMMENT ON AN ARTIFACT WAKES AND TELLS (ADR-0153, ADR-0012). The
// routing snapshot a comment record carries ([artifactsdomain.CommentNotify])
// is decided here, ONCE, for every surface that writes one: the tools below
// and the artifacts API's routes. It is decided at write time from the
// artifact's owner, the assignees of the work items it was made for or linked
// to, the seats the remark @-mentions and the seats already in the thread,
// and the record carries it, so the wake derived later reads the record and no
// state that moved since.
//
// ONE RULE FOR A STAKE (ADR-0113). A person's remark naming nobody wakes the
// agents with a standing stake (the owner, a linked item's assignee, an agent
// already in the thread); one naming somebody wakes the agents it names and no
// stake. An agent's remark wakes another agent only when it states that it
// asks (`asks: true`), and then by the same two rules; with `asks: false` it
// wakes no agent. People are never woken: they are told, the person owner of
// a new thread, the people in a thread of every reply, resolve and reopen, and
// the people a remark names.

// ArtifactAssignees reads, when a comment is written, the assignee of every
// work item an artifact was made for or linked to: a removed item, one the
// tracker does not hold and one nobody is assigned add nobody.
type ArtifactAssignees interface {
	Assignees(ctx context.Context, id artifacts.ID) ([]string, error)
}

// ArtifactCommentRouter decides a comment record's routing snapshot. The zero
// value names nobody beyond the artifact's owner and the thread's own seats:
// no chart reads every handle as unknown, and no resolver mentions nobody.
type ArtifactCommentRouter struct {
	// Org is the company chart now, which says who is an agent.
	Org func() *org.Organization

	// Mentions resolves the seats a remark's words @-mention.
	Mentions MentionResolver

	// Assignees reads the linked work items' assignees. Nil adds none.
	Assignees ArtifactAssignees
}

// CommentWrite is one comment record as its routing is decided: what it does,
// on which artifact, by whom, and the thread it is in (nil for an open).
type CommentWrite struct {
	Op     artifactsdomain.OpKind
	Head   artifactsdomain.Head
	Author iam.Actor

	// Mentions are the seats the remark @-mentions, resolved
	// ([ArtifactCommentRouter.Mention]).
	Mentions []string

	// Asks is an agent's statement that its remark asks for an answer or an
	// action (ADR-0113); a person's words ask whatever they say.
	Asks bool

	Thread *artifactsdomain.CommentThread

	// Anchor, Version (zero for the head) and Quote are an open's new thread.
	Anchor  commentanchor.Anchor
	Version int
	Quote   string

	// SlideOrder is the head's slide ids, which a slide's label is numbered
	// by; nil for a kind with no slides.
	SlideOrder []string

	// WorkingFor is whom an agent's turn writes for (ADR-0109).
	WorkingFor *types.WorkingFor
}

// Mention is the seats text @-mentions, sorted and unique: every handle the
// resolver keeps, which are seats of the company.
func (r ArtifactCommentRouter) Mention(text string) []string {
	if r.Mentions == nil {
		return nil
	}
	return slices.Compact(slices.Sorted(slices.Values(r.Mentions.Mentions(text))))
}

// Route is the routing snapshot of w: the agents it wakes and the people it
// tells, each once under the strongest reason, the author never.
func (r ArtifactCommentRouter) Route(ctx context.Context, w CommentWrite) (
	artifactsdomain.CommentNotify, error) {

	var assignees []string
	if r.Assignees != nil && wakesStakes(w) && (w.Op == artifactsdomain.OpCommentOpen ||
		w.Op == artifactsdomain.OpCommentReply) {
		var err error
		if assignees, err = r.Assignees.Assignees(ctx, w.Head.ID); err != nil {
			return artifactsdomain.CommentNotify{}, fmt.Errorf("builtin: read who %s's work "+
				"items are assigned to: %w", w.Head.ID, err)
		}
	}
	var chart *org.Organization
	if r.Org != nil {
		chart = r.Org()
	}
	n := routeComment(w, assignees, kindIn(chart))
	n.About = aboutComment(w)
	if agentWrote(w) {
		n.WorkingFor = w.WorkingFor.Clone()
	}
	return n, nil
}

// aboutComment is what w's wake says of where it was made: the thread's
// version, label and quote, the open's own where it opens one.
func aboutComment(w CommentWrite) *artifactsdomain.CommentAbout {
	about := &artifactsdomain.CommentAbout{Title: w.Head.Title, Kind: w.Head.Kind, Head: w.Head.HeadNumber}
	anchor, version, quote := w.Anchor, w.Version, w.Quote
	if w.Thread != nil {
		anchor, version, quote = w.Thread.Anchor, w.Thread.Version, w.Thread.Quote
	}
	if anchor.Kind == "" {
		anchor = commentanchor.Whole()
	}
	if version == 0 {
		version = w.Head.HeadNumber
	}
	about.Version, about.Quote = version, quote
	about.Label = textcut.Within(commentanchor.Label(anchor, w.SlideOrder), artifactsdomain.MaxCommentLabelBytes)
	if len(about.Title) > artifacts.MaxTitleBytes {
		about.Title = textcut.Within(about.Title, artifacts.MaxTitleBytes)
	}
	return about
}

// kindIn answers a handle's kind on chart: an agent, a person, or unknown.
func kindIn(chart *org.Organization) func(string) iam.ActorKind {
	return func(handle string) iam.ActorKind {
		if chart == nil {
			return ""
		}
		seat := chart.SeatByHandle(handle)
		switch {
		case seat == nil:
			return ""
		case seat.IsHuman():
			return iam.ActorHuman
		}
		return iam.ActorAgent
	}
}

// agentWrote reports a remark an agent seat wrote, whose words wake another
// agent only when they ask.
func agentWrote(w CommentWrite) bool { return w.Author.Kind == iam.ActorAgent }

// wakesStakes reports a write that wakes the agents with a standing stake: a
// person's naming nobody, or an agent's that asks and names nobody.
func wakesStakes(w CommentWrite) bool {
	switch w.Op {
	case artifactsdomain.OpCommentResolve, artifactsdomain.OpCommentReopen:
		return !agentWrote(w)
	}
	return len(w.Mentions) == 0 && (!agentWrote(w) || w.Asks)
}

// routeComment is [ArtifactCommentRouter.Route] over what was read: the
// assignees of the linked items and the chart's kind of each handle.
func routeComment(w CommentWrite, assignees []string, kindOf func(string) iam.ActorKind) artifactsdomain.CommentNotify {
	var agents, people recipients
	author := w.Author.Name
	agents.not, people.not = author, author
	owner := w.Head.Owner
	ownerAgent := w.Head.OwnerKind == iam.ActorAgent
	stakes := wakesStakes(w)
	named := !agentWrote(w) || w.Asks
	switch w.Op {
	case artifactsdomain.OpCommentOpen, artifactsdomain.OpCommentReply:
		for _, m := range w.Mentions {
			switch kindOf(m) {
			case iam.ActorAgent:
				if named {
					agents.add(m, artifactsdomain.NotifyMention, true)
				}
			case iam.ActorHuman:
				people.add(m, artifactsdomain.NotifyMention, true)
			}
		}
		if stakes && ownerAgent {
			agents.add(owner, artifactsdomain.NotifyOwner, true)
		}
		if w.Op == artifactsdomain.OpCommentOpen && !ownerAgent {
			people.add(owner, artifactsdomain.NotifyOwner, false)
		}
		if stakes {
			for _, a := range assignees {
				if kindOf(a) == iam.ActorAgent {
					agents.add(a, artifactsdomain.NotifyAssignee, false)
				}
			}
		}
	case artifactsdomain.OpCommentResolve, artifactsdomain.OpCommentReopen:
		if stakes && ownerAgent {
			agents.add(owner, artifactsdomain.NotifyOwner, w.Op == artifactsdomain.OpCommentReopen)
		}
	}
	if w.Thread != nil && w.Op != artifactsdomain.OpCommentOpen {
		// A resolve or a reopen wakes the owner alone; the people in the
		// thread are told of each.
		for _, rm := range w.Thread.Remarks {
			switch rm.Author.Kind {
			case iam.ActorAgent:
				if stakes && w.Op == artifactsdomain.OpCommentReply {
					agents.add(rm.Author.Name, artifactsdomain.NotifyParticipant, false)
				}
			case iam.ActorHuman:
				people.add(rm.Author.Name, artifactsdomain.NotifyParticipant, false)
			}
		}
	}
	return artifactsdomain.CommentNotify{Agents: agents.list, People: people.list}
}

// recipients is one side of a snapshot as it is built: each seat once, under
// the first reason that named it, never the author, at most the bound.
type recipients struct {
	not  string
	list []artifactsdomain.Recipient
}

func (r *recipients) add(seat string, reason artifactsdomain.NotifyReason, addressed bool) {
	if seat == "" || seat == r.not || len(r.list) >= artifactsdomain.MaxNotifyRecipients ||
		slices.ContainsFunc(r.list, func(x artifactsdomain.Recipient) bool { return x.Seat == seat }) {
		return
	}
	r.list = append(r.list, artifactsdomain.Recipient{Seat: seat, Reason: reason, Addressed: addressed})
}
