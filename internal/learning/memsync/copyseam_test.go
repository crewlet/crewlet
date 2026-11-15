package memsync

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	seathost "github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/store"
)

// newSyncer is [New] for a node in its broker's own process, which a case
// over a real broker's own connection is: answered whole, since New refuses
// only a link with no heartbeat.
func newSyncer(db *store.DB, js jetstream.JetStream, agentID AgentIDFor) *Syncer {
	return mustNew(db, js, agentID, Config{})
}

// newSyncerAcross is [New] for a node across a link from its changelog, a
// leaf's, renewing its leases at the shipped heartbeat.
func newSyncerAcross(db *store.DB, js jetstream.JetStream, agentID AgentIDFor) *Syncer {
	return mustNew(db, js, agentID, Config{AcrossLink: true, LeaseHeartbeat: seathost.HeartbeatInterval})
}

func mustNew(db *store.DB, js jetstream.JetStream, agentID AgentIDFor, cfg Config) *Syncer {
	s, err := New(db, js, agentID, cfg)
	if err != nil {
		panic(err)
	}
	return s
}

// bubbleSyncer is a syncer of the seat over db whose changelog is the model
// copies is played over (copyfake_test.go): in this node's own process, or
// across a link at the shipped lease where link says so.
func bubbleSyncer(t *testing.T, db *store.DB, copies *fakeCopies, link bool) *Syncer {
	t.Helper()
	js := &fakeJS{log: copies.log, created: time.Unix(1_700_000_000, 0).UTC()}
	newer := newSyncer
	if link {
		newer = newSyncerAcross
	}
	s := newer(db, js, func(string) string { return seat.AgentID })
	s.copies = copies
	return s
}

// The model copies, as a replay's consumers.

func (c *fakeCopies) open(ctx context.Context, cfg jetstream.ConsumerConfig) (replayPulls, error) {
	return c.placing(ctx, cfg), nil
}

func (c *fakeCopies) drop(_ context.Context, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropped = append(c.dropped, name)
}

func (c *fakeCopies) maxPayload() int64 { return c.payload }

// stallOf is s's stall for a pull of maxBytes.
func stallOf(s *Syncer, maxBytes int) time.Duration { return s.stall(maxBytes) }

// pullBytesOf is the transport's bound of s's pulls.
func pullBytesOf(s *Syncer) int { return s.pullBytes() }

// windowOf is s's window across a link.
func windowOf(s *Syncer) int { return s.window }
