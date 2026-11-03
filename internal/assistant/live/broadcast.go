package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/signedask"
)

// THE FLEET HALF OF A TURN'S STREAM: the frames a node appends, told to every
// node following the turn, and the two instructions a node sends the one
// running it (the frames its client missed, and a Stop).
//
// # Encrypted, then sealed
//
// A frame carries what the person asked and what their reads returned, the
// same prose and cards internal/assistantdomain encrypts at rest (ADR-0053).
// The broker authenticates nothing (ADR-0031), so a frame in the clear would
// make reaching the broker a way to read every person's conversation; the
// broker's private ports are defence in depth, never the control. So every
// frame and every replayed page is ENCRYPTED with the fleet keyring's AEAD,
// the one internal/secrets cipher the conversation is sealed with at rest,
// under associated data that names the turn ([FrameAAD], [ReplayAAD]): a
// frame captured from one turn and broadcast on another's channel does not
// open there. The ciphertext then travels inside the signed one-way frame
// presence uses (internal/signedask), in a key domain of its own
// ([FrameChannel]), whose expiry bounds how long a captured copy is believed
// and whose dispositions (not the fleet's, a key this node does not hold, a
// build it cannot read, expired) are the fleet's one vocabulary. A frame the
// fleet did not seal, or that does not open under the turn it arrived for, is
// dropped.
//
// # Lossy broadcast, exact stream
//
// A broadcast is at most once, and a transcript rebuilt from deltas cannot
// survive a hole, so the broadcast is never what a follower trusts for
// completeness. It numbers what it hears against the last seq it wrote, and
// any gap (a frame lost, a listener that fell behind) is filled by asking the
// running node for every frame after it ([ReplayChannel]), which answers from
// its frame log exactly as it would its own client: a snapshot of the
// transcript where the log no longer holds the frames, then what it holds. A
// quiet stream asks again on every keepalive, so a lost final frame costs one
// keepalive interval and never a stream that hangs.

// The key domains, each the stem signedask derives its keys from: their own,
// so no state-log record, no presence frame and no other channel's envelope
// verifies as one of these, and none of these as any of theirs.
const (
	// FrameChannel is the one-way channel a turn's live frames cross on.
	FrameChannel = "assistant.frame"
	// ReplayChannel is the instruction channel a node asks the running node
	// for the frames its client missed on.
	ReplayChannel = "assistant.replay"
	// StopChannel is the instruction channel a Stop pressed on another node
	// reaches the running node on.
	StopChannel = "assistant.stop"
)

// The fleet half's bounds.
const (
	// FleetWireVersion is the shape this build writes on all three
	// channels. Additive only: two builds share them during a rolling
	// upgrade, an unknown field is ignored and an unknown answer is one the
	// asker refuses to act on.
	FleetWireVersion = 1

	// FrameValidity is how long a sealed frame is believed after it was
	// sealed: thirty seconds, twice the keepalive, so a frame delayed in a
	// busy broker still lands, while a captured copy replayed later is
	// refused (and one replayed inside it is a seq the follower already
	// wrote, so it shows nothing twice).
	FrameValidity = 30 * time.Second

	// DefaultAskBudget is how long a node waits for the running node to
	// answer a replay or a Stop: the fleet's read budget, the bound every
	// scatter a person waits on is held to (internal/eventfan).
	DefaultAskBudget = 2 * time.Second

	// ReplayPageBytes is the most encoded frames one replay answer carries:
	// the frame log's own bound, so a whole log is one page at most, and
	// with the AEAD's base64 and the seal still under the queue's largest
	// message ([queue.MaxPayloadBytes]). One frame larger than it travels
	// alone.
	ReplayPageBytes = FrameLogBytes

	// PublishBacklog is how many frames this node holds for the broker
	// before it drops one: a thousand and twenty-four, a few seconds of
	// every turn a busy node runs at the 200 ms delta interval. A dropped
	// broadcast is a gap its followers fill by replay, so the backlog
	// bounds memory and never correctness.
	PublishBacklog = 2 * SubscriberFrames
)

// ErrNotRunning reports a turn the asked node is not running and holds no
// frames for: its runner restarted, or it was never there.
var ErrNotRunning = errors.New("live: the turn is not running on this node")

// ErrNotYours reports a turn that is not the asking person's. The HTTP surface
// answers it as not found, never as forbidden, so it tells nobody the turn
// exists.
var ErrNotYours = errors.New("live: the turn is not the asking person's")

// FrameAAD is the associated data a turn's broadcast frames are encrypted
// under: the channel and the turn, so a frame opens on its own turn only.
func FrameAAD(turn uuid.UUID) string { return FrameChannel + "/" + turn.String() }

// ReplayAAD is the associated data one replay answer is encrypted under: the
// channel, the turn and the request it answers, so a page opens only as the
// answer to the request that asked for it.
func ReplayAAD(turn uuid.UUID, request string) string {
	return ReplayChannel + "/" + turn.String() + "/" + request
}

// Sealed is one broadcast frame as it crosses the broker: whose it is in the
// clear (the node, the turn), the frame itself encrypted, and the instant it
// stops being believed, all under the seal.
//
// EXPORTED ONLY SO THE ENGINE CAN BUILD THE ONE-WAY CHANNEL it travels in
// (`signedask.NewOneWay[live.Sealed](live.FrameChannel, ring)`); nothing but
// this package builds or reads one.
type Sealed struct {
	V     int       `json:"v"`
	Node  string    `json:"node"`
	Turn  uuid.UUID `json:"turn"`
	Frame string    `json:"frame"`
	Until time.Time `json:"until"`
}

// Expires is the instant [signedask.OneWay] judges the frame by.
func (s Sealed) Expires() time.Time { return s.Until }

// Cipher is the fleet keyring's AEAD as this package uses it: internal/secrets'
// Cipher as written.
type Cipher interface {
	Encrypt(plaintext, aad string) (string, error)
	Decrypt(token, aad string) (string, error)
}

// Broker is the queue's ephemeral verbs, as the fleet half uses them.
type Broker interface {
	Broadcast(ctx context.Context, subject string, payload []byte) error
	Listen(ctx context.Context, pattern string, h queue.ListenFunc) (queue.Unsubscribe, error)
	AskEach(ctx context.Context, subject string, request []byte, each func(reply []byte) bool) error
	Serve(ctx context.Context, subject string, answers int, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// Roster is the fleet's membership: the presence leases (internal/coord,
// ListLive's node class).
type Roster interface {
	// Present says whether a node is still in the fleet. (false, nil) is a
	// node definitively gone; an error is a store that could not say, which
	// is never read as gone.
	Present(ctx context.Context, node string) (bool, error)
	// Nodes is every node in the fleet now: how many answers a probe waits
	// for.
	Nodes(ctx context.Context) ([]string, error)
}

// Server is the running node's half: the service that holds its turns. The
// assistant's service implements it.
type Server interface {
	// ReplayHere is every frame of owner's turn after seq after that this
	// node can answer, with a snapshot of the transcript first where its
	// log no longer holds them, and whether the segment has ended.
	// [ErrNotRunning] for a turn this node holds no log for, and
	// [ErrNotYours] for somebody else's.
	ReplayHere(ctx context.Context, owner, turn uuid.UUID, after uint64) ([]wire.Frame, bool, error)
	// StopHere stops owner's turn running on this node and answers the
	// state it ended in. [ErrNotRunning] and [ErrNotYours] as above.
	StopHere(ctx context.Context, owner, turn uuid.UUID) (wire.TurnState, error)
	// LastHere is the newest seq this node numbered for owner's turn
	// ([Hub.Known]). [ErrNotRunning] for a turn it knows nothing of, and
	// [ErrNotYours] as above.
	LastHere(ctx context.Context, owner, turn uuid.UUID) (uint64, error)
}

// Writer is a live stream as a follower writes to it: the assistant's
// FrameWriter.
type Writer interface {
	Frame(f wire.Frame) error
	Keepalive() error
}

// Reach is what following or stopping a turn on another node found.
type Reach string

const (
	// ReachServed is the running node answering.
	ReachServed Reach = "served"
	// ReachUnreachable is no answer from a node the fleet still counts, or
	// cannot say it does not: the stream answers what the transcript holds
	// and the client asks again.
	ReachUnreachable Reach = "unreachable"
	// ReachGone is a runner that is gone: its node said it is not running
	// the turn, or its node has left the fleet. The turn died with it and is
	// closed interrupted.
	ReachGone Reach = "gone"
)

// Reaches are every [Reach].
var Reaches = []Reach{ReachServed, ReachUnreachable, ReachGone}

// Valid reports whether r is a reach this build knows.
func (r Reach) Valid() bool { return slices.Contains(Reaches, r) }

// FleetOptions is what a [Fleet] is built from. Every field but AskBudget and
// Now is REQUIRED.
type FleetOptions struct {
	// Node is this node's id: what a request names to reach it, and what
	// its frames say they came from.
	Node   string
	Broker Broker
	Roster Roster
	// Cipher encrypts every frame and replayed page (internal/secrets).
	Cipher Cipher
	// Frames, Replays and Stops are the three channels, built by the
	// engine over the fleet keyring in [FrameChannel], [ReplayChannel] and
	// [StopChannel].
	Frames  *signedask.OneWay[Sealed]
	Replays *signedask.Channel[ReplayRequest, ReplayReply]
	Stops   *signedask.Channel[StopRequest, StopReply]
	// AskBudget bounds one ask; zero is [DefaultAskBudget].
	AskBudget time.Duration
	Now       func() time.Time
}

// Fleet is one node's half of the assistant's fleet stream: it tells the fleet
// every frame this node appends, follows and stops turns running elsewhere,
// and, once [Fleet.Start] hands it this node's [Server], answers its peers.
type Fleet struct {
	node      string
	broker    Broker
	roster    Roster
	cipher    Cipher
	frames    *signedask.OneWay[Sealed]
	replays   *signedask.Channel[ReplayRequest, ReplayReply]
	stops     *signedask.Channel[StopRequest, StopReply]
	askBudget time.Duration
	now       func() time.Time
	log       *slog.Logger

	// out is the publish backlog the sender drains in order.
	out chan wire.Frame
	// replayTaken and stopTaken are the request ids this node has taken.
	replayTaken, stopTaken *signedask.Guard

	mu      sync.Mutex
	server  Server
	stopped []queue.Unsubscribe
	cancel  context.CancelFunc
	sending sync.WaitGroup
}

// NewFleet builds a node's fleet half, refusing a missing field by name.
func NewFleet(o FleetOptions) (*Fleet, error) {
	var missing []string
	for name, absent := range map[string]bool{
		"Node": strings.TrimSpace(o.Node) == "", "Broker": o.Broker == nil,
		"Roster": o.Roster == nil, "Cipher": o.Cipher == nil, "Frames": o.Frames == nil,
		"Replays": o.Replays == nil, "Stops": o.Stops == nil,
	} {
		if absent {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("live: FleetOptions.%s required: a frame names a person "+
			"and crosses a broker that authenticates nothing", strings.Join(missing,
			", FleetOptions."))
	}
	if _, err := o.Cipher.Encrypt("", FrameChannel+"/probe"); err != nil {
		return nil, fmt.Errorf("live: the keyring cannot encrypt a frame: %w", err)
	}
	budget := o.AskBudget
	if budget <= 0 {
		budget = DefaultAskBudget
	}
	now := o.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Fleet{node: o.Node, broker: o.Broker, roster: o.Roster, cipher: o.Cipher,
		frames: o.Frames, replays: o.Replays, stops: o.Stops, askBudget: budget, now: now,
		log: logging.Get("assistant.live"), out: make(chan wire.Frame, PublishBacklog),
		replayTaken: signedask.NewGuard(), stopTaken: signedask.NewGuard()}, nil
}

// Start makes this node an answerer of its peers' replays and Stops for the
// turns server holds, and starts the sender that tells the fleet this node's
// frames. Once.
func (f *Fleet) Start(ctx context.Context, server Server) error {
	if server == nil {
		return errors.New("live: a fleet answers for a server, and none was given")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.server != nil {
		return errors.New("live: the fleet half is already started")
	}
	for _, s := range []struct {
		subject string
		answer  queue.AnswerFunc
	}{
		{topics.AssistantReplay, f.answerReplay},
		{topics.AssistantStop, f.answerStop},
	} {
		// WORK DONE NOW: a replay reads this node's ring and a stop
		// cancels a run, neither waiting in a queue of its own.
		stop, err := f.broker.Serve(ctx, s.subject, queue.ConcurrentAnswers, s.answer)
		if err != nil {
			// The answerers already serving are withdrawn, and a failure to
			// withdraw one is reported beside the failure to serve.
			return errors.Join(fmt.Errorf("live: serve %s: %w", s.subject, err),
				f.withdrawLocked(context.WithoutCancel(ctx)))
		}
		f.stopped = append(f.stopped, stop)
	}
	f.server = server
	sendCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	f.cancel = cancel
	f.sending.Add(1)
	go f.send(sendCtx)
	return nil
}

// Close withdraws this node as an answerer and stops its sender, before the
// broker closes. The frames still in the backlog are not sent: every follower
// of them falls back to the transcript.
func (f *Fleet) Close(ctx context.Context) error {
	f.mu.Lock()
	cancel := f.cancel
	err := f.withdrawLocked(ctx)
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	f.sending.Wait()
	return err
}

// withdrawLocked unsubscribes every answerer, under f.mu.
func (f *Fleet) withdrawLocked(ctx context.Context) error {
	var errs []error
	for _, stop := range f.stopped {
		if err := stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	f.stopped = nil
	return errors.Join(errs...)
}

// Publish hands one frame this node appended to the sender, in the order its
// log numbered it. It never blocks ([Tap]): past [PublishBacklog] the frame is
// dropped and logged, and its followers fill the gap by replay.
func (f *Fleet) Publish(fr wire.Frame) {
	select {
	case f.out <- fr:
	default:
		f.log.Warn("assistant_frame_not_broadcast", "turn", fr.TurnID, "seq", fr.Seq,
			"detail", "the broadcast backlog is full; followers fill the gap by replay")
	}
}

// send drains the backlog onto the broker until ctx ends.
func (f *Fleet) send(ctx context.Context) {
	defer f.sending.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case fr := <-f.out:
			sealed, err := f.sealFrame(fr)
			if err != nil {
				f.log.Error("assistant_frame_not_sealed", "turn", fr.TurnID, "seq", fr.Seq,
					"error", err)
				continue
			}
			if err := f.broker.Broadcast(ctx, topics.AssistantTurnSubject(fr.TurnID),
				sealed); err != nil {
				f.log.Warn("assistant_frame_not_broadcast", "turn", fr.TurnID, "seq", fr.Seq,
					"error", err, "detail", "followers fill the gap by replay")
			}
		}
	}
}

// sealFrame encrypts one frame under its turn and seals it.
func (f *Fleet) sealFrame(fr wire.Frame) ([]byte, error) {
	raw, err := json.Marshal(fr)
	if err != nil {
		return nil, fmt.Errorf("encode the frame: %w", err)
	}
	enc, err := f.cipher.Encrypt(string(raw), FrameAAD(fr.TurnID))
	if err != nil {
		return nil, fmt.Errorf("encrypt the frame: %w", err)
	}
	return f.frames.Seal(Sealed{V: FleetWireVersion, Node: f.node, Turn: fr.TurnID,
		Frame: enc, Until: f.now().Add(FrameValidity)})
}

// openFrame verifies, decrypts and checks one broadcast frame heard for turn,
// reporting false for one that must not be shown: not sealed by this fleet,
// expired, about another turn, or not opening under this turn's associated
// data. Each refusal is logged; nothing about a refused frame is believed.
func (f *Fleet) openFrame(ctx context.Context, payload []byte, turn uuid.UUID) (wire.Frame, bool) {
	s, disposition, err := f.frames.Open(payload, f.now())
	if err != nil {
		level := slog.LevelWarn
		if disposition == signedask.Tampered {
			// NOT THE FLEET'S: whoever wrote it reached the broker
			// and is not the fleet.
			level = slog.LevelError
		}
		f.log.Log(ctx, level, "assistant_frame_refused", "turn", turn,
			"disposition", string(disposition), "error", err)
		return wire.Frame{}, false
	}
	if s.Turn != turn {
		f.log.ErrorContext(ctx, "assistant_frame_refused", "turn", turn,
			"disposition", "another_turn",
			"detail", "a frame sealed for another turn was heard on this turn's channel")
		return wire.Frame{}, false
	}
	plain, err := f.cipher.Decrypt(s.Frame, FrameAAD(turn))
	if err != nil {
		f.log.ErrorContext(ctx, "assistant_frame_refused", "turn", turn,
			"disposition", "unopened", "error", err)
		return wire.Frame{}, false
	}
	var fr wire.Frame
	if err := json.Unmarshal([]byte(plain), &fr); err != nil {
		f.log.WarnContext(ctx, "assistant_frame_refused", "turn", turn,
			"disposition", "undecodable", "error", err)
		return wire.Frame{}, false
	}
	if fr.TurnID != turn || fr.Validate() != nil {
		f.log.ErrorContext(ctx, "assistant_frame_refused", "turn", turn,
			"disposition", "invalid",
			"detail", "the frame inside the seal does not belong to this turn")
		return wire.Frame{}, false
	}
	return fr, true
}

// Follow streams owner's turn, running on node, from after seq after to w,
// until its segment ends, the runner stops answering or ctx ends. It answers
// [ReachServed] once the runner has answered (whatever happens after: a
// stream that ends early is resumed by its client), and otherwise what asking
// found, having written nothing. A segment that had already ended with nothing
// after seq after answers [ErrCaughtUp], having written nothing.
func (f *Fleet) Follow(ctx context.Context, owner, turn uuid.UUID, node string, after uint64,
	w Writer) (Reach, error) {

	heard := make(chan wire.Frame, SubscriberFrames)
	var lostMu sync.Mutex
	lost := false
	// LISTEN FIRST, then ask: a frame appended between the two is in the
	// replay or heard here, never in neither.
	unsub, err := f.broker.Listen(ctx, topics.AssistantTurnSubject(turn),
		func(_ string, payload []byte) {
			fr, ok := f.openFrame(ctx, payload, turn)
			if !ok {
				return
			}
			select {
			case heard <- fr:
			default:
				lostMu.Lock()
				lost = true
				lostMu.Unlock()
			}
		})
	if err != nil {
		return "", fmt.Errorf("live: listen to turn %s: %w", turn, err)
	}
	defer func() {
		if unsubErr := unsub(context.WithoutCancel(ctx)); unsubErr != nil {
			f.log.Warn("assistant_follow_not_withdrawn", "turn", turn, "error", unsubErr)
		}
	}()

	last := after
	reach, ended, err := f.catchUp(ctx, owner, turn, node, &last, w)
	switch {
	case err != nil || reach != ReachServed:
		return reach, err
	case ended && last == after:
		return ReachServed, ErrCaughtUp
	case ended:
		return ReachServed, nil
	}
	keepalive := time.NewTicker(wire.KeepaliveSeconds * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return ReachServed, nil
		case <-keepalive.C:
			if err := w.Keepalive(); err != nil {
				return ReachServed, err
			}
			// A QUIET STREAM ASKS AGAIN: a lost final frame would
			// otherwise hold the stream open until the client gave up.
			if reach, ended, err := f.catchUp(ctx, owner, turn, node, &last, w); err != nil ||
				reach != ReachServed || ended {
				return ReachServed, err
			}
		case fr := <-heard:
			lostMu.Lock()
			missed := lost
			lost = false
			lostMu.Unlock()
			if fr.Seq <= last && !missed {
				continue
			}
			if missed || fr.Seq > last+1 {
				if reach, ended, err := f.catchUp(ctx, owner, turn, node, &last, w); err != nil ||
					reach != ReachServed || ended {
					return ReachServed, err
				}
				continue
			}
			if err := w.Frame(fr); err != nil {
				return ReachServed, err
			}
			last = fr.Seq
			if fr.Kind == wire.EventDone {
				return ReachServed, nil
			}
		}
	}
}

// catchUp asks the runner for every frame after *last, page by page, writes
// those it has not written, and reports whether the segment has ended.
func (f *Fleet) catchUp(ctx context.Context, owner, turn uuid.UUID, node string, last *uint64,
	w Writer) (Reach, bool, error) {

	for {
		page, reach, err := f.askReplay(ctx, owner, turn, node, *last)
		if err != nil || reach != ReachServed {
			return reach, false, err
		}
		for _, fr := range page.Frames {
			if fr.Seq <= *last {
				continue
			}
			if err := w.Frame(fr); err != nil {
				return ReachServed, false, err
			}
			*last = fr.Seq
		}
		if !page.More {
			return ReachServed, page.Ended, nil
		}
	}
}

// Stop asks node to stop owner's turn, answering the state it ended in.
func (f *Fleet) Stop(ctx context.Context, owner, turn uuid.UUID, node string) (wire.TurnState,
	Reach, error) {

	req := StopRequest{V: FleetWireVersion, Node: node, Owner: owner, Turn: turn}
	reply, answered, err := ask(ctx, f, f.stops, topics.AssistantStop, &req)
	if err != nil {
		return "", "", err
	}
	if !answered {
		return "", f.absent(ctx, node), nil
	}
	switch reply.Answer {
	case AnswerDone:
		if !reply.State.Ended() {
			return "", "", fmt.Errorf("live: node %s stopped turn %s and answered state %q, "+
				"which is not an end", node, turn, reply.State)
		}
		return reply.State, ReachServed, nil
	case AnswerNotRunning:
		return "", ReachGone, nil
	case AnswerNotYours:
		return "", "", ErrNotYours
	case AnswerUnverified:
		return "", "", fmt.Errorf("live: node %s could not verify the stop, so it stopped "+
			"nothing: %s", node, reply.Unverified)
	}
	return "", "", fmt.Errorf("live: node %s answered the stop %q, which this build does "+
		"not know", node, reply.Answer)
}

// absent is what a request nobody answered found: a runner whose node has
// left the fleet is gone, and one the roster still counts, or cannot say it
// does not, is unreachable. A roster that cannot answer is logged rather than
// returned: the turn is left open, never ended on a guess.
func (f *Fleet) absent(ctx context.Context, node string) Reach {
	present, err := f.roster.Present(ctx, node)
	if err != nil {
		f.log.WarnContext(ctx, "assistant_runner_unknown", "node", node, "error", err,
			"detail", "the runner did not answer and the fleet's roster could not say "+
				"whether its node is still present; the turn is left open")
		return ReachUnreachable
	}
	if present {
		return ReachUnreachable
	}
	return ReachGone
}
