package queries

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The reader's own views of the company's chat: Activity, the threads they
// follow, what they saved, what they sent, a room's shared links, and one
// message read in place.
//
// # The record is read here and crosses as a value
//
// Each view needs the reader's own record (where they have read to, the
// threads they marked, what they saved), which is coordination's, while the
// rows are the copy's that serves the read. So this surface reads the record
// ([Sources.chatReadState]) and hands what the view needs to the reader as a
// value ([chatlet.ReaderMarks]), as the rail hands it the cursors. A record
// that could not be read is said, never counted from zero: the views answer
// `read_state: false`, and Activity's unread alone, which only the record can
// tell, is refused unavailable rather than answered empty.

// ChatReaderViews is the reader's own views as this surface calls them.
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

// readerMarks is the reader's record as a view needs it.
func readerMarks(state coord.ChatReadState, measured bool) chatlet.ReaderMarks {
	if !measured {
		return chatlet.ReaderMarks{}
	}
	marks := chatlet.ReaderMarks{Known: true, Cursors: state.Cursors,
		ThreadCursors: state.ThreadCursors}
	for _, m := range state.Threads {
		ref := chatlet.ThreadRef{ChannelID: m.ChannelID, RootID: m.RootID}
		if m.Following {
			marks.Following = append(marks.Following, ref)
		} else {
			marks.Unfollowed = append(marks.Unfollowed, ref)
		}
	}
	return marks
}

// errReadStateUnreadable refuses a question only the reader's own record can
// answer while it cannot be read.
func errReadStateUnreadable(what string) error {
	return fmt.Errorf("%w: %s cannot be said now, because your read state "+
		"could not be read. Ask again shortly", ErrUnavailable, what)
}

// chatActivity answers the viewer's Activity: mentions of them, replies in
// the threads they follow and reactions to their messages, newest first.
// `kind` narrows it to one of the three, `unread` to the unread mentions and
// replies, whole.
func (s Sources) chatActivity(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatRead)
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	state, measured := s.chatReadState(ctx, who)
	unread := p.Bool("unread", false)
	if unread && !measured {
		return nil, errReadStateUnreadable("which rows of your Activity are unread")
	}
	answer, err := s.Chat.Activity(ctx, who, chatlet.ActivityQuery{
		Kind:   chatlet.ActivityKind(strings.TrimSpace(p.String("kind"))),
		Unread: unread, Marks: readerMarks(state, measured),
		Cursor: strings.TrimSpace(p.String("cursor")), Limit: chatPage(p),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return answer, nil
}

// chatThreads answers the threads the viewer follows, newest reply first.
func (s Sources) chatThreads(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatRead)
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	state, measured := s.chatReadState(ctx, who)
	answer, err := s.Chat.Threads(ctx, who, chatlet.ThreadsQuery{
		Marks:  readerMarks(state, measured),
		Cursor: strings.TrimSpace(p.String("cursor")),
		Limit: Clamp(p.Int("limit", 0), chatlet.DefaultThreadsLimit,
			chatlet.MaxThreadsLimit),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return answer, nil
}

// chatSaved answers the viewer's saved messages, the newest save first, each
// with its message unless it is gone.
func (s Sources) chatSaved(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatRead)
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	// A RECORD NOBODY COULD READ LISTS NOTHING and says so (`read_state:
	// false`), as the rail does: the saves are the record's alone.
	state, measured := s.chatReadState(ctx, who)
	saved := make([]chatlet.SavedMark, 0, len(state.Saved))
	for _, m := range state.Saved {
		saved = append(saved, chatlet.SavedMark{ChannelID: m.ChannelID,
			MessageID: m.MessageID, At: m.At})
	}
	answer, err := s.Chat.Saved(ctx, who, chatlet.SavedQuery{
		Saved: saved, Cursor: strings.TrimSpace(p.String("cursor")),
		Limit: chatPage(p),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	answer.ReadState = measured
	return answer, nil
}

// chatSent answers what the viewer said, in every room they may still read,
// newest first.
func (s Sources) chatSent(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatRead)
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	answer, err := s.Chat.Sent(ctx, who, chatlet.SentQuery{
		Cursor: strings.TrimSpace(p.String("cursor")), Limit: chatPage(p),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return answer, nil
}

// chatLinks answers the links one room shared, newest first, the room named
// by its id or by its name.
func (s Sources) chatLinks(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatRead)
	if err != nil {
		return nil, err
	}
	ref, err := chatChannelID(p)
	if err != nil {
		return nil, err
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	id, err := s.chatRoomID(ctx, who, ref, fresh)
	if err != nil {
		return nil, err
	}
	answer, err := s.Chat.Links(ctx, who, chatlet.LinksQuery{
		ChannelID: id, Cursor: strings.TrimSpace(p.String("cursor")),
		Limit: chatPage(p),
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return answer, nil
}

// chatMessage answers one message by its id, with its thread's root and the
// replies before it when it is a reply: what reading it in place needs.
func (s Sources) chatMessage(ctx context.Context, p Params) (any, error) {
	who, err := s.chatViewer(ctx, authz.ActionChatRead)
	if err != nil {
		return nil, err
	}
	ref, err := chatChannelID(p)
	if err != nil {
		return nil, err
	}
	message := strings.TrimSpace(p.String("message_id"))
	if message == "" {
		return nil, badParams("message_id", "", nil)
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	id, err := s.chatRoomID(ctx, who, ref, fresh)
	if err != nil {
		return nil, err
	}
	answer, err := s.Chat.Message(ctx, who, chatlet.MessageQuery{
		ChannelID: id, MessageID: message,
	}, fresh)
	if err != nil {
		return nil, chatFailure(err)
	}
	return answer, nil
}
