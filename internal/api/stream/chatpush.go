package stream

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

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
// # The identity is the SOCKET'S and is fixed at accept
//
// A query frame may carry a token that upgrades that one question
// ([runQuery]). A push stream may not: it is long-lived, frames have already
// gone out under the identity the socket was accepted with, and an identity
// that could move mid-stream would make "what has this socket been shown"
// unanswerable. A socket whose credential resolves to no seat watches nothing
// — reads included — which is the same rule the REST and MCP surfaces apply,
// for the same reason.
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
// applied, and none of it may ever become a record — a keystroke that landed
// on the chat log would be a row every node applies and every backup carries,
// for ever. It rides the queue's EPHEMERAL scatter on [topics.ChatPresence],
// which is deliberately outside the chat log's own wildcard.
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
	KindChatPosted = "chat_posted"

	// KindChatEdited is a message whose body was rewritten in place.
	KindChatEdited = "chat_edited"

	// KindChatDeleted is a tombstone: the row survives with its body
	// blanked, so a thread does not lose the message it hangs off.
	KindChatDeleted = "chat_deleted"

	// KindChatReacted is one emoji put on or taken off a message.
	KindChatReacted = "chat_reacted"

	// KindChatRoomChanged is a room's own state moving: created, patched,
	// archived, erased or pruned.
	//
	// ONE KIND FOR ALL FIVE, because what a client does with them is one
	// thing — read the room again. An erase and a prune remove messages
	// and the record does not carry which, so a frame that tried to name
	// them would be a frame a client could not act on anyway.
	KindChatRoomChanged = "chat_room_changed"

	// KindChatMembershipChanged is a room's membership set being replaced,
	// which includes somebody joining or leaving.
	KindChatMembershipChanged = "chat_membership_changed"

	// KindChatCursorMoved is one person's own read state moving — where
	// they have read to, what they have muted, whether they are in
	// do-not-disturb.
	//
	// IT REACHES THAT PERSON'S OWN SOCKETS AND NOBODY ELSE'S. A cursor is
	// a fact about one reader's attention: it is not on the log, nobody
	// replays it, and a second tab is the only thing in the world that
	// wants it.
	KindChatCursorMoved = "chat_cursor_moved"

	// KindChatPresence is who is looking at, typing in, or working on a
	// message in the rooms this socket said it is watching.
	//
	// NOT PAST TENSE, because it is not a thing that happened: it is the
	// current answer, whole, for the rooms this socket asked about. A
	// delta would need a starting point a reconnecting tab does not have.
	KindChatPresence = "chat_presence"
)

// ChatPushKinds is every kind this arm emits.
//
// Stated as a list so a gate can hold it against the read registry's own
// names: see the kinds above for why the two namespaces may not overlap.
func ChatPushKinds() []string {
	return []string{
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
// watching. An older client that never sends one is a tab that sees no
// presence and is seen by nobody, which is exactly what it was before.
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
	// here costs nothing anybody can see.
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
// Declared by the consumer and kept to the one method this arm calls:
// [chat.Reader.Channel] refuses a room the viewer may not read as NOT FOUND,
// and hands back the room and this viewer's membership for the rooms it does
// serve — which is exactly the pair [chat.Visible] decides from.
type ChatRooms interface {
	Channel(ctx context.Context, viewer, channelID string,
		fresh statelog.Freshness) (chat.ChannelDetail, error)
}

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

	// Status is the phrase to render. Empty is a backend that raised the
	// indicator with no words, which this one never does — see
	// [ChatHub.SupportsStatusText] — but a peer running an older build
	// may.
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
// the RECORD already carries them for the wake: an excerpt is at most
// [chat.MaxExcerpt] bytes of what a card shows, and the mentions are how a
// tab raises its own badge without a fetch. Both are absent on a record that
// woke nobody — an import, a reaction, a prune — so neither is a substitute
// for reading the room.
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
	// not looking at. Empty for a direct conversation, which has no name.
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
	Viewer func(operatorID string) string

	// Peers carries presence to the rest of the fleet. Nil keeps presence
	// local to this node's own sockets, which is honest and is the whole
	// truth on a single-node deployment.
	Peers func() ChatPeers

	// NodeID names this node in its own presence answer, so an asker can
	// drop the reply to its own scatter rather than merging itself twice.
	NodeID string

	// Now is injectable so a test can pin the typing window.
	Now func() time.Time

	// PresenceInterval and AskBudget override the two cadences above.
	// Injectable for the reason [Options.HealthInterval] is: a suite that
	// waited out the production value would either be slow or assert
	// nothing.
	PresenceInterval time.Duration
	AskBudget        time.Duration
}

// chatWatcher is one socket watching chat.
type chatWatcher struct {
	client *Client

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

	started bool
	stop    chan struct{}
	done    chan struct{}

	// unwired says the missing-reader warning has been given. Once, not
	// per record: this is the busiest log in the engine. unserved is the
	// same for the presence subscription, which is retried per tick.
	unwired  sync.Once
	unserved sync.Once

	// dropped counts batches the fan-out could not keep up with, for the
	// line a shutdown logs.
	dropped int
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
		watchers: map[*Client]*chatWatcher{},
		working:  map[chatStatusKey]ChatWorking{},
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

// Start runs the fan-out and the presence loop until ctx is done or [Stop] is
// called.
//
// TWO GOROUTINES, not one. A presence probe waits out its own deadline by
// construction (a scatter has no roster, so there is no reply count that ends
// it early), and a live message queued behind that wait would be delivered
// three quarters of a second after it was said.
//
// Idempotent: a second call while one is running is a no-op rather than a
// second pair of loops, which would probe twice per interval and leave
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

	go func() {
		defer close(done)
		loops.Wait()
	}()
}

// Stop ends both loops and waits for them.
func (h *ChatHub) Stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	stop, done, running := h.stop, h.done, h.started
	h.started = false
	h.stop, h.done = nil, nil
	dropped := h.dropped
	h.mu.Unlock()
	if !running {
		return
	}
	close(stop)
	<-done
	if dropped > 0 {
		log.Info("chat_live_batches_dropped", "batches", dropped,
			"hint", "the live fan-out fell behind the applier; the rows are "+
				"unaffected and a screen that missed a frame refetches")
	}
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
		h.mu.Lock()
		h.dropped++
		h.mu.Unlock()
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
func (h *ChatHub) fan(ctx context.Context, batch []chat.Applied) {
	watchers := h.snapshot()
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
	decided := map[chatStatusKey]bool{}
	for _, applied := range batch {
		kind, push := chatPushKind(applied.Op)
		if !push {
			continue
		}
		frame := chatChangeOf(applied)
		for _, w := range watchers {
			if !h.maySee(ctx, rooms, decided, w.viewer, applied.ChannelID) {
				continue
			}
			w.client.send(Push(kind, frame, applied.At))
		}
	}
}

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
func (h *ChatHub) maySee(ctx context.Context, rooms ChatRooms,
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
	// STALE IS THE HONEST LEVEL HERE. This runs after the applier's own
	// commit, so this node's rows already hold the record being announced
	// — and a linearizable read would put a quorum barrier append on the
	// broker for every viewer of every message.
	detail, err := rooms.Channel(readCtx, viewer, channelID,
		statelog.Freshness{Level: statelog.ReadStale})
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
func chatPushKind(op chat.OpKind) (string, bool) {
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

// PushTo sends one frame to every socket belonging to one person.
//
// THE ONE PUSH HERE THAT IS NOT FILTERED BY A ROOM, because it is not about a
// room: a read cursor is a fact about one reader's attention, and the only
// thing in the world that wants it is that same person's other tabs. The
// recipient set is therefore the identity itself.
func (h *ChatHub) PushTo(handle, kind string, data any) {
	if h == nil || handle == "" {
		return
	}
	env := Push(kind, data, h.now())
	for _, w := range h.snapshot() {
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
func (h *ChatHub) join(client *Client, operatorID string) bool {
	if h == nil || client == nil || h.opts.Viewer == nil {
		return false
	}
	viewer := strings.TrimSpace(h.opts.Viewer(operatorID))
	if viewer == "" {
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

// snapshot copies the watcher set, so the fan-out never holds the lock across
// a store read or a send.
func (h *ChatHub) snapshot() []chatWatcher {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]chatWatcher, 0, len(h.watchers))
	for _, w := range h.watchers {
		out = append(out, *w)
	}
	return out
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
// IT ALSO OWNS THIS NODE'S ANSWERING HALF, and it is attempted on every tick
// until it takes rather than once at start-up. A hub is built before the
// broker client exists (see [ChatHubOptions]), so a subscription made once
// would be made against whatever was wired at that instant — and if that was
// nothing, this node would answer nobody's probe for the rest of its life,
// silently, while continuing to probe everybody else's.
func (h *ChatHub) presenceLoop(ctx context.Context, stop <-chan struct{}) {
	ticker := time.NewTicker(h.opts.PresenceInterval)
	defer ticker.Stop()
	var unsubscribe queue.Unsubscribe
	defer func() {
		if unsubscribe != nil {
			// WITHOUT THE CALLER'S CANCELLATION, because this
			// teardown is usually undoing the shutdown's own
			// context, and a cleanup that inherits a dead context
			// does nothing at all.
			_ = unsubscribe(context.WithoutCancel(ctx))
		}
	}()
	serve := func() {
		if unsubscribe == nil {
			unsubscribe = h.serve(ctx)
		}
	}
	serve()

	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			serve()
			last = h.sweep(ctx)
		case <-h.wake:
			serve()
			// THE FLOOR IS ENFORCED HERE rather than by the sender,
			// because the sender is a keystroke: a client that
			// signals per character must cost one scatter, not one
			// per character. The tick above is what picks up
			// whatever this skipped.
			if gap := h.now().Sub(last); gap < ChatPresenceGap {
				continue
			}
			last = h.sweep(ctx)
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
	for _, w := range h.snapshot() {
		if w.channel == "" || !h.maySee(ctx, rooms, decided, w.viewer, w.channel) {
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
			// [ChatHub.localPresence] already has it, first-hand
			// and without the round trip.
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
	for _, w := range h.snapshot() {
		if w.channel == "" || !slices.Contains(req.Channels, w.channel) {
			continue
		}
		if !h.maySee(ctx, reader, decided, w.viewer, w.channel) {
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

// The two silences, named so the reason a node did not answer is readable
// where it is decided rather than inferred from a nil.
var (
	errChatPresenceUnreadable = errChat("a presence probe this build does not speak")
	errChatPresenceEmpty      = errChat("nobody here is in those rooms")
)

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
