package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE COMPANY'S OWN CHAT, served to a person.
//
// # The one rule the whole surface is built around
//
// THE VIEWER IS RESOLVED BY THE SERVER, ON EVERY CALL, AND A CALLER MAY NEVER
// NAME A SEAT. A person in chat IS a `kind: human` seat whose
// `contact.crewlet_operator_id` names one of Tier A's `api.auth.tokens`, and
// the chain from the presented credential to that seat is walked by
// [operator.ChatActor] — the same function the operator catalogue resolves its
// author with, because two implementations of "who is this" is two answers to
// the only question that decides what a transcript may show.
//
// A TOKEN BOUND TO NO SEAT GETS NOTHING, READS INCLUDED, and the refusal names
// `contact.crewlet_operator_id` because the remedy is a line of company
// configuration rather than a different credential. That is stricter than
// every other personal surface in this tree, and it is deliberate: a private
// room's contents are decided by its membership, a caller the engine cannot
// resolve to a seat has no membership anywhere, and a pipeline's credential is
// not a person. The two destructions below are the one exception, and on the
// purge's terms rather than chat's: they are said in nobody's voice, so they
// take any operator credential and name it ([App.chatModerator]).
//
// # What lives here, and what does not
//
// Every chat READ is a registered question in internal/api/queries, reached by
// a named route in rest.go exactly as the board's and the wiki's are: the
// socket's query channel is a thin adapter over the same function each REST
// route calls, and a read implemented here would be the second implementation
// that makes the two surfaces disagree.
//
// Every ordinary chat WRITE — a post, a reply, an edit, a delete, a reaction,
// a room, a membership, a person's read state — is a TOOL of the operator
// catalogue, reached through `POST /operator/act/{tool}` (ADR-0024). Not here,
// and there is no second route for any of them: one gesture with two write
// paths is two identities, two audit records — the act transport's
// `operator_acted` and nothing at all — and two answers to "what happened",
// and the copy that drifts is always the one somebody is pressing.
//
// What stays is what the TRACKER'S PURGE is to the tracker: the two gestures
// that DESTROY ([App.mountChat]). Neither is a person's ordinary conversation,
// neither has an inverse, and both carry a typed confirmation and the
// caller's own operation id — the shape `POST /work/{id}/purge` already
// answers in, so a destructive operator route is one convention however many
// domains have one.
//
// # A write does not push
//
// Neither route touches the live channel. A committed record reaches an open
// socket through the applier's own observer ([stream.ChatHub]), which every
// node runs — so a room pruned through this node is redrawn on a tab connected
// to another one. A push from the route would reach only the sockets of
// whichever node happened to serve the request, and would arrive BEFORE the
// rows it describes.

// MaxChatBody bounds the erase route's request body, the one chat write here
// that carries one.
//
// 128 KiB, from what an erase can legitimately say: [chat.MaxEraseMessages]
// ids of a uuid each — about ten kibibytes once quoted and separated — and a
// reason, the whole of which travels on a record no larger than
// [chat.MaxRecordBytes]. It is deliberately about twice that record rather
// than equal to it: this bounds the READ, so a body the record could carry is
// never refused here, and an oversized reason is refused by the domain naming
// its own field, which is the refusal a person can act on.
const MaxChatBody = 128 << 10

// ChatPurger is the chat half of the destructive operator routes, beside the
// tracker's [TaskPurger].
//
// Declared here, by the consumer, and kept to the two writes these routes
// make: a seam that also carried the ordinary gestures would be a second
// write path waiting for a route to use it, and every one of those is the act
// transport's.
//
// THE OPERATION IS THE CALLER'S, handed over whole, for the purge's reason: a
// gesture that answered `unknown` is finished by sending the id it answered
// with, and one sent with a fresh id is a second gesture.
type ChatPurger interface {
	Erase(ctx context.Context, actor chat.Actor, opID, channelID string,
		messageIDs []string, reason string) (chat.Written, error)
	Prune(ctx context.Context, actor chat.Actor, opID, channelID string,
		cutoff time.Time) (chat.Written, error)
}

// chatViewer resolves an operator id to the seat it is bound to, or "" for a
// credential bound to none.
//
// It is what the live channel identifies a socket with, and it is
// [operator.ChatViewer] — the resolution the operator catalogue attributes a
// chat write with for every caller its dispatch did not pin — so a person who
// may post in a room is exactly the person whose socket may be shown it. It
// runs at a socket's accept and again on every pass the hub makes afterwards
// ([stream.ChatHubOptions.Viewer]), which is why the chart it walks is built
// once per company document ([chatChartOf]).
//
// HANDED THE CREDENTIAL AND NOTHING ELSE. It used to put the operator on the
// context of whichever pass asked and resolve through [operator.ChatActor] —
// and one pass is a person's own request pushing their read state, carrying
// the seat the dispatch pinned for that person, which ChatActor reads before
// the chart. Every socket on the node then resolved to the caller, and every
// other person's was closed as rebound.
func chatViewer(resolve func(operatorID string) (chat.Actor, error)) func(string) string {
	return func(operatorID string) string {
		who, err := resolve(operatorID)
		if err != nil {
			return ""
		}
		return who.Handle
	}
}

// ChatLiveOptions configure the chat arm of the live channel.
//
// See [NewChatLive] for why this is built outside an App.
type ChatLiveOptions struct {
	// Company reads the CURRENT epoch. It is the same accessor
	// [Options.Sources.Company] takes, and handing both the SAME function
	// is what makes "who may write in a room" and "whose socket may be
	// shown it" one answer.
	Company func() *config.Company

	// Env resolves a `${VAR}` binding in that company's chart through the
	// node's own chain — the lookup [Options.Sources.Env] takes, for the
	// same reason: a person bound by a sealed or a handed variable is
	// admitted by every other surface, and a socket resolved through the
	// process environment instead would show them no room at all. Nil reads
	// the process environment.
	Env org.EnvLookup

	// Rooms is chat's read side, for the visibility filter. A function
	// because the reader does not exist yet when this is built.
	Rooms func() stream.ChatRooms

	// Peers carries presence across the fleet. Nil keeps it local to this
	// node's own sockets.
	Peers func() stream.ChatPeers

	// NodeID names this node in its own presence answer.
	NodeID string
}

// NewChatLive builds the chat arm of the live channel over seams the caller
// supplies. A node builds it through [NewEngineChatLive], which supplies them
// from its engine; this is the shape a test hands its own rooms to.
//
// # Why this is not built inside [New]
//
// The chat applier takes its observer when the state log comes up, and the
// state log comes up before the API exists — so a hub built by [New] could
// never be the thing the applier announces to. The process therefore builds
// this first, hands it to the engine (as the chat applier's [chat.Observer]
// and as the working indicator's [notify.StatusPoster]) and to
// [Options.ChatLive], and every seam it reads through is a function so that
// none of them has to exist yet.
//
// What it adds over [stream.NewChatHub] is the one thing this package owns:
// the resolution from a credential to a seat. That is deliberately not the
// caller's to supply — a second resolution would be a second answer to who a
// socket is, and the whole surface rests on there being one.
func NewChatLive(opts ChatLiveOptions) *stream.ChatHub {
	return stream.NewChatHub(stream.ChatHubOptions{
		Rooms:  opts.Rooms,
		Viewer: chatViewer(operator.ChatViewer(chatChartOf(opts.Company), opts.Env)),
		Peers:  opts.Peers,
		NodeID: opts.NodeID,
	})
}

// EngineChatLive is the chat arm of the live channel as a node runs it over
// its engine: one hub, and the engine it reads through once there is one.
// [NewEngineWithChat] is how a node gets one.
//
// HERE, beside [NewEngineOperator], for that constructor's reason: `crewlet
// run` and the end-to-end suite must run the same hub over the same seams, and
// a harness that assembled its own would certify what it wired rather than
// what a node runs.
//
// # Its lifetime is the PROCESS's, not the API's
//
// The hub is three loops, and only one of them is about this node's sockets.
// The others are the fleet's: the answerer a peer's presence probe reaches and
// the person fan-out ([stream.ChatHub.Start]). The answerer is how a working
// indicator raised HERE reaches a room open somewhere else — and the node
// running a seat's turn is rarely the node a browser is attached to, so the
// nodes most likely to raise one are exactly the nodes with no listener. The
// hub used to be started and stopped by [App], so a node without the ingress
// role, or with `api.port: 0`, recorded every indicator its seats raised in a
// map no probe ever read: the seat working, and every room showing nothing.
//
// So it STARTS BEFORE THE ENGINE ([NewEngineWithChat]), on every node,
// whether or not one serves an API. Before rather than after: the chat applier
// announces to it from inside [engine.New] on, and a catch-up at boot that
// outran a hub not yet draining its queue was reported as a live fan-out that
// fell behind the applier — a warning about a fault that did not exist.
//
// # It runs until Stop, and the context does not end it
//
// The context that wakes a node's shutdown is cancelled at the START of the
// drain, and the drain is when the turns already running finish: their
// replies are committed and pushed, and their indicators come down, while it
// runs. A hub that ended with that context went dark for the one stretch an
// operator watching a rolling restart most wants it. Its owner stops it after
// the drain and before the engine's teardown, which closes the broker its
// answerers are subscribed on.
type EngineChatLive struct {
	hub *stream.ChatHub

	// running is the engine the seams read through: nil until [Attach],
	// which every seam reads as its half not being up yet. ATOMIC, because
	// the hub's loops are already running when the engine that fills it is
	// built.
	running atomic.Pointer[engine.Engine]
}

// The hub is what the engine is handed as its chat's live half, in one value
// for both of the halves [engine.ChatLive] names.
var _ engine.ChatLive = (*stream.ChatHub)(nil)

// NewEngineWithChat builds a node's engine with its chat hub already running:
// the one sequence `crewlet run` and the end-to-end suite both boot through.
//
// THE HUB IS BUILT AND STARTED BEFORE THE ENGINE, and it is the one seam of a
// node that has to be. It is a single object with consumers that come up at
// different times: the chat applier announces every committed record to it
// from inside [engine.New], a native seat's working indicator is raised on it,
// the API serves its announcements onto open sockets — and its presence
// answerer is how a peer's room shows an indicator raised HERE. So its
// lifetime is the process's, on every node whatever its roles, and never the
// API's ([EngineChatLive]).
//
// ONE HUB, not one per consumer. Two would mean the applier announces to an
// object no socket is attached to, which is indistinguishable from a company
// where nobody is talking.
//
// ONE FUNCTION, not a sequence each boot path writes out: started, handed to
// [engine.New] as its live half, stopped if the engine is refused, attached
// once it exists. Written out twice the copies drifted — the end-to-end suite
// once booted an unstarted hub — and a harness that assembled its own
// certified its own ordering rather than the one a node runs.
//
// ITS NODE ID IS THE ONE THE ENGINE RUNS UNDER, resolved here exactly as
// [engine.New] resolves it ([config.ResolveNodeID]), never a raw `node.id`:
// that is empty on a node named by CREWLET_NODE_ID, and a hub told "" never
// recognises its own presence reply.
//
// The caller owns both halves from here: it stops the hub after the drain
// and before the engine's teardown, which closes the broker the hub's
// answerers are subscribed on.
func NewEngineWithChat(ctx context.Context, opts engine.Options) (
	*engine.Engine, *EngineChatLive, error) {

	nodeID, err := config.ResolveNodeID(opts.Bootstrap, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("node identity: %w", err)
	}
	live := NewEngineChatLive(nodeID)
	live.Start(ctx)
	opts.ChatLive = live.Hub()
	e, err := engine.New(ctx, opts)
	if err != nil {
		live.Stop()
		return nil, nil, err
	}
	live.Attach(ctx, e)
	return e, live, nil
}

// NewEngineChatLive builds a node's chat hub; [EngineChatLive.Start] runs it.
// nodeID is the name the engine runs under, never a raw `node.id` that may be
// empty: it is how this node's own presence answer is told apart from its
// peers'.
func NewEngineChatLive(nodeID string) *EngineChatLive {
	l := &EngineChatLive{}
	l.hub = NewChatLive(ChatLiveOptions{
		Company: func() *config.Company {
			if e := l.running.Load(); e != nil && e.Company() != nil {
				return e.Company().Config
			}
			return nil
		},
		// THE ENGINE'S OWN CHAIN, once there is an engine: before
		// [EngineChatLive.Attach] there is no company either, so nothing
		// is resolved through it.
		Env: func(name string) (string, bool) {
			if e := l.running.Load(); e != nil {
				return e.LookupSecret(name)
			}
			return "", false
		},
		Rooms:  func() stream.ChatRooms { return EngineChatRooms(l.running.Load()) },
		Peers:  func() stream.ChatPeers { return engineChatPeers(l.running.Load()) },
		NodeID: nodeID,
	})
	return l
}

// Start runs the hub's loops until [EngineChatLive.Stop].
//
// WITHOUT THE CALLER'S CANCELLATION; see the type's doc. The context's values
// travel and its cancellation does not, so the drain a cancelled context
// begins is one the hub is still running for.
func (l *EngineChatLive) Start(ctx context.Context) {
	if l != nil {
		l.hub.Start(context.WithoutCancel(ctx))
	}
}

// Hub is the hub itself: what the engine is handed as its chat's live half
// ([engine.Options.ChatLive]) and the API as the socket's ([Options.ChatLive]).
// Nil on a nil receiver, which is a node that runs none.
func (l *EngineChatLive) Hub() *stream.ChatHub {
	if l == nil {
		return nil
	}
	return l.hub
}

// Attach hands the hub the engine its seams read through, once it exists,
// and returns once the hub answers its peers through that engine's broker
// ([stream.ChatHub.Wired]) — so a node is never one its colleagues' scatters
// pass over for the first presence tick of its life.
func (l *EngineChatLive) Attach(ctx context.Context, e *engine.Engine) {
	if l != nil {
		l.running.Store(e)
		l.hub.Wired(ctx)
	}
}

// Stop ends the hub's loops and waits for them. Idempotent, and a no-op on a
// nil receiver.
func (l *EngineChatLive) Stop() {
	if l != nil {
		l.hub.Stop()
	}
}

// EngineChatRooms is chat's read side as the live hub decides visibility with
// it — whether one viewer may read one room — or nil where the engine's chat is
// not its own.
//
// THE ESTATE ROUTER'S ([engine.OperatorChat]), and not this node's copy, for
// two reasons that land on different nodes. A node that holds no data has no
// copy at all, and it is exactly the node whose seats are running the turns:
// read off a copy, its hub refused every presence probe it was sent ("no
// reader, no answer"), and the indicator never left it. And a data node whose
// copy is out of service would decide who may see a private room from the
// rows it has stopped serving.
//
// THE TWO READS ARE ASKED DIFFERENTLY ([stream.ChatRooms]). A presence
// decision is the router's ordinary walk, which passes a copy that LAGS its
// log over for a peer's. A committed frame's decision is
// [estate.Chat.ChannelHere]: this node's own copy first even while it lags —
// its applier is the one announcing the record, so it holds it by
// construction, and the ordinary walk would make every decision during the
// burst that caused the lag a round trip to a peer that may not have applied
// it — held to the batch's own position, so whichever copy answers holds the
// records being announced.
//
// UNTYPED NIL where there is none: a zero facade held in the interface is not
// a nil interface, and the hub reads a non-nil reader as permission to decide.
func EngineChatRooms(e *engine.Engine) stream.ChatRooms {
	if e == nil {
		return nil
	}
	if rooms, ok := engine.OperatorChat(e); ok {
		return rooms
	}
	return nil
}

// EngineChat fills a node's API with its native chat: the read surface's
// rooms and its ranking over them ([queries.Sources.Chat],
// [queries.Sources.ChatSearch]), each person's read state
// ([queries.Sources.ChatReads]), the two destructions ([Options.ChatPurger])
// and the live arm ([Options.ChatLive]) — from the engine and the hub
// [NewEngineWithChat] built beside it.
//
// ONE WIRING, here beside [NewEngineOperator] and [EngineFiles] for their
// reason: `crewlet run` and the end-to-end suite must serve chat over the same
// seams. Assembled by hand in each, the two copies could drift with nothing to
// notice — a seam one of them read off the node's own copy, or left unset,
// passes every case that exercises the other.
//
// SEPARATE SEAMS FOR ONE FEATURE, because they are separate authorities: the
// reader reads, the search ranks for a viewer, the writer destroys and the
// fleet holds a person's own cursors. Handing the API one object that did all
// of it would invent a facade nothing else in the process has.
//
// EVERY ESTATE SEAM IS THE ROUTER'S ([engine.OperatorChat],
// [engine.OperatorChatSearch], [engine.OperatorChatWriter]): read straight off
// this node's copy, a data node whose copy was out of service went on drawing
// its operator's rooms from the rows it had stopped serving its seats from.
// Through the router it answers from a peer's copy that is serving, as its
// seats' own tools do. That is the whole reason: the routes these seams feed
// are served only on a node that holds a copy, since Tier A refuses `ingress`
// without `data`.
//
// A SEAM WITH NOTHING BEHIND IT IS LEFT NIL — an untyped nil, never a facade
// held in the interface, which the read surface and [New] would take for a
// surface and mount. A company on `chat.backend: vendor` or `none` has no
// native chat, so its questions and routes stay unregistered rather than
// answering an empty room as though it had been read; and the writer is
// narrower than the reads, on the purge's terms — a data node in a maintenance
// mode publishes nothing, so it serves the rooms and mounts no destruction it
// cannot carry out. THE READ STATE IS THE FLEET'S, whatever the backend: a
// cursor is one person's attention and follows them across nodes, so a
// per-node answer would reset their unread counts on every failover.
//
// live is the hub the applier announces to — the one [NewEngineWithChat]
// built before the engine — or nil for a node that serves no live chat.
// Every field this sets is this function's: a caller that also set one would
// be a second answer to the same question, so each is overwritten.
func EngineChat(opts *Options, e *engine.Engine, live *stream.ChatHub) {
	var reader queries.ChatReader
	if rooms, ok := engine.OperatorChat(e); ok {
		reader = rooms
	}
	var search queries.ChatSearcher
	if searcher, ok := engine.OperatorChatSearch(e); ok {
		search = searcher
	}
	var purger ChatPurger
	if writer, ok := engine.OperatorChatWriter(e); ok {
		purger = writer
	}
	opts.Sources.Chat, opts.Sources.ChatSearch = reader, search
	opts.Sources.ChatReads = e.Backends().Fleet
	opts.ChatPurger, opts.ChatLive = purger, live
}

// engineChatPeers is the ephemeral scatter presence and a person's pushes ride:
// the engine's own queue, or nil before there is one.
func engineChatPeers(e *engine.Engine) stream.ChatPeers {
	if e == nil || e.Backends() == nil || e.Backends().Queue == nil {
		return nil
	}
	return e.Backends().Queue
}

// chatActorFor builds the per-request author resolution over the live epoch:
// [operator.ChatActor] over [chatChartOf].
//
// lookup resolves a `${VAR}` binding through the serving node's own chain
// ([Options.Sources.Env]), which is where the act transport admits the same
// person: nothing here reaches the act transport's pin, so a person bound by a
// sealed or a handed variable was refused as unbound by these routes and shown
// no room on the socket while every act they made was attributed to them.
func chatActorFor(company func() *config.Company,
	lookup org.EnvLookup) func(context.Context) (chat.Actor, error) {
	resolve := operator.ChatActor(chatChartOf(company), lookup)
	return func(ctx context.Context) (chat.Actor, error) {
		// THE TURN IS NIL AND THERE IS NOWHERE TO PUT ONE. That
		// argument exists for the seat surface, where a write is
		// attributed to the turn's own seat; here the credential is the
		// whole of the identity, which is exactly what makes a caller
		// unable to name one.
		return resolve(ctx, nil)
	}
}

// chatChartOf is the org chart of the live epoch, for the two chat
// resolutions this package makes — a destruction's author ([chatActorFor])
// and a socket's seat ([chatViewer]).
//
// A FUNCTION OF THE COMPANY, read on every call, for the reason every other
// source here is one: an apply replaces the org chart, and a resolution
// captured at boot would keep attributing a message to the handle a renamed
// seat used to have — or keep admitting a token whose binding a revision
// removed.
//
// # The chart is built once per company, not once per call
//
// The live channel asks for it on every pass, for every socket's credential
// ([stream.ChatHub]), so an org chart built afresh per call would be a chart
// per credential per committed batch. A company document is a value — an
// apply installs a new one rather than editing the old — so the chart built
// from one is kept beside it and rebuilt only when the document changes. The
// binding itself is still read on every call: a `${VAR}` names a value in the
// node's chain, which a secret's rotation moves without any apply.
func chatChartOf(company func() *config.Company) func() *org.Organization {
	var built atomic.Pointer[chartOf]
	return func() *org.Organization {
		if company == nil {
			return nil
		}
		c := company()
		if c == nil {
			return nil
		}
		if held := built.Load(); held != nil && held.company == c {
			return held.chart
		}
		organization, err := c.Organization()
		if err != nil {
			// A company that will not resolve into an org chart is one
			// no node is running, so there is no seat to be. The
			// refusal names the binding, which is the same remedy
			// either way.
			organization = nil
		}
		built.Store(&chartOf{company: c, chart: organization})
		return organization
	}
}

// chartOf is one company document and the org chart built from it.
type chartOf struct {
	company *config.Company
	chart   *org.Organization
}

// mountChat registers the two chat writes that destroy.
//
// THE TWO THAT DESTROY, beside each other as the tracker's purge sits beside
// the eviction: an erase removes message rows for ever and a prune removes
// everything a room said before an instant, neither has an inverse, and both
// therefore carry a confirmation and the caller's own operation id, and are
// recorded as an OPERATOR's act.
//
// UNMOUNTED on a company whose chat is not the engine's own, rather than
// mounted and refusing: an endpoint that exists and answers 503 to everything
// reads as broken, while one that is not there matches what `chat.backend`
// says.
func (a *App) mountChat(mux httpjson.Router) {
	if a.chatPurger == nil {
		return
	}
	mux.Handle("POST /chat/channels/{channel_id}/erase", http.HandlerFunc(a.serveChatErase))
	mux.Handle("POST /chat/channels/{channel_id}/prune", http.HandlerFunc(a.serveChatPrune))
}

// chatModerator resolves the request's credential to whom a destruction is
// recorded as, or refuses it.
//
// # Credential-scoped, as every destructive operator route is
//
// The tracker's purge and the node gate — which writes its evictions to this
// same chat log — accept ANY operator credential and record the token beside
// the gesture. An erase and a prune are the same kind of act on a sibling
// domain, and an authority narrower than theirs left a compliance pipeline able
// to purge a task and evict a node from this very log while unable to erase a
// credential somebody pasted into a room — and a deployment with `api.auth`
// disabled unable to erase anything by hand. So the credential is the
// authority, and the binding decides only how the act is NAMED:
//
//   - a credential bound to a person's `kind: human` seat is recorded as that
//     person ([operator.ChatActor]): the seat's handle, with the token beside
//     it, so the marker says WHO destroyed the messages and under which
//     credential;
//   - a credential no seat binds — a pipeline's token, a disabled guard's
//     anonymous caller — is recorded under its own id with kind `operator`,
//     exactly as the purge and the gate record it. Chat refuses such a
//     credential everywhere else because a message needs a speaker; a
//     destruction is said in nobody's voice, so it needs none.
//
// The write path demands the kind in its own decide ([chat.Store.Erase],
// [chat.Store.Prune]) — an author that could erase its own messages could
// erase the evidence of what it did.
//
// A REQUEST WITH NO OPERATOR AT ALL is refused `operator_required`, the
// purge's own refusal of the same build: the guard already turned away an
// unauthenticated caller, so this is a build that put no identity on the
// context. The resolution's other two refusals — an agent seat, a seat an
// apply removed mid-call — need a seat the act transport's admission pinned,
// and nothing pins one for this route; were one to reach it, it is answered
// under its own code rather than re-attributed to the token, because that
// caller was somebody and the remedy is theirs to make.
func (a *App) chatModerator(w http.ResponseWriter, r *http.Request) (chat.Actor, bool) {
	operatorID, ok := auth.OperatorFrom(r.Context())
	if !ok || operatorID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "operator_required",
			"detail": "an erase and a prune are operator gestures and this request " +
				"carries no operator identity",
		})
		return chat.Actor{}, false
	}
	actor, err := a.chatActor(r.Context())
	var refused *operator.ChatActorError
	switch {
	case err == nil:
		return actor, true
	case errors.As(err, &refused) && refused.Refusal == operator.ChatUnbound:
		return chat.Actor{Handle: operatorID, Kind: chat.AuthorOperator,
			OperatorID: operatorID}, true
	case errors.As(err, &refused):
		// 403 AND NOT 401. The credential was accepted — the guard let
		// this request through — and what is wrong is the seat it names.
		httpjson.FailWith(w, http.StatusForbidden, refused.Refusal.Code(),
			map[string]string{"detail": err.Error()})
	default:
		// A REFUSAL WITH NO REASON is this build's, never the caller's:
		// the resolution types every refusal it makes.
		log.ErrorContext(r.Context(), "chat_moderator_unclassified", "error", err)
		httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeInternalError,
			map[string]string{"detail": err.Error()})
	}
	return chat.Actor{}, false
}

// chatErase is what an erase removes and why.
type chatErase struct {
	MessageIDs []string `json:"message_ids"`

	// Reason is REQUIRED here although the record makes it optional. The
	// rows are destroyed and there is no inverse, so the reason is the
	// account of them that is left — on the erase's runtime audit record
	// ([types.ChatErased]), the one record of it a person can read
	// afterwards. The tracker's purge route requires one for the same reason.
	Reason string `json:"reason"`
}

// serveChatErase answers POST /chat/channels/{channel_id}/erase.
//
// THE CONFIRMATION IS THE COUNT, echoed in `?confirm=`. Repeating the room id
// would confirm nothing — it is already in the URL that carries the request —
// while the number of messages is a value the caller had to establish, which
// is the whole point of asking for one.
func (a *App) serveChatErase(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatModerator(w, r)
	if !ok {
		return
	}
	room := r.PathValue("channel_id")
	var body chatErase
	if !readChatBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "reason_required",
			"detail": "state why in `reason` — the rows are destroyed on every " +
				"node with no inverse, and the reason is what this erase's " +
				"audit record says about them",
		})
		return
	}
	if confirm := r.URL.Query().Get("confirm"); confirm != strconv.Itoa(len(body.MessageIDs)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "confirm_required",
			"detail": "repeat the number of messages in ?confirm= — an erase " +
				"removes those rows permanently, on every node, and nothing " +
				"undoes it",
		})
		return
	}
	// THE CALLER MAY BRING ITS OWN OPERATION ID, for the purge's reason: an
	// erase that answers `unknown` may have landed, and a retry under a
	// fresh id would be a second gesture. HELD TO THIS ERASE, because the
	// room it arbitrates on is not the gesture ([heldOpID]).
	opID, ok := heldOpID(w, r, chat.EraseOpName(room, body.MessageIDs))
	if !ok {
		return
	}
	echo := map[string]any{"channel_id": room, "messages": len(body.MessageIDs)}
	record := types.ChatErased{
		OperatorID: actor.OperatorID,
		ActorSeat:  operator.BoundSeat(a.chart, a.contacts, actor.OperatorID),
		ChannelID:  room, Messages: len(body.MessageIDs), Reason: body.Reason, OpID: opID,
	}
	// AUDITED WHATEVER BECAME OF IT, once the write path was asked — the
	// runtime audit's rule, and the only account of an erase a person can
	// read afterwards, since what it named is gone from every node.
	defer func(ctx context.Context) {
		operator.Audit(ctx, a.audit, types.NewChatErased(record))
	}(r.Context())
	written, err := a.chatPurger.Erase(r.Context(), actor, opID, room,
		body.MessageIDs, body.Reason)
	if err != nil {
		record.Outcome, record.Refusal = refuseChatPurge(w, r, "erase", echo, opID, err)
		return
	}
	// LOGGED AT INFO WITH THE REASON, because this is one of the two
	// gestures here whose subject no longer exists to be inspected.
	log.InfoContext(r.Context(), "chat_erased", "channel", room,
		"messages", len(body.MessageIDs), "operator", actor.OperatorID,
		"seat", actor.Handle, "reason", body.Reason, "op_id", opID,
		"outcome", written.Outcome.Outcome)
	record.OpID = chatOpID(written, opID)
	record.Outcome, record.Position = destroyedAudit(written.Outcome)
	answerDestroyed(w, echo, written.Outcome.Outcome, record.OpID,
		written.Outcome.Position)
}

// serveChatPrune answers POST /chat/channels/{channel_id}/prune.
//
// THE CUTOFF IS A QUERY PARAMETER AND IS ECHOED IN `?confirm=`, so the
// instant that decides how much of a room disappears appears twice in one
// request — the same guard the capacity window's byte ceiling takes, and for
// the same reason: this is not a value to inherit from a shell history.
//
// The ordinary producer of a prune is the retention duty, which publishes a
// cutoff as the engine's own [chat.AuthorSystem]. This route is the other
// caller: a person answering an erasure request for one room, now.
func (a *App) serveChatPrune(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.chatModerator(w, r)
	if !ok {
		return
	}
	room := r.PathValue("channel_id")
	raw := strings.TrimSpace(r.URL.Query().Get("cutoff"))
	cutoff, err := time.Parse(time.RFC3339, raw)
	if raw == "" || err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "cutoff_required",
			"detail": "name the instant in ?cutoff= as RFC 3339 — a prune " +
				"deletes everything the room said before it, on every node, " +
				"so there is no value to guess",
		})
		return
	}
	if r.URL.Query().Get("confirm") != raw {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "confirm_required",
			"detail": "repeat the cutoff in ?confirm= — everything said in " +
				"this room before it is destroyed on every node, with no inverse",
		})
		return
	}
	opID, ok := heldOpID(w, r, chat.PruneOpName(room, cutoff))
	if !ok {
		return
	}
	echo := map[string]any{"channel_id": room, "cutoff": cutoff.UTC()}
	record := types.ChatPruned{
		OperatorID: actor.OperatorID,
		ActorSeat:  operator.BoundSeat(a.chart, a.contacts, actor.OperatorID),
		ChannelID:  room, Cutoff: cutoff.UTC(), OpID: opID,
	}
	defer func(ctx context.Context) {
		operator.Audit(ctx, a.audit, types.NewChatPruned(record))
	}(r.Context())
	written, err := a.chatPurger.Prune(r.Context(), actor, opID, room, cutoff)
	if err != nil {
		record.Outcome, record.Refusal = refuseChatPurge(w, r, "prune", echo, opID, err)
		return
	}
	log.InfoContext(r.Context(), "chat_pruned", "channel", room,
		"cutoff", cutoff.UTC().Format(time.RFC3339),
		"operator", actor.OperatorID, "seat", actor.Handle, "op_id", opID,
		"outcome", written.Outcome.Outcome)
	record.OpID = chatOpID(written, opID)
	record.Outcome, record.Position = destroyedAudit(written.Outcome)
	answerDestroyed(w, echo, written.Outcome.Outcome, record.OpID,
		written.Outcome.Position)
}

// destroyedAudit is a destruction's write outcome as its runtime audit records
// it: the outcome in the audit's one vocabulary, which spells the three write
// outcomes as the state log does, and where the record landed — none for
// `unknown`, whose whole content is that nobody knows.
func destroyedAudit(result statelog.Result) (types.AuditOutcome, string) {
	outcome := types.AuditOutcome(result.Outcome)
	if result.Outcome == statelog.OutcomeUnknown {
		return outcome, ""
	}
	return outcome, result.Position.String()
}

// chatOpID is the operation a destruction's answer names: the one the write
// path reports, or the one this route handed it where an answer carried none.
//
// THE SAME VALUE ON EVERY PATH THAT WROTE, since the route hands the whole id
// over and the store records it unchanged; the fallback exists because an
// answer with no id is one a caller could not retry under, and the id this
// route sent is the one a retry must carry.
func chatOpID(written chat.Written, sent string) string {
	if written.ChangeID != "" {
		return written.ChangeID
	}
	return sent
}

// readChatBody decodes the erase route's body, or answers the refusal itself.
//
// UNKNOWN FIELDS ARE REFUSED, unlike on the socket's query channel. A query
// frame is lenient about content on purpose: an unknown field costs nothing to
// ignore. A destruction is the opposite — a key this build does not know is a
// caller saying something it will not get (`messages_ids`, `reasons`), and on
// a gesture with no inverse a silently ignored word is a different set of rows
// destroyed than the caller meant. The same refusal covers a body that tries
// to name whose act this is: the server resolves that, and there is no field
// for it.
func readChatBody[T any](w http.ResponseWriter, r *http.Request, into *T) bool {
	raw, err := httpjson.ReadBody(w, r, MaxChatBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		// NAMED rather than left to the decoder, whose own word for an
		// empty body is "EOF" — true, and nothing a caller can act on.
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]any{"detail": "an erase names the messages it destroys and " +
				"why, as a JSON body: {\"message_ids\": [...], \"reason\": \"...\"}"})
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]any{"detail": err.Error()})
		return false
	}
	return true
}

// refuseChatPurge answers a destruction the write path did not answer with an
// outcome, and reports what became of it in the runtime audit's terms — the
// outcome and, for a refusal, the code it answered with — so the audit record
// and the answer cannot classify one failure two ways.
//
// # A refusal says what to change, in the domain's own words
//
// EACH CLASS IS A DIFFERENT THING FOR THE CALLER TO DO, which is why they are
// not folded into one code: a refused value is a request to change, a room or
// a message that is not there is a dead link — a room the caller may not read
// answers identically, because a private room's EXISTENCE is information — a
// refusal is a permission to ask for, and a conflict is a moment to wait. The
// detail is [chat.Sentence]: the writer's own words without the package's
// prefix, which crossed the estate router intact ([chat.InvalidError] is
// carried on its wire).
//
// EVERY ANSWER NAMES THE OPERATION, a refusal included, as every answer the
// purge gives once it holds one: a client keeps one rule for every
// destruction — retry under the id the answer names — and is never left
// holding only the id it sent, or none where the route minted it. The one
// exception is the one that rule cannot finish: an id that already names a
// record on another subject is `409 op_reused` and names none
// ([answerReused]), because a retry under it is refused the same way for ever.
//
// # A destruction nobody answered is UNKNOWN, never failed
//
// The router repeats an unanswered erase or prune under this same id, as it
// repeats a purge, and reports one that NO data node answered as
// [estate.ErrOutcomeUnknown] rather than as an estate nobody serves — any node
// that stayed silent may have run it. That, and a request whose own deadline
// ended the wait, is answered exactly as a write path's own `unknown` is
// ([destructionUnknown]) — 200, the operation to retry under, and no position
// — rather than as a failure a caller would act on by sending a second gesture
// under a fresh id.
//
// # Anything else is this node's, and says so
//
// The route is mounted only on a data node out of a maintenance mode — one in
// a maintenance mode hands out no chat writer ([EngineChat]), and Tier A
// refuses `ingress` without `data` — so this is a node whose own copy is out
// of service and whose every peer it asked could not run the gesture either:
// each answered that its copy is out of service too, or that it is in a
// maintenance mode and holds no writer, as every node is once a capacity
// window opens after this route was mounted. Or the room is of a kind this
// build cannot serve. 503 with the reason. Nothing ran — but the operation
// travels beside it all the same, because a retry under the same id is safe
// whatever happened, and a client that keeps one rule for every destruction
// never has to decide which failure was which.
func refuseChatPurge(w http.ResponseWriter, r *http.Request, what string,
	echo map[string]any, opID string, err error) (types.AuditOutcome, string) {

	refused := func(status int, code string) (types.AuditOutcome, string) {
		writeJSON(w, status, map[string]string{
			"error": code, "detail": chat.Sentence(err), "op_id": opID,
		})
		return types.AuditRefused, code
	}
	switch {
	case errors.Is(err, chat.ErrInvalid):
		return refused(http.StatusBadRequest, "invalid")
	case errors.Is(err, chat.ErrNotFound):
		return refused(http.StatusNotFound, "not_found")
	case errors.Is(err, chat.ErrForbidden):
		return refused(http.StatusForbidden, "forbidden")
	case errors.Is(err, chat.ErrArchived):
		return refused(http.StatusConflict, "archived")
	case errors.Is(err, chat.ErrConflict):
		return refused(http.StatusConflict, "conflict")
	case operationReused(err):
		answerReused(w, what, err)
		return types.AuditRefused, string(statelog.ReasonOpReused)
	case destructionUnknown(err):
		log.WarnContext(r.Context(), "chat_destroy_outcome_unknown", "what", what,
			"op_id", opID, "error", err,
			"hint", "retry under the same op_id; a fresh one is a second gesture")
		answerDestroyed(w, echo, statelog.OutcomeUnknown, opID, statelog.Position{})
		return types.AuditUnknown, ""
	default:
		log.WarnContext(r.Context(), "api_chat_destroy_unserved", "what", what,
			"op_id", opID, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "unavailable", "detail": chat.Sentence(err), "op_id": opID,
		})
		return types.AuditFailed, ""
	}
}
