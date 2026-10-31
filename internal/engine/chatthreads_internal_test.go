package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// threadAnswerer is a data node's chat reader answering one thread, and
// recording what it was asked. Every other read is a case's mistake.
type threadAnswerer struct {
	noReaderViews

	mu     sync.Mutex
	thread chatlet.Thread
	asked  struct {
		viewer string
		query  chatlet.ThreadQuery
		called bool
	}
}

var _ estate.ChatReader = (*threadAnswerer)(nil)

func (a *threadAnswerer) Thread(_ context.Context, viewer string, q chatlet.ThreadQuery,
	_ statelog.Freshness) (chatlet.Thread, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked.viewer, a.asked.query, a.asked.called = viewer, q, true
	return a.thread, nil
}

func (*threadAnswerer) Channels(context.Context, string, chatlet.ChannelsQuery,
	statelog.Freshness) (chatlet.ChannelListing, error) {
	return chatlet.ChannelListing{}, errors.New("only the thread is read here")
}

func (*threadAnswerer) Channel(context.Context, string, string,
	statelog.Freshness) (chatlet.ChannelDetail, error) {
	return chatlet.ChannelDetail{}, errors.New("only the thread is read here")
}

func (*threadAnswerer) ChannelNamed(context.Context, string, string,
	statelog.Freshness) (chatlet.ChannelDetail, error) {
	return chatlet.ChannelDetail{}, errors.New("only the thread is read here")
}

func (*threadAnswerer) Messages(context.Context, string, chatlet.TranscriptQuery,
	statelog.Freshness) (chatlet.Transcript, error) {
	return chatlet.Transcript{}, errors.New("only the thread is read here")
}

func (*threadAnswerer) RoomNames(context.Context, string,
	statelog.Freshness) (chatlet.RoomNames, error) {
	return chatlet.RoomNames{}, errors.New("only the thread is read here")
}

func (*threadAnswerer) Directory(context.Context, string,
	statelog.Freshness) (chatlet.Directory, error) {
	return chatlet.Directory{}, errors.New("only the thread is read here")
}

func (*threadAnswerer) Readable(context.Context, string,
	statelog.Freshness) ([]chatlet.ReadableRoom, error) {
	return nil, errors.New("only the thread is read here")
}

func (*threadAnswerer) At() statelog.Position { return statelog.Position{} }

// A NODE THAT HOLDS NO DATA HANDS ITS SEAT THE THREAD THROUGH THE ROUTER.
//
// The Chatlet thread reader reads through the estate's router so that a node
// holding no copy of chat — a seats-only member of the fleet — hands its woken
// seats their thread exactly as a data node does. The only engine case read on
// a data node, so a reader built over this node's own copy would have passed
// it — and on a seats-only node that reader is no reader at all, every wake
// rendering the "could not be read" hint. This one has no copy to fall back
// to: the router is the only way the transcript can arrive.
func TestANodeHoldingNoDataReadsTheThreadThroughTheRouter(t *testing.T) {
	t.Parallel()
	answerer := &threadAnswerer{thread: chatlet.Thread{
		ChannelID: "room-1",
		Root: chatlet.MessageView{Message: chatlet.Message{
			ID: "root", Author: "ana", AuthorKind: chatlet.AuthorHuman,
			Body: "what is the plan", V: chatlet.DocumentVersion,
		}},
		Replies: []chatlet.MessageView{{Message: chatlet.Message{
			ID: "m-1", Author: "swe", AuthorKind: chatlet.AuthorAgent,
			Body: "on it", ThreadRoot: "root", V: chatlet.DocumentVersion,
		}}},
		Earlier: 7,
	}}
	broker := memory.NewBroker()
	started := func() *memory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		return q
	}
	envelopes := routerEnvelopes(t)
	stop, err := estate.Serve(t.Context(), started(), estate.ServerOptions{
		Self: "data-1", Envelopes: envelopes,
		Local: &servedEstate{backend: estate.Backend{Chat: answerer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "agent-1", Queue: started(), Placement: oneDataNode{},
		Session: estate.NewSession(), Envelopes: envelopes,
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{router: router}
	e.remote.Store(&remoteNative{chat: true})

	got, ok := e.ChatThreads().ReadThread(t.Context(), "swe", notify.Thread{
		Backend: chatlet.Source, Channel: "room-1", Root: "root",
	})
	if !ok {
		t.Fatal("a node holding no data could not read a Chatlet thread — its " +
			"seats would be told on every wake that the thread is unreadable")
	}
	answerer.mu.Lock()
	asked := answerer.asked
	answerer.mu.Unlock()
	if !asked.called || asked.viewer != "swe" || asked.query.ChannelID != "room-1" ||
		asked.query.RootID != "root" || asked.query.Newest != chatlet.MaxLimit {
		t.Errorf("the data node was asked (called=%v) as %q for %+v, want the woken "+
			"seat's own newest-end read of the thread", asked.called, asked.viewer, asked.query)
	}
	if len(got.Messages) != 2 || got.Messages[0].Text != "what is the plan" ||
		!got.Messages[1].Own || got.Older != 7 {
		t.Errorf("the transcript is %+v, want the root, the seat's own reply and "+
			"the seven earlier replies counted", got)
	}
}

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
