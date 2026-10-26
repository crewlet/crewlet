package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// prose is what the person was told: it must cross the broker only encrypted.
const prose = "The signing keys rotate on Friday at noon."

var (
	owner  = uuid.MustParse("0192f0aa-61c2-7d1e-9b40-3c5d7e8f9a10")
	other  = uuid.MustParse("0192f0aa-61c2-7d1e-9b40-3c5d7e8f9a11")
	turnB  = uuid.MustParse("0192f0aa-61c2-7d1e-9b40-3c5d7e8f9a12")
	answer = uuid.MustParse("0192f0aa-61c2-7d1e-9b40-3c5d7e8f9a13")
)

// fleetKeys is one fleet's keyring, as both halves of every channel derive
// their keys from it.
type fleetKeys struct {
	ring   statelog.Keyring
	cipher secrets.Cipher
}

func keysOf(t *testing.T, signing string, aead byte) fleetKeys {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": bytes.Repeat([]byte{aead}, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	return fleetKeys{ring: statelog.OneKey("k1", signing), cipher: cipher}
}

// spy records every payload a node broadcast and every reply it was handed.
type spy struct {
	Broker
	mu      sync.Mutex
	payload [][]byte
}

func (s *spy) Broadcast(ctx context.Context, subject string, payload []byte) error {
	s.mu.Lock()
	s.payload = append(s.payload, bytes.Clone(payload))
	s.mu.Unlock()
	return s.Broker.Broadcast(ctx, subject, payload)
}

func (s *spy) AskEach(ctx context.Context, subject string, request []byte,
	each func([]byte) bool) error {

	return s.Broker.AskEach(ctx, subject, request, func(reply []byte) bool {
		s.mu.Lock()
		s.payload = append(s.payload, bytes.Clone(reply))
		s.mu.Unlock()
		return each(reply)
	})
}

func (s *spy) seen() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.payload...)
}

// roster is the fleet's presence: the nodes named are present.
type roster map[string]bool

func (r roster) Present(_ context.Context, node string) (bool, error) { return r[node], nil }

func (r roster) Nodes(context.Context) ([]string, error) {
	var out []string
	for n, ok := range r {
		if ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// server is a running node's turns, as the fleet half asks them.
type server struct {
	mu      sync.Mutex
	frames  map[uuid.UUID][]wire.Frame
	stopped []uuid.UUID
	// ended are the turns whose segment has ended (its done told).
	ended map[uuid.UUID]bool
}

func (s *server) ReplayHere(_ context.Context, who, turn uuid.UUID, after uint64) ([]wire.Frame,
	bool, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	frames, ok := s.frames[turn]
	switch {
	case !ok:
		return nil, false, ErrNotRunning
	case who != owner:
		return nil, false, ErrNotYours
	}
	var out []wire.Frame
	for _, f := range frames {
		if f.Seq > after {
			out = append(out, f)
		}
	}
	return out, s.ended[turn], nil
}

func (s *server) LastHere(_ context.Context, who, turn uuid.UUID) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frames, ok := s.frames[turn]
	switch {
	case !ok || len(frames) == 0:
		return 0, ErrNotRunning
	case who != owner:
		return 0, ErrNotYours
	}
	return frames[len(frames)-1].Seq, nil
}

func (s *server) StopHere(_ context.Context, who, turn uuid.UUID) (wire.TurnState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.frames[turn]; !ok {
		return "", ErrNotRunning
	}
	if who != owner {
		return "", ErrNotYours
	}
	s.stopped = append(s.stopped, turn)
	return wire.TurnStopped, nil
}

// node is one fleet member on a shared in-memory broker.
func node(t *testing.T, b *memory.Broker, id string, keys fleetKeys, s Server) (*Fleet, *spy) {
	t.Helper()
	q := b.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	frames, err := signedask.NewOneWay[Sealed](FrameChannel, keys.ring)
	if err != nil {
		t.Fatal(err)
	}
	replays, err := signedask.New[ReplayRequest, ReplayReply](ReplayChannel, keys.ring)
	if err != nil {
		t.Fatal(err)
	}
	stops, err := signedask.New[StopRequest, StopReply](StopChannel, keys.ring)
	if err != nil {
		t.Fatal(err)
	}
	sp := &spy{Broker: q}
	f, err := NewFleet(FleetOptions{Node: id, Broker: sp, Roster: roster{"node-a": true,
		"node-b": true}, Cipher: keys.cipher, Frames: frames, Replays: replays, Stops: stops})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Start(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(context.Background()); err != nil {
			t.Errorf("close %s: %v", id, err)
		}
	})
	return f, sp
}

func frameOf(turn uuid.UUID, seq uint64, kind wire.EventKind) wire.Frame {
	f := wire.Frame{V: wire.ProtocolVersion, ConversationID: answer, TurnID: turn, Seq: seq,
		At: time.Now().UTC(), Kind: kind}
	switch kind {
	case wire.EventDelta:
		f.Delta = &wire.DeltaEvent{MessageID: answer, Text: prose}
	case wire.EventDone:
		f.Done = &wire.DoneEvent{State: wire.TurnCompleted}
	}
	return f
}

// collector is a stream as a client draws it.
type collector struct {
	mu     sync.Mutex
	frames []wire.Frame
}

func (c *collector) Frame(f wire.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, f)
	return nil
}

func (c *collector) Keepalive() error { return nil }

func (c *collector) drawn() []wire.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]wire.Frame(nil), c.frames...)
}

// follow follows turn from node-b while send runs, once node-b listens.
func follow(t *testing.T, follower *Fleet, turn uuid.UUID,
	send func()) []wire.Frame {

	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c := &collector{}
	done := make(chan error, 1)
	go func() {
		reach, err := follower.Follow(ctx, owner, turn, "node-a", 0, c)
		if err == nil && reach != ReachServed {
			err = errors.New("the runner was not reached: " + string(reach))
		}
		done <- err
	}()
	waitFor(t, func() bool { return len(c.drawn()) >= 1 })
	send()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("the stream never ended")
	}
	return c.drawn()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A FRAME THE FLEET DID NOT SEAL IS DROPPED (ADR-0053): a node following a
// turn shows only frames sealed under the fleet's keyring, never one written
// by whoever reached the broker, and never one sealed under another fleet's
// keys or encrypted under another fleet's AEAD key.
//
// Mutation: read a payload that does not open as a frame in the clear (a
// lenient fallback in Follow's listener) and the forged seq 2 is drawn in
// place of the fleet's.
func TestAFrameTheFleetDidNotSealIsDropped(t *testing.T) {
	t.Parallel()
	b := memory.NewBroker()
	fleet := keysOf(t, "fleet-material", 1)
	srv := &server{frames: map[uuid.UUID][]wire.Frame{turnB: {frameOf(turnB, 1, wire.EventDelta)}}}
	runner, _ := node(t, b, "node-a", fleet, srv)
	follower, _ := node(t, b, "node-b", fleet, &server{})

	stranger := keysOf(t, "another-fleets-material", 2)
	strangers, err := signedask.NewOneWay[Sealed](FrameChannel, stranger.ring)
	if err != nil {
		t.Fatal(err)
	}
	q := b.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	forged := frameOf(turnB, 2, wire.EventDone)
	forged.Done.State = wire.TurnFailed
	plain, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	forgeries := [][]byte{
		// A frame in the clear, as anybody on the broker could write one.
		plain,
		sealWith(t, strangers, stranger.cipher, forged),
		// Signed by a peer holding the fleet's signing keys but encrypted
		// under an AEAD key that is not the fleet's.
		sealWith(t, mustOneWay(t, fleet.ring), stranger.cipher, forged),
	}
	drawn := follow(t, follower, turnB, func() {
		for _, p := range forgeries {
			if err := q.Broadcast(t.Context(), topics.AssistantTurnSubject(turnB), p); err != nil {
				t.Fatal(err)
			}
		}
		runner.Publish(frameOf(turnB, 2, wire.EventDone))
	})
	if len(drawn) != 2 || drawn[1].Seq != 2 || drawn[1].Done == nil ||
		drawn[1].Done.State != wire.TurnCompleted {
		t.Fatalf("drew %+v, want the fleet's own seq 1 and its completed seq 2", drawn)
	}
}

// NO FRAME ON THE QUEUE CARRIES THE TURN'S PROSE (ADR-0053): every broadcast
// frame and every replayed page crosses the broker encrypted, so reaching the
// broker reads no conversation.
//
// Mutation: seal the frame's JSON in place of its ciphertext in sealFrame (or
// the page's in answerReplay) and the prose is found in the bytes.
func TestAFrameOnTheQueueCarriesNoneOfTheTurnsProse(t *testing.T) {
	t.Parallel()
	b := memory.NewBroker()
	fleet := keysOf(t, "fleet-material", 1)
	srv := &server{frames: map[uuid.UUID][]wire.Frame{turnB: {frameOf(turnB, 1, wire.EventDelta)}}}
	runner, sent := node(t, b, "node-a", fleet, srv)
	follower, heard := node(t, b, "node-b", fleet, &server{})

	drawn := follow(t, follower, turnB, func() {
		runner.Publish(frameOf(turnB, 2, wire.EventDelta))
		runner.Publish(frameOf(turnB, 3, wire.EventDone))
	})
	if len(drawn) != 3 || drawn[0].Delta == nil || drawn[0].Delta.Text != prose ||
		drawn[1].Delta == nil || drawn[1].Delta.Text != prose {
		t.Fatalf("drew %+v, want the replayed and the broadcast prose, decrypted", drawn)
	}
	crossed := append(sent.seen(), heard.seen()...)
	if len(crossed) < 3 {
		t.Fatalf("saw %d payloads cross the broker, want the replay and two broadcasts",
			len(crossed))
	}
	for _, p := range crossed {
		for _, word := range strings.Fields(prose) {
			if len(word) > 4 && bytes.Contains(p, []byte(word)) {
				t.Fatalf("a payload on the queue carries %q from the turn's prose: %q", word, p)
			}
		}
	}
}

// A FRAME REPLAYED ONTO ANOTHER TURN IS DROPPED (ADR-0053): a frame captured
// off one turn's channel and broadcast on another's does not open there, even
// re-sealed by a peer that can sign, because the turn is in its associated data.
//
// Mutation: drop the turn binding (the turn from FrameAAD, and openFrame's
// checks of the turn in the clear and inside the frame) and the re-sealed
// frame of the first turn is drawn on the second.
func TestAFrameReplayedOntoAnotherTurnIsDropped(t *testing.T) {
	t.Parallel()
	b := memory.NewBroker()
	fleet := keysOf(t, "fleet-material", 1)
	turnA := uuid.MustParse("0192f0aa-61c2-7d1e-9b40-3c5d7e8f9a14")
	srv := &server{frames: map[uuid.UUID][]wire.Frame{turnB: {frameOf(turnB, 1, wire.EventDelta)}}}
	runner, _ := node(t, b, "node-a", fleet, srv)
	follower, _ := node(t, b, "node-b", fleet, &server{})

	// A genuine frame of turn A, as it crossed the broker.
	captured := frameOf(turnA, 2, wire.EventDone)
	captured.Done.State = wire.TurnFailed
	original := sealWith(t, mustOneWay(t, fleet.ring), fleet.cipher, captured)
	// The same ciphertext re-sealed as turn B's by a peer that can sign.
	frames := mustOneWay(t, fleet.ring)
	opened, _, err := frames.Open(original, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	opened.Turn = turnB
	resealed, err := frames.Seal(opened)
	if err != nil {
		t.Fatal(err)
	}
	q := b.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	drawn := follow(t, follower, turnB, func() {
		for _, p := range [][]byte{original, resealed} {
			if err := q.Broadcast(t.Context(), topics.AssistantTurnSubject(turnB), p); err != nil {
				t.Fatal(err)
			}
		}
		runner.Publish(frameOf(turnB, 2, wire.EventDone))
	})
	if len(drawn) != 2 || drawn[1].TurnID != turnB || drawn[1].Done.State != wire.TurnCompleted {
		t.Fatalf("drew %+v, want turn B's own frames only", drawn)
	}
}

// A STOP OR A REPLAY FOR SOMEBODY ELSE'S TURN IS REFUSED BY THE RUNNER (ADR-0053):
// the node running a turn checks the person the request names, so a peer can
// neither read nor stop another person's turn on its word.
//
// Mutation: read the runner's not_yours as a runner that is gone in Stop and
// another person's stop is answered as if the turn had died.
func TestTheRunnerRefusesAnotherPersonsTurn(t *testing.T) {
	t.Parallel()
	b := memory.NewBroker()
	fleet := keysOf(t, "fleet-material", 1)
	srv := &server{frames: map[uuid.UUID][]wire.Frame{turnB: {frameOf(turnB, 1, wire.EventDelta)}}}
	node(t, b, "node-a", fleet, srv)
	asker, _ := node(t, b, "node-b", fleet, &server{})

	if _, _, err := asker.Stop(t.Context(), other, turnB, "node-a"); !errors.Is(err, ErrNotYours) {
		t.Fatalf("another person's stop answered %v, want ErrNotYours", err)
	}
	if _, err := asker.Follow(t.Context(), other, turnB, "node-a", 0, &collector{}); !errors.Is(err,
		ErrNotYours) {
		t.Fatalf("another person's replay answered %v, want ErrNotYours", err)
	}
	if len(srv.stopped) != 0 {
		t.Fatal("another person's stop stopped the turn")
	}
	state, reach, err := asker.Stop(t.Context(), owner, turnB, "node-a")
	if err != nil || reach != ReachServed || state != wire.TurnStopped {
		t.Fatalf("the owner's stop answered %q %q %v, want stopped by the runner", state, reach, err)
	}
	if _, reach, err := asker.Stop(t.Context(), owner, turnB, "node-gone"); err != nil ||
		reach != ReachGone {
		t.Fatalf("a stop for a node the roster does not hold answered %q %v, want gone",
			reach, err)
	}
}

func mustOneWay(t *testing.T, ring statelog.Keyring) *signedask.OneWay[Sealed] {
	t.Helper()
	w, err := signedask.NewOneWay[Sealed](FrameChannel, ring)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// sealWith seals f as a node holding w's signing keys and cipher's AEAD key
// would.
func sealWith(t *testing.T, w *signedask.OneWay[Sealed], cipher secrets.Cipher,
	f wire.Frame) []byte {

	t.Helper()
	fl := &Fleet{node: "node-a", cipher: cipher, frames: w,
		now: func() time.Time { return time.Now().UTC() }}
	sealed, err := fl.sealFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// A SEGMENT THAT ENDED BEFORE THE STREAM RESUMED IS CAUGHT UP, NOT SERVED
// EMPTY: a stream following a turn on another node, resumed after the frame
// that ended its segment (a parked turn reopened from the transcript), answers
// ErrCaughtUp having written nothing, which the HTTP surface answers 204. A
// stream resumed inside the segment is served to its end as before.
//
// Mutation: return ReachServed and nil for an ended segment with nothing
// after, and the follower is answered an empty stream it reads as a drop.
func TestAnEndedSegmentFollowedFromItsEndIsCaughtUp(t *testing.T) {
	t.Parallel()
	b := memory.NewBroker()
	fleet := keysOf(t, "fleet-material", 1)
	srv := &server{frames: map[uuid.UUID][]wire.Frame{turnB: {frameOf(turnB, 1, wire.EventDelta),
		frameOf(turnB, 2, wire.EventDone)}}, ended: map[uuid.UUID]bool{turnB: true}}
	node(t, b, "node-a", fleet, srv)
	follower, _ := node(t, b, "node-b", fleet, &server{})

	c := &collector{}
	reach, err := follower.Follow(t.Context(), owner, turnB, "node-a", 2, c)
	if reach != ReachServed || !errors.Is(err, ErrCaughtUp) || len(c.drawn()) != 0 {
		t.Fatalf("a follow from the segment's end = %q %v, %d frames; want served, caught up, "+
			"nothing written", reach, err, len(c.drawn()))
	}
	c = &collector{}
	reach, err = follower.Follow(t.Context(), owner, turnB, "node-a", 1, c)
	if reach != ReachServed || err != nil || len(c.drawn()) != 1 || c.drawn()[0].Seq != 2 {
		t.Fatalf("a follow from inside the segment = %q %v %+v; want the done frame alone",
			reach, err, c.drawn())
	}
}
