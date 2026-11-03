package queries

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
)

// ChatDrafter is a post's dry run, through the router: whom a post or a reply
// would reach if it were sent now, decided on a data node by the post's own
// decision ([chatlet.Store.Reach]) and published nowhere.
type ChatDrafter interface {
	Reach(ctx context.Context, actor chatlet.Actor, channelID, replyTo string,
		in chatlet.NewMessage) (chatlet.Draft, error)
}

// chatReach answers `chat_reach{channel_id, body, reply_to?}`: whom the
// caller's draft would reach, and why none if it would reach no agent.
//
// THE POST'S OWN PATH, END TO END, so a composer's warning cannot disagree
// with what the send does: the words are read by the post tool's own reading
// ([builtin.ChatMessage]: the seats they name, and `@channel`), the caller
// acts as the post tool's person does ([builtin.PrincipalChatActor]), and the
// routing is the post's decision run dry. The verb is decided first, as the
// send is: a reply's for a reply, a post's otherwise.
//
// THE BODY IS A PARAMETER because the routing reads it, the way a search
// carries its words; nothing of it is kept.
func (s Sources) chatReach(ctx context.Context, p Params) (any, error) {
	replyTo := strings.TrimSpace(p.String("reply_to"))
	action := authz.ActionChatPost
	if replyTo != "" {
		action = authz.ActionChatReply
	}
	who, err := s.chatViewer(ctx, action)
	if err != nil {
		return nil, err
	}
	ref, err := chatChannelID(p)
	if err != nil {
		return nil, err
	}
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	id, err := s.chatRoomID(ctx, who, ref, fresh)
	if err != nil {
		return nil, err
	}
	actor, err := builtin.PrincipalChatActor(ctx, nil)
	if err != nil {
		return nil, unresolved(ctx, string(action))
	}
	draft, err := s.ChatDraft.Reach(ctx, actor, id, replyTo,
		builtin.ChatMessage(s.ChatMentions, p.String("body"), nil))
	if err != nil {
		return nil, chatFailure(err)
	}
	return draft, nil
}
