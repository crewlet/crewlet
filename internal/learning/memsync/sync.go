package memsync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// hydrateWait bounds how long a seat waits for its memory to arrive.
//
// It is spent inside seat acquisition, before the mailbox attaches, so it is
// time the seat is not serving. Fifteen seconds is far past a replay from a
// local or clustered broker — the rows are already materialised on the stream
// and the read is sequential — and short enough that a broker having a bad
// minute delays a seat rather than stalling placement behind it.
const hydrateWait = 15 * time.Second

// consumerCleanupTimeout bounds deleting the ephemeral replay consumer.
//
// Five seconds against one API round trip: far past a healthy answer, and
// bounded because this runs inside seat acquisition. What it costs when it
// expires is nothing an operator sees — the server reaps an idle ephemeral
// consumer on its own, which is why the deletion is hygiene rather than a
// step whose failure means anything.
const consumerCleanupTimeout = 5 * time.Second

// fetchBatch is how many rows one replay pull takes.
//
// The rows are small and the transfer is local, so this is about round trips
// rather than memory: 256 clears a seat with a few hundred episodes in a
// couple of pulls.
const fetchBatch = 256

// AgentIDFor derives a seat's stable id from its handle.
//
// Injected rather than imported, because the derivation belongs to the
// organization model and this package needs exactly one function from it —
// the same rule every other seam in this tree follows.
type AgentIDFor func(handle string) string

// Syncer carries a seat's memory between the nodes that run it.
type Syncer struct {
	db      *store.DB
	js      jetstream.JetStream
	agentID AgentIDFor

	// marks is the per-seat, per-table watermark: the highest rowid
	// already published. In memory only, and deliberately: a restart
	// republishes what it has, which the stream collapses onto the same
	// subjects, so the cost is one extra pass and the benefit is a
	// subsystem with no state of its own to keep correct.
	mu    sync.Mutex
	marks map[string]int64
}

// New builds a syncer, or returns nil when this node cannot carry memory.
//
// Nil rather than a syncer that quietly does nothing: a node with no broker
// has no way to publish or replay, and a caller that got a working-looking
// object would believe its seats' memory was travelling when it was not.
func New(db *store.DB, conn *nats.Conn, agentID AgentIDFor) (*Syncer, error) {
	if db == nil || conn == nil || agentID == nil {
		return nil, nil
	}
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, fmt.Errorf("memsync: reach the JetStream API: %w", err)
	}
	return &Syncer{db: db, js: js, agentID: agentID, marks: map[string]int64{}}, nil
}

// seat resolves a handle into both spellings the schema uses.
func (s *Syncer) seat(handle string) seatRef {
	return seatRef{Handle: handle, AgentID: s.agentID(handle)}
}

// Publish carries whatever is new in a seat's memory onto the changelog.
//
// Called for the seats this node HOLDS, on a cadence — see the worker. It is
// incremental for the append-only tables and whole for the small mutable
// ones (see table.wholeEachCycle), so the steady-state cost is proportional
// to what the seat just learned rather than to everything it knows.
func (s *Syncer) Publish(ctx context.Context, handle string) (int, error) {
	if s == nil {
		return 0, nil
	}
	ref := s.seat(handle)
	published := 0
	for _, spec := range tables {
		mark := s.mark(handle, spec.name)
		rows, high, err := export(ctx, s.db.SQL(), spec, ref, mark)
		if err != nil {
			return published, err
		}
		for _, row := range rows {
			body, err := encode(row)
			if err != nil {
				return published, err
			}
			subject := spec.subject(handle, row.Values)
			if _, err := s.js.Publish(ctx, subject, body); err != nil {
				return published, fmt.Errorf("memsync: publish a %s row for %s: %w",
					spec.name, handle, err)
			}
			published++
		}
		// ADVANCED ONLY AFTER THE PUBLISHES SUCCEEDED. A watermark moved
		// first would skip, for the life of the process, exactly the rows
		// whose publish failed.
		s.setMark(handle, spec.name, high)
	}
	if published > 0 {
		log.DebugContext(ctx, "memory_published", "seat", handle, "rows", published)
	}
	return published, nil
}

// Hydrate brings a seat's memory into this node's store.
//
// Called in seat acquisition BEFORE the mailbox attaches, so a seat never
// serves a turn against a store that has forgotten it. A failure refuses the
// seat, which is the honest outcome: a peer that takes the seat instead may
// have the memory, and a seat that runs with amnesia produces work its own
// history contradicts.
func (s *Syncer) Hydrate(ctx context.Context, handle string) (int, error) {
	if s == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, hydrateWait)
	defer cancel()

	// An EPHEMERAL consumer over this seat's subjects, from the start of
	// the stream. Ephemeral because this is a one-shot read of a keyed
	// table rather than a subscription with a position to remember: the
	// next hydration wants the whole current picture again, not what has
	// changed since some previous node read it.
	consumer, err := s.js.CreateConsumer(ctx, topics.MemoryStream, jetstream.ConsumerConfig{
		FilterSubject: topics.MemoryPrefix + handle + ".>",
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckNonePolicy,
	})
	if err != nil {
		return 0, fmt.Errorf("memsync: open a replay for %s: %w", handle, err)
	}

	defer func() {
		// The consumer is ephemeral, so the server reaps it when it goes
		// idle — but a node acquiring seats in a sweep would otherwise
		// leave one per seat lying around until then.
		//
		// WithoutCancel because a cleanup that inherits the failure it is
		// cleaning up after does nothing at all — and then its OWN bound,
		// because WithoutCancel drops the deadline with the cancellation.
		// This runs before Hydrate returns, inside seat acquisition, so an
		// unbounded call here would hang the acquisition on exactly the
		// wedged broker that made the hydration fail.
		cleanupCtx, stop := context.WithTimeout(
			context.WithoutCancel(ctx), consumerCleanupTimeout)
		defer stop()
		_ = s.js.DeleteConsumer(cleanupCtx,
			topics.MemoryStream, consumer.CachedInfo().Name)
	}()

	// HOW MANY ROWS THERE ARE, BEFORE READING ANY. This runs inside seat
	// acquisition, so a blind fetch is not affordable: with nothing to
	// replay it would wait out its own timeout, and the seat would still
	// be acquiring when its first lease renewal came due. Measured — two
	// seats hydrating nothing cost four seconds and the node lost both
	// leases. Asking the consumer what is pending turns the empty case,
	// which is every seat on a fresh company, into one round trip.
	info, err := consumer.Info(ctx)
	if err != nil {
		// FAILS THE HYDRATION rather than reading as "nothing to carry".
		// Those are not the same fact, and collapsing them here is worse
		// than anywhere else in this package: every other failure path
		// refuses the seat, so a swallowed error would be the one way a
		// seat is admitted with an empty memory AND a success reported.
		return 0, fmt.Errorf("memsync: count %s's memory: %w", handle, err)
	}
	pending := int(info.NumPending)
	if pending == 0 {
		return 0, nil
	}

	carried := 0
	for pending > 0 {
		want := min(pending, fetchBatch)
		batch, err := consumer.Fetch(want, jetstream.FetchMaxWait(hydrateWait))
		if err != nil {
			return carried, fmt.Errorf("memsync: replay %s: %w", handle, err)
		}
		got := 0
		if err := s.db.Tx(ctx, func(tx *sql.Tx) error {
			for msg := range batch.Messages() {
				got++
				row, spec, known, decodeErr := decode(msg.Data())
				if decodeErr != nil {
					return decodeErr
				}
				if !known {
					// A table a newer peer replicates and this
					// build does not carry. Skipped rather than
					// fatal: a rolling upgrade puts both builds
					// on one stream, and refusing to hydrate at
					// all would be worse than hydrating what
					// this build understands.
					log.WarnContext(ctx, "memory_row_unknown_table",
						"seat", handle, "table", row.Table)
					continue
				}
				if err := upsert(ctx, tx, spec, row); err != nil {
					return err
				}
				carried++
			}
			return batch.Error()
		}); err != nil {
			return carried, err
		}
		if got == 0 {
			// The stream said there were more and delivered none:
			// stopping is the only way this loop is guaranteed to
			// end, and a short hydration is better than a stuck one.
			break
		}
		pending -= got
	}
	log.InfoContext(ctx, "memory_hydrated", "seat", handle, "rows", carried)
	return carried, nil
}

// mark reads a watermark.
func (s *Syncer) mark(handle, table string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marks[handle+"\x00"+table]
}

// setMark advances a watermark, never backwards.
func (s *Syncer) setMark(handle, table string, high int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := handle + "\x00" + table
	if high > s.marks[key] {
		s.marks[key] = high
	}
}

// Forget drops a released seat's watermarks.
//
// Without this a node that released a seat and later took it back would
// resume from its old marks and never republish what it wrote in between —
// and what it wrote in between is exactly the memory the other node made.
func (s *Syncer) Forget(handle string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.marks {
		// HasPrefix, not a slice: the map holds every seat this node
		// has published, and slicing a key shorter than this handle
		// panics rather than simply not matching.
		if strings.HasPrefix(key, handle+"\x00") {
			delete(s.marks, key)
		}
	}
}
