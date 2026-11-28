package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask/signedasktest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// threadAnswerer is a data node's chat reader answering one thread and whom its
// messages were written for, recording what it was asked. Every other read fails.
type threadAnswerer struct {
	noReaderViews

	mu         sync.Mutex
	thread     chatlet.Thread
	workingFor map[string]*types.WorkingFor
	asked      struct {
		viewer string
		query  chatlet.ThreadQuery
		called bool
		// plain records a plain Thread read, which the turn-start read never
		// asks of a node that serves its own.
		plain bool
	}
}

var _ estate.ChatReader = (*threadAnswerer)(nil)

func (a *threadAnswerer) TurnThread(_ context.Context, viewer string, q chatlet.ThreadQuery,
	_ statelog.Freshness) (chatlet.TurnThread, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked.viewer, a.asked.query, a.asked.called = viewer, q, true
	return chatlet.TurnThread{Thread: a.thread, WorkingFor: a.workingFor, Carried: true}, nil
}

func (a *threadAnswerer) Thread(context.Context, string, chatlet.ThreadQuery,
	statelog.Freshness) (chatlet.Thread, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked.plain = true
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
// The Chatlet thread reader reads through the estate's router, so a seats-only
// node hands its woken seats their thread as a data node does. This node has no
// copy to fall back to, so a reader built over its own copy fails here rather
// than render the "could not be read" hint on every wake.
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
	}, workingFor: map[string]*types.WorkingFor{
		"root": {Kind: types.WorkingForSeat, Seat: "ada"},
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
	stop, err := estate.Serve(t.Context(), started(), estate.ServerOptions{Taken: signedasktest.NewLedger(),
		Self: "data-1", Envelopes: envelopes,
		Local: &servedEstate{backend: estate.Backend{Chat: answerer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "agent-1", Queue: started(), Placement: oneDataNode{}, Git: noGitletNodes{},
		Session: estate.NewSession(), Timing: estate.Production(), Envelopes: envelopes,
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
	// Only the turn's own read: whom each message was written for crosses with
	// the thread, and the transcript says it was read. The plain thread is a
	// fallback a node serving it never sees.
	if asked.plain {
		t.Error("the data node was asked the plain thread, which the turn's own read never asks " +
			"of a node that serves it")
	}
	if got.WorkingForUnread || len(got.Messages) == 0 || got.Messages[0].WorkingFor == nil ||
		got.Messages[0].WorkingFor.Seat != "ada" {
		t.Errorf("the transcript is %+v, want the root's carrier ada crossed and the read saying it "+
			"read whom each message was written for", got)
	}
	if len(got.Messages) != 2 || got.Messages[0].Text != "what is the plan" ||
		!got.Messages[1].Own || got.Older != 7 {
		t.Errorf("the transcript is %+v, want the root, the seat's own reply and "+
			"the seven earlier replies counted", got)
	}
}

// noReaderViews refuses every reader view of a chat reader double, so a case
// that reaches one by mistake fails rather than read an empty answer as real.
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
