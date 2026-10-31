package estate

import (
	"context"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ChatReaderViews are the reader's own views of the engine's chat: Activity,
// the threads they follow, what they saved, what they sent, a room's shared
// links, and one message read in place.
//
// EACH IS ANSWERED FOR ITS VIEWER on whichever data node serves it, as every
// chat read is ([ChatReader]): the viewer crosses, the rooms it may read are
// the serving reader's to decide, and the reader's own record (their cursors,
// their thread marks, their saves) crosses as a value the asking node read
// from coordination, because coordination is the fleet's and the rows are the
// copy's.
type ChatReaderViews interface {
	Activity(ctx context.Context, viewer string, q chatlet.ActivityQuery,
		fresh statelog.Freshness) (chatlet.Activity, error)
	Threads(ctx context.Context, viewer string, q chatlet.ThreadsQuery,
		fresh statelog.Freshness) (chatlet.FollowedThreads, error)
	Saved(ctx context.Context, viewer string, q chatlet.SavedQuery,
		fresh statelog.Freshness) (chatlet.SavedMessages, error)
	Sent(ctx context.Context, viewer string, q chatlet.SentQuery,
		fresh statelog.Freshness) (chatlet.SentMessages, error)
	Links(ctx context.Context, viewer string, q chatlet.LinksQuery,
		fresh statelog.Freshness) (chatlet.SharedLinks, error)
	Message(ctx context.Context, viewer string, q chatlet.MessageQuery,
		fresh statelog.Freshness) (chatlet.OneMessage, error)
}

type chatActivityArgs struct {
	Viewer string
	Query  chatlet.ActivityQuery
	Fresh  statelog.Freshness
}

var opChatActivity = define("chat.activity", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatActivityArgs) (chatlet.Activity, error) {
		if b.Chat == nil {
			return chatlet.Activity{}, errNoHalf
		}
		return b.Chat.Activity(ctx, a.Viewer, a.Query, a.Fresh)
	})

type chatThreadsArgs struct {
	Viewer string
	Query  chatlet.ThreadsQuery
	Fresh  statelog.Freshness
}

var opChatThreads = define("chat.threads", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatThreadsArgs) (chatlet.FollowedThreads, error) {
		if b.Chat == nil {
			return chatlet.FollowedThreads{}, errNoHalf
		}
		return b.Chat.Threads(ctx, a.Viewer, a.Query, a.Fresh)
	})

type chatSavedArgs struct {
	Viewer string
	Query  chatlet.SavedQuery
	Fresh  statelog.Freshness
}

var opChatSaved = define("chat.saved", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatSavedArgs) (chatlet.SavedMessages, error) {
		if b.Chat == nil {
			return chatlet.SavedMessages{}, errNoHalf
		}
		return b.Chat.Saved(ctx, a.Viewer, a.Query, a.Fresh)
	})

type chatSentArgs struct {
	Viewer string
	Query  chatlet.SentQuery
	Fresh  statelog.Freshness
}

var opChatSent = define("chat.sent", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatSentArgs) (chatlet.SentMessages, error) {
		if b.Chat == nil {
			return chatlet.SentMessages{}, errNoHalf
		}
		return b.Chat.Sent(ctx, a.Viewer, a.Query, a.Fresh)
	})

type chatLinksArgs struct {
	Viewer string
	Query  chatlet.LinksQuery
	Fresh  statelog.Freshness
}

var opChatLinks = define("chat.links", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatLinksArgs) (chatlet.SharedLinks, error) {
		if b.Chat == nil {
			return chatlet.SharedLinks{}, errNoHalf
		}
		return b.Chat.Links(ctx, a.Viewer, a.Query, a.Fresh)
	})

type chatMessageArgs struct {
	Viewer string
	Query  chatlet.MessageQuery
	Fresh  statelog.Freshness
}

var opChatMessage = define("chat.message", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatMessageArgs) (chatlet.OneMessage, error) {
		if b.Chat == nil {
			return chatlet.OneMessage{}, errNoHalf
		}
		return b.Chat.Message(ctx, a.Viewer, a.Query, a.Fresh)
	})

// Activity answers a page of the viewer's Activity.
func (c Chat) Activity(ctx context.Context, viewer string, q chatlet.ActivityQuery,
	fresh statelog.Freshness) (chatlet.Activity, error) {
	return call(ctx, c.r, opChatActivity, nil, chatActivityArgs{Viewer: viewer, Query: q, Fresh: fresh})
}

// Threads answers a page of the threads the viewer follows.
func (c Chat) Threads(ctx context.Context, viewer string, q chatlet.ThreadsQuery,
	fresh statelog.Freshness) (chatlet.FollowedThreads, error) {
	return call(ctx, c.r, opChatThreads, nil, chatThreadsArgs{Viewer: viewer, Query: q, Fresh: fresh})
}

// Saved answers a page of the viewer's saved messages.
func (c Chat) Saved(ctx context.Context, viewer string, q chatlet.SavedQuery,
	fresh statelog.Freshness) (chatlet.SavedMessages, error) {
	return call(ctx, c.r, opChatSaved, nil, chatSavedArgs{Viewer: viewer, Query: q, Fresh: fresh})
}

// Sent answers a page of what the viewer said.
func (c Chat) Sent(ctx context.Context, viewer string, q chatlet.SentQuery,
	fresh statelog.Freshness) (chatlet.SentMessages, error) {
	return call(ctx, c.r, opChatSent, nil, chatSentArgs{Viewer: viewer, Query: q, Fresh: fresh})
}

// Links answers a page of the links one room shared.
func (c Chat) Links(ctx context.Context, viewer string, q chatlet.LinksQuery,
	fresh statelog.Freshness) (chatlet.SharedLinks, error) {
	return call(ctx, c.r, opChatLinks, nil, chatLinksArgs{Viewer: viewer, Query: q, Fresh: fresh})
}

// Message answers one message, read in place.
func (c Chat) Message(ctx context.Context, viewer string, q chatlet.MessageQuery,
	fresh statelog.Freshness) (chatlet.OneMessage, error) {
	return call(ctx, c.r, opChatMessage, nil, chatMessageArgs{Viewer: viewer, Query: q, Fresh: fresh})
}
