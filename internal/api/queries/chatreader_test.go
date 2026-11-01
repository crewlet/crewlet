package queries_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// noReaderViews is a chat reader double's reader views when its suite reads
// none of them: each is refused, so a case that reaches one by mistake fails
// rather than reading an empty answer as a real one.
type noReaderViews struct{}

var errNoReaderViews = errors.New("this double answers no reader view")

func (noReaderViews) Activity(context.Context, string, chatlet.ActivityQuery,
	statelog.Freshness) (chatlet.Activity, error) {
	return chatlet.Activity{}, errNoReaderViews
}

func (noReaderViews) Threads(context.Context, string, chatlet.ThreadsQuery,
	statelog.Freshness) (chatlet.FollowedThreads, error) {
	return chatlet.FollowedThreads{}, errNoReaderViews
}

func (noReaderViews) Saved(context.Context, string, chatlet.SavedQuery,
	statelog.Freshness) (chatlet.SavedMessages, error) {
	return chatlet.SavedMessages{}, errNoReaderViews
}

func (noReaderViews) Sent(context.Context, string, chatlet.SentQuery,
	statelog.Freshness) (chatlet.SentMessages, error) {
	return chatlet.SentMessages{}, errNoReaderViews
}

func (noReaderViews) Links(context.Context, string, chatlet.LinksQuery,
	statelog.Freshness) (chatlet.SharedLinks, error) {
	return chatlet.SharedLinks{}, errNoReaderViews
}

func (noReaderViews) Message(context.Context, string, chatlet.MessageQuery,
	statelog.Freshness) (chatlet.OneMessage, error) {
	return chatlet.OneMessage{}, errNoReaderViews
}

// The reader views of [stubChat]: each records what the surface asked, refuses
// as the double is told to, and otherwise answers the canned served block, so
// the family's sweeps hold these questions to what they hold every chat
// question to. A room-scoped view decides the room as the reader does.

func (s *stubChat) asked(viewer string, fresh statelog.Freshness) error {
	s.reads++
	s.viewer, s.fresh = viewer, fresh
	return s.err
}

func (s *stubChat) Activity(_ context.Context, viewer string, q chatlet.ActivityQuery,
	fresh statelog.Freshness) (chatlet.Activity, error) {
	if err := s.asked(viewer, fresh); err != nil {
		return chatlet.Activity{}, err
	}
	s.activity = q
	out := s.feed
	if out.Items == nil {
		out.Items = []chatlet.ActivityItem{}
	}
	out.Served, out.ReadState = chatServed, q.Marks.Known
	return out, nil
}

func (s *stubChat) Threads(_ context.Context, viewer string, q chatlet.ThreadsQuery,
	fresh statelog.Freshness) (chatlet.FollowedThreads, error) {
	if err := s.asked(viewer, fresh); err != nil {
		return chatlet.FollowedThreads{}, err
	}
	return chatlet.FollowedThreads{Served: chatServed, Threads: []chatlet.FollowedThread{},
		ReadState: q.Marks.Known}, nil
}

func (s *stubChat) Saved(_ context.Context, viewer string, q chatlet.SavedQuery,
	fresh statelog.Freshness) (chatlet.SavedMessages, error) {
	if err := s.asked(viewer, fresh); err != nil {
		return chatlet.SavedMessages{}, err
	}
	s.saved = q
	return chatlet.SavedMessages{Served: chatServed, Saved: []chatlet.SavedEntry{}}, nil
}

func (s *stubChat) Sent(_ context.Context, viewer string, _ chatlet.SentQuery,
	fresh statelog.Freshness) (chatlet.SentMessages, error) {
	if err := s.asked(viewer, fresh); err != nil {
		return chatlet.SentMessages{}, err
	}
	return chatlet.SentMessages{Served: chatServed, Messages: []chatlet.SentMessage{}}, nil
}

func (s *stubChat) Links(_ context.Context, viewer string, q chatlet.LinksQuery,
	fresh statelog.Freshness) (chatlet.SharedLinks, error) {
	if err := s.asked(viewer, fresh); err != nil {
		return chatlet.SharedLinks{}, err
	}
	if err := s.see(viewer, q.ChannelID); err != nil {
		return chatlet.SharedLinks{}, err
	}
	return chatlet.SharedLinks{Served: chatServed, ChannelID: q.ChannelID,
		Links: []chatlet.SharedLink{}}, nil
}

func (s *stubChat) Message(_ context.Context, viewer string, q chatlet.MessageQuery,
	fresh statelog.Freshness) (chatlet.OneMessage, error) {
	if err := s.asked(viewer, fresh); err != nil {
		return chatlet.OneMessage{}, err
	}
	if err := s.see(viewer, q.ChannelID); err != nil {
		return chatlet.OneMessage{}, err
	}
	return chatlet.OneMessage{Served: chatServed, View: chatlet.MessageView{
		Message: chatlet.Message{ID: q.MessageID, ChannelID: q.ChannelID}}}, nil
}

// THE READER'S OWN RECORD REACHES HER VIEWS AS SHE KEPT IT, read by this
// surface and handed over as a value: her places, the threads she marked
// either way, and her saves in the record's order. A record nobody could read
// is said, and Activity's unread alone, which only the record can tell, is
// refused rather than answered empty.
//
// Mutation: hand the views a zero record and every assertion below fails.
func TestTheReadersRecordReachesHerViews(t *testing.T) {
	t.Parallel()
	saved := time.Date(2031, 5, 6, 7, 0, 0, 0, time.UTC)
	reads := chatReadsFor(t, "ana", coord.ChatReadDelta{
		Cursors:  map[string]int64{cEng: 41},
		Follow:   []coord.ThreadRef{{ChannelID: cEng, RootID: "m-1"}},
		Unfollow: []coord.ThreadRef{{ChannelID: cEng, RootID: "m-2"}},
		Save:     []coord.MessageRef{{ChannelID: cEng, MessageID: "m-3"}},
	})
	reader := newStubChat()
	s := chatSources(t, reader, &stubChatSearch{}, reads)
	got := askChatJSON(t, s, "chat_activity", map[string]any{"kind": "mention"})
	marks := reader.activity.Marks
	switch {
	case got["read_state"] != true || !marks.Known:
		t.Errorf("Activity over a readable record answered read_state %v", got["read_state"])
	case marks.Cursors[cEng] != 41:
		t.Errorf("her place in the room reached the view as %v", marks.Cursors)
	case len(marks.Following) != 1 || marks.Following[0].RootID != "m-1":
		t.Errorf("her followed thread reached the view as %+v", marks.Following)
	case len(marks.Unfollowed) != 1 || marks.Unfollowed[0].RootID != "m-2":
		t.Errorf("her unfollowed thread reached the view as %+v", marks.Unfollowed)
	case reader.activity.Kind != chatlet.ActivityMention:
		t.Errorf("the kind reached the view as %q", reader.activity.Kind)
	}
	askChatJSON(t, s, "chat_saved", nil)
	if len(reader.saved.Saved) != 1 || reader.saved.Saved[0].MessageID != "m-3" ||
		reader.saved.Saved[0].At.Before(saved) {
		t.Errorf("her saves reached the view as %+v", reader.saved.Saved)
	}

	unread := chatSources(t, newStubChat(), &stubChatSearch{}, &unreadableReads{})
	if _, err := askChat(t, unread, "chat_activity", map[string]any{"unread": true}); !errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("unread alone over an unreadable record answered %v, want unavailable", err)
	}
	if got := askChatJSON(t, unread, "chat_saved", nil); got["read_state"] != false {
		t.Errorf("saved over an unreadable record answered read_state %v, want false",
			got["read_state"])
	}
}
