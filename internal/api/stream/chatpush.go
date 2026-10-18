package stream

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
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
// It rides the queue's EPHEMERAL scatter on [topics.ChatPresence], which is
// deliberately outside the chat log's own wildcard, and it is a READ: see
// [ChatHub.answer] for why it is not signed.
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
	// message in the room this socket said it is watching.
	//
	// NOT PAST TENSE, because it is not a thing that happened: it is the
	// current answer, whole, for the room this socket asked about. A delta
	// would need a starting point a reconnecting tab does not have.
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

// ChatFocusKind is the ONE client-to-server frame this arm adds: which room a
// tab is looking at, and whether the person is typing in it.
//
// A FRAME RATHER THAN A QUERY, because it asks nothing and answers nothing —
// it is the client telling the server where its attention is, which is what
// makes a fleet-wide presence probe bounded to the rooms anybody is actually
// watching. A tab that sends none is in no room's presence probe, so it sees
// no presence and nobody sees it: bounding the probe to the rooms a tab has
// open is what makes that true. A reconnecting tab sends it again, as it sends
// its `watch`, because the new socket is a new watcher.
const ChatFocusKind = "chat_focus"

// The presence cadence, and every number here is tied to what it buys.
const (
	// ChatPresenceInterval is the IDLE cadence: how often a node
	// re-probes its peers when nothing has changed locally.
	//
	// THREE SECONDS, and it is sized against [ChatTypingTTL] rather than
	// against latency, because latency is the wake path's job (a focus
	// change probes immediately). What this interval alone has to cover
	// is the tab that stopped typing without saying so — a lid closed
	// mid-word — and the indicator is then wrong for at most one interval
	// past the TTL.
	ChatPresenceInterval = 3 * time.Second

	// ChatPresenceGap is the floor between two probes.
	//
	// HALF A SECOND, because a focus change probes at once and typing is
	// a focus change per keystroke: without a floor a fast typist is one
	// fleet-wide scatter per character. It is under the threshold at
	// which a person perceives the indicator as lagging, so coalescing
	// here costs nothing anybody can see — which holds only because a
	// change the floor skips is probed at the gap's END rather than left
	// to the idle tick ([ChatHub.presenceLoop]).
	ChatPresenceGap = 500 * time.Millisecond

	// ChatPresenceAsk is how long one probe waits for its peers.
	//
	// The scatter asks for no particular number of replies — a node
	// cannot know how many peers are serving — so it waits out this
	// deadline every time, which makes it the cost of a probe rather than
	// a timeout. THREE QUARTERS OF A SECOND: enough for a reply across a
	// region (a same-region round trip is single-digit milliseconds and a
	// cross-region one tens), short enough that it stays well inside
	// [ChatPresenceGap] plus a tick, so probes never pile up on each
	// other. A peer that does not answer inside it is treated as having
	// nobody in the room, and the next probe corrects it.
	ChatPresenceAsk = 750 * time.Millisecond

	// ChatTypingTTL is how long a typing flag survives without being
	// re-asserted.
	//
	// SIX SECONDS, which is two idle intervals: a client re-asserts while
	// somebody types, so one lost frame must not blink the indicator off,
	// and two must. Closing the socket withdraws it immediately — this
	// covers the tab that stops typing and says nothing, not the tab that
	// goes away.
	ChatTypingTTL = 6 * time.Second

	// ChatStatusRefresh is what the working-indicator driver re-asserts
	// at. See [ChatHub.StatusRefresh] for why it is long.
	ChatStatusRefresh = 30 * time.Second

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

// chatPresenceProtocol is the presence scatter's payload version.
//
// ONE, and it moves only for a reshape — a new FIELD needs no bump, because a
// peer ignores what it does not know. A reply this build cannot read is a
// MISSING reply, exactly as the search fan-out treats one: presence is a
// courtesy, and a rolling upgrade must degrade it rather than break it.
const chatPresenceProtocol = 1

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

// ChatPeers is the ephemeral scatter presence rides.
//
// The two verbs that leave no trace, declared by the consumer so this arm
// depends on the shape rather than on the broker: nothing here may be
// published, retained, redelivered or recorded. See [queue.EventQueue.Ask].
type ChatPeers interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
	Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// ChatReadWatch is the coordination store's read-state watch, as this arm
// consumes it: every record that exists, the marker, then every change.
// See [coord.ChatReads.WatchChatReads].
type ChatReadWatch interface {
	WatchChatReads(ctx context.Context) (<-chan coord.ChatReadUpdate, error)
}

// ChatWorking is one seat working on a message in a room.
type ChatWorking struct {
	Handle string `json:"handle"`

	// Thread is the message the work will be answered under, which is
	// what puts the indicator where the reply will land rather than
	// somewhere else in the room.
	Thread string `json:"thread,omitempty"`

	// Status is the phrase to render, and empty renders the bare
	// indicator. Empty is what [notify.StatusDriver] hands a poster that
	// renders no words ([notify.StatusPoster.SupportsStatusText] false);
	// this hub declares it renders them ([ChatHub.SupportsStatusText]), so
	// every frame it sends carries one.
	Status string `json:"status,omitempty"`
}

// ChatRoomPresence is who is in one room right now.
type ChatRoomPresence struct {
	// Viewing are the people with this room open. Handles, because a
	// person in chat IS a seat.
	Viewing []string `json:"viewing,omitempty"`

	// Typing are the people composing in it, which is a subset of
	// Viewing that expires on its own — see [ChatTypingTTL].
	Typing []string `json:"typing,omitempty"`

	// Working are the seats running a turn against a message here.
	Working []ChatWorking `json:"working,omitempty"`
}

// ChatPresenceFrame is the presence push's payload: the room this socket
// asked about, whole.
//
// WHOLE RATHER THAN A DELTA, because a tab that reconnects has no starting
// point to apply one against, and presence is small: a handful of handles per
// room somebody is actually looking at.
type ChatPresenceFrame struct {
	Rooms map[string]ChatRoomPresence `json:"rooms"`
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

	// Peers carries presence to the rest of the fleet. Nil keeps it on
	// this node's own sockets, which is honest and is the whole truth on a
	// single-node deployment.
	Peers ChatPeers

	// Reads is the coordination store's read-state watch, which every
	// node serving sockets follows to push a person's read state to their
	// own tabs. Nil pushes none: a tab then learns its read state from the
	// rail alone.
	Reads ChatReadWatch

	// NodeID names this node in its own presence answer, so an asker can
	// drop the reply to its own scatter rather than merging itself twice.
	NodeID string

	// Now is injectable so a test can pin the typing window.
	Now func() time.Time

	// PresenceInterval and AskBudget override [ChatPresenceInterval] and
	// [ChatPresenceAsk] — the presence probe's cadence and its wait for
	// replies. Injectable for the reason [Options.HealthInterval] is: a
	// suite that waited out the production value would either be slow or
	// assert nothing.
	PresenceInterval time.Duration
	AskBudget        time.Duration
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

	// channel is the room this tab is looking at, and typingAt is when it
	// last said it was composing there. A zero instant is not typing.
	channel  string
	typingAt time.Time

	// sent is the fingerprint of the last presence frame this socket was
	// given, so an unchanged view costs no frame. Presence is the one
	// thing here that is re-evaluated on a timer rather than on an event,
	// so without this every tab would get a frame every interval for ever.
	sent string
}

// chatStatusKey identifies one live working indicator.
type chatStatusKey struct{ handle, channel, thread string }

// ChatHub is the chat arm of the live channel: the watchers, the visibility
// filter, the presence scatter and the read-state push.
//
// ONE PER PROCESS, and its lifetime is the process's rather than the API's: a
// seat raises its working indicator on whichever node its turn runs on, and
// the presence answer that shows it is served by every node, so the engine
// is attached to a hub on every node, whether or not that node serves an API
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

	// wake asks the presence loop to probe now rather than at the next
	// tick. Buffered to one: a burst of keystrokes is one probe.
	wake chan struct{}

	// sockets is closed once the API's socket surface is built over this
	// hub ([ChatHub.AttachSockets]); serving says so to the engine.
	sockets     chan struct{}
	socketsOnce sync.Once
	serving     atomic.Bool

	mu       sync.Mutex
	watchers map[*Client]*chatWatcher
	working  map[chatStatusKey]ChatWorking

	// pushed is the read-state revision last pushed per seat, so a watch
	// begun again re-delivering every record pushes only what moved.
	pushed map[string]uint64

	started bool
	stop    chan struct{}
	done    chan struct{}

	// unwired says the missing-reader warning has been given. Once, not
	// per record: this is the busiest log in the engine. unserved is the
	// same for the presence subscription, which is retried per tick.
	unwired  sync.Once
	unserved sync.Once

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
		wake:       make(chan struct{}, 1),
		sockets:    make(chan struct{}),
		watchers:   map[*Client]*chatWatcher{},
		working:    map[chatStatusKey]ChatWorking{},
		pushed:     map[string]uint64{},
	}
	if h.now == nil {
		h.now = func() time.Time { return time.Now().UTC() }
	}
	if h.opts.PresenceInterval <= 0 {
		h.opts.PresenceInterval = ChatPresenceInterval
	}
	if h.opts.AskBudget <= 0 {
		h.opts.AskBudget = ChatPresenceAsk
	}
	return h
}

// Start runs the fan-out, the presence loop and the read-state push until ctx
// is done or [Stop] is called.
//
// THREE GOROUTINES, not one. A presence probe waits out its scatter's deadline
// by construction (a scatter has no roster, so there is no reply count that
// ends it early), and a live message queued behind that wait would be
// delivered that much after it was said; a read-state watch is a stream with
// a loop of its own.
//
// AND THIS NODE ANSWERS ITS PEERS' PRESENCE PROBES BEFORE IT RETURNS: the
// subscription is attempted here rather than on the loop's first turn, so the
// node a boot hands back is one already in the fleet's presence. A refused
// attempt is retried on every tick.
//
// Idempotent: a second call while one is running is a no-op rather than a
// second set of loops, which would probe twice per interval and leave
// goroutines [Stop] never joins.
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
	h.mu.Unlock()

	unsubscribe := h.serve(ctx)
	var loops sync.WaitGroup
	loops.Go(func() { h.fanLoop(ctx, stop) })
	loops.Go(func() { h.presenceLoop(ctx, stop, unsubscribe) })
	loops.Go(func() { h.readsLoop(ctx, stop) })

	go func() {
		defer close(done)
		loops.Wait()
	}()
}

// Stop ends the three loops and waits for them, and says whatever was dropped
// since the last line [chatDropReportEvery] allowed. A hub stopped is not
// started again by its process.
func (h *ChatHub) Stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	stop, done, running := h.stop, h.done, h.started
	h.started = false
	h.stop, h.done = nil, nil
	batches, total := h.dropped.rest(), h.dropped.total
	h.mu.Unlock()
	if !running {
		return
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
// holds.
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
			h.reread()
		}
	}
}

// reread sends every watcher the frame that names no room.
func (h *ChatHub) reread() {
	watchers := h.current()
	if len(watchers) == 0 {
		return
	}
	targets := make([]*Client, 0, len(watchers))
	for _, w := range watchers {
		targets = append(targets, w.client)
	}
	fanOut(targets, Push(KindChatRoomChanged, ChatChange{}, h.now()))
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
	decided := map[chatStatusKey]bool{}
	targets := make([]*Client, 0, len(watchers))
	for _, applied := range batch {
		kind, push := chatPushKind(applied.Op)
		if !push {
			continue
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
			fanOut(targets, Push(kind, chatChangeOf(applied), applied.At))
		}
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

// presenceRead is the freshness a presence decision reads at: STALE, with no
// floor, because it is about no record — who may be told somebody is looking
// at a room — and a linearizable read would put a quorum barrier append on the
// broker for every watcher of every room on every probe.
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
// presence decision whichever copy the walk prefers ([presenceRead]).
func (h *ChatHub) maySee(ctx context.Context, read roomRead, fresh statelog.Freshness,
	decided map[chatStatusKey]bool, viewer, channelID string) bool {

	if viewer == "" || channelID == "" {
		return false
	}
	key := chatStatusKey{handle: viewer, channel: channelID}
	if answer, known := decided[key]; known {
		return answer
	}
	readCtx, cancel := context.WithTimeout(ctx, ChatVisibilityBudget)
	defer cancel()
	detail, err := read(readCtx, viewer, channelID, fresh)
	answer := err == nil && chatlet.Visible(viewer, detail.Channel, detail.Member)
	if err != nil {
		// DEBUG. A room the viewer may not read answers `not found`
		// here, which is the ordinary case on any message in a private
		// room — at warn it would be a log line per outsider per
		// message.
		log.DebugContext(ctx, "chat_live_frame_withheld", "channel", channelID,
			"error", err)
	}
	decided[key] = answer
	return answer
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

// leave withdraws a socket, and with it whatever it was looking at.
//
// IMMEDIATELY, rather than waiting out [ChatTypingTTL]: a socket that closed
// is a person who is demonstrably not typing, and the TTL exists for the tab
// that stays open and stops.
func (h *ChatHub) leave(client *Client) {
	if h == nil || client == nil {
		return
	}
	h.mu.Lock()
	_, watching := h.watchers[client]
	delete(h.watchers, client)
	h.mu.Unlock()
	if watching {
		h.probe()
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

// focus records what one socket is looking at.
//
// THE FRAME IS TRUSTED ABOUT ATTENTION AND NEVER ABOUT PERMISSION. A client
// may name any room it likes here — it is saying where it is looking, and the
// answer it gets back is filtered by [ChatHub.maySee] exactly as a committed
// frame is. So naming a private room somebody is not in produces nothing at
// all, which is the same answer as a room that does not exist.
func (h *ChatHub) focus(client *Client, channelID string, typing bool) {
	if h == nil || client == nil {
		return
	}
	channelID = strings.TrimSpace(channelID)
	h.mu.Lock()
	w, watching := h.watchers[client]
	if watching {
		if w.channel != channelID {
			// A NEW ROOM IS A NEW FRAME. The fingerprint is of the
			// last view SENT, and the view is per room, so a tab
			// that moved has to be told about the room it moved to
			// even if that room's presence has not changed.
			w.sent = ""
		}
		w.channel = channelID
		w.typingAt = time.Time{}
		if typing && channelID != "" {
			w.typingAt = h.now()
		}
	}
	h.mu.Unlock()
	if watching {
		h.probe()
	}
}

func (h *ChatHub) rooms() ChatRooms {
	if h.opts.Rooms == nil {
		return nil
	}
	return h.opts.Rooms()
}

// ---- presence ---------------------------------------------------------- //

// presenceLoop probes the fleet on a tick, or sooner when a tab moves.
//
// IT ALSO OWNS THIS NODE'S ANSWERING HALF, attempted at [ChatHub.Start] and
// again on every tick until it takes: a broker that refused the subscription
// once would otherwise leave this node answering nobody's probe for the rest
// of its life, silently, while it went on sending its own to everybody else.
func (h *ChatHub) presenceLoop(ctx context.Context, stop <-chan struct{},
	unsubscribe queue.Unsubscribe) {

	ticker := time.NewTicker(h.opts.PresenceInterval)
	defer ticker.Stop()
	defer func() {
		// WITHOUT THE CALLER'S CANCELLATION, because this teardown is
		// usually undoing the shutdown's own context, and a cleanup that
		// inherits a dead context does nothing at all.
		if unsubscribe != nil {
			_ = unsubscribe(context.WithoutCancel(ctx))
		}
	}()
	serve := func() {
		if unsubscribe == nil {
			unsubscribe = h.serve(ctx)
		}
	}

	// owed is the one probe a wake inside [ChatPresenceGap] is owed, at the
	// gap's end: nil while none is. ONE TIMER, RESET RATHER THAN STACKED, so
	// a burst of keystrokes inside one gap is one probe at its end — and not
	// the next idle tick, which would be up to [ChatPresenceInterval], six
	// times the gap the floor's own reasoning promises nobody can see.
	gap := time.NewTimer(ChatPresenceGap)
	gap.Stop()
	defer gap.Stop()
	var owed <-chan time.Time

	var last time.Time
	sweep := func() {
		last = h.sweep(ctx)
		// A SWEEP PAYS WHATEVER PROBE WAS OWED, whichever arm ran it: the
		// state it was owed for is what this one just gathered.
		if owed != nil {
			gap.Stop()
			owed = nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			serve()
			sweep()
		case <-owed:
			owed = nil
			sweep()
		case <-h.wake:
			// THE FLOOR IS ENFORCED HERE rather than by the sender,
			// because the sender is a keystroke: a client that
			// signals per character must cost one scatter, not one
			// per character. What it skips is owed at the gap's end.
			if since := h.now().Sub(last); since < ChatPresenceGap {
				if owed == nil {
					gap.Reset(ChatPresenceGap - since)
					owed = gap.C
				}
				continue
			}
			sweep()
		}
	}
}

// probe asks the presence loop to run now. Never blocks: a full channel
// already means a probe is pending, and two are one.
func (h *ChatHub) probe() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// sweep gathers this node's presence, asks the fleet for theirs, and pushes
// each socket the room it is watching.
//
// It reports the instant the sweep ran, which is what the floor above is
// measured from.
func (h *ChatHub) sweep(ctx context.Context) time.Time {
	at := h.now()
	rooms := h.rooms()
	if rooms == nil {
		return at
	}
	// ONE VISIBILITY PASS SERVES BOTH HALVES, and they really are two
	// questions: who is LEGITIMATELY looking at a room, and who may be
	// TOLD about it. A watcher whose tab names a room it cannot open is
	// neither — it contributes nobody and it hears nothing — which is
	// what stops a socket advertising itself into a private conversation
	// by naming its id.
	decided := map[chatStatusKey]bool{}
	eligible := make([]chatWatcher, 0, 8)
	for _, w := range h.current() {
		if w.channel == "" || !h.maySee(ctx, rooms.Channel, presenceRead, decided,
			w.viewer, w.channel) {
			continue
		}
		eligible = append(eligible, w)
	}
	if len(eligible) == 0 {
		// NOBODY IS LOOKING, so nothing is asked. This is what keeps a
		// fleet-wide scatter off an idle company's broker entirely: the
		// probe's cost is a function of how many tabs are open on a
		// room, not of how many nodes exist.
		return at
	}
	asked := focusedRooms(eligible)
	merged := h.presenceOf(eligible, asked, at)
	for _, reply := range h.askPeers(ctx, asked) {
		mergePresence(merged, reply)
	}
	h.deliverPresence(eligible, merged)
	return at
}

// deliverPresence hands each eligible socket the presence of the room it is
// watching — each through [fanOut] alone, because each socket's payload is its
// own room.
func (h *ChatHub) deliverPresence(eligible []chatWatcher,
	merged map[string]ChatRoomPresence) {

	for _, w := range eligible {
		frame := ChatPresenceFrame{
			Rooms: map[string]ChatRoomPresence{w.channel: merged[w.channel]},
		}
		print := presenceFingerprint(frame)
		h.mu.Lock()
		held, watching := h.watchers[w.client]
		unchanged := watching && held.sent == print
		if watching {
			held.sent = print
		}
		h.mu.Unlock()
		if !watching || unchanged {
			// AN UNCHANGED VIEW COSTS NO FRAME. Presence is the one
			// thing here evaluated on a timer rather than on an
			// event, so without this every open tab would receive a
			// frame every interval for as long as it stayed open.
			continue
		}
		fanOut([]*Client{w.client}, Push(KindChatPresence, frame, h.now()))
	}
}

// focusedRooms is every room these sockets are looking at, deduped and
// ordered so two probes of one state ask an identical question.
func focusedRooms(watchers []chatWatcher) []string {
	seen := make(map[string]bool, len(watchers))
	for _, w := range watchers {
		if w.channel != "" {
			seen[w.channel] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// presenceOf is this node's own answer for a set of rooms, from the watchers
// already established as entitled to be in them.
//
// IT IS THE WHOLE ANSWER ON A SINGLE-NODE DEPLOYMENT and half of it on a
// fleet, which is why it is computed rather than obtained by asking ourselves:
// a node that learned its own sockets' focus from the broker would show
// nothing at all whenever the broker was slow.
//
// THE WORKING INDICATORS ARE NOT TIED TO A WATCHER, and that is the common
// case rather than an edge: the node running a seat's turn is rarely the node
// somebody's browser is connected to, so it answers with no watchers at all
// and the indicator still reaches the room.
func (h *ChatHub) presenceOf(watchers []chatWatcher, rooms []string,
	at time.Time) map[string]ChatRoomPresence {

	want := make(map[string]bool, len(rooms))
	for _, room := range rooms {
		want[room] = true
	}
	out := make(map[string]ChatRoomPresence, len(rooms))
	for _, w := range watchers {
		if w.channel == "" || !want[w.channel] {
			continue
		}
		room := out[w.channel]
		room.Viewing = insertHandle(room.Viewing, w.viewer)
		if !w.typingAt.IsZero() && at.Sub(w.typingAt) < ChatTypingTTL {
			room.Typing = insertHandle(room.Typing, w.viewer)
		}
		out[w.channel] = room
	}
	h.mu.Lock()
	for key, working := range h.working {
		if !want[key.channel] {
			continue
		}
		room := out[key.channel]
		room.Working = append(room.Working, working)
		out[key.channel] = room
	}
	h.mu.Unlock()
	for id, room := range out {
		slices.SortFunc(room.Working, func(a, b ChatWorking) int {
			return strings.Compare(a.Handle+a.Thread, b.Handle+b.Thread)
		})
		out[id] = room
	}
	return out
}

// chatPresenceRequest is one probe on the wire.
type chatPresenceRequest struct {
	// Version is what the ASKER speaks. A peer that does not know it
	// answers nothing, which is the same fact as a peer with nobody in
	// the room.
	Version int `json:"version"`

	// Channels are the rooms being asked about. INSIDE THE REQUEST rather
	// than in the subject, because the scatter is served on ONE subject
	// for the whole fleet — a subject per room would need every node to
	// serve a wildcard, which a request/reply answerer cannot.
	Channels []string `json:"channels"`
}

// chatPresenceReply is one node's answer.
type chatPresenceReply struct {
	Version int    `json:"version"`
	Node    string `json:"node"`

	Rooms map[string]ChatRoomPresence `json:"rooms,omitempty"`
}

// askPeers scatters one probe and returns the replies this build can read.
func (h *ChatHub) askPeers(ctx context.Context, rooms []string) []chatPresenceReply {
	peers := h.opts.Peers
	if peers == nil {
		return nil
	}
	request, err := json.Marshal(chatPresenceRequest{
		Version: chatPresenceProtocol, Channels: rooms,
	})
	if err != nil {
		log.DebugContext(ctx, "chat_presence_encode_failed", "error", err)
		return nil
	}
	askCtx, cancel := context.WithTimeout(ctx, h.opts.AskBudget)
	defer cancel()
	// WANT ZERO, which waits out the deadline. A scatter has no roster —
	// nothing here knows how many nodes are serving — so any other number
	// would either cut the probe short of a peer that was about to answer
	// or wait for one that does not exist.
	raw, err := peers.Ask(askCtx, topics.ChatPresence, request, 0)
	if err != nil {
		log.DebugContext(ctx, "chat_presence_ask_failed", "error", err)
		return nil
	}
	out := make([]chatPresenceReply, 0, len(raw))
	for _, blob := range raw {
		var reply chatPresenceReply
		if err := json.Unmarshal(blob, &reply); err != nil ||
			reply.Version != chatPresenceProtocol {
			// AN UNREADABLE REPLY IS A MISSING ONE, on the search
			// fan-out's rule: it came from a build this one does
			// not speak, and guessing at its fields would render
			// people into a room on the strength of a shape nobody
			// declared.
			continue
		}
		if reply.Node != "" && reply.Node == h.opts.NodeID {
			// OUR OWN ANSWER, which the broker delivers back to us
			// because every server of a subject sees every request.
			// [ChatHub.presenceOf], which [ChatHub.sweep] calls for
			// this node's own watchers before peers' replies are
			// merged, already has it, first-hand and without the
			// round trip.
			continue
		}
		out = append(out, reply)
	}
	return out
}

// serve makes this node an answerer for the fleet's presence probes.
func (h *ChatHub) serve(ctx context.Context) queue.Unsubscribe {
	peers := h.opts.Peers
	if peers == nil {
		return nil
	}
	unsubscribe, err := peers.Serve(ctx, topics.ChatPresence,
		func(askCtx context.Context, raw []byte) ([]byte, error) {
			return h.answer(askCtx, raw)
		})
	if err != nil {
		// NOT FATAL, AND SAID ONCE. Presence is a courtesy: a node that
		// cannot serve probes still applies every record, still pushes
		// every committed frame, and is merely invisible to its peers'
		// typing indicators. The loop retries on every tick, so a line
		// per attempt would be a warning every few seconds for as long
		// as the broker was unhappy.
		h.unserved.Do(func() {
			log.Warn("chat_presence_serve_failed", "error", err,
				"hint", "this node's own readers will not appear in anybody "+
					"else's presence; its transcripts are unaffected, and it "+
					"keeps retrying")
		})
		return nil
	}
	return unsubscribe
}

// answer is one presence probe, answered from this node's own memory.
//
// # Read-only, and so not signed
//
// The fleet's ephemeral channels that are INSTRUCTIONS are signed
// (internal/signedask): a node carries the request out as somebody. This is a
// READ — the asker asks who is looking at, typing in or working in some rooms,
// and this node answers about its own sockets and its own seats' indicators —
// and carries nothing out: what the asker does with the answer is light an
// indicator for its own watchers, each filtered by that watcher's own
// visibility of the room. It is the scatters that only read (ADR-0031). A
// forged probe learns which seats are viewing, typing or working in a room
// whose id it knows — this node filters its watchers by THEIR visibility, not
// the asker's, because the asker is nobody it can check — and a forged reply
// can name a seat in a room's presence: what a party on the broker can do on
// any subject it reaches, and why keeping the broker's ports private is the
// operator's half (SECURITY.md).
//
// A NODE WITH NOTHING TO SAY SAYS NOTHING. An answerer that returns an error
// answers nothing at all, which the asker reads as a node that is not there —
// and those are the same fact for presence, so the empty answer is spelled as
// the cheap one rather than as an empty map on the wire per node per probe.
func (h *ChatHub) answer(ctx context.Context, raw []byte) ([]byte, error) {
	var req chatPresenceRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.Version != chatPresenceProtocol || len(req.Channels) == 0 {
		return nil, errChatPresenceUnreadable
	}
	reader := h.rooms()
	if reader == nil {
		// NO READER, NO ANSWER. Who is entitled to be in a room is the
		// same question here as it is at delivery, and a node that
		// cannot answer it must not report its watchers as present in a
		// room they may not be able to open.
		return nil, errChatPresenceEmpty
	}
	decided := map[chatStatusKey]bool{}
	eligible := make([]chatWatcher, 0, 8)
	for _, w := range h.current() {
		if w.channel == "" || !slices.Contains(req.Channels, w.channel) {
			continue
		}
		if !h.maySee(ctx, reader.Channel, presenceRead, decided, w.viewer, w.channel) {
			continue
		}
		eligible = append(eligible, w)
	}
	rooms := h.presenceOf(eligible, req.Channels, h.now())
	if len(rooms) == 0 {
		return nil, errChatPresenceEmpty
	}
	return json.Marshal(chatPresenceReply{
		Version: chatPresenceProtocol, Node: h.opts.NodeID, Rooms: rooms,
	})
}

// The silences, named so the reason a node did not answer is readable where it
// is decided rather than inferred from a nil.
var (
	errChatPresenceUnreadable = errChat("a presence probe this build does not speak")
	errChatPresenceEmpty      = errChat("nobody here is in those rooms")
)

// errChat is one of this arm's named silences.
type errChat string

func (e errChat) Error() string { return "stream: " + string(e) }

// mergePresence folds one peer's answer into the merged view.
//
// UNION ON THE SETS, because two nodes describe DIFFERENT people: each answers
// about its own sockets, so nobody appears twice and nothing has to be
// reconciled. The handles are kept sorted so a fingerprint over the result is
// stable whatever order the replies arrived in — which is the whole of what
// stops an unchanged view being pushed as a change.
func mergePresence(into map[string]ChatRoomPresence, reply chatPresenceReply) {
	for id, room := range reply.Rooms {
		held := into[id]
		for _, handle := range room.Viewing {
			held.Viewing = insertHandle(held.Viewing, handle)
		}
		for _, handle := range room.Typing {
			held.Typing = insertHandle(held.Typing, handle)
		}
		held.Working = append(held.Working, room.Working...)
		slices.SortFunc(held.Working, func(a, b ChatWorking) int {
			return strings.Compare(a.Handle+a.Thread, b.Handle+b.Thread)
		})
		held.Working = slices.CompactFunc(held.Working, func(a, b ChatWorking) bool {
			return a.Handle == b.Handle && a.Thread == b.Thread
		})
		into[id] = held
	}
}

// insertHandle keeps a sorted set of handles.
func insertHandle(held []string, handle string) []string {
	if handle == "" {
		return held
	}
	at, found := slices.BinarySearch(held, handle)
	if found {
		return held
	}
	return slices.Insert(held, at, handle)
}

// presenceFingerprint is a stable string for one socket's view.
//
// Built rather than marshalled: this runs per socket per probe, and a JSON
// encode per tab per three seconds to decide whether to send anything would
// cost more than the frame it saves.
func presenceFingerprint(frame ChatPresenceFrame) string {
	var b strings.Builder
	for _, id := range slices.Sorted(maps.Keys(frame.Rooms)) {
		room := frame.Rooms[id]
		b.WriteString(id)
		b.WriteString("|v:")
		b.WriteString(strings.Join(room.Viewing, ","))
		b.WriteString("|t:")
		b.WriteString(strings.Join(room.Typing, ","))
		b.WriteString("|w:")
		for _, working := range room.Working {
			b.WriteString(working.Handle)
			b.WriteString("@")
			b.WriteString(working.Thread)
			b.WriteString("=")
			b.WriteString(working.Status)
			b.WriteString(";")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ---- the working indicator --------------------------------------------- //

// THE WORKING INDICATOR RIDES PRESENCE, NOT THE LOG.
//
// "Agent SWE is thinking…" is in-flight state about a turn that is running
// right now: it is true for minutes, it is worthless the moment the turn ends,
// and nothing replays it. On the log it would be two records per turn on the
// busiest stream in the engine, applied by every node and carried in every
// backup, to say something no reader will ever ask about again.
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
// THIRTY SECONDS, and it is long because nothing here expires. A vendor's
// indicator lapses on the vendor's own timer, so its refresh is sized against
// that; this entry lives in the memory of the node running the turn and is
// removed when the turn ends. What the heartbeat buys instead is the one case
// that memory cannot cover — a re-raise after this node restarted mid-turn —
// so it is sized against how long an indicator may be missing after a restart
// rather than against an expiry that does not exist.
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

// SetStatus implements [notify.StatusPoster]: raise or update the indicator.
//
// It never fails. A failed indicator is cosmetic and is not a turn's problem,
// which is why the seam reports a bool rather than an error — and here there
// is nothing that can fail at all: the entry is a map write, and the frame it
// produces is sent by the next probe.
func (h *ChatHub) SetStatus(_ context.Context, handle, channel, thread, status string) bool {
	if h == nil || handle == "" || channel == "" {
		return false
	}
	h.mu.Lock()
	h.working[chatStatusKey{handle: handle, channel: channel, thread: thread}] =
		ChatWorking{Handle: handle, Thread: thread, Status: status}
	h.mu.Unlock()
	h.probe()
	return true
}

// ClearStatus implements [notify.StatusPoster]: take the indicator down.
//
// SEPARATE FROM SetStatus WITH AN EMPTY STRING, on the seam's own rule: the
// two are the same operation only on a backend that renders text, and
// overloading them makes "raise" and "clear" indistinguishable for one that
// does not.
func (h *ChatHub) ClearStatus(_ context.Context, handle, channel, thread string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	delete(h.working, chatStatusKey{handle: handle, channel: channel, thread: thread})
	h.mu.Unlock()
	h.probe()
	return true
}
