package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/search"
)

// The chat surface's own wiring guards.
//
// THE REGRESSION THESE EXIST FOR: the nine chat tools were written, tested,
// catalogued and documented, and `equip` never assigned `deps.Chat` — so every
// one of them was gated off against a zero [builtin.ChatDeps]. A company on
// `chat.backend: native` ran the domain, applied its records and woke its
// seats about messages they had no tool to answer with, and nothing anywhere
// went red. The same omission had already cost this surface its operator half.

// A NODE THAT HOLDS THE ROOMS HANDS THEM TO ITS SEATS.
func TestChatDepsCarryTheNodesOwnRooms(t *testing.T) {
	t.Parallel()
	e := &Engine{native: &native{
		chat: &chat.Store{}, chatReader: &chat.Reader{},
	}}
	c := &Company{Config: &config.Company{}, Org: &org.Organization{}}

	deps := e.chatDeps(c)
	if deps.Reader == nil {
		t.Error("a node holding the chat read side gave its seats no reader " +
			"— read_channel and list_channels are registered on it")
	}
	if deps.Writer == nil {
		t.Error("a node holding the chat write side gave its seats no writer " +
			"— six of the nine tools are registered on it, including every " +
			"one that counts as a delivery")
	}
	if deps.Mentions == nil {
		t.Error("chat deps carry no mention resolver, so `@alice` in a " +
			"message would resolve to nobody and wake nobody, while the " +
			"tool's own description promises otherwise")
	}
	if deps.Await == nil {
		t.Error("chat deps carry no Await, so a seat that posts and then " +
			"reads the room may not see its own message — which is exactly " +
			"how a model comes to say the same thing twice")
	}
	// AND NO ACTOR, which is what tells a seat's deps from the operator
	// surface's: nil attributes the write to the turn's own seat, and a
	// model that could name its author could speak as anybody.
	if deps.Actor != nil {
		t.Error("the seat surface set an Actor — a seat writes as itself, " +
			"resolved from the turn rather than from anything a model says")
	}
}

// A NODE WITH NO ROOMS OFFERS NOTHING, rather than a live adapter over a nil
// store: the tools are OMITTED on a zero deps, which is how a seat is never
// offered a tool its company cannot serve.
func TestChatDepsAreEmptyWithoutTheBackend(t *testing.T) {
	t.Parallel()
	c := &Company{Config: &config.Company{}, Org: &org.Organization{}}

	if got := (&Engine{}).chatDeps(c); got.Reader != nil || got.Writer != nil {
		t.Errorf("an engine with no native backends offered chat deps %+v", got)
	}
	// AND A HALF-BUILT NODE IS STILL NOTHING. The reader and the store are
	// built in one step and a node that has one without the other is a
	// bring-up that failed, not a surface to serve half of.
	half := &Engine{native: &native{chat: &chat.Store{}}}
	if got := half.chatDeps(c); got.Reader != nil || got.Writer != nil {
		t.Errorf("a node holding only the chat write side offered %+v", got)
	}
}

// THE SEARCHER IS GATED ON THIS NODE'S INDEX, not on the rooms.
//
// They are different facts: the store and the reader come up with the chat
// domain, and the index is this node's own keyword corpus behind it. A
// searcher offered without one would register `search_messages` against an
// index that does not exist, and the tool answers "nothing matched" for every
// query a company ever runs — indistinguishable from a quiet company.
func TestChatSearchIsGatedOnTheIndex(t *testing.T) {
	t.Parallel()
	rooms := &native{chat: &chat.Store{}, chatReader: &chat.Reader{}}
	if got := (&Engine{native: rooms}).chatSearch(); got != nil {
		t.Errorf("a node with rooms but no chat index offered a searcher %v — "+
			"search_messages must be omitted, not registered and empty", got)
	}

	rooms.chatIndex = &search.ChatIndexer{}
	if got := (&Engine{native: rooms}).chatSearch(); got == nil {
		t.Error("a node holding the chat index offered no searcher, so " +
			"search_messages is missing from a company that can serve it")
	}
	// AND THE EXPORTED SEAM IS THE SAME ONE. The operator surface takes
	// it through [LiveChatSearch]; a second implementation there would be
	// a second answer to which rooms somebody may read.
	if got := LiveChatSearch(&Engine{native: rooms}); got == nil {
		t.Error("LiveChatSearch gave the operator surface nothing on a node " +
			"that serves the seat surface, so the two disagree about whether " +
			"this company has chat search at all")
	}
}
