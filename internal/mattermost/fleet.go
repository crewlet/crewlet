package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
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

// FleetOptions configure a [Fleet].
type FleetOptions struct {
	Publisher Publisher

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

	// Now is the clock used for LOCAL bookkeeping only. The backfill
	// cursor comes from the server — see [Client.ServerTime].
	Now func() time.Time
}

// Fleet holds one socket per seat.
type Fleet struct {
	publisher Publisher
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
	f := &Fleet{
		publisher:  opts.Publisher,
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
	out := make([]string, 0, len(f.seats))
	for handle := range f.seats {
		out = append(out, handle)
	}
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
		f.replay(ctx, s)
		attempt = 0
		f.pump(ctx, s, socket)
		_ = socket.Close()
	}
}

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
		f.deliver(ctx, s, body, false)
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
func (f *Fleet) replay(ctx context.Context, s *seatSocket) {
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
		return
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

	channels, err := f.channels(ctx, s)
	if err != nil {
		log.WarnContext(ctx, "mattermost_backfill_channels_unavailable",
			"handle", s.seat.Handle, "error", err.Error())
		return
	}
	var replayed int
	for _, ch := range channels {
		posts, err := s.client.PostsSince(ctx, ch.ID, since)
		if err != nil {
			log.WarnContext(ctx, "mattermost_backfill_failed", "handle", s.seat.Handle,
				"channel", ch.ID, "error", err.Error())
			continue
		}
		for _, p := range posts {
			f.deliver(ctx, s, map[string]any{
				"event": "posted", "post": postMap(p),
				"channel_type": ch.Type, "channel_name": ch.Name,
			}, true)
			replayed++
		}
	}
	if replayed > 0 {
		log.InfoContext(ctx, "mattermost_backfilled", "handle", s.seat.Handle,
			"posts", replayed, "since", since.UTC().String())
	}
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
// to in, across every team it belongs to.
func (f *Fleet) channels(ctx context.Context, s *seatSocket) ([]Channel, error) {
	teams, err := s.client.Teams(ctx, s.seat.UserID)
	if err != nil {
		return nil, err
	}
	var out []Channel
	for _, team := range teams {
		channels, err := s.client.Channels(ctx, s.seat.UserID, team.ID)
		if err != nil {
			// One team failing must not lose the others: a seat in
			// three teams should still hear two of them.
			log.WarnContext(ctx, "mattermost_team_channels_unavailable",
				"handle", s.seat.Handle, "team", team.ID, "error", err.Error())
			continue
		}
		out = append(out, channels...)
	}
	return out, nil
}

// deliver republishes one post onto the raw-webhook envelope.
func (f *Fleet) deliver(ctx context.Context, s *seatSocket, body map[string]any, replayed bool) {
	// ONE check for two cases that lead to the same place: this is not a
	// post event at all — the socket carries typing indicators, presence
	// changes and status updates, none of which is something to wake a
	// seat for — or it is one with no id, which cannot be deduped and so
	// cannot be delivered safely. Indexing a nil map is legal, so the id
	// read covers both.
	post, _ := body["post"].(map[string]any)
	id := str(post, "id")
	if id == "" {
		return
	}
	if !s.first(id) {
		// The reconnect boundary: this post arrived through the
		// backfill read AND from the live socket that came up
		// mid-read. A duplicate here is a duplicate agent turn.
		return
	}
	if at := stamp(post, "create_at"); !at.IsZero() {
		s.mark(at)
	}

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
		log.ErrorContext(ctx, "mattermost_publish_failed", "handle", s.seat.Handle,
			"post", id, "error", err.Error(),
			"detail", "the post was read off the socket and could not be queued; "+
				"it will not be re-read, because the cursor has moved past it")
	}
}

// since is the cursor to resume from, and whether there is one.
func (s *seatSocket) since() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor, !s.cursor.IsZero()
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
