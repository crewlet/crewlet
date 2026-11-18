package memsync

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/store"
)

// newSyncer is [New] for a node in its broker's own process, as a case over
// a real broker's connection is; it cannot fail, since New refuses only a
// meter on a node that crosses no link.
func newSyncer(db *store.DB, js jetstream.JetStream, agentID AgentIDFor) *Syncer {
	return mustNew(db, js, agentID, Config{})
}

// newSyncerAcross is [New] for a node across a link from its changelog, a
// leaf's, unmetered.
func newSyncerAcross(db *store.DB, js jetstream.JetStream, agentID AgentIDFor) *Syncer {
	return mustNew(db, js, agentID, Config{AcrossLink: true, Window: jsapi.NewLinkWindow()})
}

func mustNew(db *store.DB, js jetstream.JetStream, agentID AgentIDFor, cfg Config) *Syncer {
	s, err := New(db, js, agentID, cfg)
	if err != nil {
		panic(err)
	}
	return s
}

// bubbleSyncer is a syncer of the seat over db on copies' model changelog
// (copyfake_test.go), in the broker's process or, where link says so, across
// a link.
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

// windowOf is the size of s's window across a link, 0 where it has none.
func windowOf(s *Syncer) int {
	if s.window == nil {
		return 0
	}
	return s.window.Size()
}
