package stream

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/chat"
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
// have a tab open.
//
// So this arm keeps its own registry of watchers, each with the SEAT its
// socket's credential resolved to, and decides per watcher. The decision is
// [chat.Visible] — the domain's own function, applied to the room and the
// membership this node holds — and never a second idea of it: a filter that
// re-derived "is this room private" from a kind would be a second rule, and
// the two would disagree the first time a kind was added.
//
// # The identity is the SOCKET'S, and a socket whose binding moves is closed
//
// A query frame may carry a token that upgrades that one question
// ([runQuery]). A push stream may not: it is long-lived, frames have already
// gone out under the identity the socket was accepted with, and an identity
// that could move mid-stream would make "what has this socket been shown"
// unanswerable. A socket whose credential resolves to no seat watches nothing
// — reads included — which is the same rule the REST and MCP surfaces apply,
// for the same reason.
//
// BUT THE BINDING IS RE-READ, on every pass that decides what a socket is
// shown ([ChatHub.current]), because the credential is the socket's and the
// seat it names is the company's: `contact.crewlet_operator_id` is live
// configuration, and an apply that unbinds a token, rebinds it to somebody
// else or removes the seat must not leave an open tab receiving the old
// seat's private rooms, presence and read state for as long as it stays open —
// days, for a tab nobody closes. Every other chat surface resolves per call
// for exactly that reason. A socket whose resolution no longer matches the
// seat it was registered as is therefore CLOSED rather than switched: the
// dashboard reconnects at once, and the new connection is accepted under the
// binding the company holds now — which is also how a socket accepted under a
// token nobody had bound yet starts watching when an apply binds it.
//
// # Two things travel here, and only one of them is durable
//
// A COMMITTED RECORD arrives through [chat.Observer], which the applier calls
// after its transaction commits. That is a courtesy on top of the rows rather
// than a delivery anybody may rely on: a wake is derived by something that
// outlives the writer (see internal/changefeed), while a socket frame is
// worthless to a viewer who is not looking and is wanted in milliseconds by
// whoever is. Anything that misses a frame refetches, and the transcript's
// contiguous per-channel sequence is what makes that refetch exact.
//
// PRESENCE is the opposite: who is looking at which room, who is typing, and
// which seat is working on a message. None of it is durable, none of it is
// applied, and none of it may ever become a record — a keystroke that landed on
// the chat log would be a row every data node applies and every backup carries,
// for ever. It rides the queue's EPHEMERAL scatter on [topics.ChatPresence],
// which is deliberately outside the chat log's own wildcard.
//
// A PERSON'S OWN STATE is the third thing, and it rides the scatter too: their
// read state moving is news for their own screens, on whichever nodes those
// are attached to, and for nobody else ([ChatHub.PushTo]). It is on no log, so
// no applier on any node will ever announce it — the node that took the write
// tells its peers on [topics.ChatPerson], and each delivers it to that
// person's sockets of its own.
//
// # Nothing here may block the applier
//
// [ChatHub.Applied] runs on the applier's goroutine, between one batch's
// commit and the next batch's transaction, so a slow observer is a node whose
// log applies slowly. It therefore hands the batch to this hub's own buffer
// and returns, and a full buffer DROPS rather than waits: a dropped frame
// costs one screen a refetch, and a blocked applier costs the node its place
// in the fleet.

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
	// archived, erased or pruned.
	//
	// ONE KIND FOR ALL FIVE, because what a client does with them is one
	// thing — read the room again. An erase and a prune remove messages
	// and the record does not carry which, so a frame that tried to name
	// them would be a frame a client could not act on anyway.
	KindChatRoomChanged Kind = "chat_room_changed"

	// KindChatMembershipChanged is a room's membership set being replaced,
	// which includes somebody joining or leaving.
	KindChatMembershipChanged Kind = "chat_membership_changed"

	// KindChatCursorMoved is one person's own read state moving — where
	// they have read to, what they have muted, whether they are in
	// do-not-disturb.
	//
	// IT REACHES THAT PERSON'S OWN SOCKETS AND NOBODY ELSE'S, on every
	// node. A cursor is a fact about one reader's attention: it is not on
	// the log, nobody replays it, and a second tab — attached to whichever
	// node its load balancer picked — is the only thing in the world that
	// wants it.
	KindChatCursorMoved Kind = "chat_cursor_moved"

	// KindChatPresence is who is looking at, typing in, or working on a
	// message in the rooms this socket said it is watching.
	//
	// NOT PAST TENSE, because it is not a thing that happened: it is the
	// current answer, whole, for the rooms this socket asked about. A
	// delta would need a starting point a reconnecting tab does not have.
	KindChatPresence Kind = "chat_presence"
)

// ChatPushKinds is every kind this arm emits.
//
// Stated as a list so a gate can hold it against the read registry's own
// names: see the kinds above for why the two namespaces may not overlap.
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
// open is what makes that true.
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
	// goes away. The dashboard's re-assert interval (`TYPING_THROTTLE_MS`)
	// is held to at most half of it by
	// TestTheDashboardReassertsTypingInsideTheEnginesExpiry.
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
	// one viewer.
	ChatVisibilityBudget = 2 * time.Second

	// chatAppliedQueue is how many committed batches may wait for the
	// fan-out.
	//
	// Sixty-four. The applier must never wait here (see the package doc
	// above), so this is what absorbs a burst of applies while one
	// batch's visibility reads are in flight; past it a batch is dropped
	// and the screens that missed it refetch. It is small deliberately:
	// a deep queue would mean rendering a conversation minutes behind the
	// rows, which is worse than a refetch.
	chatAppliedQueue = 64

	// chatPersonQueue is how many person pushes may wait for the fleet
	// fan-out, and how many one scatter carries.
	//
	// Sixty-four, and it is sized against the scatter's cost rather than
	// against latency. A scatter has no roster, so each one waits out
	// [chatPersonSend] before the next can leave — but the pushes that
	// queue meanwhile all leave in the NEXT one, so the bound is pushes
	// per send: see [chatPersonSend] for what that carries. Past it a push
	// is dropped and counted ([chatDropReportEvery]), and the screen it
	// was for repairs on its next read of the rail; the local delivery has
	// already happened by then, so what is lost is only the other nodes'.
	chatPersonQueue = 64

	// chatPersonSend is the deadline one person fan-out gives its scatter.
	//
	// IT WAITS FOR NOTHING, and that is what decides it. Every answerer of
	// the person subject answers nothing ([errChatPersonDelivered]), and
	// the scatter verb collects replies until its deadline — so the whole
	// window is spent on replies that never come, and the window is this
	// loop's PACE: one batch of up to [chatPersonQueue] pushes per window.
	// On the broker the request is published and flushed before the wait
	// begins, and the deadline bounds none of that; on the in-process twin
	// each answerer runs INSIDE the window, which is why it is not zero.
	//
	// A TENTH OF A SECOND, set against the load it has to carry. A tab
	// flushes its read state at most once per the dashboard's fifteen
	// seconds (READ_FLUSH_MS, which is coord.ReconcileInterval), and
	// sixty-four pushes per tenth of a second is 640 a second: every tab
	// of over nine thousand on ONE node flushing on that cadence, with a
	// burst of sixty-four inside any one window — everybody opening chat
	// at nine — absorbed by the queue on top. And it is two orders of
	// magnitude above an in-process answerer handing a frame to a socket
	// buffer, so the twin always delivers.
	//
	// DELIBERATELY NOT [ChatPresenceAsk]. That is a probe's wait for
	// replies it needs, and borrowed here it paced this loop at sixty-four
	// pushes per three quarters of a second for no reply at all — past
	// which every push was dropped, silently, until the hub stopped.
	chatPersonSend = 100 * time.Millisecond

	// chatDropReportEvery is how often a hub says it is dropping, while it
	// runs.
	//
	// A MINUTE. A drop is a frame some screen did not get — a committed
	// batch the live fan-out could not take, or another node's copy of a
	// person's push — and each is repaired by that screen's next read, so
	// no single one is worth a line. A stream of them is a node that
	// cannot keep up, though, and the only place that was ever said was
	// the line the hub wrote when it STOPPED, which for a node that runs
	// for months is never. So the first drop is said at once, and then at
	// most one line a minute names how many went since the last: a burst
	// is one line, a sustained overload a line a minute rather than one
	// per frame, and the rest is said as the hub stops.
	chatDropReportEvery = time.Minute
)

// chatPresenceProtocol is the presence scatter's payload version.
//
// ONE, and it moves only for a reshape — a new FIELD needs no bump, because a
// peer ignores what it does not know. A reply this build cannot read is a
// MISSING reply, exactly as the search fan-out treats one: presence is a
// courtesy, and a rolling upgrade must degrade it rather than break it.
const chatPresenceProtocol = 1

// chatPersonProtocol is the person fan-out's payload version, on
// [chatPresenceProtocol]'s rule: a new field needs no bump, and a push this
// build cannot read is a push it does not deliver — a screen that misses one
// repairs on its next read.
const chatPersonProtocol = 1

// ChatRooms answers whether one viewer may read one room.
//
// Declared by the consumer and kept to the two reads this arm makes: a room
// read refuses a room the viewer may not read as NOT FOUND, and hands back the
// room and this viewer's membership for the rooms it does serve — which is
// exactly the pair [chat.Visible] decides from.
type ChatRooms interface {
	// Channel is the read a PRESENCE decision makes — who may be told
	// about a room somebody is looking at — which is about no record in
	// particular, so whichever copy the estate's walk prefers answers it.
	Channel(ctx context.Context, viewer, channelID string,
		fresh statelog.Freshness) (chat.ChannelDetail, error)

	// ChannelHere is the read a COMMITTED FRAME'S decision makes: asked of
	// this node's own copy first even while it lags its log, because that
	// copy's applier is the one announcing the record and holds it by
	// construction, where a peer the walk preferred might not yet — see
	// [ChatHub.fan].
	ChannelHere(ctx context.Context, viewer, channelID string,
		fresh statelog.Freshness) (chat.ChannelDetail, error)
}

// roomRead is one of [ChatRooms]' two reads, as a pass chooses it.
type roomRead func(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chat.ChannelDetail, error)

// ChatPeers is the ephemeral scatter presence rides.
//
// The two verbs that leave no trace, declared by the consumer so this arm
// depends on the shape rather than on the broker: nothing here may be
// published, retained, redelivered or recorded. See [queue.EventQueue.Ask].
type ChatPeers interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
	Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error)
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

// ChatPresenceFrame is the presence push's payload: the rooms this socket
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
// badge: an excerpt is at most [chat.MaxExcerpt] bytes of what a card shows,
// and the mentions are how a tab raises its own badge without a fetch. (The
// wake does not need them: a woken seat reads the whole message.) Every post
// and edit an author made carries a snapshot, even one that wakes nobody; the
// records that carry none — a system line, a delete, a reaction, and every
// room record — have neither, and neither is a substitute for reading the
// room. The thread root and the room's name and kind come from the same
// snapshot, and are absent on the same records.
//
// THE WAKE SET IS DELIBERATELY NOT HERE. Who the engine woke is a routing
// fact about seats, and a browser frame carrying it would publish the
// company's delivery decisions to every tab in the room.
type ChatChange struct {
	ChannelID string `json:"channel_id"`

	// Op is the record's own word for what happened — `post`, `edit`,
	// `react` — rather than a second vocabulary of this transport's. It
	// is the value the event store's own column carries.
	Op chat.OpKind `json:"op"`

	// OpID is the record's idempotency key, so a client that made this
	// gesture recognises its own effect coming back and renders it once.
	OpID string `json:"op_id"`

	MessageID  string `json:"message_id,omitempty"`
	ChannelSeq int64  `json:"channel_seq,omitempty"`
	ThreadRoot string `json:"thread_root,omitempty"`

	Author     string          `json:"author,omitempty"`
	AuthorKind chat.AuthorKind `json:"author_kind,omitempty"`

	// At is the BROKER'S own instant, which is what every node renders —
	// a local clock here would make one viewer's timeline disagree with
	// another's.
	At time.Time `json:"at"`

	// Position is where the record sits on the log. It is what a read
	// cursor stores, so a client that has rendered this frame can flush
	// its own cursor without a fetch.
	Position statelog.Position `json:"position"`

	Excerpt  string   `json:"excerpt,omitempty"`
	Mentions []string `json:"mentions,omitempty"`

	// ChannelName and ChannelKind render a toast for a room the tab is
	// not looking at. Empty for a direct conversation, which has no name,
	// and on a record that carries no routing snapshot (see above).
	ChannelName string    `json:"channel_name,omitempty"`
	ChannelKind chat.Kind `json:"channel_kind,omitempty"`
}

// ChatHubOptions configure the chat arm.
//
// EVERY SEAM IS A FUNCTION, and that is an ordering fact rather than a style:
// the applier takes its observer when the state log comes up, which is before
// the reader, the broker client and the company's org chart exist. A hub built
// from values would have to be built after them and could therefore never be
// the applier's observer. Each function may answer nil until its half is up,
// and the degradation is stated at the field.
type ChatHubOptions struct {
	// Rooms decides who may see what. NIL SERVES NO FRAME AT ALL — a
	// filter that cannot be run is not permission to skip it — and the
	// first record applied with none says so once, at warn, because a
	// silently dead push path looks exactly like a quiet company.
	Rooms func() ChatRooms

	// Viewer resolves a socket's credential to the seat it watches as, or
	// "" for a credential bound to none. It is the SERVER'S answer on
	// every socket: a caller may never name a seat.
	//
	// ASKED ON EVERY PASS, once per credential, so it must be cheap: a
	// lookup over the live chart, never a fetch.
	//
	// AND HANDED NOTHING BUT THE CREDENTIAL — no context — because a
	// context is what could carry somebody else into it. A pass runs on
	// behalf of whatever triggered it: a socket's accept, the fan-out's
	// loop, a peer's probe, or a person's own request pushing their read
	// state ([ChatHub.PushTo]), which carries the seat its dispatch pinned
	// for that one caller. Resolved under that request, every socket on
	// the node resolved to the caller, and each other person's was closed
	// as a binding that moved — on every read flush, mute and
	// do-not-disturb anybody made. One credential over one chart needs
	// nothing else, so it is given nothing else.
	Viewer func(operatorID string) string

	// Peers carries presence, and every push for one person's own
	// sockets ([ChatHub.PushTo]), to the rest of the fleet. Nil keeps both
	// on this node's own sockets, which is honest and is the whole truth
	// on a single-node deployment.
	Peers func() ChatPeers

	// NodeID names this node in its own presence answer, so an asker can
	// drop the reply to its own scatter rather than merging itself twice.
	NodeID string

	// Now is injectable so a test can pin the typing window.
	Now func() time.Time

	// PresenceInterval and AskBudget override [ChatPresenceInterval] and
	// [ChatPresenceAsk] — the presence probe's cadence and its wait for
	// replies, and nothing else: the person fan-out waits for no reply
	// and is paced by [chatPersonSend]. Injectable for the reason
	// [Options.HealthInterval] is: a suite that waited out the production
	// value would either be slow or assert nothing.
	PresenceInterval time.Duration
	AskBudget        time.Duration
}

// chatWatcher is one socket watching chat.
type chatWatcher struct {
	client *Client

	// operatorID is the credential the socket was accepted with, which
	// every pass resolves again ([ChatHub.current]).
	operatorID string

	// viewer is the seat this socket's credential resolved to. NEVER
	// empty in the registry: a socket that resolved to no seat is not
	// registered at all, which is what makes "an unbound token gets
	// nothing" a property of the data structure rather than of a check
	// somebody has to remember.
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
// filter, and the presence scatter.
type ChatHub struct {
	opts ChatHubOptions
	now  func() time.Time

	// applied carries committed batches from the applier's goroutine to
	// this hub's own. Buffered, and a full buffer drops — see the package
	// doc.
	applied chan []chat.Applied

	// wake asks the presence loop to probe now rather than at the next
	// tick. Buffered to one: a burst of keystrokes is one probe.
	wake chan struct{}

	mu       sync.Mutex
	watchers map[*Client]*chatWatcher
	working  map[chatStatusKey]ChatWorking

	// idle is every socket whose credential resolved to NO seat when it
	// was accepted, by that credential. Never a watcher — see [ChatHub.join]
	// — and held only so a pass can see the apply that binds it
	// ([ChatHub.current]).
	idle map[*Client]string

	started bool
	stop    chan struct{}
	done    chan struct{}

	// wired carries [ChatHub.Wired]'s requests to the presence loop, which
	// owns the answering halves; each carries the channel the loop closes
	// once it has attempted them.
	wired chan chan struct{}

	// unwired says the missing-reader warning has been given. Once, not
	// per record: this is the busiest log in the engine. unserved is the
	// same for the presence subscription, which is retried per tick.
	unwired  sync.Once
	unserved sync.Once

	// dropped counts batches the fan-out could not keep up with, and says
	// so while the hub runs — see [chatDropReportEvery].
	dropped dropTally

	// origin names THIS HUB on the person fan-out, so its own push coming
	// back off the scatter is recognised and not delivered twice — every
	// server of a subject sees every request, the asker's included. Minted
	// per hub rather than taken from [ChatHubOptions.NodeID], which may be
	// empty and which two hubs in one process can share.
	origin string

	// personOut carries person pushes from [ChatHub.PushTo] to the fan-out
	// loop. Buffered, and a full buffer drops — see [chatPersonQueue].
	personOut chan chatPersonPush

	// unservedPerson is [ChatHub.unserved] for the person subject, and
	// personDropped is [ChatHub.dropped] for the person fan-out.
	unservedPerson sync.Once
	personDropped  dropTally
}

// NewChatHub builds the chat arm.
//
// It refuses nothing, which is deliberate and is the other half of the
// ordering note on [ChatHubOptions]: this is built before the halves it reads
// through exist, so there is nothing yet to check. What a missing seam costs
// is stated at its field and reported by the hub itself when it bites.
func NewChatHub(opts ChatHubOptions) *ChatHub {
	h := &ChatHub{
		opts:     opts,
		now:      opts.Now,
		applied:  make(chan []chat.Applied, chatAppliedQueue),
		wake:     make(chan struct{}, 1),
		wired:    make(chan chan struct{}),
		watchers: map[*Client]*chatWatcher{},
		working:  map[chatStatusKey]ChatWorking{},
		idle:     map[*Client]string{},

		origin:    uuid.NewString(),
		personOut: make(chan chatPersonPush, chatPersonQueue),
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

// Start runs the fan-out, the presence loop and the person fan-out until ctx
// is done or [Stop] is called.
//
// THREE GOROUTINES, not one. A presence probe and a person push each wait out
// their scatter's deadline by construction (a scatter has no roster, so there
// is no reply count that ends it early) — [ChatPresenceAsk] and
// [chatPersonSend] — and a live message queued behind either wait would be
// delivered that much after it was said, as would a person's push queued
// behind a probe.
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

	var loops sync.WaitGroup
	loops.Go(func() { h.fanLoop(ctx, stop) })
	loops.Go(func() { h.presenceLoop(ctx, stop) })
	loops.Go(func() { h.personLoop(ctx, stop) })

	go func() {
		defer close(done)
		loops.Wait()
	}()
}

// Stop ends the three loops and waits for them, and says whatever was dropped
// since the last line [chatDropReportEvery] allowed.
func (h *ChatHub) Stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	stop, done, running := h.stop, h.done, h.started
	h.started = false
	h.stop, h.done = nil, nil
	batches, batchesTotal := h.dropped.rest(), h.dropped.total
	pushes, pushesTotal := h.personDropped.rest(), h.personDropped.total
	h.mu.Unlock()
	if !running {
		return
	}
	close(stop)
	<-done
	reportBatchesDropped(batches, batchesTotal)
	reportPushesDropped(pushes, pushesTotal)
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
			"unaffected and a screen that missed a frame refetches")
}

// reportPushesDropped says n person pushes never left this node, if any did.
func reportPushesDropped(n, total int) {
	if n == 0 {
		return
	}
	log.Warn("chat_person_pushes_dropped", "pushes", n, "since_start", total,
		"hint", "the fleet fan-out of people's read state fell behind; each "+
			"was delivered on the node that took it, and a screen on another "+
			"node that missed one repairs on its next read of the rail")
}

// ---- the committed half ------------------------------------------------ //

// Applied is [chat.Observer]: the applier hands over what its transaction
// committed.
//
// IT NEVER BLOCKS AND IT NEVER READS. Both are the applier's contract: this
// runs on its goroutine between two transactions, so a visibility read here
// would put a store query inside the log's own loop. The batch is handed to
// this hub's buffer, and a full buffer is a DROP with a count — see
// [chatAppliedQueue].
func (h *ChatHub) Applied(batch []chat.Applied) {
	if h == nil || len(batch) == 0 {
		return
	}
	select {
	case h.applied <- batch:
	default:
		// SAID AS IT HAPPENS, at most once per [chatDropReportEvery] — a
		// line, never a wait, so the applier's contract above holds.
		h.mu.Lock()
		say, total := h.dropped.drop(h.now()), h.dropped.total
		h.mu.Unlock()
		reportBatchesDropped(say, total)
	}
}

// fanLoop delivers committed batches.
func (h *ChatHub) fanLoop(ctx context.Context, stop <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case batch := <-h.applied:
			h.fan(ctx, batch)
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
// # Read at the batch, here first
//
// Each decision is held to the BATCH'S OWN POSITION ([statelog.Freshness]'s
// floor, honoured at `stale`), so whichever copy answers holds the records
// being announced: a copy that had not applied a membership change would
// decide it against the rows from before it. And it is asked of this node's
// copy first even while that copy lags its log ([ChatRooms.ChannelHere]):
// this node's applier is the one announcing the batch, so its copy holds it
// by construction — while the estate's ordinary walk asks a peer whose copy
// does not lag first, and a burst is what makes a copy lag, so every decision
// during one was a network round trip on this one goroutine, past which the
// batches queued behind it were dropped.
func (h *ChatHub) fan(ctx context.Context, batch []chat.Applied) {
	watchers := h.current()
	if len(watchers) == 0 {
		return
	}
	rooms := h.rooms()
	if rooms == nil {
		h.unwired.Do(func() {
			log.Warn("chat_live_has_no_room_reader",
				"hint", "chat records are applying and no live frame can be "+
					"filtered, so no screen will update until it refetches; "+
					"wire ChatHubOptions.Rooms")
		})
		return
	}
	at := statelog.Freshness{Level: statelog.ReadStale, MinPosition: batchPosition(batch)}
	decided := map[chatStatusKey]bool{}
	for _, applied := range batch {
		kind, push := chatPushKind(applied.Op)
		if !push {
			continue
		}
		frame := chatChangeOf(applied)
		for _, w := range watchers {
			if !h.maySee(ctx, rooms.ChannelHere, at, decided, w.viewer, applied.ChannelID) {
				continue
			}
			w.client.send(Push(kind, frame, applied.At))
		}
	}
}

// batchPosition is the furthest position a committed batch reached: the floor
// every decision about it is held to. One applier wrote it, so every record in
// it is on the one log and the comparison never crosses streams.
func batchPosition(batch []chat.Applied) statelog.Position {
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
// for the rooms it declines to serve; [chat.Visible] is then applied to the
// room it DOES serve, because this seam is an interface and a filter that
// trusted its implementation to have already decided would be a filter with
// no rule of its own.
//
// IT FAILS CLOSED. A read that errors, times out or cannot be served drops the
// frame: the alternative is showing a transcript to somebody on the strength
// of a store that would not answer. The screen repairs itself on its next
// fetch, which is what every dropped frame here relies on.
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
	answer := err == nil && chat.Visible(viewer, detail.Channel, detail.Member)
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
// says so — rather than silently arriving as a kind nobody handles.
func chatPushKind(op chat.OpKind) (Kind, bool) {
	switch op {
	case chat.OpPost:
		return KindChatPosted, true
	case chat.OpEdit:
		return KindChatEdited, true
	case chat.OpDelete:
		return KindChatDeleted, true
	case chat.OpReact:
		return KindChatReacted, true
	case chat.OpCreate, chat.OpPatch, chat.OpErase, chat.OpPrune:
		return KindChatRoomChanged, true
	case chat.OpMembers:
		return KindChatMembershipChanged, true
	case chat.OpEviction, chat.OpGeneration, chat.OpBarrier:
		return "", false
	}
	return "", false
}

// chatChangeOf renders one committed record as a frame.
func chatChangeOf(applied chat.Applied) ChatChange {
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

// PushTo sends one frame to every socket belonging to one person, on every
// node of the fleet.
//
// THE ONE PUSH HERE THAT IS NOT FILTERED BY A ROOM, because it is not about a
// room: a read cursor is a fact about one reader's attention, and the only
// thing in the world that wants it is that same person's other tabs. The
// recipient set is therefore the identity itself.
//
// AND IT LEAVES THIS NODE, because that identity's sockets are wherever their
// load balancer put them: a second tab is as likely to be attached to another
// ingress node as to this one, and a person's assistant writes through
// whichever node its MCP client reached. So this node's own sockets are sent
// the frame at once, and it is handed to [ChatHub.personLoop] for the rest of
// the fleet ([topics.ChatPerson]). It NEVER BLOCKS: the caller is a request
// whose write has already landed, and a fan-out that has fallen behind drops
// and counts rather than holding it — see [chatPersonQueue].
//
// IT TAKES NO CONTEXT, and that is the guard rather than an omission: the
// caller is one person's request, and nothing of it may reach the pass that
// decides whose every other socket is ([ChatHubOptions.Viewer]).
func (h *ChatHub) PushTo(handle string, kind Kind, data any) {
	if h == nil || handle == "" {
		return
	}
	at := h.now()
	h.deliverTo(handle, Push(kind, data, at))
	if h.opts.Peers == nil {
		// NO FLEET TO TELL — a single node, whose sockets are all here.
		return
	}
	raw, err := json.Marshal(data)
	if err != nil {
		log.Debug("chat_person_push_encode_failed", "kind", kind, "error", err)
		return
	}
	select {
	case h.personOut <- chatPersonPush{Handle: handle, Kind: kind, Data: raw, At: at}:
	default:
		h.mu.Lock()
		say, total := h.personDropped.drop(at), h.personDropped.total
		h.mu.Unlock()
		reportPushesDropped(say, total)
	}
}

// deliverTo hands one frame to every socket of one person on this node.
func (h *ChatHub) deliverTo(handle string, env Envelope) {
	for _, w := range h.current() {
		if w.viewer == handle {
			w.client.send(env)
		}
	}
}

// ---- the socket registry ----------------------------------------------- //

// join registers one socket, reporting whether it watches chat at all.
//
// A SOCKET WHOSE CREDENTIAL RESOLVES TO NO SEAT IS NOT REGISTERED. Not
// registered and then filtered: an unbound credential has no membership
// anywhere, so every decision about it would be "no" — and a registry that
// held it would be one accidental change away from serving it the public
// rooms, which is a viewer the company never declared.
//
// IT IS REMEMBERED, though, when it carried a credential at all: an apply that
// binds that credential later is a person who should start watching, and only
// a socket the hub can still see can be told to reconnect ([ChatHub.current]).
func (h *ChatHub) join(client *Client, operatorID string) bool {
	if h == nil || client == nil || h.opts.Viewer == nil {
		return false
	}
	viewer := strings.TrimSpace(h.opts.Viewer(operatorID))
	h.mu.Lock()
	defer h.mu.Unlock()
	if viewer == "" {
		if operatorID != "" {
			h.idle[client] = operatorID
		}
		return false
	}
	h.watchers[client] = &chatWatcher{client: client, operatorID: operatorID, viewer: viewer}
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
	delete(h.idle, client)
	h.mu.Unlock()
	if watching {
		h.probe()
	}
}

// current is the watcher set as the company binds it NOW: every socket's
// credential resolved again, once per credential, and every socket whose
// resolution moved CLOSED — see the package doc for why closed rather than
// switched.
//
// CALLED AT THE TOP OF EVERY PASS that decides what a socket is shown — a
// committed batch, a presence sweep, a peer's presence probe, a person's own
// push — so no frame is decided against a binding an apply has replaced. A
// pass resolves each credential once, which is the cost of it: the
// resolution is a lookup over the live chart ([ChatHubOptions.Viewer]).
//
// NOTHING OF THE PASS REACHES IT. The set is the same whoever asked, so it is
// computed from the registry and the chart alone — never under the context of
// whatever triggered the pass, which for a person's own push is a request
// carrying that person ([ChatHub.PushTo]).
//
// A COPY, so no pass holds the lock across a store read or a send.
func (h *ChatHub) current() []chatWatcher {
	h.mu.Lock()
	watchers := make([]chatWatcher, 0, len(h.watchers))
	for _, w := range h.watchers {
		watchers = append(watchers, *w)
	}
	idle := maps.Clone(h.idle)
	h.mu.Unlock()
	if h.opts.Viewer == nil {
		return watchers
	}
	resolved := make(map[string]string, len(watchers)+len(idle))
	resolve := func(operatorID string) string {
		viewer, known := resolved[operatorID]
		if !known {
			viewer = strings.TrimSpace(h.opts.Viewer(operatorID))
			resolved[operatorID] = viewer
		}
		return viewer
	}
	var moved []chatWatcher
	held := watchers[:0]
	for _, w := range watchers {
		if resolve(w.operatorID) != w.viewer {
			moved = append(moved, w)
			continue
		}
		held = append(held, w)
	}
	for client, operatorID := range idle {
		if resolve(operatorID) != "" {
			moved = append(moved, chatWatcher{client: client, operatorID: operatorID})
		}
	}
	if len(moved) > 0 {
		h.rebound(moved, resolve)
	}
	return held
}

// rebound withdraws the sockets whose binding moved and closes them, so each
// reconnects under the binding the company holds now.
//
// ONLY WHAT THIS CALL WITHDREW, which is not everything it was handed. The set
// was copied before the lock was let go to resolve it, and several passes run
// at once — the fan loop, the presence sweep, a peer's probe, a person's own
// push — so two that copied before either withdrew a socket would both log
// its rebinding, and a socket its tab closed in that window would be logged
// as dropped for a binding that moved when it had simply gone. A socket still
// registered is one no other pass has withdrawn and that has not left, so the
// line is said exactly once, by the pass that took it out.
//
// ITS LINE CARRIES NO REQUEST'S TRACE: the apply that moved the binding is the
// cause, and whichever pass noticed it — a person's own push among them — is
// not.
func (h *ChatHub) rebound(moved []chatWatcher, resolve func(string) string) {
	h.mu.Lock()
	watching := false
	withdrawn := moved[:0]
	for _, w := range moved {
		_, was := h.watchers[w.client]
		_, waiting := h.idle[w.client]
		if !was && !waiting {
			continue
		}
		watching = watching || was
		delete(h.watchers, w.client)
		delete(h.idle, w.client)
		withdrawn = append(withdrawn, w)
	}
	h.mu.Unlock()
	for _, w := range withdrawn {
		// INFO, because it is the visible half of an apply: a tab that
		// drops and reconnects, and the one line that says why.
		log.Info("chat_socket_rebound", "operator", w.operatorID,
			"was", w.viewer, "now", resolve(w.operatorID),
			"detail", "the credential's binding moved, so the socket is closed "+
				"and reconnects under the one the company holds now")
		w.client.Close()
	}
	if watching {
		// AND IT LEAVES EVERY ROOM IT WAS IN, as a socket that closed does.
		h.probe()
	}
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

func (h *ChatHub) peers() ChatPeers {
	if h.opts.Peers == nil {
		return nil
	}
	return h.opts.Peers()
}

// ---- presence ---------------------------------------------------------- //

// presenceLoop probes the fleet on a tick, or sooner when a tab moves.
//
// IT ALSO OWNS THIS NODE'S TWO ANSWERING HALVES — the presence probe's and the
// person fan-out's — and each is attempted on every tick until it takes rather
// than once at start-up. A hub is built before the broker client exists (see
// [ChatHubOptions]), so a subscription made once would be made against
// whatever was wired at that instant — and if that was nothing, this node
// would answer nobody's probe and show its sockets nobody's read state for the
// rest of its life, silently, while continuing to send its own to everybody
// else. The tick is the retry; [ChatHub.Wired] is the attempt that should
// succeed, made the moment the broker client exists rather than a tick later.
func (h *ChatHub) presenceLoop(ctx context.Context, stop <-chan struct{}) {
	ticker := time.NewTicker(h.opts.PresenceInterval)
	defer ticker.Stop()
	var unsubscribe, unsubscribePerson queue.Unsubscribe
	defer func() {
		// WITHOUT THE CALLER'S CANCELLATION, because this teardown is
		// usually undoing the shutdown's own context, and a cleanup that
		// inherits a dead context does nothing at all.
		for _, u := range []queue.Unsubscribe{unsubscribe, unsubscribePerson} {
			if u != nil {
				_ = u(context.WithoutCancel(ctx))
			}
		}
	}()
	serve := func() {
		if unsubscribe == nil {
			unsubscribe = h.serve(ctx)
		}
		if unsubscribePerson == nil {
			unsubscribePerson = h.servePerson(ctx)
		}
	}
	serve()

	// owed is the one probe a wake inside [ChatPresenceGap] is owed, at the
	// gap's end: nil while none is. ONE TIMER, RESET RATHER THAN STACKED, so
	// a burst of keystrokes inside one gap is one probe at its end — and not
	// the next idle tick, which is what a skipped wake used to wait for: up
	// to [ChatPresenceInterval], six times the gap the floor's own reasoning
	// promises nobody can see.
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
		case answered := <-h.wired:
			serve()
			close(answered)
		case <-ticker.C:
			serve()
			sweep()
		case <-owed:
			owed = nil
			serve()
			sweep()
		case <-h.wake:
			serve()
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

// Wired tells the hub that the seams it reads through now reach something —
// the engine its [ChatHubOptions.Peers] and [ChatHubOptions.Rooms] read is
// built — and returns once the presence loop has attempted this node's
// answering halves against them: the presence probe's and the person
// fan-out's. Or once ctx is done, or at once when the hub is not running,
// whose [ChatHub.Start] makes that same attempt first.
//
// WITHOUT IT A NODE WAS DEAF FOR ITS FIRST TICK. The hub is started before
// the engine exists, so its start-up attempt found no broker and the next was
// a [ChatHubOptions.PresenceInterval] later — and a person push is sent ONCE:
// read state moved through a colleague node in that window never reached this
// node's sockets, and nothing would ever send it again. Measured end to end:
// a mute made through the stateless node of a pair, seconds after both
// booted, reached the data node's socket never rather than late.
//
// SYNCHRONOUS, so the node a boot returns is one that already answers: a
// subscription is registered when the broker has it, and the scatter's own
// Serve flushes before returning.
func (h *ChatHub) Wired(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	stop := h.stop
	h.mu.Unlock()
	if stop == nil {
		return
	}
	answered := make(chan struct{})
	select {
	case h.wired <- answered:
	case <-stop:
		return
	case <-ctx.Done():
		return
	}
	select {
	case <-answered:
	case <-ctx.Done():
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
// watching.
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
		w.client.send(Push(KindChatPresence, frame, h.now()))
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
	peers := h.peers()
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
	peers := h.peers()
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
		// typing indicators. The caller retries on every tick, so a
		// line per attempt would be a warning every few seconds for as
		// long as the broker was unhappy.
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
	errChatPersonUnreadable   = errChat("a person push this build does not speak")
	errChatPersonOwn          = errChat("this hub's own push, already delivered here")
	errChatPersonDelivered    = errChat("a person push answers nothing")
)

// ---- a person's own state, across the fleet ---------------------------- //

// chatPersonPush is one frame for one person's sockets, on the wire.
//
// THE FRAME'S DATA TRAVELS AS ITS OWN BYTES, never re-decoded into a type
// here: the receiving node relays what the writing node said, so a field one
// build adds reaches the screen through a peer that does not know it.
type chatPersonPush struct {
	Handle string          `json:"handle"`
	Kind   Kind            `json:"kind"`
	Data   json.RawMessage `json:"data"`

	// At is the WRITING node's instant, so every node stamps the frame
	// alike.
	At time.Time `json:"at"`
}

// chatPersonRequest is one person fan-out: every push queued since the last.
type chatPersonRequest struct {
	Version int `json:"version"`

	// Origin is the hub that took the writes — see [ChatHub.origin].
	Origin string `json:"origin"`

	Pushes []chatPersonPush `json:"pushes"`
}

// personLoop carries person pushes to the rest of the fleet.
//
// ONE SCATTER AT A TIME, CARRYING EVERYTHING QUEUED. A scatter has no roster,
// so it waits out its deadline whatever happens — and the pushes made while it
// waits leave together in the next one rather than each paying the wait. What
// a push costs the person is therefore network latency: every peer's answerer
// has the request as soon as the broker does, and delivers before the asker's
// wait is over.
func (h *ChatHub) personLoop(ctx context.Context, stop <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case first := <-h.personOut:
			batch := []chatPersonPush{first}
		drain:
			for len(batch) < chatPersonQueue {
				select {
				case next := <-h.personOut:
					batch = append(batch, next)
				default:
					break drain
				}
			}
			h.askPerson(ctx, batch)
		}
	}
}

// askPerson scatters one batch of person pushes to every peer.
func (h *ChatHub) askPerson(ctx context.Context, batch []chatPersonPush) {
	peers := h.peers()
	if peers == nil {
		return
	}
	request, err := json.Marshal(chatPersonRequest{
		Version: chatPersonProtocol, Origin: h.origin, Pushes: batch,
	})
	if err != nil {
		log.DebugContext(ctx, "chat_person_encode_failed", "error", err)
		return
	}
	askCtx, cancel := context.WithTimeout(ctx, chatPersonSend)
	defer cancel()
	// NOTHING IS WAITED FOR, AND WANT ZERO SAYS SO: every answerer answers
	// nothing ([errChatPersonDelivered]), so this returns at the deadline,
	// and the deadline bounds only how soon the next batch may leave —
	// which is why it is [chatPersonSend] and not the presence probe's.
	if _, err := peers.Ask(askCtx, topics.ChatPerson, request, 0); err != nil {
		log.DebugContext(ctx, "chat_person_ask_failed", "error", err)
	}
}

// servePerson makes this node a deliverer of the fleet's person pushes.
func (h *ChatHub) servePerson(ctx context.Context) queue.Unsubscribe {
	peers := h.peers()
	if peers == nil {
		return nil
	}
	unsubscribe, err := peers.Serve(ctx, topics.ChatPerson,
		func(_ context.Context, raw []byte) ([]byte, error) {
			return nil, h.answerPerson(raw)
		})
	if err != nil {
		// NOT FATAL, AND SAID ONCE, for [ChatHub.serve]'s reason.
		h.unservedPerson.Do(func() {
			log.Warn("chat_person_serve_failed", "error", err,
				"hint", "people's read state moved on other nodes will not reach "+
					"their screens attached here until those screens read the "+
					"rail again; it keeps retrying")
		})
		return nil
	}
	return unsubscribe
}

// answerPerson delivers one peer's person pushes to this node's own sockets.
//
// IT ALWAYS ANSWERS NOTHING, because there is nothing to answer: a push is
// news, and the asker is waiting for nobody. Its own hub's push is skipped —
// [ChatHub.PushTo] delivered it here already — and a payload this build does
// not speak is not delivered at all.
//
// NOTHING OF THE ASKER'S REQUEST REACHES THE DELIVERY, for [ChatHub.PushTo]'s
// reason: whose socket is whose is this hub's own answer.
func (h *ChatHub) answerPerson(raw []byte) error {
	var req chatPersonRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	if req.Version != chatPersonProtocol {
		return errChatPersonUnreadable
	}
	if req.Origin == h.origin {
		return errChatPersonOwn
	}
	for _, push := range req.Pushes {
		if push.Handle == "" || push.Kind == "" {
			continue
		}
		h.deliverTo(push.Handle, Push(push.Kind, push.Data, push.At))
	}
	return errChatPersonDelivered
}

// errChat is a tiny error type, so the two silences above are values rather
// than fmt calls on a path that runs per probe per node.
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
// company's `chat.native.typing_status` is the driver's mode, and
// [chat.AddressRule] is what `addressed` means.

// The hub is both halves the engine is handed for its own chat: the applier's
// observer and the working indicator's poster. Held here, where the methods
// are, so a signature drifting from either seam fails this package's build
// rather than the first consumer that happens to pass the hub along.
var (
	_ chat.Observer       = (*ChatHub)(nil)
	_ notify.StatusPoster = (*ChatHub)(nil)
)

// StatusBackend implements [notify.StatusPoster]: the transport name the chat
// parser stamps on every trigger it produces.
//
// [chat.Source] rather than a literal, because the driver matches this against
// the trigger's own `transport` key — a second spelling would raise no
// indicator at all, silently, on every native turn.
func (h *ChatHub) StatusBackend() string { return chat.Source }

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
// [chat.AddressRule] and not a rule of this package's, because the same value
// decides three things that must never disagree — the prompt that tells an
// agent it was asked, the delivery gate that refuses to let an addressed turn
// end in silence, and this indicator. A spinner over a turn that is allowed to
// end in silence, and a silence after a spinner, are one bug.
func (h *ChatHub) AddressRule() notify.AddressRule { return chat.AddressRule() }

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
