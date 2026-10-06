package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// One websocket per agent seat.
//
// # Why a socket at all
//
// Mattermost has no usable inbound webhook. Outgoing hooks fire only in
// public channels and carry no thread id, no channel type and no mention
// list — none of which the routing model can do without. So the engine
// connects as each bot and listens.
//
// Each post is republished onto the STANDARD raw-webhook envelope, so
// everything downstream — dedupe, the parser seam, the guards, the valve —
// stays webhook-shaped and this backend needs no special case anywhere else.
// Only the fact that it arrived over a socket is different, and only here.
//
// # Reconnects are the hard part
//
// Mattermost replays nothing. A connection that drops and comes back has
// simply missed whatever happened in between, with no cursor, sequence gap
// or resume token to detect it with. So each seat records the newest post it
// has seen and, on reconnect, re-reads every channel it belongs to and
// replays the gap in order.
//
// The window is BOUNDED, because the purpose is to cover a blip and not to
// catch up after an outage: every replayed message costs a full agent turn,
// so an hour-long gap replayed in full would be both expensive and wrong —
// those conversations have moved on and been resolved by people.
//
// # Every node listens, and one delivers
//
// The transport opens EVERY seat's socket on EVERY node, not only the seats a
// node holds: a node that restarts, drains or loses a seat's lease leaves the
// seat heard by every other node meanwhile, so there is no gap for a backfill
// to cover and no handover to get wrong. The price is that each post arrives
// once per node, so a post is CLAIMED fleet-wide before it is published
// (`mattermost|<handle>|<post id>`, [ClaimTTL]): the node that wins publishes
// it and records the delivery, and every other node drops it. A claim store
// that cannot answer fails OPEN — a post published by two nodes is a duplicate
// the second layer collapses, while one suppressed by a store blip is a
// message nobody answers. The second layer is the wake's id, derived from the
// seat and the post ([WakeID]), which the inbox and the fleet completion ledger
// both key on.
//
// A POST IS NOT SPENT UNTIL IT IS QUEUED. A publish that fails releases the
// claim, forgets the post and holds the seat's cursor before it, and the seat
// reconnects and replays from there — so the post is read again, by this node
// or by whichever peer gets there first. It used to move the cursor past the
// post first, which lost it for good.

// ReconnectBackoff is the delay before each reconnect attempt; the last
// value repeats.
//
// CAPPED rather than unbounded-exponential. A seat that cannot connect is a
// configuration problem an operator has to see, and a five-minute ceiling
// keeps the retry visible in the log without hammering a server that is down.
var ReconnectBackoff = []time.Duration{
	time.Second, 2 * time.Second, 5 * time.Second,
	15 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute,
}

// MaxBackfill is how far back a reconnect will replay.
//
// Fifteen minutes covers what backfill exists for — a network blip, a
// rolling restart, a brief engine pause — while refusing to flood the fleet
// after a real outage. A gap wider than this is logged with the amount
// skipped rather than silently truncated, because "we missed two hours" is
// something an operator needs to know and a seat cannot infer.
const MaxBackfill = 15 * time.Minute

// ClaimTTL is how long a post stays claimed by the node that delivered it.
//
// IT MUST OUTLIVE THE REPLAY HORIZON, because a replay is where a duplicate
// comes from: a peer's socket that drops and returns re-reads up to
// [MaxBackfill] behind the moment it reconnects, so a post can be read again
// up to MaxBackfill after it was written — and a claim that lapsed first lets
// that peer publish the post a second time and wake its seat about a message
// already answered. The first claim on a post is taken no earlier than the
// post was written, so MaxBackfill is the floor; on top of it sit the replay's
// own floor falling back to the ENGINE's clock when the server cannot be asked
// (minutes of skew are possible, seconds are usual), the claim judged against
// each claiming node's own clock, and a reconnect that waited out the
// [ReconnectBackoff] ceiling (5 minutes, plus a quarter of jitter) before it
// replayed. TWICE THE WINDOW — thirty minutes — covers all of it with room,
// and is coord.MaxClaimTTL, the longest claim the coordination store keeps.
// Longer buys nothing: a post older than the window is never replayed.
const ClaimTTL = 2 * MaxBackfill

// dedupeRing is how many post ids each seat remembers.
//
// A post can legitimately arrive twice at the reconnect boundary — once
// through the backfill read, once from the live socket that came up
// mid-read — and a duplicate here is a duplicate agent turn. 512 is far more
// than any single backfill window yields while staying trivially small.
const dedupeRing = 512

// PingInterval is how often a seat's socket asks its server to answer.
//
// THIRTY SECONDS. What this detects is a connection that is up as far as TCP
// can tell and dead above it — an L7 half-open, where a load balancer tore
// down the upstream side while still answering keepalives — which nothing
// else in this package can see: coder/websocket answers a server's pings
// internally without returning from Read, so a quiet channel and a dead
// server are identical from here.
//
// The value trades detection latency against traffic. Thirty seconds is well
// inside the idle timeout of every proxy that sits in front of a Mattermost
// instance (60s is the common default, and a ping also keeps that timer from
// firing), and it costs two tiny frames per seat per interval. TCP keepalives
// alone would take about eleven minutes on a genuinely dead path and never
// resolve an L7 half-open at all.
const PingInterval = 30 * time.Second

// PongTimeout is how long a seat waits for the answer.
//
// TEN SECONDS is generous for a frame the server answers from its read loop,
// and short enough that a dead socket is replaced well inside the next ping.
// Being wrong here is cheap and self-correcting: a false positive costs one
// reconnect and a backfill, whose duplicates the dedupe ring absorbs.
const PongTimeout = 10 * time.Second

// reconnectJitter is the proportional spread added to each delay.
//
// Every seat drops at the same instant when the server restarts, so an
// unjittered fleet reconnects in lockstep and hands the recovering server N
// simultaneous authentications and N simultaneous backfills.
const reconnectJitter = 0.25

// Publisher is where a post is republished. The queue.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// Connector opens one seat's socket. Injectable so the fleet's reconnect,
// backfill and dedupe logic can be tested without a server.
type Connector func(ctx context.Context, seat Seat, c *Client) (Socket, error)

// Socket is one live connection.
type Socket interface {
	// Read returns the next event payload. It blocks, and an error ends
	// the connection — the fleet reconnects.
	Read(ctx context.Context) (map[string]any, error)

	// Ping asks the peer to answer, returning when it has or when ctx is
	// done.
	//
	// The seam needs it because NOTHING ELSE can tell a dead server from a
	// quiet one. coder/websocket answers a server's pings itself, without
	// returning from Read, so this side has no signal at all: TCP
	// keepalives rescue a genuinely dead path in ~11 minutes, and an L7
	// half-open — a load balancer that terminated the connection upstream
	// while still answering keepalives — never resolves, leaving the seat
	// deaf indefinitely with no log line.
	Ping(ctx context.Context) error

	Close() error
}

// Claims is the fleet-wide first-claim registry a post is claimed in before it
// is published. Satisfied by coord.Claims.
type Claims interface {
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)
	Release(ctx context.Context, key string) error
}

// FleetOptions configure a [Fleet].
type FleetOptions struct {
	Publisher Publisher

	// Claims is where a post is claimed before it is published, so that of
	// the nodes all reading one seat's socket exactly one delivers each
	// post. REQUIRED: every node opens every seat's socket, and a fleet
	// that deduplicated only within one process would wake a seat once per
	// node for every message.
	Claims Claims

	// Connect opens a socket; nil takes the real websocket dialer.
	Connect Connector

	// PingInterval and PongTimeout override the heartbeat. Zero takes the
	// package defaults.
	PingInterval time.Duration
	PongTimeout  time.Duration

	// Backfill is how far a reconnect replays. Zero takes [MaxBackfill].
	Backfill time.Duration

	// Backoff is the reconnect schedule; empty takes [ReconnectBackoff].
	// A seam rather than a constant because the schedule's floor is a
	// whole second — right against a real server, and long enough that a
	// test of the reconnect PATH would be testing the sleep instead.
	Backoff []time.Duration

	// Now is this node's clock: the instant a claim is taken at, and the
	// backfill floor when the server cannot be asked. The backfill cursor
	// itself comes from the server — see [Client.ServerTime].
	Now func() time.Time
}

// Fleet holds one socket per seat.
type Fleet struct {
	publisher Publisher
	claims    Claims
	connect   Connector
	backfill  time.Duration
	backoff   []time.Duration
	now       func() time.Time

	// pingEvery and pongWithin are the heartbeat's cadence and patience.
	// Fields rather than constants so a test can drive the loop without
	// waiting out a real interval.
	pingEvery  time.Duration
	pongWithin time.Duration

	mu    sync.Mutex
	seats map[string]*seatSocket
}

// NewFleet builds the fleet.
func NewFleet(opts FleetOptions) (*Fleet, error) {
	if opts.Publisher == nil {
		return nil, fmt.Errorf("mattermost: the fleet needs a publisher")
	}
	if opts.Claims == nil {
		return nil, fmt.Errorf("mattermost: the fleet needs a claim registry — every node " +
			"reads every seat's socket, and without one each post wakes its seat once per node")
	}
	f := &Fleet{
		publisher:  opts.Publisher,
		claims:     opts.Claims,
		connect:    opts.Connect,
		backfill:   opts.Backfill,
		backoff:    opts.Backoff,
		now:        opts.Now,
		pingEvery:  opts.PingInterval,
		pongWithin: opts.PongTimeout,
		seats:      map[string]*seatSocket{},
	}
	if f.connect == nil {
		f.connect = dialWebsocket
	}
	if f.backfill <= 0 {
		f.backfill = MaxBackfill
	}
	if len(f.backoff) == 0 {
		f.backoff = ReconnectBackoff
	}
	if f.now == nil {
		f.now = time.Now
	}
	if f.pingEvery <= 0 {
		f.pingEvery = PingInterval
	}
	if f.pongWithin <= 0 {
		f.pongWithin = PongTimeout
	}
	return f, nil
}

// seatSocket is one seat's connection and its cursor.
type seatSocket struct {
	seat   Seat
	client *Client
	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	// cursor is the newest post this seat has seen, on the SERVER's
	// clock. Zero means it has seen nothing, and a reconnect then reads
	// from the connect moment rather than from the epoch.
	cursor time.Time
	// held is the cursor a replay must resume from INSTEAD, when a post at
	// or after it was read and could not be queued: the replay re-reads it.
	// Zero when nothing is owed. See [seatSocket.since].
	held   time.Time
	seen   []string
	seenAt map[string]bool
}

// Add connects a seat, replacing any connection it already had.
//
// Replacing rather than refusing: a live config apply legitimately changes a
// seat's token, and a fleet that kept the old socket would keep listening
// as an identity the operator has revoked.
func (f *Fleet) Add(ctx context.Context, seat Seat, c *Client) error {
	if seat.Handle == "" || c == nil {
		return fmt.Errorf("mattermost: a fleet seat needs a handle and a client")
	}
	f.Remove(seat.Handle)

	s := &seatSocket{seat: seat, client: c, seenAt: map[string]bool{}}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done = cancel, make(chan struct{})

	f.mu.Lock()
	f.seats[seat.Handle] = s
	f.mu.Unlock()

	go func() {
		defer close(s.done)
		f.run(loopCtx, s)
	}()
	log.InfoContext(ctx, "mattermost_seat_attached", "handle", seat.Handle,
		"url", c.URL(), "username", seat.Username)
	return nil
}

// Remove disconnects a seat, waiting for its loop to stop.
func (f *Fleet) Remove(handle string) {
	f.mu.Lock()
	s, ok := f.seats[handle]
	delete(f.seats, handle)
	f.mu.Unlock()
	if !ok {
		return
	}
	s.cancel()
	<-s.done
	log.Info("mattermost_seat_detached", "handle", handle)
}

// Handles lists the attached seats.
func (f *Fleet) Handles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Collect(maps.Keys(f.seats))
	return out
}

// Stop disconnects every seat.
func (f *Fleet) Stop() {
	for _, handle := range f.Handles() {
		f.Remove(handle)
	}
}

// run is one seat's connect / read / reconnect loop.
func (f *Fleet) run(ctx context.Context, s *seatSocket) {
	for attempt := 0; ctx.Err() == nil; attempt++ {
		if attempt > 0 {
			if !sleep(ctx, f.delay(attempt-1)) {
				return
			}
		}
		socket, err := f.connect(ctx, s.seat, s.client)
		if err != nil {
			log.WarnContext(ctx, "mattermost_connect_failed", "handle", s.seat.Handle,
				"attempt", attempt+1, "error", err.Error())
			continue
		}
		// BACKFILL BEFORE READING, and after the socket is open: the
		// other order leaves a hole. Reading first means anything said
		// between the last cursor and the connect is lost; backfilling
		// before connecting means anything said DURING the backfill is
		// lost. Doing it in this order can only produce duplicates,
		// which the dedupe ring absorbs.
		//
		// A replay that could not queue a post ends this connection
		// WITHOUT resetting the backoff: the queue is what is down, and
		// the next attempt replays from before that post — so a broker
		// that stays down is retried on the backoff, not in a loop.
		if err := f.replay(ctx, s); err != nil {
			_ = socket.Close()
			continue
		}
		// THE BACKOFF RESETS ONLY AFTER A CONNECTION THAT STAYED UP.
		// Mattermost closes without a close frame, so a server that
		// accepts a socket and hangs up on sight looks exactly like an
		// ordinary drop — and resetting on every accepted socket retried
		// such a server at the one-second floor for ever, each retry a
		// backfill walking every channel the seat is in.
		opened := time.Now()
		f.pump(ctx, s, socket)
		_ = socket.Close()
		if time.Since(opened) >= stableConnection {
			attempt = 0
		}
	}
}

// stableConnection is how long a socket must have stayed up for the next drop
// to be treated as a fresh one, with the backoff back at its floor.
//
// A MINUTE: twice the heartbeat, so a connection that lived through at least
// one ping round trip has proved the server keeps it — while a server that
// hangs up at once, or after its first frame, keeps climbing the backoff.
const stableConnection = time.Minute

// delay is the backoff for an attempt, jittered.
func (f *Fleet) delay(attempt int) time.Duration {
	if attempt >= len(f.backoff) {
		attempt = len(f.backoff) - 1
	}
	base := f.backoff[attempt]
	// Proportional and one-sided: every seat drops at the same instant
	// when a server restarts, and an unjittered fleet hands the
	// recovering server N simultaneous authentications and N backfills.
	return base + time.Duration(rand.Float64()*reconnectJitter*float64(base))
}

// pump reads one connection until it fails, with a heartbeat under it.
//
// THE HEARTBEAT IS THE ONLY LIVENESS SIGNAL THIS SIDE HAS. Nothing in this
// package pinged, and coder/websocket answers a server's pings internally
// without returning from Read — so a quiet channel and a dead server look
// identical from here. TCP keepalives rescue a genuinely dead path in about
// eleven minutes; an L7 half-open, where a load balancer has torn down the
// upstream connection but still answers keepalives, never resolves at all,
// and the seat is deaf until something else restarts it.
//
// An idle READ deadline is deliberately NOT the instrument: control frames do
// not surface through Read, so a merely-quiet channel would trip it and force
// a spurious backfill on every idle seat.
//
// A failed ping closes the socket, which ends Read and drops into the
// existing reconnect-and-replay path — so recovery needs no new machinery.
// So does a post that could not be queued: the pump ends, and the replay
// after the reconnect reads it again.
func (f *Fleet) pump(ctx context.Context, s *seatSocket, socket Socket) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var beat sync.WaitGroup
	beat.Go(func() { f.heartbeat(ctx, s, socket) })
	defer beat.Wait()
	defer cancel()

	for {
		body, err := socket.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.InfoContext(ctx, "mattermost_socket_closed", "handle", s.seat.Handle,
					"error", err.Error())
			}
			return
		}
		if err := f.deliver(ctx, s, body, false); err != nil {
			return
		}
	}
}

// heartbeat pings until the peer stops answering or the pump ends.
func (f *Fleet) heartbeat(ctx context.Context, s *seatSocket, socket Socket) {
	ticker := time.NewTicker(f.pingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, f.pongWithin)
		err := socket.Ping(pingCtx)
		cancel()
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			// The pump is ending anyway; a ping cut short by that is
			// not a dead peer.
			return
		}
		log.WarnContext(ctx, "mattermost_heartbeat_failed", "handle", s.seat.Handle,
			"error", err.Error(),
			"detail", "the server stopped answering; reconnecting and replaying")
		// Closing is what ends Read. Cancelling the pump's context alone
		// would leave the connection open and the server still believing
		// this seat is attached.
		_ = socket.Close()
		return
	}
}

// replay re-reads what a seat missed while it was disconnected.
//
// An error means a post was read and could not be queued; the replay stops
// there, with the seat's cursor held where this replay began, so the next one
// reads every post this one had not yet queued.
func (f *Fleet) replay(ctx context.Context, s *seatSocket) error {
	since, ok := s.since()
	if !ok {
		// Nothing seen yet: this seat is starting, not resuming, so
		// there is no gap. Anchoring on the SERVER's clock here rather
		// than replaying from the epoch is what stops a first connect
		// waking every seat with the channel's whole history.
		if when, err := s.client.ServerTime(ctx); err == nil {
			s.mark(when)
		} else {
			log.WarnContext(ctx, "mattermost_server_time_unavailable",
				"handle", s.seat.Handle, "error", err.Error())
		}
		return nil
	}

	// The floor is measured from NOW, not from the cursor — measuring it
	// from the cursor makes it unreachable, because the cursor is by
	// definition the newest thing seen and the floor would always sit
	// behind it. The window would then never bound anything, and an
	// hour-long outage would replay the whole hour.
	if floor, ok := f.floor(ctx, s); ok && since.Before(floor) {
		// Logged with what is being skipped rather than silently
		// truncated: "we missed two hours" is something an operator
		// needs to know, and a seat cannot infer it.
		log.WarnContext(ctx, "mattermost_backfill_window_exceeded", "handle", s.seat.Handle,
			"gap", floor.Add(f.backfill).Sub(since).String(),
			"window", f.backfill.String())
		since = floor
	}

	// A CHANNEL THAT COULD NOT BE READ IS STILL OWED. The cursor moves
	// with every post the other channels deliver, so without holding it
	// here the next replay would start past whatever this one could not
	// read, and those posts would be lost for good.
	channels, complete, err := f.channels(ctx, s)
	if err != nil {
		log.WarnContext(ctx, "mattermost_backfill_channels_unavailable",
			"handle", s.seat.Handle, "error", err.Error())
		s.hold(since)
		return nil
	}
	var replayed int
	for _, ch := range channels {
		posts, err := s.client.PostsSince(ctx, ch.ID, since)
		if err != nil {
			log.WarnContext(ctx, "mattermost_backfill_failed", "handle", s.seat.Handle,
				"channel", ch.ID, "error", err.Error())
			complete = false
			continue
		}
		for _, p := range posts {
			// CREATED IN THE GAP, not merely touched in it. `since=`
			// is UPDATE-based: a reaction touches a post and deleting
			// a reply touches its thread root, so the read hands back
			// posts the seat already answered, and replaying them
			// woke it to answer again. The live socket delivers only
			// what was posted, and the replay delivers no more.
			if p.CreateAt <= since.UnixMilli() {
				continue
			}
			if err := f.deliver(ctx, s, map[string]any{
				"event": "posted", "post": postMap(p),
				"channel_type": ch.Type, "channel_name": ch.Name,
			}, true); err != nil {
				// Posts in channels not yet read may be OLDER than
				// this one, so the whole replay is owed again —
				// from where it began, not from the post.
				s.hold(since)
				return err
			}
			replayed++
		}
	}
	if complete {
		s.settle(since)
	} else {
		s.hold(since)
	}
	if replayed > 0 {
		log.InfoContext(ctx, "mattermost_backfilled", "handle", s.seat.Handle,
			"posts", replayed, "since", since.UTC().String())
	}
	return nil
}

// floor is the oldest instant a replay may reach back to.
//
// The reference is the SERVER's clock, like the cursor it is compared
// against — comparing a server-stamped cursor to this process's clock is the
// skew this whole file is arranged to avoid.
//
// When the server cannot be asked, the ENGINE's clock is used and said so.
// That is a deliberate trade in one direction: the decision here is at
// minute granularity, where a few seconds of skew changes nothing, while
// skipping the bound entirely risks replaying an outage in full — N agent
// turns about conversations people resolved hours ago.
func (f *Fleet) floor(ctx context.Context, s *seatSocket) (time.Time, bool) {
	now, err := s.client.ServerTime(ctx)
	if err != nil {
		log.WarnContext(ctx, "mattermost_backfill_floor_from_local_clock",
			"handle", s.seat.Handle, "error", err.Error())
		now = f.now().UTC()
	}
	return now.Add(-f.backfill), true
}

// channels is the seat's work list: every channel it could have been spoken
// to in, across every team it belongs to — and whether that is every team's,
// since a team that could not be listed is a gap the replay still owes.
func (f *Fleet) channels(ctx context.Context, s *seatSocket) ([]Channel, bool, error) {
	teams, err := s.client.Teams(ctx, s.seat.UserID)
	if err != nil {
		return nil, false, err
	}
	var out []Channel
	complete := true
	for _, team := range teams {
		channels, err := s.client.Channels(ctx, s.seat.UserID, team.ID)
		if err != nil {
			// One team failing must not lose the others: a seat in
			// three teams should still hear two of them.
			log.WarnContext(ctx, "mattermost_team_channels_unavailable",
				"handle", s.seat.Handle, "team", team.ID, "error", err.Error())
			complete = false
			continue
		}
		out = append(out, channels...)
	}
	return out, complete, nil
}

// deliver claims one post fleet-wide, republishes it onto the raw-webhook
// envelope and records the delivery.
//
// An error means the post was read and could NOT be queued. Nothing about it is
// kept — the claim is released, the id forgotten and the cursor held before
// it — so the caller ends the connection and the replay after the reconnect
// reads it again.
func (f *Fleet) deliver(ctx context.Context, s *seatSocket, body map[string]any, replayed bool) error {
	// ONE check for two cases that lead to the same place: this is not a
	// post event at all — the socket carries typing indicators, presence
	// changes and status updates, none of which is something to wake a
	// seat for — or it is one with no id, which cannot be deduped and so
	// cannot be delivered safely. Indexing a nil map is legal, so the id
	// read covers both.
	post, _ := body["post"].(map[string]any)
	id := str(post, "id")
	if id == "" {
		return nil
	}
	if !s.first(id) {
		// The reconnect boundary: this post arrived through the
		// backfill read AND from the live socket that came up
		// mid-read. A duplicate here is a duplicate agent turn.
		return nil
	}
	at := stamp(post, "create_at")

	// AND ACROSS THE FLEET. Every node reads this seat's socket, so the
	// ring above only stops this process seeing a post twice; the claim is
	// what stops the fleet delivering it once per node.
	key := ClaimKey(s.seat.Handle, id)
	won, err := f.claims.Claim(ctx, key, ClaimTTL, f.now())
	switch {
	case err != nil:
		// FAILED OPEN. A post suppressed because the store blinked is
		// a message nobody answers; one delivered twice is collapsed by
		// the wake's derived id ([WakeID]) at the inbox and the ledger.
		log.WarnContext(ctx, "mattermost_post_dedupe_unavailable", "handle", s.seat.Handle,
			"post", id, "error", err.Error(),
			"detail", "delivering the post, which a peer may deliver too")
	case !won:
		// A PEER DELIVERED IT. Nothing is owed here: the post is that
		// node's to queue, and to read again should its publish fail.
		s.mark(at)
		return nil
	}
	claimed := err == nil

	// WHAT ARRIVED, for the record — before the seat's identity is added
	// below, which is this engine's annotation rather than the server's.
	arrived, _ := json.Marshal(body)

	// The seat's own identity rides along, because the parser needs it to
	// suppress the seat's own posts and the prompt needs it to teach the
	// agent how it is addressed. Neither is recoverable from the payload.
	body["bot_user_id"] = s.seat.UserID
	body["bot_username"] = s.seat.Username
	if replayed {
		body["replayed"] = true
	}

	ev := events.New(types.RawWebhook{
		Body: body, Headers: map[string]string{}, Handle: s.seat.Handle,
	}, tracing.TraceOf(ctx))
	ev.Source = Backend
	if err := f.publisher.Publish(ctx, topics.NotificationsInbound, ev); err != nil {
		// NOTHING IS KEPT, so the post is read again. WithoutCancel on
		// the release: a pump ending because its context did is often
		// WHY the publish failed, and a claim left standing would refuse
		// every node's re-read of this post for the whole ClaimTTL.
		if claimed {
			if rerr := f.claims.Release(context.WithoutCancel(ctx), key); rerr != nil {
				log.WarnContext(ctx, "mattermost_post_release_failed", "handle", s.seat.Handle,
					"post", id, "error", rerr.Error(),
					"detail", "the post's re-read is refused until its claim lapses")
			}
		}
		s.forget(id)
		if !at.IsZero() {
			s.hold(at.Add(-time.Millisecond))
		}
		log.ErrorContext(ctx, "mattermost_publish_failed", "handle", s.seat.Handle,
			"post", id, "error", err.Error(),
			"detail", "the post was read off the socket and could not be queued; "+
				"reconnecting to read it again")
		return fmt.Errorf("mattermost: queue post %s for %s: %w", id, s.seat.Handle, err)
	}
	s.mark(at)
	f.record(ctx, s, post, arrived, replayed)
	return nil
}

// record publishes the delivery's record: one post presented to one seat,
// counted once across the fleet because only the claim's winner gets here.
//
// PUBLISHED rather than written, because this runs on every node and a node
// without `data` keeps no event log — its records reach a data node through
// custody, as every event it publishes does. Best effort: the wake is queued,
// and a lost record costs a row on a screen, not a message.
func (f *Fleet) record(ctx context.Context, s *seatSocket, post map[string]any,
	arrived []byte, replayed bool) {

	ev := events.New(types.InboundDelivery{
		Label: "socket:posted", Route: Backend,
		Text:      deliverySummary(post, s.seat, replayed),
		Recipient: s.seat.Handle, DeliveryKey: str(post, "id"),
		Channel: str(post, "channel_id"), Replayed: replayed,
		Body: arrived,
	}, tracing.TraceOf(ctx))
	ev.Source = Backend
	if err := f.publisher.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "mattermost_delivery_unrecorded", "handle", s.seat.Handle,
			"post", str(post, "id"), "error", err.Error(),
			"detail", "the post was queued and will wake its seat; only its row "+
				"on the integrations screen and in the event log is missing")
	}
}

// deliverySummary is a delivery row's one line: which bot heard it, and where.
func deliverySummary(post map[string]any, seat Seat, replayed bool) string {
	out := "post for @" + seat.Username
	if replayed {
		out = "replayed " + out
	}
	if channel := str(post, "channel_id"); channel != "" {
		out += " in channel " + channel
	}
	return out
}

// ClaimKey is the fleet-wide identity of one post presented to one seat.
//
// PER SEAT, because one post is a delivery to every bot in its channel — each
// has its own socket and its own turn to take — and a key on the post alone
// would let the first seat's delivery suppress the rest.
func ClaimKey(handle, postID string) string { return Backend + "|" + handle + "|" + postID }

// since is the cursor to resume from, and whether there is one: the newest
// post seen, or the HELD cursor when that is earlier — a post at or after it
// was read and is still owed.
func (s *seatSocket) since() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.held.IsZero() && (s.cursor.IsZero() || s.held.Before(s.cursor)) {
		return s.held, true
	}
	return s.cursor, !s.cursor.IsZero()
}

// hold makes a replay resume from at or earlier, never later.
func (s *seatSocket) hold(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held.IsZero() || at.Before(s.held) {
		s.held = at
	}
}

// settle drops a hold a completed replay from `from` has paid off. A hold
// taken after the replay began — by a live post that failed meanwhile — is
// later than `from` only if it was taken in the gap, and is kept.
func (s *seatSocket) settle(from time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.held.After(from) {
		s.held = time.Time{}
	}
}

// forget drops a post id from the ring, so its re-read is not taken for a
// duplicate.
func (s *seatSocket) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seenAt[id] {
		return
	}
	delete(s.seenAt, id)
	if i := slices.Index(s.seen, id); i >= 0 {
		s.seen = slices.Delete(s.seen, i, i+1)
	}
}

// mark advances the cursor, never backwards.
//
// A backfill replays oldest-first while the live socket delivers newest, so
// the two interleave at a reconnect — and a cursor that moved backwards
// would re-read the gap again on the next drop.
func (s *seatSocket) mark(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.After(s.cursor) {
		s.cursor = at
	}
}

// first reports whether this post id is new, remembering it.
func (s *seatSocket) first(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenAt[id] {
		return false
	}
	s.seenAt[id] = true
	s.seen = append(s.seen, id)
	if len(s.seen) > dedupeRing {
		delete(s.seenAt, s.seen[0])
		s.seen = s.seen[1:]
	}
	return true
}

// postMap renders a post back into the wire shape the parser reads, so a
// replayed post and a live one are the same thing downstream.
func postMap(p Post) map[string]any {
	m := map[string]any{
		"id": p.ID, "channel_id": p.ChannelID, "user_id": p.UserID,
		"message": p.Message, "create_at": float64(p.CreateAt),
		"delete_at": float64(p.DeleteAt),
	}
	if p.RootID != "" {
		m["root_id"] = p.RootID
	}
	if p.Type != "" {
		m["type"] = p.Type
	}
	if len(p.FileIDs) > 0 {
		files := make([]any, len(p.FileIDs))
		for i, f := range p.FileIDs {
			files[i] = f
		}
		m["file_ids"] = files
	}
	return m
}

// stamp reads a millisecond timestamp field.
func stamp(m map[string]any, key string) time.Time {
	switch v := m[key].(type) {
	case float64:
		if v > 0 {
			return time.UnixMilli(int64(v)).UTC()
		}
	case int64:
		if v > 0 {
			return time.UnixMilli(v).UTC()
		}
	}
	return time.Time{}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// dialWebsocket is the real connection.
//
// The Origin header is set to what a browser at this instance would send,
// because Mattermost accepts a socket only from an origin matching its own
// Site URL — and a client sending none is refused outright.
func dialWebsocket(ctx context.Context, seat Seat, c *Client) (Socket, error) {
	//nolint:bodyclose // Deliberate: see dialOnce in doctor.go.
	conn, _, err := websocket.Dial(ctx, c.WebsocketURL(), &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Authorization": {"Bearer " + c.Token()},
			"Origin":        {BrowserOrigin(c.URL())},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("mattermost: dial %s: %w", c.WebsocketURL(), err)
	}
	// Mattermost sends whole posts as JSON strings inside an event
	// envelope, and a busy channel's message with attachments is well
	// past the library's default.
	conn.SetReadLimit(4 << 20)
	return &wsSocket{conn: conn}, nil
}

type wsSocket struct{ conn *websocket.Conn }

func (s *wsSocket) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}

// Ping sends a websocket PING and waits for the PONG.
//
// The library handles the round trip, including matching the payload, so a
// returned nil means the peer answered rather than merely that a write
// succeeded — which is the distinction that makes this worth doing at all.
func (s *wsSocket) Ping(ctx context.Context) error { return s.conn.Ping(ctx) }

// Read returns the next POST event, skipping everything else.
//
// The envelope's `data` carries its fields as JSON STRINGS rather than
// objects — `post` is a serialised post, not a nested one — so it is decoded
// a second time here. A consumer that read it as an object gets nothing and
// reports no error.
func (s *wsSocket) Read(ctx context.Context) (map[string]any, error) {
	for {
		_, raw, err := s.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		var frame struct {
			Event     string         `json:"event"`
			Data      map[string]any `json:"data"`
			Broadcast struct {
				ChannelID string `json:"channel_id"`
			} `json:"broadcast"`
		}
		if err := json.Unmarshal(raw, &frame); err != nil {
			// A frame this process cannot read will not become
			// readable, and the connection is otherwise healthy —
			// dropping it beats tearing down a live socket.
			log.DebugContext(ctx, "mattermost_frame_unreadable", "error", err.Error())
			continue
		}
		if frame.Event != "posted" || frame.Data == nil {
			continue
		}
		body := map[string]any{"event": frame.Event}
		for k, v := range frame.Data {
			body[k] = v
		}
		serialised, _ := frame.Data["post"].(string)
		var post map[string]any
		if err := json.Unmarshal([]byte(serialised), &post); err != nil {
			log.DebugContext(ctx, "mattermost_post_unreadable", "error", err.Error())
			continue
		}
		body["post"] = post
		if frame.Broadcast.ChannelID != "" {
			// The channel a broadcast names is authoritative, and
			// the post's own field can be absent on some events.
			if _, ok := post["channel_id"]; !ok {
				post["channel_id"] = frame.Broadcast.ChannelID
			}
		}
		// mentions arrives as a JSON-encoded ARRAY in a string, for the
		// same reason post does.
		if encoded, ok := frame.Data["mentions"].(string); ok && encoded != "" {
			var mentions []any
			if err := json.Unmarshal([]byte(encoded), &mentions); err == nil {
				body["mentions"] = mentions
			}
		}
		return body, nil
	}
}
