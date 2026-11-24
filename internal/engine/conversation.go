package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CONVERSATION'S NEXT TURN, AS ITS DISPATCH READS IT: the head, read ONCE at
// dispatch through the estate router at `linearizable` (internal/executordomain's
// Reader.Head), so an end another node published is in the answering copy. The
// turn renders how earlier turns ended (internal/agent/prompts'
// BuildConversationEndings), and its first launch freezes the read on the run
// record (internal/sandbox's conversationbase.go); a relaunch continues that
// base and never reads the head again. FAIL OPEN, as every log reader is: a
// failed read is logged (`executor_conversation_unread`) and the turn runs with
// less context. No key (an A2A ask) or
// `turn_engine.conversation_session.enabled: false` reads nothing, though ends
// are recorded. A SCHEDULE'S FIRE IS A TURN OF ONE: it carries the schedule's
// conversation (internal/notify's ScheduleConversation).

// headReader is the one read a turn's dispatch makes of its conversation on the
// executor log ([estate.ExecutorReads.Head]): the router's on every node, a
// case's own in a test.
type headReader interface {
	Head(ctx context.Context, handle, conversationKey string,
		fresh statelog.Freshness) (executordomain.Head, error)
}

// conversationRead is what a turn's dispatch made of its conversation: the
// account of how its earlier turns ended, rendered under the turn's token, the
// turns that account told, and the read as the turn's first launch freezes it —
// or why it read none.
type conversationRead struct {
	// why is why the turn read no head of its conversation: no key, a company
	// that keeps no session, no router, or a read that failed. "" where it
	// read one.
	why string

	// retired is why the read named no session where one ran out of context
	// ([executordomain.Head.RetiredWhy]), "" otherwise.
	retired string

	// endings is the rendered block, "" where there was nothing to tell; told
	// the turns it told, in the order they ended.
	endings string
	told    []string

	// base is the read as the turn's first launch freezes it
	// ([sandbox.LaunchRequest.Base]), nil where the turn read none.
	base *sandbox.ConversationBase
}

// endingsBlock is the rendered block a turn's runner is handed, "" for a turn
// that read none — a resumed segment, whose runner thaws the one its first
// froze.
func (r *conversationRead) endingsBlock() string {
	if r == nil {
		return ""
	}
	return r.endings
}

// frozen is the read a turn's first launch freezes on its run record, nil where
// the turn read none.
func (r *conversationRead) frozen() *sandbox.ConversationBase {
	if r == nil {
		return nil
	}
	return r.base
}

// leftNone reports whether the read was made and named no session to continue:
// a conversation's first turn, or one whose every earlier turn's session was
// released, absorbed or aged out — nothing was there to continue, which no
// launch logs. A session retired for running out of context is not among them.
func (r *conversationRead) leftNone() bool {
	return r != nil && r.why == "" && r.retired == "" && (r.base == nil || r.base.Session == nil)
}

// basis is which session a turn's FIRST launch continues, as the read answered
// it, with no further read: the base, where some member of the seat's chain
// restores it (restorer, [sandbox.BaseContinuityOf] of no row and no record,
// since neither exists before that launch), or why none.
func (r *conversationRead) basis(restorer sandbox.ArchiveRestorer) runner.SessionBasis {
	switch {
	case r.why != "":
		return runner.SessionBasis{Why: r.why}
	case r.retired != "":
		return runner.SessionBasis{Why: r.retired}
	case r.base == nil || r.base.Session == nil:
		return runner.SessionBasis{Why: "the conversation's last turn left no session to continue"}
	}
	base := *r.base.Session
	// THE HEAD IS KEPT, continued or not: the read named it because its row
	// names its session, and the begin this launch publishes holds it against
	// every release but a retirement while the turn has kept nothing
	// ([sandbox.LaunchRequest.Base]), so a run that starts a session of its
	// own beside it may read it. A relaunch names it only where the turn's
	// row was read holding it ([sandbox.ContinuedSession.KeptTurn]).
	if continuity := sandbox.BaseContinuityOf(base, nil, nil, restorer); !continuity.Continues {
		return runner.SessionBasis{Why: continuity.Why, KeptTurn: base.Turn}
	}
	basis := baseBasis(base, 0)
	basis.KeptTurn = base.Turn
	return basis
}

// baseBasis is the session a conversation's last turn left as a launch's basis:
// that session's id and harness, which the member a launch picks must run, and
// what a run continuing it is told of it — where its turn went on past it, that
// it did, and when it was kept. copyContext is the job before's own session's
// last request, for a relaunch's in-place floor
// ([runner.SessionBasis.CopyContext]), zero on a first launch; the base's own
// context is its own ([runner.BaseBasis.ContextTokens]).
func baseBasis(base sandbox.BaseSession, copyContext int64) runner.SessionBasis {
	return runner.SessionBasis{SessionID: base.SessionID, Harness: base.Harness, Entry: base.Entry,
		CopyContext: copyContext,
		Base: &runner.BaseBasis{Turn: base.Turn, Home: base.Home, Interrupted: base.Interrupted,
			ContextTokens: base.ContextTokens, EndedAt: base.EndedAt, CapturedAt: base.CapturedAt,
			WentOn: base.WentOn, Lost: base.Lost}}
}

// readConversation reads the conversation req's turn serves, once, under the
// turn's pinned company, and renders how its earlier turns ended under the
// turn's token boundary — see the file comment.
func (e *Engine) readConversation(ctx context.Context, company *Company, req Request,
	boundary provenance.Boundary) *conversationRead {

	switch {
	case req.ConversationKey == "":
		return &conversationRead{why: "the turn serves no conversation, so it continues no earlier turn's session"}
	case !keepsSessions(company):
		return &conversationRead{why: sandbox.NoKeptSession}
	case e.executorHeads == nil:
		return &conversationRead{why: "this node has no estate router to read the turn's conversation through"}
	}
	head, err := e.executorHeads.Head(ctx, req.Handle, req.ConversationKey,
		statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		log.WarnContext(ctx, "executor_conversation_unread", "seat", req.Handle, "turn_id", req.RunID,
			"conversation", req.ConversationKey, "error", err.Error(),
			"detail", "the turn runs with no account of how its conversation's earlier turns ended, and its "+
				"executor starts a session of its own")
		return &conversationRead{why: fmt.Sprintf("the turn's conversation could not be read: %v", err)}
	}
	endings, told := conversationEndings(head, req.Handle, notify.Scheduled(req.ConversationKey), boundary)
	return &conversationRead{retired: head.RetiredWhy(), endings: endings, told: told,
		base: sandbox.NewConversationBase(head.Kept(), told)}
}

// keepsSessions reports whether c keeps executor sessions, the switch that also
// decides whether a turn reads its conversation at all; a company built with no
// configuration takes the shipped default, on.
func keepsSessions(c *Company) bool {
	if c == nil || c.Config == nil {
		return true
	}
	return c.Config.TurnEngine.ConversationSession.KeepsSessions()
}

// conversationEndings renders how the conversation head reads ended, for the
// seat, under b: the head and the conversation's other ended turns no ended
// turn was shown, in the order they ended, and how many older ones the read
// itself left out — in a schedule's words where fires says the conversation is
// a schedule's — and answers the turns it told.
func conversationEndings(head executordomain.Head, seat string, fires bool,
	b provenance.Boundary) (string, []string) {

	turns := make([]prompts.TurnEnding, 0, len(head.Endings)+1)
	var headTurn string
	placed := head.Session == nil
	if !placed {
		headTurn = head.Session.Turn
	}
	for _, ended := range head.Endings {
		if !placed && head.Session.EndedAtPosition.Packed() < ended.EndedAtPosition.Packed() {
			turns, placed = append(turns, turnEndingOf(*head.Session)), true
		}
		turns = append(turns, turnEndingOf(ended))
	}
	if !placed {
		turns = append(turns, turnEndingOf(*head.Session))
	}
	return prompts.BuildConversationEndings(b, prompts.ConversationEndings{
		Seat: seat, Head: headTurn, Turns: turns, Omitted: head.EndingsOmitted, Fires: fires,
	})
}

// exhaustedAccount is what the block tells of a turn a launch of which could not
// go on in its session (out of its model's context, or refused by its provider):
// why its transcript is not what carries the conversation on.
const exhaustedAccount = "the session it could not go on in is retired, so no later turn continues it, and " +
	"what came before reaches later turns through these accounts rather than a transcript"

// turnEndingOf is one ended turn's row as the block tells it: the engine's
// account of how it ended (its executorkeep.Ending), and what it was asked —
// through the row's one redaction, as every reader of a row is shown it
// ([executordomain.Session.Redacted]): its words are a person's, a model's or
// a box's, and the next turn's model is a reader like any other.
func turnEndingOf(s executordomain.Session) prompts.TurnEnding {
	s = s.Redacted()
	t := prompts.TurnEnding{Turn: s.Turn, EndedAt: s.EndedAt, Ask: s.AskExcerpt}
	e := s.Ending
	if e == nil {
		return t
	}
	t.Outcome, t.Decision, t.Answer = string(e.Outcome), e.Decision, e.Answer
	t.Delivered, t.DeliveredTo, t.DeliveryUnknown = e.Delivered, e.DeliveredTo, e.DeliveryUnknown
	t.CompletedWork, t.BlockedOn = e.CompletedWork, e.BlockedOn
	t.StoppedBy, t.StopReason, t.Failure = e.StoppedBy, e.StopReason, e.Failure
	// THE UNKEPT CAPTURE AS THE REDACTION SHOWS IT, which is as it stands on
	// the row ([executordomain.Session.Unkept]), never as the end named it: one
	// whose record the log took before the end is the row's own, the very
	// session a head's next turn continues, and told as not kept that turn
	// would read its own session as lost.
	if u := e.Unkept; u != nil {
		t.Unkept = u.Reason
		if t.Unkept == "" {
			t.Unkept = "no reason was recorded"
		}
	}
	if len(e.Exhausted) > 0 {
		t.Exhausted = exhaustedAccount
	}
	return t
}
