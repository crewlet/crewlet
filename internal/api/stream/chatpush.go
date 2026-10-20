package stream

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CHAT ARM OF THE LIVE CHANNEL, and the one rule that shapes all of it:
// EVERY FRAME IS FILTERED BY THE RECIPIENT'S OWN VISIBILITY BEFORE IT LEAVES.
//
// # Why chat cannot use the hub's broadcast
//
// [Hub.Broadcast] is the right shape for everything else here: a health tick,
// a roster, a token rollup and an event row are facts about the company, and
// every socket that may read the company at all may read them. A transcript is
// not. A private room's membership is the only thing deciding who may see what
// was said in it — and its very EXISTENCE is information, because who is
// talking to whom is most of what a conversation discloses — so a chat frame
// on the shared fan-out is a private room delivered to whoever happened to
// have a tab open. Every chat kind is therefore routed [RouteViewer], which
// [Hub.Broadcast] refuses.
//
// So this arm keeps its own registry of watchers, each with the SEAT its
// socket acts as, and decides per watcher. The decision is [chatlet.Visible] —
// the domain's own function, applied to the room and the membership a copy of
// the estate holds — and never a second idea of it: a filter that re-derived
// "is this room private" from a kind would be a second rule, and the two would
// disagree the first time a kind was added. And what it admits leaves by the
// one fan-out every push takes ([fanOut]), so the reader's `chatlet` need
// ([NeedChatlet]), the node's posture and the one encode per posture are each
// decided where they are for every other kind — and the first two are asked
// before the room is read ([Client.admits]), so a socket the frame would never
// reach costs no visibility read.
//
// # The viewer is the socket's, fixed for its life
//
// A socket is opened as one principal and keeps it: lifetime.go closes a
// socket the moment what it acts as moves — its seat, its login, its
// permissions, the seat leaving the company — and the reconnect is a new
// client with a new join. So the seat a socket watches as is decided ONCE, at
// its join, by [chatViewerOf] — [iam.ActorFor], the function a chat write's
// author comes from, so whoever may post in a room is exactly whoever's socket
// may be shown it — and never resolved again here: a second resolution per
// pass would be a second answer to who the socket is, and a read of the
// identity estate per socket per committed batch.
//
// # Three things travel here, and only one of them is a record
//
// A COMMITTED RECORD arrives through [chatlet.Observer], which the engine calls
// after a chat batch commits — its own applier's, or a peer's relayed while its
// own copy is out of service. That is a courtesy on top of the rows rather
// than a delivery anybody may rely on: a wake is derived by something that
// outlives the writer (see internal/changefeed), while a socket frame is
// worthless to a viewer who is not looking and is wanted in milliseconds by
// whoever is. Anything that misses a frame refetches, and the transcript's
// contiguous per-channel sequence is what makes that refetch exact — and a
// batch this hub could not take, or a gap in what the engine hears, owes every
// screen a re-read, which is sent rather than hoped for ([ChatHub.Reread]).
//
// PRESENCE is who is looking at which room, who is typing, and which seat is
// working on a message. None of it is durable, none of it is applied, and none
// of it may ever become a record — a keystroke that landed on the chat log
// would be a row every data node applies and every backup carries, for ever.
// It is the presence hub's (internal/presence, ADR-0038), as an artifact's
// room is: a tab's `presence` frame naming a chat room joins this socket to
// that room's presence ([ChatHub.presenceFrame]) once the room's own
// visibility admits its viewer, every frame between nodes is sealed under the
// fleet keyring, and what a socket is pushed is the hub's fan-out of the room
// ([ChatHub.presenceChanged]). A seat's working indicator is a member of the
// same room ([ChatHub.SetStatus]).
//
// A PERSON'S OWN READ STATE is the third, and it is pushed from the DURABLE
// RECORD rather than from whoever wrote it: every node serving sockets watches
// the coordination store's read-state records ([coord.ChatReads]), and a
// changed record is pushed to that seat's own sockets on that node and to
// nobody else ([ChatHub.watchReads]). So a flush made through any surface on
// any node reaches every tab, and no node carries out a delivery on another's
// word.
//
// # Nothing here may block the applier
//
// [ChatHub.Applied] runs on the applier's goroutine (or the relay follower's),
// between one batch's commit and the next batch's transaction, so a slow
// observer is a node whose log applies slowly. It therefore hands the batch to
// this hub's own buffer and returns, and a full buffer DROPS rather than
// waits — and owes every screen the re-read that covers what it dropped.

// The chat push kinds.
//
// A PUSH KIND IS WHAT HAPPENED AND A QUERY NAME IS WHAT IS BEING ASKED, and
// the two namespaces must stay disjoint — an envelope carries a `kind` and
// nothing else says which direction it was travelling, so a name in both
// makes a frame unreadable without knowing who sent it. The past tense is how
// that is kept true by construction here, and a gate asserts it against the
// registry's own names.
const (
	// KindChatPosted is a new message: a room post or a thread reply.
	KindChatPosted Kind = "chat_posted"

	// KindChatEdited is a message whose body was rewritten in place.
	KindChatEdited Kind = "chat_edited"

	// KindChatDeleted is a tombstone: the row survives with its body
	// blanked, so a thread does not lose the message it hangs off.
	KindChatDeleted Kind = "chat_deleted"

	// KindChatReacted is one emoji put on or taken off a message.
	KindChatReacted Kind = "chat_reacted"

	// KindChatRoomChanged is a room's own state moving: created, patched,
	// its horizon set, erased or pruned.
	//
	// ONE KIND FOR ALL OF THEM, because what a client does with them is
	// one thing — read the room again. An erase and a prune remove
	// messages and the record does not carry which, so a frame that tried
	// to name them would be a frame a client could not act on anyway.
	//
	// AND A FRAME OF IT NAMING NO ROOM (`channel_id` empty) IS A GAP: what
	// this socket was shown has a hole in it — the engine switched the
	// source its batches come from, or this hub dropped a batch it could
	// not keep up with — and the screen reads its rooms again, every one
	// it holds, rather than one. See [ChatHub.Reread].
	KindChatRoomChanged Kind = "chat_room_changed"

	// KindChatMembershipChanged is a room's membership set being replaced,
	// which includes somebody joining or leaving.
	KindChatMembershipChanged Kind = "chat_membership_changed"

	// KindChatCursorMoved is one person's own read state moving — where
	// they have read to, what they have muted, whether they are in
	// do-not-disturb — whole, as a [coord.PersonChatRead].
	//
	// IT REACHES THAT PERSON'S OWN SOCKETS AND NOBODY ELSE'S, on every
	// node serving sockets, derived from the coordination record. A cursor
	// is a fact about one reader's attention: it is not on the log, nobody
	// replays it, and a second tab — attached to whichever node its load
	// balancer picked — is the only thing in the world that wants it. A
	// client keeps the state with the highest `revision`, so a push and a
	// rail read racing each other settle on the newer.
	KindChatCursorMoved Kind = "chat_cursor_moved"

	// KindChatPresence is who is looking at, typing in, or working on a
	// message in the room this socket's `presence` frame named
	// ([ChatPresence]): the room whole as the socket joins it, and every
	// change the presence hub fans out after.
	//
	// NOT PAST TENSE, because it is not a thing that happened: it is the
	// room's presence, as [KindArtifactPresence] is an artifact's.
	KindChatPresence Kind = "chat_presence"
)

// ChatPushKinds is every kind this arm emits.
//
// Stated as a list so a gate can hold it against the read registry's own
// names — see the kinds above for why the two namespaces may not overlap —
// and against the route table, where every one is [RouteViewer].
func ChatPushKinds() []Kind {
	return []Kind{
		KindChatPosted, KindChatEdited, KindChatDeleted, KindChatReacted,
		KindChatRoomChanged, KindChatMembershipChanged, KindChatCursorMoved,
		KindChatPresence,
	}
}

// The arm's timings, and every number here is tied to what it buys.
const (
	// ChatStatusRefresh is what the working-indicator driver re-asserts
	// at: [presence.Rebroadcast]. See [ChatHub.StatusRefresh].
	ChatStatusRefresh = presence.Rebroadcast

	// ChatVisibilityBudget bounds ONE visibility decision.
	//
	// A live frame that cannot be decided inside it is DROPPED, never
	// sent: the room may be one this viewer is not in. Two seconds is
	// past the point at which a frame is live at all — a person watching
	// a room would rather refetch than be handed a line late — and it
	// stops a stalled store read from holding the whole fan-out behind
	// one viewer. It is the budget a fleet-wide history read gets
	// ([eventfan.FleetReadBudget]) for the same reason: past it, the
	// screen is better served by its own next read.
	ChatVisibilityBudget = 2 * time.Second

	// chatAppliedQueue is how many committed batches may wait for the
	// fan-out.
	//
	// Sixty-four. The applier must never wait here (see the package doc
	// above), so this is what absorbs a burst of applies while one
	// batch's visibility reads are in flight; past it a batch is dropped
	// and every screen is owed a re-read. It is small deliberately: a deep
	// queue would mean rendering a conversation minutes behind the rows,
	// which is worse than a re-read.
	chatAppliedQueue = 64

	// chatDropReportEvery is how often a hub says it is dropping, while it
	// runs.
	//
	// A MINUTE. A drop is a batch the live fan-out could not take, each
	// repaired by the re-read it owes every screen, so no single one is
	// worth a line. A stream of them is a node that cannot keep up,
	// though, and a line written only when the hub STOPPED is, for a node
	// that runs for months, never. So the first drop is said at once, and
	// then at most one line a minute names how many went since the last:
	// a burst is one line, a sustained overload a line a minute rather
	// than one per batch, and the rest is said as the hub stops.
	chatDropReportEvery = time.Minute

	// chatReadRewatchBase and chatReadRewatchCeiling space the read-state
	// watch's re-watch after it ends. The ceiling is how long a person's
	// other tabs may go without hearing their read state move while the
	// store recovers, so it is the fleet's own freshness cadence
	// ([coord.ReconcileInterval], fifteen seconds) — which is also how
	// often a tab flushes its read state, so a re-watch at the ceiling
	// misses at most the one flush its next rail read repairs. The base is
	// the seat-pause watch's, for the same store.
	chatReadRewatchBase    = 500 * time.Millisecond
	chatReadRewatchCeiling = coord.ReconcileInterval
)

// ChatRooms answers whether one viewer may read one room.
//
// Declared by the consumer and kept to the two reads this arm makes: a room
// read refuses a room the viewer may not read as NOT FOUND, and hands back the
// room and this viewer's membership for the rooms it does serve — which is
// exactly the pair [chatlet.Visible] decides from. A node hands it the estate
// router's facade ([estate.Chat]).
type ChatRooms interface {
	// Channel is the read a PRESENCE decision makes — who may be told
	// about a room somebody is looking at — which is about no record in
	// particular, so whichever copy the estate's walk prefers answers it.
	Channel(ctx context.Context, viewer, channelID string,
		fresh statelog.Freshness) (chatlet.ChannelDetail, error)

	// ChannelHere is the read a COMMITTED FRAME'S decision makes: asked of
	// this node's own copy first even while it lags its log, because that
	// copy's applier is the one announcing the record and holds it by
	// construction, where a peer the walk preferred might not yet — see
	// [ChatHub.fan].
	ChannelHere(ctx context.Context, viewer, channelID string,
		fresh statelog.Freshness) (chatlet.ChannelDetail, error)
}

// roomRead is one of [ChatRooms]' two reads, as a pass chooses it.
type roomRead func(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error)

// ChatReadWatch is the coordination store's read-state watch, as this arm
// consumes it: every record that exists, the marker, then every change.
// See [coord.ChatReads.WatchChatReads].
type ChatReadWatch interface {
	WatchChatReads(ctx context.Context) (<-chan coord.ChatReadUpdate, error)
}

// ChatPresence is a `chat_presence` push: one chat room's presence, as the
// presence hub fans it out ([presence.Change]), named by the room's id as
// every chat frame names its room.
//
// Roster set is the room WHOLE, as the socket joining it is handed it: replace
// what was held. Otherwise it is a change: the members now presented that are
// new or changed, and the sessions no longer presented. A person's member is
// viewing or, while they type, editing, its focus the thread they are typing
// under; a seat working on an answer is an agent's member, editing, its focus
// the thread the answer will land in and its status the line shown beside it.
// Past [presence.RoomTop] members the room presents the most recent and counts
// the rest, as an artifact's does.
//
// A ROSTER NAMING NO MEMBER is also what a socket is sent when it is taken out
// of a room it may no longer be in ([ChatHub.withdrawPresence]), so the room it
// was shown empties rather than standing.
type ChatPresence struct {
	ChannelID string            `json:"channel_id"`
	Roster    bool              `json:"roster,omitempty"`
	Upsert    []presence.Member `json:"upsert"`
	Leave     []string          `json:"leave"`
	Counts    *presence.Counts  `json:"counts,omitempty"`
	Truncated bool              `json:"truncated"`
}

// ChatChange is one committed chat record as a socket renders it.
//
// # It is evidence that something landed, never a copy of the message
//
// There is no body here. A frame carrying one would be a second rendering
// path for a conversation, and the two would disagree the first time an edit
// raced a reload — while the transcript is what a reload shows and is
// therefore the one that has to be right. What the frame carries instead is
// enough to decide what to do: the room, the message, and the CONTIGUOUS
// per-room sequence, so a client holding 41 and handed 43 asks for 42 rather
// than for the whole room.
//
// The excerpt and the mentions are the exception, and they are here because
// the RECORD already carries them, in its routing snapshot, for a card and a
// badge: an excerpt is at most [chatlet.MaxExcerpt] bytes of what a card shows,
// and the mentions are how a tab raises its own badge without a fetch. Every
// post and edit an author made carries a snapshot, even one that wakes
// nobody; the records that carry none — a system line, a delete, a reaction,
// and every room record — have neither, and neither is a substitute for
// reading the room. The thread root and the room's name and kind come from
// the same snapshot, and are absent on the same records.
//
// THE WAKE SET IS DELIBERATELY NOT HERE. Who the engine woke is a routing
// fact about seats, and a browser frame carrying it would publish the
// company's delivery decisions to every tab in the room.
type ChatChange struct {
	// ChannelID is the room. EMPTY ONLY ON A GAP — a `chat_room_changed`
	// that names no room, which asks a screen to read every room again
	// ([ChatHub.Reread]).
	ChannelID string `json:"channel_id"`

	// Op is the record's own word for what happened — `post`, `edit`,
	// `react` — rather than a second vocabulary of this transport's. It
	// is the value the event store's own column carries.
	Op chatlet.OpKind `json:"op,omitempty"`

	// OpID is the record's idempotency key, so a client that made this
	// gesture recognises its own effect coming back and renders it once.
	OpID string `json:"op_id,omitempty"`

	MessageID  string `json:"message_id,omitempty"`
	ChannelSeq int64  `json:"channel_seq,omitempty"`
	ThreadRoot string `json:"thread_root,omitempty"`

	Author     string             `json:"author,omitempty"`
	AuthorKind chatlet.AuthorKind `json:"author_kind,omitempty"`

	// At is the BROKER'S own instant, which is what every node renders —
	// a local clock here would make one viewer's timeline disagree with
	// another's.
	At time.Time `json:"at,omitzero"`

	// Position is where the record sits on the log. It is what a read
	// cursor stores, so a client that has rendered this frame can flush
	// its own cursor without a fetch.
	Position statelog.Position `json:"position,omitzero"`

	Excerpt  string   `json:"excerpt,omitempty"`
	Mentions []string `json:"mentions,omitempty"`

	// ChannelName and ChannelKind render a toast for a room the tab is
	// not looking at. Empty for a direct conversation, which has no name,
	// and on a record that carries no routing snapshot (see above).
	ChannelName string       `json:"channel_name,omitempty"`
	ChannelKind chatlet.Kind `json:"channel_kind,omitempty"`
}

// ChatHubOptions configure the chat arm.
type ChatHubOptions struct {
	// Rooms decides who may see what, answered per call: the estate's chat
	// half comes up with the node's first company, which on a node that
	// booted with none is after this hub exists. NIL, OR A NIL ANSWER,
	// SERVES NO FRAME AT ALL — a filter that cannot be run is not
	// permission to skip it — and the first record applied with none says
	// so once, at warn, because a silently dead push path looks exactly
	// like a quiet company.
	Rooms func() ChatRooms

	// Presence is this node's presence hub, which a chat room's presence
	// rides as an artifact's does: who has a room open, who is typing in it
	// and which seat is answering there. Nil shows nobody in any room and
	// raises no working indicator — honest on an engine with no broker,
	// which has no peer to tell either.
	Presence ChatPresenceHub

	// Reads is the coordination store's read-state watch, which every
	// node serving sockets follows to push a person's read state to their
	// own tabs. Nil pushes none: a tab then learns its read state from the
	// rail alone.
	Reads ChatReadWatch

	// Objectives records the change visibility of every committed record
	// this hub pushes ([Objectives]); nil records nothing.
	Objectives Objectives

	// Now is injectable so a test can pin the instant a frame is stamped.
	Now func() time.Time
}

// ChatPresenceHub is the presence hub as a chat room uses it: a socket's
// join, its frames and its leave, an agent's working line touched and
// withdrawn, and the fan-out.
type ChatPresenceHub interface {
	Join(room presence.Room, who presence.Local) (string, presence.Roster, error)
	Update(room presence.Room, session string, activity presence.Activity,
		focus presence.Focus) error
	Leave(room presence.Room, session string)
	Touch(room presence.Room, a presence.Agent)
	Withdraw(room presence.Room, seat string, focus presence.Focus)
	OnChange(fn func(presence.Change)) (stop func())
}

// chatWatcher is one socket watching chat.
type chatWatcher struct {
	client *Client

	// viewer is the seat this socket watches as ([chatViewerOf]). NEVER
	// empty in the registry: a socket whose principal may not be shown
	// chat is not registered at all, which is what makes "a credential
	// acting as itself sees nothing" a property of the data structure
	// rather than of a check somebody has to remember.
	viewer string

	// room is the chat room this socket's presence is in, by its id, and
	// session the member it is there; the zero room is in none. One room
	// at a time, because a tab looks at one room at a time.
	room    uuid.UUID
	session string
}

// seenKey is one visibility decision: one viewer, one room.
type seenKey struct{ viewer, channel string }

// ChatHub is the chat arm of the live channel: the watchers, the visibility
// filter, a chat room's presence and the read-state push.
//
// ONE PER PROCESS, and its lifetime is the process's rather than the API's: a
// seat raises its working indicator on whichever node its turn runs on, and
// the presence hub tells the room open on another node, so the engine is
// attached to a hub on every node, whether or not that node serves an API
// ([ChatHub.ServesSockets] says which).
type ChatHub struct {
	opts ChatHubOptions
	now  func() time.Time

	// applied carries committed batches from the applier's goroutine to
	// this hub's own. Buffered, and a full buffer drops — see the package
	// doc.
	applied chan []chatlet.Applied

	// rereadOwed is set when every screen is owed a re-read — a gap in
	// what the engine hears, or a batch this hub dropped — and rereadWake
	// tells the fan loop. A FLAG rather than an item on the queue, so it
	// can never be the thing a full queue drops: the one signal that
	// repairs a drop cannot itself be dropped.
	rereadOwed atomic.Bool
	rereadWake chan struct{}

	// sockets is closed once the API's socket surface is built over this
	// hub ([ChatHub.AttachSockets]); serving says so to the engine.
	sockets     chan struct{}
	socketsOnce sync.Once
	serving     atomic.Bool

	mu       sync.Mutex
	watchers map[*Client]*chatWatcher

	// pushed is the read-state revision last pushed per seat, so a watch
	// begun again re-delivering every record pushes only what moved.
	pushed map[string]uint64

	started bool
	stop    chan struct{}
	done    chan struct{}

	// stopPresence stops hearing the presence hub's fan-outs; nil while
	// the hub is not running or has no presence.
	stopPresence func()

	// unwired says the missing-reader warning has been given. Once, not
	// per record: this is the busiest log in the engine.
	unwired sync.Once

	// dropped counts batches the fan-out could not keep up with, and says
	// so while the hub runs — see [chatDropReportEvery].
	dropped dropTally
}

// The hub is both halves the engine is handed for its own chat: the applier's
// observer and the working indicator's poster. Held here, where the methods
// are, so a signature drifting from either seam fails this package's build
// rather than the first consumer that happens to pass the hub along.
var (
	_ chatlet.Observer    = (*ChatHub)(nil)
	_ notify.StatusPoster = (*ChatHub)(nil)
)

// NewChatHub builds the chat arm.
func NewChatHub(opts ChatHubOptions) *ChatHub {
	h := &ChatHub{
		opts:       opts,
		now:        opts.Now,
		applied:    make(chan []chatlet.Applied, chatAppliedQueue),
		rereadWake: make(chan struct{}, 1),
		sockets:    make(chan struct{}),
		watchers:   map[*Client]*chatWatcher{},
		pushed:     map[string]uint64{},
	}
	if h.now == nil {
		h.now = func() time.Time { return time.Now().UTC() }
	}
	return h
}

// Start runs the fan-out and the read-state push until ctx is done or [Stop] is
// called, and hears the presence hub's fan-outs of chat rooms.
//
// TWO GOROUTINES, not one: a read-state watch is a stream with a loop of its
// own, and a live message must not wait behind it.
//
// Idempotent: a second call while one is running is a no-op rather than a
// second set of loops, which would leave goroutines [Stop] never joins.
func (h *ChatHub) Start(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started = true
	stop, done := make(chan struct{}), make(chan struct{})
	h.stop, h.done = stop, done
	if h.opts.Presence != nil {
		h.stopPresence = h.opts.Presence.OnChange(h.presenceChanged)
	}
	h.mu.Unlock()

	var loops sync.WaitGroup
	loops.Go(func() { h.fanLoop(ctx, stop) })
	loops.Go(func() { h.readsLoop(ctx, stop) })

	go func() {
		defer close(done)
		loops.Wait()
	}()
}

// Stop ends the two loops and waits for them, stops hearing the presence hub,
// and says whatever was dropped since the last line [chatDropReportEvery]
// allowed. A hub stopped is not started again by its process.
func (h *ChatHub) Stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	stop, done, running := h.stop, h.done, h.started
	h.started = false
	h.stop, h.done = nil, nil
	stopPresence := h.stopPresence
	h.stopPresence = nil
	batches, total := h.dropped.rest(), h.dropped.total
	h.mu.Unlock()
	if !running {
		return
	}
	if stopPresence != nil {
		stopPresence()
	}
	close(stop)
	<-done
	reportBatchesDropped(batches, total)
}

// AttachSockets says the API's socket surface is built over this hub, so what
// it hears is shown to somebody on this node — see [ChatHub.ServesSockets] —
// and starts its read-state push, which only a node serving sockets has
// anybody to push to. Called once by the API that mounts the socket; a second
// call changes nothing.
func (h *ChatHub) AttachSockets() {
	if h == nil {
		return
	}
	h.socketsOnce.Do(func() {
		h.serving.Store(true)
		close(h.sockets)
	})
}

// ServesSockets reports whether this node shows anybody what [ChatHub.Applied]
// hears: true once the API's socket surface is built over this hub, whether or
// not a screen is open right now, and false on a node that serves no API and
// attached its hub for the working indicator alone. A FACT ABOUT THE NODE,
// never a count of open screens — the engine's live relay follows a peer only
// for a node that answers true.
func (h *ChatHub) ServesSockets() bool {
	return h != nil && h.serving.Load()
}

// dropTally counts what one full buffer dropped and decides when to say so.
// Guarded by the hub's mutex.
type dropTally struct {
	total  int       // since the hub was built
	unsaid int       // since the last line
	said   time.Time // when the last line was written; zero before the first
}

// drop counts one, answering how many a line should say now: everything since
// the last line, or zero while the last is inside [chatDropReportEvery].
func (d *dropTally) drop(now time.Time) int {
	d.total++
	d.unsaid++
	if !d.said.IsZero() && now.Sub(d.said) < chatDropReportEvery {
		return 0
	}
	n := d.unsaid
	d.unsaid, d.said = 0, now
	return n
}

// rest answers what no line has said yet, for the hub's last word.
func (d *dropTally) rest() int {
	n := d.unsaid
	d.unsaid = 0
	return n
}

// reportBatchesDropped says n committed batches went undelivered, if any did.
//
// A WARN, because each is a frame every watching screen missed, and the
// cause — a fan-out slower than the applier — is the node's to fix.
func reportBatchesDropped(n, total int) {
	if n == 0 {
		return
	}
	log.Warn("chat_live_batches_dropped", "batches", n, "since_start", total,
		"hint", "the live fan-out fell behind the applier; the rows are "+
			"unaffected and every screen is told to read its rooms again")
}

// ---- the committed half ------------------------------------------------ //

// Applied is [chatlet.Observer]: the engine hands over what a transaction
// committed.
//
// IT NEVER BLOCKS AND IT NEVER READS. Both are the applier's contract: this
// runs on its goroutine between two transactions, so a visibility read here
// would put a store query inside the log's own loop. The batch is handed to
// this hub's buffer, and a full buffer is a DROP with a count — and a re-read
// owed to every screen, since a dropped batch is a hole no later frame
// reveals: a client sees a gap in a room's sequence only when the next message
// arrives, and the last ones said before a quiet spell would be missing until
// somebody reloaded.
func (h *ChatHub) Applied(batch []chatlet.Applied) {
	if h == nil || len(batch) == 0 {
		return
	}
	select {
	case h.applied <- batch:
	default:
		h.mu.Lock()
		say, total := h.dropped.drop(h.now()), h.dropped.total
		h.mu.Unlock()
		reportBatchesDropped(say, total)
		h.Reread()
	}
}

// Reread tells every chat screen on this node that what it was shown has a
// GAP: one `chat_room_changed` naming no room, to every watcher, which reads
// its rooms again — the engine switched the source its batches come from (its
// own applier, a peer's relayed), or this hub dropped a batch. Nothing between
// is replayed, and a frame per room would need to know every room each screen
// holds — and every socket in a room's presence is decided again, since the
// gap may have held the record that took its viewer out of the room
// ([ChatHub.presenceAfterGap]).
//
// NEVER BLOCKS AND IS NEVER LOST: it sets a flag the fan loop reads after
// every batch and on its own wake, so a burst of gaps is one re-read and a
// full queue cannot drop it. A screen is told after the batches already queued
// ahead of it, which is the order that covers them.
func (h *ChatHub) Reread() {
	if h == nil {
		return
	}
	h.rereadOwed.Store(true)
	select {
	case h.rereadWake <- struct{}{}:
	default:
	}
}

// fanLoop delivers committed batches, and the re-read a gap owes.
func (h *ChatHub) fanLoop(ctx context.Context, stop <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case batch := <-h.applied:
			h.fan(ctx, batch)
		case <-h.rereadWake:
		}
		if h.rereadOwed.Swap(false) {
			h.reread(ctx)
		}
	}
}

// reread sends every watcher the frame that names no room, and decides again
// who may stay in the rooms' presence.
func (h *ChatHub) reread(ctx context.Context) {
	watchers := h.current()
	if len(watchers) == 0 {
		return
	}
	targets := make([]*Client, 0, len(watchers))
	for _, w := range watchers {
		targets = append(targets, w.client)
	}
	fanOut(targets, Push(KindChatRoomChanged, ChatChange{}, h.now()))
	h.presenceAfterGap(ctx, watchers)
}

// presenceAfterGap decides again whether every socket in a room's presence may
// still see the room, and takes out the ones it definitely may not.
//
// A GAP MAY HAVE HELD THE RECORD THAT NARROWED A ROOM — a batch this hub
// dropped, or one applied while the engine's source switched — and that
// record is what takes a removed person out of the room's presence
// ([ChatHub.redecidePresence]). Missed, its subject would go on seeing who is
// typing in a room they were taken out of, and be shown there, until their
// tab moved. So a gap re-decides every socket in a room, as the record would
// have.
//
// WHAT IT CANNOT DECIDE IT KEEPS, the way an artifact's room keeps an
// undecidable member on a reset, rather than failing closed as a narrowing
// record's re-decision does: that record is evidence the room changed, while
// a gap is only the possibility, and a store blip during one would otherwise
// empty every room's presence on the node — with nothing to bring a person
// who is only looking back until they typed or reconnected.
func (h *ChatHub) presenceAfterGap(ctx context.Context, watchers []chatWatcher) {
	rooms := h.rooms()
	if rooms == nil {
		return
	}
	for _, w := range watchers {
		if w.session == "" {
			continue
		}
		visible, err := h.sees(ctx, rooms.Channel, presenceRead, w.viewer, w.room.String())
		if err != nil {
			log.DebugContext(ctx, "chat_presence_kept_undecided", "channel", w.room,
				"error", err)
			continue
		}
		if !visible {
			h.withdrawPresence(w.client, w.room)
		}
	}
}

// fan delivers one committed batch to every socket entitled to see it.
//
// THE VISIBILITY DECISION IS CACHED PER BATCH AND NEVER BEYOND IT. A batch is
// one transaction, so every read here sees one settled state and a room's
// membership cannot move inside it — while a cache that outlived the batch
// would serve a private room to somebody who had just been removed from it,
// which is the one mistake this filter exists to prevent.
//
// ONE ENCODE PER RECORD, for every viewer it admits: the frame is the same
// for all of them, so the admitted sockets leave together by [fanOut].
//
// THE READER'S NEED AND ITS POSTURE ARE ASKED FIRST ([Client.admits]), before
// the room's visibility is read: a socket the frame would never reach is not
// worth a read to decide whether it may see it — and on a node whose posture
// is degraded that is every socket on it, each decision a read through the
// router for a frame [fanOut] then withholds. [fanOut] asks them again, so a
// posture that moves in between is still the one the frame is sent under.
//
// # Read at the batch, here first
//
// Each decision is held to the BATCH'S OWN POSITION ([statelog.Freshness]'s
// floor, honoured at `stale`), so whichever copy answers holds the records
// being announced: a copy that had not applied a membership change would
// decide it against the rows from before it. And it is asked of this node's
// copy first even while that copy lags its log ([ChatRooms.ChannelHere]): this
// node's applier is the one announcing the batch, so its copy holds it by
// construction — while the estate's ordinary walk asks a peer whose copy does
// not lag first, and a burst is what makes a copy lag, so every decision
// during one was a network round trip on this one goroutine, past which the
// batches queued behind it were dropped. A copy out of service is passed over
// as ever, which is what serves a batch the engine relayed from a peer.
func (h *ChatHub) fan(ctx context.Context, batch []chatlet.Applied) {
	watchers := h.current()
	if len(watchers) == 0 {
		return
	}
	rooms := h.rooms()
	if rooms == nil {
		h.unwired.Do(func() {
			log.Warn("chat_live_has_no_room_reader",
				"hint", "chat records are applying and no live frame can be "+
					"filtered, so no screen updates until it reads its rooms "+
					"again; the node's Chatlet half is not up")
		})
		return
	}
	at := statelog.Freshness{Level: statelog.ReadStale, MinPosition: batchPosition(batch)}
	decided := map[seenKey]bool{}
	targets := make([]*Client, 0, len(watchers))
	for _, applied := range batch {
		kind, push := chatPushKind(applied.Op)
		if !push {
			continue
		}
		if kind == KindChatRoomChanged || kind == KindChatMembershipChanged {
			h.redecidePresence(ctx, rooms, at, decided, watchers, applied)
		}
		targets = targets[:0]
		for _, w := range watchers {
			if _, sent := w.client.admits(kind); !sent {
				continue
			}
			// THE PEOPLE THIS RECORD TOOK OUT OF THE ROOM are told it
			// changed, though at the batch's position they may no
			// longer read it — that position is exactly where they
			// stopped being able to, so the visibility rule alone left
			// a private room in their rail until something else made
			// it read again ([chatlet.Applied.Removed]). The frame names
			// the room and the record and no member, which is no more
			// than they knew.
			if slices.Contains(applied.Removed, w.viewer) ||
				h.maySee(ctx, rooms.ChannelHere, at, decided, w.viewer, applied.ChannelID) {
				targets = append(targets, w.client)
			}
		}
		if len(targets) > 0 {
			visible(h.opts.Objectives,
				fanOut(targets, Push(kind, chatChangeOf(applied), applied.At)), applied.At)
		}
	}
}

// redecidePresence takes out of a room's presence every socket in it whose
// viewer the record took out of the room or that may no longer read it at the
// batch's position: a membership replaced, a room made private, erased. Who
// may be in a room's presence is who may read the room, decided again by the
// one record that can narrow it, as an artifact's room is by a share
// withdrawn.
func (h *ChatHub) redecidePresence(ctx context.Context, rooms ChatRooms,
	at statelog.Freshness, decided map[seenKey]bool, watchers []chatWatcher,
	applied chatlet.Applied) {

	room, err := uuid.Parse(applied.ChannelID)
	if err != nil {
		return
	}
	for _, w := range watchers {
		if w.room != room {
			continue
		}
		if !slices.Contains(applied.Removed, w.viewer) &&
			h.maySee(ctx, rooms.ChannelHere, at, decided, w.viewer, applied.ChannelID) {
			continue
		}
		h.withdrawPresence(w.client, room)
	}
}

// batchPosition is the furthest position a committed batch reached: the floor
// every decision about it is held to. One applier wrote it, so every record in
// it is on the one log and the comparison never crosses streams.
func batchPosition(batch []chatlet.Applied) statelog.Position {
	var furthest statelog.Position
	for _, applied := range batch {
		if behind, err := furthest.Before(applied.Position); furthest.IsZero() ||
			(err == nil && behind) {
			furthest = applied.Position
		}
	}
	return furthest
}

// presenceRead is the freshness a presence join reads at: STALE, with no
// floor, because it is about no record — who may be told somebody is looking
// at a room — and a linearizable read would put a quorum barrier append on the
// broker for every tab that opens a room. A record that narrows the room is
// decided again at its own position ([ChatHub.redecidePresence]).
var presenceRead = statelog.Freshness{Level: statelog.ReadStale}

// maySee answers whether one viewer may be shown one room, through the
// domain's own rule.
//
// TWO NARROWINGS OF ONE DECISION, and the second is not redundant. The reader
// refuses a room this viewer may not read as NOT FOUND, which is the answer
// for the rooms it declines to serve; [chatlet.Visible] is then applied to the
// room it DOES serve, because this seam is an interface and a filter that
// trusted its implementation to have already decided would be a filter with
// no rule of its own.
//
// IT FAILS CLOSED. A read that errors, times out or cannot be served drops the
// frame: the alternative is showing a transcript to somebody on the strength
// of a store that would not answer. The screen repairs itself on its next
// read, which is what every dropped frame here relies on.
//
// THE READ AND ITS FRESHNESS ARE THE PASS'S: a committed frame's decision
// reads this node's copy first at the batch's own position ([ChatHub.fan]), a
// presence join whichever copy the walk prefers ([presenceRead]).
func (h *ChatHub) maySee(ctx context.Context, read roomRead, fresh statelog.Freshness,
	decided map[seenKey]bool, viewer, channelID string) bool {

	if viewer == "" || channelID == "" {
		return false
	}
	key := seenKey{viewer: viewer, channel: channelID}
	if answer, known := decided[key]; known {
		return answer
	}
	answer, err := h.sees(ctx, read, fresh, viewer, channelID)
	if err != nil {
		// DEBUG, for the reason [ChatHub.sees] gives for its own line.
		log.DebugContext(ctx, "chat_live_frame_withheld", "channel", channelID,
			"error", err)
	}
	decided[key] = answer
	return answer
}

// sees is one visibility decision, THREE-VALUED: (true, nil) the viewer may
// see the room, (false, nil) definitely not — the room's own rule refuses
// them, or the reader answers that it serves them no such room — and (false,
// err) undecided, a read that failed or ran out of [ChatVisibilityBudget].
// [ChatHub.maySee] fails closed on the third; a re-decision after a gap keeps
// what it cannot decide ([ChatHub.presenceAfterGap]).
func (h *ChatHub) sees(ctx context.Context, read roomRead, fresh statelog.Freshness,
	viewer, channelID string) (bool, error) {

	readCtx, cancel := context.WithTimeout(ctx, ChatVisibilityBudget)
	defer cancel()
	detail, err := read(readCtx, viewer, channelID, fresh)
	switch {
	case errors.Is(err, chatlet.ErrNotFound):
		// A room the viewer may not read answers `not found`, which is
		// the ordinary case on any message in a private room: a
		// decision, and no line at all.
		return false, nil
	case err != nil:
		return false, err
	}
	return chatlet.Visible(viewer, detail.Channel, detail.Member), nil
}

// chatPushKind maps a record's op onto the frame it becomes, reporting false
// for the ops that are not about a conversation at all.
//
// TOTAL, with the machinery named rather than left to a default. An eviction,
// a generation and a barrier are the framework's own records: they change no
// room, and a default arm that pushed them would put a frame a client has no
// case for onto every open socket. Naming them is also what makes a NEW op
// fail visibly here — it is not pushed, and the switch is the one place that
// says so — rather than silently arriving as a kind nobody handles; a gate
// holds the switch to every op the domain declares.
func chatPushKind(op chatlet.OpKind) (Kind, bool) {
	switch op {
	case chatlet.OpPost:
		return KindChatPosted, true
	case chatlet.OpEdit:
		return KindChatEdited, true
	case chatlet.OpDelete:
		return KindChatDeleted, true
	case chatlet.OpReact:
		return KindChatReacted, true
	case chatlet.OpCreate, chatlet.OpPatch, chatlet.OpRetention, chatlet.OpErase, chatlet.OpPrune:
		return KindChatRoomChanged, true
	case chatlet.OpMembers:
		return KindChatMembershipChanged, true
	case chatlet.OpEviction, chatlet.OpGeneration, chatlet.OpBarrier:
		return "", false
	}
	return "", false
}

// chatChangeOf renders one committed record as a frame.
func chatChangeOf(applied chatlet.Applied) ChatChange {
	change := ChatChange{
		ChannelID: applied.ChannelID, Op: applied.Op, OpID: applied.OpID,
		MessageID: applied.MessageID, ChannelSeq: applied.ChannelSeq,
		Author: applied.Actor, AuthorKind: applied.ActorKind,
		At: applied.At, Position: applied.Position,
	}
	if n := applied.Notify; n != nil {
		change.ThreadRoot = n.ThreadRoot
		change.Excerpt = n.Excerpt
		change.Mentions = n.Mentions
		change.ChannelName = n.ChannelName
		change.ChannelKind = n.ChannelKind
	}
	return change
}

// ---- a person's own read state ----------------------------------------- //

// readsLoop pushes each person's read state to their own sockets on this node,
// from the coordination store's watch, for as long as the hub runs — once this
// node serves sockets ([ChatHub.AttachSockets]): a node that serves none has
// nobody to push to, and a watch there would hear every flush in the company
// for nothing.
//
// A WATCH THAT ENDS IS BEGUN AGAIN, spaced by [backoff.Doubling] from
// [chatReadRewatchBase] up to [chatReadRewatchCeiling]: the store's watch
// re-delivers every record that exists before its marker, so a flush made
// while this node was not hearing is in that answer, and the revision this hub
// last pushed per seat is what stops it being pushed twice.
func (h *ChatHub) readsLoop(ctx context.Context, stop <-chan struct{}) {
	if h.opts.Reads == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-stop:
		return
	case <-h.sockets:
	}
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-watchCtx.Done():
		}
	}()
	for attempt := 0; ; attempt++ {
		if h.watchReads(watchCtx) {
			attempt = 0
		}
		if watchCtx.Err() != nil {
			return
		}
		select {
		case <-watchCtx.Done():
			return
		case <-time.After(backoff.Doubling(attempt, chatReadRewatchBase, chatReadRewatchCeiling)):
		}
	}
}

// watchReads runs one watch to its end, reporting whether it ever reached the
// marker — which is what resets the re-watch's backoff.
func (h *ChatHub) watchReads(ctx context.Context) bool {
	updates, err := h.opts.Reads.WatchChatReads(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.WarnContext(ctx, "chat_read_watch_failed", "error", err,
				"detail", "a person's other tabs do not hear their read state move "+
					"until this node watches again; each repairs on its next rail read")
		}
		return false
	}
	current := false
	// ENDED BY ctx AS WELL AS BY THE CHANNEL, so the hub's stop never waits
	// on the store to keep its half of the contract: a goroutine this hub
	// started is one this hub can end.
	for done := false; !done; {
		select {
		case <-ctx.Done():
			return current
		case u, open := <-updates:
			switch {
			case !open:
				done = true
			case u.Current:
				current = true
			default:
				h.pushRead(u)
			}
		}
	}
	if ctx.Err() == nil {
		log.WarnContext(ctx, "chat_read_watch_ended",
			"detail", "the watch stopped hearing the store; this node watches again")
	}
	return current
}

// pushRead pushes one record's state to its seat's sockets on this node.
//
// A FORGOTTEN RECORD PUSHES NOTHING: the maintenance sweep forgets a seat the
// company has lacked for a day, and by then every socket acting as it was
// closed as a seat that left (lifetime.go).
//
// THE RECONCILE'S ABSENCE STAMP IS NOT PUSHED, for the rail's reason: it is
// the sweep's machinery, and handed to a tab it is a date nobody chose sitting
// in their state. A stamp still moves the record's revision, so it is pushed
// as the state it leaves, which a tab keeps by that revision.
func (h *ChatHub) pushRead(u coord.ChatReadUpdate) {
	if u.Handle == "" {
		return
	}
	h.mu.Lock()
	if u.State == nil {
		delete(h.pushed, u.Handle)
		h.mu.Unlock()
		return
	}
	if last, seen := h.pushed[u.Handle]; seen && u.State.Revision <= last {
		h.mu.Unlock()
		return
	}
	h.pushed[u.Handle] = u.State.Revision
	var targets []*Client
	for _, w := range h.watchers {
		if w.viewer == u.Handle {
			targets = append(targets, w.client)
		}
	}
	h.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	state := *u.State
	state.AbsentSince = time.Time{}
	fanOut(targets, Push(KindChatCursorMoved,
		coord.PersonChatRead{Handle: u.Handle, ChatReadState: state}, h.now()))
}

// ---- the socket registry ----------------------------------------------- //

// chatViewerOf is the seat a principal is shown chat as, or "" for one that
// may not be shown any: a person — or a machine the identity directory binds
// to them, which the guard composes as that person — acting as their human
// seat, and holding Chatlet.
//
// [iam.ActorFor] IS THE SAME FUNCTION A CHAT WRITE'S AUTHOR COMES FROM, so
// whoever may post in a room is exactly whoever's socket may be shown it. And
// it is never [iam.RecordOwner]: for a credential acting as itself that is
// its LOGIN, and [chatlet.Visible] reads any non-empty viewer as somebody who may
// read every public room.
func chatViewerOf(p iam.Principal) string {
	a := iam.ActorFor(p)
	if a.Kind != iam.ActorHuman || !p.Can(iam.GrantChatlet) {
		return ""
	}
	return a.Name
}

// join registers one socket under the seat it watches as, reporting whether it
// watches chat at all.
//
// A SOCKET THAT MAY NOT BE SHOWN CHAT IS NOT REGISTERED. Not registered and
// then filtered: such a principal has no membership anywhere, so every
// decision about it would be "no" — and a registry that held it would be one
// accidental change away from serving it the public rooms, which is a viewer
// the company never declared.
func (h *ChatHub) join(client *Client, viewer string) bool {
	viewer = strings.TrimSpace(viewer)
	if h == nil || client == nil || viewer == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.watchers[client] = &chatWatcher{client: client, viewer: viewer}
	return true
}

// leave withdraws a socket, and with it the room's presence it was in.
//
// IMMEDIATELY, rather than waiting out [presence.TypingLapse]: a socket that
// closed is a person who is demonstrably not in the room, and the lapse exists
// for the tab that stays open and stops.
func (h *ChatHub) leave(client *Client) {
	if h == nil || client == nil {
		return
	}
	h.mu.Lock()
	w, watching := h.watchers[client]
	delete(h.watchers, client)
	h.mu.Unlock()
	if watching && w.session != "" && h.opts.Presence != nil {
		h.opts.Presence.Leave(presence.ChatRoom(w.room), w.session)
	}
}

// current is a copy of the watcher set, so no pass holds the lock across a
// store read or a send.
func (h *ChatHub) current() []chatWatcher {
	h.mu.Lock()
	defer h.mu.Unlock()
	watchers := make([]chatWatcher, 0, len(h.watchers))
	for _, w := range h.watchers {
		watchers = append(watchers, *w)
	}
	return watchers
}

func (h *ChatHub) rooms() ChatRooms {
	if h.opts.Rooms == nil {
		return nil
	}
	return h.opts.Rooms()
}

// ---- presence ---------------------------------------------------------- //

// presenceFrame is one socket's `presence` frame naming a chat room: where its
// tab is looking and whether its person is typing, under which thread. Called
// on the socket's room goroutine, in the order its frames arrived, so a leave
// never overtakes the join it undoes.
//
// A FRAME NAMING ANOTHER ROOM MOVES THE SOCKET, because a tab looks at one room
// at a time: out of the room it was in, and into the one it names once the
// room's own visibility admits its viewer ([ChatHub.maySee], the decision a
// committed frame takes) — and a refusal leaves it in none, with nothing said,
// which is the answer a room that does not exist gets too. A frame naming no
// room takes it out of the one it was in.
//
// THE FRAME IS TRUSTED ABOUT ATTENTION AND NEVER ABOUT PERMISSION: a client may
// name any room it likes, and naming a private room its viewer is not in puts
// it in nobody's presence and shows it nobody's.
func (h *ChatHub) presenceFrame(ctx context.Context, client *Client, channelID string,
	activity presence.Activity, focus presence.Focus) {

	if h == nil || client == nil || h.opts.Presence == nil {
		return
	}
	channelID = strings.TrimSpace(channelID)
	var room uuid.UUID
	if channelID != "" {
		parsed, err := uuid.Parse(channelID)
		if err != nil {
			// NOT A ROOM ANYBODY CAN BE IN: every room's id is a UUID.
			log.DebugContext(ctx, "chat_presence_frame_refused", "channel", channelID,
				"error", err.Error())
			return
		}
		room = parsed
	}
	h.mu.Lock()
	w, watching := h.watchers[client]
	var held chatWatcher
	if watching {
		held = *w
	}
	h.mu.Unlock()
	if !watching {
		return
	}
	if held.session != "" && held.room == room {
		if err := h.opts.Presence.Update(presence.ChatRoom(room), held.session, activity,
			focus); err != nil {
			log.DebugContext(ctx, "chat_presence_frame_refused", "channel", channelID,
				"error", err.Error())
		}
		return
	}
	if held.session != "" {
		h.withdrawPresence(client, held.room)
	}
	if room == uuid.Nil {
		return
	}
	rooms := h.rooms()
	if rooms == nil || !h.maySee(ctx, rooms.Channel, presenceRead, map[seenKey]bool{},
		held.viewer, channelID) {
		return
	}
	// THE JOIN, THE SOCKET'S PLACE IN THE ROOM AND THE ROSTER IT IS HANDED
	// ARE ONE STEP UNDER THE HUB'S LOCK, which every fan-out of the room's
	// presence holds while it chooses its sockets and sends
	// ([ChatHub.presenceChanged]): a fan-out the presence hub computed before
	// the join reaches the socket before its roster or not at all, and one
	// computed after it reaches it after. Taken apart, the room's own
	// fan-out of this join could reach the tab ahead of the roster it is part
	// of, and one computed in between — another member moving — reached
	// nobody, so the tab showed that member as they were until they moved
	// again. The presence hub never holds its own lock across a sink, so the
	// two locks are always taken in this order and never the other.
	h.mu.Lock()
	w, watching = h.watchers[client]
	if !watching || w.session != "" {
		// THE SOCKET LEFT while the room's visibility was being decided.
		h.mu.Unlock()
		return
	}
	session, roster, err := h.opts.Presence.Join(presence.ChatRoom(room),
		presence.Local{Seat: held.viewer, Kind: iam.ActorHuman})
	if err != nil {
		h.mu.Unlock()
		log.WarnContext(ctx, "chat_presence_join_refused", "channel", channelID,
			"error", err.Error())
		return
	}
	w.room, w.session = room, session
	fanOut([]*Client{client}, Push(KindChatPresence, ChatPresence{
		ChannelID: channelID, Roster: true, Upsert: roster.Members, Leave: []string{},
		Counts: roster.Counts, Truncated: roster.Truncated}, h.now()))
	h.mu.Unlock()
	if activity != presence.Viewing || focus != (presence.Focus{}) {
		if err := h.opts.Presence.Update(presence.ChatRoom(room), session, activity,
			focus); err != nil {
			log.DebugContext(ctx, "chat_presence_frame_refused", "channel", channelID,
				"error", err.Error())
		}
	}
}

// withdrawPresence takes a socket out of the room's presence it is in, if it is
// still in that one, and empties the room it was shown: a roster naming nobody.
//
// THE EMPTY ROSTER IS SENT UNDER THE LOCK THAT TAKES THE SOCKET OUT, as a
// join's roster is sent under the lock that puts it in: a withdrawal decided
// on the fan-out's goroutine can meet the socket's own next frame joining the
// room again, and sent after the lock the roster naming nobody could overtake
// the join's and leave a tab in the room showing it empty. A fan-out of the
// room chooses its sockets and sends under the same lock
// ([ChatHub.presenceChanged]), so it reaches this socket before this roster or
// not at all.
func (h *ChatHub) withdrawPresence(client *Client, room uuid.UUID) {
	h.mu.Lock()
	w, watching := h.watchers[client]
	if !watching || w.session == "" || w.room != room {
		h.mu.Unlock()
		return
	}
	session := w.session
	w.room, w.session = uuid.Nil, ""
	fanOut([]*Client{client}, Push(KindChatPresence, ChatPresence{
		ChannelID: room.String(), Roster: true, Upsert: []presence.Member{},
		Leave: []string{}}, h.now()))
	h.mu.Unlock()
	h.opts.Presence.Leave(presence.ChatRoom(room), session)
}

// presenceChanged pushes one chat room's presence fan-out to the sockets in
// it: the presence hub's sink, called on the hub's goroutine. A room of any
// other kind is another surface's.
//
// TO THE SOCKETS IN THE ROOM AND NOBODY ELSE, which is the visibility filter
// for presence: a socket is in a room's presence only once the room's own
// visibility admitted its viewer, and is taken out by the record that narrows
// it ([ChatHub.redecidePresence]).
//
// CHOSEN AND SENT UNDER THE HUB'S LOCK, which a join holds while it hands its
// socket the room's roster and a withdrawal while it hands its socket a roster
// naming nobody: so this fan-out reaches a socket wholly before either or
// wholly after, and a socket taken out of the room in between is never sent
// it. Sending is a non-blocking hand-off to each socket's queue
// ([Client.send]), so the lock is never held across a slow reader.
func (h *ChatHub) presenceChanged(c presence.Change) {
	if c.Room.Kind != presence.RoomChat {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var targets []*Client
	for _, w := range h.watchers {
		if w.session != "" && w.room == c.Room.ID {
			targets = append(targets, w.client)
		}
	}
	if len(targets) == 0 {
		return
	}
	fanOut(targets, Push(KindChatPresence, ChatPresence{
		ChannelID: c.Room.ID.String(), Upsert: c.Upsert, Leave: c.Leave, Counts: c.Counts,
		Truncated: c.Truncated}, h.now()))
}

// ---- the working indicator --------------------------------------------- //

// THE WORKING INDICATOR RIDES PRESENCE, NOT THE LOG.
//
// "Agent SWE is thinking…" is in-flight state about a turn that is running
// right now: it is true for minutes, it is worthless the moment the turn ends,
// and nothing replays it. On the log it would be two records per turn on the
// busiest stream in the engine, applied by every node and carried in every
// backup, to say something no reader will ever ask about again. So it is the
// seat's member of the room's presence (internal/presence): an agent, editing,
// focused on the thread its answer will land in, naming the turn and showing
// the phrase as its status — raised on the node running the turn, and told to
// whichever node holds the room's sockets as every member is.
//
// It is driven by [notify.StatusDriver] — the same driver Slack's and
// Mattermost's indicators use — so the lifecycle, the reference counting per
// turn, the heartbeat and the mode decision are written once. What this
// implements is the BACKEND half of that seam, and the two values that decide
// when the indicator appears come from the domain rather than from here: the
// company's `chat.chatlet.typing_status` is the driver's mode, and
// [chatlet.AddressRule] is what `addressed` means.

// StatusBackend implements [notify.StatusPoster]: the transport name the chat
// parser stamps on every trigger it produces.
//
// [chatlet.Source] rather than a literal, because the driver matches this against
// the trigger's own `transport` key — a second spelling would raise no
// indicator at all, silently, on every Chatlet turn.
func (h *ChatHub) StatusBackend() string { return chatlet.Source }

// SupportsStatusText implements [notify.StatusPoster].
//
// TRUE, unlike a composer typing indicator whose wording its client fixes: the
// frame carries the phrase and the dashboard renders it, so a person waiting
// learns which phase is running rather than merely that something is.
func (h *ChatHub) SupportsStatusText() bool { return true }

// StatusRefresh implements [notify.StatusPoster].
//
// [presence.Rebroadcast], FIFTEEN SECONDS, sized against the presence hub's
// own expiry: an agent's member leaves a room [presence.Expiry] after its last
// touch, three rebroadcasts, and this heartbeat is its touch — so a turn
// working longer than that keeps its line, and a node that died mid-turn takes
// it down within the expiry, as it takes its viewers. It was thirty seconds
// while the line lived in this hub's own memory and expired never; against a
// forty-five-second expiry that left one late heartbeat between a working seat
// and a room showing nobody.
func (h *ChatHub) StatusRefresh() time.Duration { return ChatStatusRefresh }

// AddressRule implements [notify.StatusPoster]: the domain's own answer to
// "was this message addressed to this seat?".
//
// [chatlet.AddressRule] and not a rule of this package's, because the same value
// decides three things that must never disagree — the prompt that tells an
// agent it was asked, the delivery gate that refuses to let an addressed turn
// end in silence, and this indicator. A spinner over a turn that is allowed to
// end in silence, and a silence after a spinner, are one bug.
func (h *ChatHub) AddressRule() notify.AddressRule { return chatlet.AddressRule() }

// SetStatus implements [notify.StatusPoster]: raise or update the indicator —
// the seat's member of the room's presence, touched.
//
// It reports false only for what no room can show: a hub with no presence, a
// post naming no seat, or a conversation that is not a room's id. A failed
// indicator is cosmetic and is not a turn's problem, which is why the seam
// reports a bool rather than an error.
func (h *ChatHub) SetStatus(_ context.Context, post notify.StatusPost) bool {
	room, ok := h.statusRoom(post.Handle, post.Channel)
	if !ok {
		return false
	}
	h.opts.Presence.Touch(room, presence.Agent{Seat: post.Handle, Turn: post.Turn,
		Activity: presence.Editing, Focus: presence.Focus{Thread: post.Thread},
		Status: post.Text})
	return true
}

// ClearStatus implements [notify.StatusPoster]: take the indicator down — the
// seat's member for the thread WITHDRAWN at once, never left to linger out its
// expiry, because a line still saying "is thinking…" after the answer landed
// says the seat is working on something it has finished.
//
// SEPARATE FROM SetStatus WITH AN EMPTY STRING, on the seam's own rule: the
// two are the same operation only on a backend that renders text, and
// overloading them makes "raise" and "clear" indistinguishable for one that
// does not.
func (h *ChatHub) ClearStatus(_ context.Context, handle, channel, thread string) bool {
	room, ok := h.statusRoom(handle, channel)
	if !ok {
		return false
	}
	h.opts.Presence.Withdraw(room, handle, presence.Focus{Thread: thread})
	return true
}

// statusRoom is the room a seat's indicator sits in, or false for one no room
// can show it in.
func (h *ChatHub) statusRoom(handle, channel string) (presence.Room, bool) {
	if h == nil || h.opts.Presence == nil || handle == "" {
		return presence.Room{}, false
	}
	id, err := uuid.Parse(channel)
	if err != nil {
		return presence.Room{}, false
	}
	return presence.ChatRoom(id), true
}
