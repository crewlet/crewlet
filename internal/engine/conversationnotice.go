package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A turn's conversation is told what its runs' sessions lost, once per turn and
// in the engine's sentence alone ([sandbox.PendingRun.ConversationNotice]): a
// colleague who asked, in the engine's answer (a2a.go); a conversation on the
// engine's own chat, by the engine, in a line of its own in the thread the turn
// answers, which wakes nobody. A reply the seat posts elsewhere is the seat's
// words, composed in its box and posted by its own tool, which the engine adds
// none to; the turn's page and the seat's Sessions say it there.

// conversationNotice says it on the engine's own chat ([sandbox.ConversationTeller]).
type conversationNotice struct{ engine *Engine }

var _ sandbox.ConversationTeller = conversationNotice{}

// NotContinued tells run's conversation verdict, where the conversation is on
// this company's chat and this node writes it; logged where it cannot be told.
func (n conversationNotice) NotContinued(ctx context.Context, run sandbox.PendingRun,
	verdict executorkeep.NotContinued) {

	if _, _, ok := chatlet.ConversationPlace(run.ConversationKey); !ok {
		return
	}
	rooms, ok := n.engine.chatHalves()
	if !ok {
		log.WarnContext(ctx, "conversation_notice_unsaid", "turn_id", run.TurnID,
			"detail", "this node hands out no chat writer (it publishes nothing, or its company's chat "+
				"is not Chatlet), so the turn's conversation is not told that its run did not "+
				"continue a session; the turn's page says so")
		return
	}
	// MINTED AS THE TURN'S OTHER OPERATIONS ARE, rebased past the ledger's
	// horizon, so a second ask of the turn's end is the same line, never two.
	at, err := n.engine.turnMintedAt(ctx, run.TurnID, run.WorkKey, run.WorkSince)
	if err != nil {
		log.WarnContext(ctx, "conversation_notice_unsaid", "turn_id", run.TurnID, "error", err.Error(),
			"detail", "the turn's operations could not be dated, so its conversation is not told that its "+
				"run did not continue a session; the turn's page says so")
		return
	}
	tellConversation(ctx, rooms, at, run.TurnID, run.ConversationKey, verdict)
}

// conversationWriter is the half of the chat's write side a notice is said
// through.
type conversationWriter interface {
	Post(ctx context.Context, actor chatlet.Actor, channelID string, in chatlet.NewMessage) (chatlet.Written, error)
	Reply(ctx context.Context, actor chatlet.Actor, channelID, replyTo string,
		in chatlet.NewMessage) (chatlet.Written, error)
}

// tellConversation says verdict where conversation is on the engine's chat
// ([chatlet.ConversationPlace]): in the thread it names, or in its direct room.
// AS THE ENGINE ([chatlet.AuthorSystem]), never the seat: a line the seat never
// wrote would read as its answer. UNDER ONE OPERATION the turn derives from at
// and the verdict's facts ([verdictFacts]), never its words, which chat leaves
// out of the engine's line's id: asked again, by this build or one that words
// it anew, it is the same line.
func tellConversation(ctx context.Context, rooms conversationWriter, at time.Time, turnID, conversation string,
	verdict executorkeep.NotContinued) {

	room, anchor, ok := chatlet.ConversationPlace(conversation)
	if !ok {
		return
	}
	actor := chatlet.Actor{Handle: conversationNoticeActor, Kind: chatlet.AuthorSystem, TurnID: turnID,
		OpKey: statelog.DeriveOpID(at, "", append([]string{conversationNoticeNamespace, turnID},
			verdictFacts(verdict)...)...)}
	said := chatlet.NewMessage{Body: verdict.Sentence()}
	var err error
	if anchor == "" {
		_, err = rooms.Post(ctx, actor, room, said)
	} else {
		_, err = rooms.Reply(ctx, actor, room, anchor, said)
	}
	if err != nil {
		log.WarnContext(ctx, "conversation_notice_unsaid", "turn_id", turnID, "channel", room,
			"error", err.Error(), "detail", "the conversation is not told that the turn's run did not "+
				"continue a session; the turn's page says so")
	}
}

// verdictFacts is what a verdict states, as the notice's operation names it:
// the loss, what the run did instead and the turns or capture either side.
// Never the detail, which the line does not say.
func verdictFacts(v executorkeep.NotContinued) []string {
	older := ""
	if v.Older != nil {
		older = fmt.Sprintf("%d.%d", v.Older.LaunchSeq, v.Older.CaptureSeq)
	}
	return []string{string(v.Loss), string(v.Instead), v.LostTurn, v.OlderTurn, older}
}

// conversationNoticeActor is the name the engine says a turn's lost session
// under, beside [chatAutoJoinActor]: a name no seat takes.
const conversationNoticeActor = iam.EngineSessions

// conversationNoticeNamespace scopes the notice's operation. FIXED for the
// life of the format: a new one would make a second ask a second line.
const conversationNoticeNamespace = "crewlet.engine.conversation-notice"
