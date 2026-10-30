package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The chat surface's own wiring guards.
//
// THE REGRESSION THESE EXIST FOR: the nine chat tools were written, tested,
// catalogued and documented, and `equip` never assigned `deps.Chat` — so every
// one of them was gated off against a zero [builtin.ChatDeps]. A company on
// `chat.backend: chatlet` ran the domain, applied its records and woke its
// seats about messages they had no tool to answer with, and nothing anywhere
// went red. The same omission had already cost this surface its operator half.

// roomsNode is a publishing data node running Chatlet, with a router.
func roomsNode(t *testing.T) *Engine {
	t.Helper()
	e, _ := statelessOver(t)
	e.mode = statelog.ModeNormal
	e.native.Store(&native{chat: &chatlet.Store{}, chatReader: &chatlet.Reader{}})
	return e
}

// EVERY NODE HANDS ITS SEATS THE ROUTER'S ROOMS, never its own copy: a data
// node answers them from its copy through the router while that copy serves and
// from a peer's while it does not, and a node that holds no data asks a data
// node for every one — so the seat's tools are the same tools on both.
func TestChatDepsReachTheRoomsThroughTheRouter(t *testing.T) {
	t.Parallel()
	c := &Company{Config: &config.Company{}, Org: &org.Organization{}}

	stateless, _ := statelessOver(t)
	stateless.remote.Store(&remoteNative{chat: true})
	for name, e := range map[string]*Engine{
		"a data node running chat": roomsNode(t),
		"a node holding no data":   stateless,
	} {
		deps := e.chatDeps(c)
		if _, routed := deps.Reader.(estate.Chat); !routed {
			t.Errorf("%s gave its seats the reader %T, want the router's — read off this "+
				"node's own copy, a copy out of service answers its seats from rows it "+
				"no longer vouches for", name, deps.Reader)
		}
		if _, routed := deps.Writer.(estate.Chat); !routed {
			t.Errorf("%s gave its seats the writer %T, want the router's — six of the "+
				"nine tools write, including every one that counts as a delivery",
				name, deps.Writer)
		}
		if _, routed := deps.Search.(estate.ChatSearch); !routed {
			t.Errorf("%s gave its seats the chat search %T, want the router's — "+
				"the serving data node resolves the viewer's rooms beside the "+
				"index it reads", name, deps.Search)
		}
		if deps.Mentions == nil {
			t.Errorf("%s: chat deps carry no mention resolver, so `@alice` in a "+
				"message would resolve to nobody and wake nobody, while the "+
				"tool's own description promises otherwise", name)
		}
		if deps.Await == nil {
			t.Errorf("%s: chat deps carry no Await, so a seat that posts and then "+
				"reads the room may not see its own message — which is exactly "+
				"how a model comes to say the same thing twice", name)
		}
		// AND NO ACTOR, which is what tells a seat's deps from the
		// operator surface's: nil attributes the write to the turn's own
		// seat, and a model that could name its author could speak as
		// anybody.
		if deps.Actor != nil {
			t.Errorf("%s: the seat surface set an Actor — a seat writes as itself, "+
				"resolved from the turn rather than from anything a model says", name)
		}
	}
}

// A NODE THAT CANNOT SERVE THE ROOMS OFFERS NOTHING, rather than tools whose
// every call is refused: the tools are OMITTED on a zero deps, which is how a
// seat is never offered a tool its company cannot serve.
func TestChatDepsAreEmptyWhereNoRoomsAreServed(t *testing.T) {
	t.Parallel()
	c := &Company{Config: &config.Company{}, Org: &org.Organization{}}

	// A HALF-BUILT NODE IS STILL NOTHING. The reader and the store are built
	// in one step and a node that has one without the other is a bring-up
	// that failed, not a surface to serve half of.
	half := roomsNode(t)
	half.native.Store(&native{chat: &chatlet.Store{}})
	// A MAINTENANCE MODE PUBLISHES NOTHING, and a seat's chat tools are
	// publishers ([Engine.writeSide]).
	sealed := roomsNode(t)
	sealed.mode = statelog.ModeSeal
	vendors, _ := statelessOver(t)
	vendors.remote.Store(&remoteNative{tracker: true, wiki: true})
	for name, e := range map[string]*Engine{
		"no native runtime":                     {},
		"a node holding only the write side":    half,
		"a data node in seal mode":              sealed,
		"a stateless node of a vendor-chat one": vendors,
	} {
		if got := e.chatDeps(c); got.Reader != nil || got.Writer != nil || got.Search != nil {
			t.Errorf("%s offered chat deps %+v", name, got)
		}
	}
}

// THE SEARCH FOLLOWS THE COMPANY'S CHAT, NOT THIS NODE'S INDEX.
//
// The index a search reads is the serving data node's, beside the rooms that
// node resolves the viewer's set from ([estate.ChatQuery]) — so a node holding
// no data, which has no index at all, still offers its operator surface the
// search, and so does a data node in a maintenance mode: the facade is a read,
// and whether a surface hands it out there is that surface's decision (its
// seats get no chat tools — [TestChatDepsAreEmptyWhereNoRoomsAreServed] — and
// the operator's catalogue serves the log on the wiki's terms). A company whose
// chat is a vendor's has nothing to search.
func TestChatSearchFollowsTheCompanysChat(t *testing.T) {
	t.Parallel()
	stateless, _ := statelessOver(t)
	stateless.remote.Store(&remoteNative{chat: true})
	sealed := roomsNode(t)
	sealed.mode = statelog.ModeMaintenance
	for name, e := range map[string]*Engine{
		"a data node running chat":          roomsNode(t),
		"a node holding no data":            stateless,
		"a data node in a maintenance mode": sealed,
	} {
		if _, ok := OperatorChatSearch(e); !ok {
			t.Errorf("%s offered its operator surface no chat search, though the "+
				"company's chat is the engine's own", name)
		}
	}
	vendors, _ := statelessOver(t)
	vendors.remote.Store(&remoteNative{tracker: true})
	if _, ok := OperatorChatSearch(vendors); ok {
		t.Error("a company whose chat is a vendor's was offered a Chatlet search")
	}
}
