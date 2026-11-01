package api

import (
	"context"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The reader's own chat views, resolved per question from the router's rooms
// as every [liveChat] read is.

// viaChat answers one read through the router's chat, or the reason this
// process has none yet.
func viaChat[A any](l liveChat, read func(estate.Chat) (A, error)) (A, error) {
	r, err := l.reader()
	if err != nil {
		var zero A
		return zero, err
	}
	return read(r)
}

func (l liveChat) Activity(ctx context.Context, viewer string, q chatlet.ActivityQuery,
	fresh statelog.Freshness) (chatlet.Activity, error) {
	return viaChat(l, func(r estate.Chat) (chatlet.Activity, error) {
		return r.Activity(ctx, viewer, q, fresh)
	})
}

func (l liveChat) Threads(ctx context.Context, viewer string, q chatlet.ThreadsQuery,
	fresh statelog.Freshness) (chatlet.FollowedThreads, error) {
	return viaChat(l, func(r estate.Chat) (chatlet.FollowedThreads, error) {
		return r.Threads(ctx, viewer, q, fresh)
	})
}

func (l liveChat) Saved(ctx context.Context, viewer string, q chatlet.SavedQuery,
	fresh statelog.Freshness) (chatlet.SavedMessages, error) {
	return viaChat(l, func(r estate.Chat) (chatlet.SavedMessages, error) {
		return r.Saved(ctx, viewer, q, fresh)
	})
}

func (l liveChat) Sent(ctx context.Context, viewer string, q chatlet.SentQuery,
	fresh statelog.Freshness) (chatlet.SentMessages, error) {
	return viaChat(l, func(r estate.Chat) (chatlet.SentMessages, error) {
		return r.Sent(ctx, viewer, q, fresh)
	})
}

func (l liveChat) Links(ctx context.Context, viewer string, q chatlet.LinksQuery,
	fresh statelog.Freshness) (chatlet.SharedLinks, error) {
	return viaChat(l, func(r estate.Chat) (chatlet.SharedLinks, error) {
		return r.Links(ctx, viewer, q, fresh)
	})
}

func (l liveChat) Message(ctx context.Context, viewer string, q chatlet.MessageQuery,
	fresh statelog.Freshness) (chatlet.OneMessage, error) {
	return viaChat(l, func(r estate.Chat) (chatlet.OneMessage, error) {
		return r.Message(ctx, viewer, q, fresh)
	})
}
