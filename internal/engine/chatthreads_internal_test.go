package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/notify"
)

// threadAnswerer is a data node that answers the router's `chat.thread` with
// one thread, and records what it was asked.
type threadAnswerer struct {
	mu     sync.Mutex
	thread chat.Thread
	op     string
	asked  struct {
		Viewer string
		Query  chat.ThreadQuery
	}
}

func (a *threadAnswerer) Ask(_ context.Context, _ string, payload []byte, _ int) ([][]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var req struct {
		Op   string          `json:"op"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	a.op = req.Op
	if err := json.Unmarshal(req.Args, &a.asked); err != nil {
		return nil, err
	}
	result, err := json.Marshal(a.thread)
	if err != nil {
		return nil, err
	}
	return [][]byte{fmt.Appendf(nil, `{"node":"data-1","result":%s}`, result)}, nil
}

// A NODE THAT HOLDS NO DATA HANDS ITS SEAT THE THREAD THROUGH THE ROUTER.
//
// The native thread reader reads through the estate's router so that a node
// holding no copy of chat — a seats-only member of the fleet — hands its woken
// seats their thread exactly as a data node does. The only engine case read on
// a data node, so a reader built over this node's own copy would have passed
// it — and on a seats-only node that reader is no reader at all, every wake
// rendering the "could not be read" hint. This one has no copy to fall back
// to: the router is the only way the transcript can arrive.
func TestANodeHoldingNoDataReadsTheThreadThroughTheRouter(t *testing.T) {
	t.Parallel()
	answerer := &threadAnswerer{thread: chat.Thread{
		ChannelID: "room-1",
		Root: chat.MessageView{Message: chat.Message{
			ID: "root", Author: "ana", AuthorKind: chat.AuthorOperator,
			Body: "what is the plan", V: chat.DocumentVersion,
		}},
		Replies: []chat.MessageView{{Message: chat.Message{
			ID: "m-1", Author: "swe", AuthorKind: chat.AuthorAgent,
			Body: "on it", ThreadRoot: "root", V: chat.DocumentVersion,
		}}},
		Earlier: 7,
	}}
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "agent-1", Queue: answerer, Placement: oneDataNode{},
		Session: estate.NewSession(),
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{router: router}
	e.remote.Store(&remoteNative{chat: true})

	got, ok := e.ChatThreads().ReadThread(t.Context(), "swe", notify.Thread{
		Backend: chat.Source, Channel: "room-1", Root: "root",
	})
	if !ok {
		t.Fatal("a node holding no data could not read a native thread — its " +
			"seats would be told on every wake that the thread is unreadable")
	}
	answerer.mu.Lock()
	op, asked := answerer.op, answerer.asked
	answerer.mu.Unlock()
	if op != "chat.thread" || asked.Viewer != "swe" || asked.Query.ChannelID != "room-1" ||
		asked.Query.RootID != "root" || asked.Query.Newest != chat.MaxLimit {
		t.Errorf("the data node was asked %q as %q for %+v, want the woken seat's "+
			"own newest-end read of the thread", op, asked.Viewer, asked.Query)
	}
	if len(got.Messages) != 2 || got.Messages[0].Text != "what is the plan" ||
		!got.Messages[1].Own || got.Older != 7 {
		t.Errorf("the transcript is %+v, want the root, the seat's own reply and "+
			"the seven earlier replies counted", got)
	}
}
