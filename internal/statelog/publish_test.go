package statelog_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

func probeSubject(id string) statelog.Subject {
	return statelog.Subject{Kind: "object", ID: id}
}

// THE FIVE ANSWERS ONE PUBLISH CAN HAVE, each reached through the seam that
// produces it in production rather than through a stub of the publisher.
//
// The three outcomes are what a caller is told; the fourth and fifth are
// refusals, which say no record happened at all. Collapsing any pair is the
// failure this framework exists to avoid — most sharply `applied` and
// `gated`, which differ only in a durable local table the answer is read out
// of.
func TestOnePublishHasFiveAnswersAndTellsThemApart(t *testing.T) {
	t.Parallel()

	t.Run("applied", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		res, err := h.write(probeSubject("a"), "op-1", "hello")
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("outcome = %q, want applied", res.Outcome)
		}
		if res.Position.Seq == 0 {
			t.Error("an applied write has no position")
		}
		if res.Version != res.Position.Packed() {
			t.Errorf("version %d is not the packed position %d — they are one "+
				"number and a second spelling is how they stop matching",
				res.Version, res.Position.Packed())
		}
	})

	t.Run("pending", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// This node's applier never reaches the record. The record is
		// DURABLE and every other node will apply it, so the honest
		// answer is neither success nor failure.
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()

		res, err := h.write(probeSubject("a"), "op-1", "hello")
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if res.Outcome != statelog.OutcomePending {
			t.Fatalf("outcome = %q, want pending", res.Outcome)
		}
		if res.Position.Seq == 0 {
			t.Error("pending must carry the position the record is durable at — " +
				"it is the whole difference from unknown")
		}
	})

	t.Run("gated", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// THE ORDINARY BRANCH, where the wait SUCCEEDS. A live node
		// applies its own record and drops it under its own eviction
		// gate; a design answering from the wait alone reports it
		// applied, out of the same database that holds the fact that
		// killed it.
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()
		h.gates.gated, h.gates.reason = true, statelog.ReasonEvicted
		h.applier.advance(statelog.Position{Stream: probeStream, Generation: 1, Seq: 1_000})

		_, err := h.write(probeSubject("a"), "op-1", "hello")
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) {
			t.Fatalf("write = %v, want an Unavailable", err)
		}
		if refusal.Reason != statelog.ReasonEvicted {
			t.Fatalf("reason = %q, want %q", refusal.Reason, statelog.ReasonEvicted)
		}
		if refusal.Position.Seq == 0 {
			t.Error("a gated refusal must name the position the record is durable at")
		}
	})

	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// An established object, so the write takes the ordinary
		// anchored path rather than the probe.
		first, err := h.write(probeSubject("a"), "op-0", "one")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		h.anchorAt(probeSubject("a"), first.Position.Seq)

		// Then the broker stops answering. The append may or may not
		// have landed and the discriminator cannot say which, which is
		// the whole content of the third value.
		h.pubUnreachable()

		res, err := h.write(probeSubject("a"), "op-1", "hello")
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if res.Outcome != statelog.OutcomeUnknown {
			t.Fatalf("outcome = %q, want unknown", res.Outcome)
		}
		if res.OpID != "op-1" {
			t.Error("unknown must carry the op id — retrying under the SAME id " +
				"is the only safe retry, and a fresh one defeats the ledger")
		}
	})

	t.Run("nothing landed, so the snapshot is retaken", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first, err := h.write(probeSubject("a"), "op-0", "one")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		h.anchorAt(probeSubject("a"), first.Position.Seq)

		// The append fails BEFORE the record reaches the stream. The
		// subject still holds nothing above the anchor, so there is
		// nothing to discriminate and nothing to resolve — the write
		// decides again from a fresh snapshot and lands.
		h.appends.fail(errors.New("no response from stream"), true)

		res, err := h.write(probeSubject("a"), "op-1", "two")
		if err != nil {
			t.Fatalf("write after a swallowed append: %v", err)
		}
		if res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("outcome = %q, want applied", res.Outcome)
		}
		if res.Rounds < 2 {
			t.Fatalf("rounds = %d, want at least 2 — the first attempt never "+
				"reached the stream, so the second is what landed", res.Rounds)
		}
	})

	t.Run("refused before the append", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.fence.evicted = true
		_, err := h.write(probeSubject("a"), "op-1", "hello")
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonEvicted {
			t.Fatalf("write = %v, want an evicted refusal", err)
		}
		if got := h.appends.appends.Load(); got != 0 {
			t.Fatalf("the publisher appended %d time(s) while evicted — the fence "+
				"exists precisely to stop a node collecting acknowledgements for "+
				"records every applier will drop", got)
		}
	})
}

// AN EVICTED NODE IS REFUSED BEFORE THE APPEND, on every pattern and after
// every round.
//
// Fencing only the expectation-zero branch is the tempting version, on the
// theory that the ordinary path is refused by the broker anyway. The broker
// refuses a stale expectation and knows nothing about the counted set — so an
// evicted node whose row is current publishes successfully and is
// acknowledged.
func TestEvictedNodeRefusesBeforeTheAppend(t *testing.T) {
	t.Parallel()
	for name, pattern := range map[string]statelog.Pattern{
		"arbitrated": statelog.PatternArbitrated,
		"create":     statelog.PatternCreate,
		"additive":   statelog.PatternAdditive,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.fence.evicted = true
			_, err := h.pub.Publish(t.Context(), statelog.Request{
				Subject:  probeSubject("a"),
				Scope:    statelog.ScopeSet{Paths: []string{"object.a"}},
				OpID:     "op-1",
				MintedAt: time.Now(),
				Pattern:  pattern,
				Decide: func(*sql.Tx) (statelog.Decision, error) {
					return statelog.Decision{Payload: []byte("x")}, nil
				},
			})
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonEvicted {
				t.Fatalf("write = %v, want an evicted refusal", err)
			}
			if got := h.appends.appends.Load(); got != 0 {
				t.Fatalf("appended %d time(s) while evicted", got)
			}
		})
	}

	// AND AN EVICTION THAT CANNOT BE READ BLOCKS. It is the third value:
	// an eviction nobody can read is not an eviction that did not happen,
	// and publishing under it produces durable records every node drops.
	t.Run("an unreadable eviction blocks", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.fence.evictErr = errors.New("coordination unreachable")
		_, err := h.write(probeSubject("a"), "op-1", "hello")
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonEvicted {
			t.Fatalf("write = %v, want an evicted refusal", err)
		}
		if got := h.appends.appends.Load(); got != 0 {
			t.Fatalf("appended %d time(s) under an unreadable eviction", got)
		}
	})
}

// A DEFERRED SCOPE REFUSES BEFORE AN EXPECTATION EXISTS, and the probe is
// over the SCOPE rather than the subject.
//
// This is the floor theorem's first clause. A record this node cannot decode
// makes the rows it touched permanently stale here, and if its position has
// also been trimmed, the retry-at-zero branch would fire and overwrite it —
// silently, with nothing anywhere reporting it and no later reprocess able to
// undo it.
func TestADeferredScopeRefusesBeforeAnyExpectation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.rows.set(func(s *statelog.Snap) {
		s.Deferred = true
		s.Deferral = statelog.Deferral{
			Position: statelog.Position{Stream: probeStream, Generation: 1, Seq: 42},
			Version:  9,
			// The deferred record's SUBJECT is b; the object being
			// written is a. There is nothing on a's own subject to
			// probe, which is exactly why the probe is over the scope.
			Scope: statelog.ScopeSet{Paths: []string{"object.b", "object.a"}},
		}
	})

	_, err := h.write(probeSubject("a"), "op-1", "hello")
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("write = %v, want an Unavailable", err)
	}
	if refusal.Reason != statelog.ReasonDeferred {
		t.Fatalf("reason = %q, want %q", refusal.Reason, statelog.ReasonDeferred)
	}
	if !strings.Contains(refusal.Detail, "9") {
		t.Errorf("the refusal does not name the record version an operator has "+
			"to run a build for: %q", refusal.Detail)
	}
	if got := h.appends.appends.Load(); got != 0 {
		t.Fatalf("appended %d time(s) with a deferred scope covering the object", got)
	}
	if got := h.fence.zeroes.Load(); got != 0 {
		t.Fatalf("reached the expectation-zero fence %d time(s) — the deferral "+
			"probe is what stops the branch being reached at all", got)
	}
}

// A CREATE IS GUARDED BY THE ROW, WHICH IS WHAT STILL HOLDS BELOW THE TRIM
// FLOOR.
//
// Once a claim's record is trimmed its subject holds nothing, an expectation
// of zero succeeds again, and the broker enforces no uniqueness at all. And
// the deletion half is not decoration: a purge deletes the guarding row while
// the marker is permanent, so without it a writer is told "it already exists"
// above the floor and "it succeeded" below it.
func TestACreateIsGuardedByTheRowAndByTheDeletionMarker(t *testing.T) {
	t.Parallel()

	create := func(h *harness) (statelog.Result, error) {
		return h.pub.Publish(h.t.Context(), statelog.Request{
			Subject:  probeSubject("a"),
			Scope:    statelog.ScopeSet{Paths: []string{"object.a"}},
			OpID:     "op-1",
			MintedAt: time.Now(),
			Pattern:  statelog.PatternCreate,
			Decide: func(*sql.Tx) (statelog.Decision, error) {
				return statelog.Decision{Payload: []byte("x")}, nil
			},
		})
	}

	t.Run("the guarding row refuses", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.rows.set(func(s *statelog.Snap) { s.Guard = true })
		if _, err := create(h); !errors.Is(err, statelog.ErrExists) {
			t.Fatalf("create over a guarding row = %v, want ErrExists", err)
		}
		if got := h.appends.appends.Load(); got != 0 {
			t.Fatalf("appended %d time(s) over an existing row", got)
		}
	})

	t.Run("a deletion marker refuses permanently", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// The guarding row is GONE — purged — and the marker is not.
		h.rows.set(func(s *statelog.Snap) { s.Deleted, s.Guard = true, false })
		_, err := create(h)
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonDeleted {
			t.Fatalf("create over a deletion marker = %v, want a deleted refusal", err)
		}
		if got := h.appends.appends.Load(); got != 0 {
			t.Fatalf("appended %d time(s) over a deletion marker", got)
		}
	})

	t.Run("a clear create publishes at zero, fenced", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		res, err := create(h)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("outcome = %q, want applied", res.Outcome)
		}
		if got := h.fence.zeroes.Load(); got != 1 {
			t.Fatalf("the expectation-zero fence ran %d time(s), want exactly 1 — "+
				"being wrong on this branch is a lost update rather than a "+
				"refused write, which is why it is the branch that pays", got)
		}
	})

	t.Run("and a fence that cannot answer refuses", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.fence.zeroErr = &statelog.Unavailable{
			Reason: statelog.ReasonFloorUnknown,
			Detail: "the trim floor could not be read",
		}
		_, err := create(h)
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonFloorUnknown {
			t.Fatalf("create under an unreadable floor = %v, want a floor_unknown "+
				"refusal — failing open here is a lost update, which is not "+
				"recoverable", err)
		}
		if got := h.appends.appends.Load(); got != 0 {
			t.Fatalf("appended %d time(s) under an unreadable floor", got)
		}
	})
}

// THE ORDINARY PATH PAYS NO COORDINATION READ, and the zero branch pays
// exactly one.
//
// The asymmetry is the whole cost argument: everywhere else a stale floor
// costs a refused write or an unnecessary snapshot; on the zero branch it
// costs a silent lost update. Asserting both halves is what stops the fence
// being "optimised" onto the hot path or off the cold one.
func TestTheOrdinaryWritePathTakesNoCoordinationRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// An anchor at a live sequence, which is what an ordinary second
	// write to an object has.
	first, err := h.write(probeSubject("a"), "op-1", "one")
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	h.fence.zeroes.Store(0)
	h.anchorAt(probeSubject("a"), first.Position.Seq)

	if _, err := h.write(probeSubject("a"), "op-2", "two"); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got := h.fence.zeroes.Load(); got != 0 {
		t.Fatalf("the ordinary path took %d coordination read(s), want 0", got)
	}
}

// A LOST RACE DRIVES THIS NODE'S APPLIER FORWARD BEFORE IT RE-DECIDES.
//
// Re-deciding with no wait re-reads the anchor the applier has not yet
// advanced — sixteen times, to a conflict — which is a refusal a model reads
// as a colleague editing the same object when nobody is.
func TestALostRaceWaitsForTheWinnerBeforeItRedecides(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A peer writes the object. This node has applied nothing, so its
	// anchor for the subject is empty while the subject is not — which
	// is exactly the state a lost race leaves behind.
	peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil, []byte("peer"))
	if err != nil {
		t.Fatalf("the peer's write: %v", err)
	}

	res, err := h.write(probeSubject("a"), "op-1", "mine")
	if err != nil {
		t.Fatalf("write after a lost race: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("outcome = %q, want applied", res.Outcome)
	}
	if res.Position.Seq <= peer {
		t.Fatalf("this write landed at %d, at or below the peer's %d",
			res.Position.Seq, peer)
	}
	if committed := h.applier.Committed(); committed.Seq < peer {
		t.Fatalf("this node committed through %d but the winner was at %d — the "+
			"re-decide did not wait, so it read the anchor the applier had not "+
			"advanced", committed.Seq, peer)
	}
	if got := h.fence.zeroes.Load(); got != 0 {
		t.Fatalf("published at zero %d time(s) over an occupied subject — the "+
			"probe must wait for the peer, never overwrite it", got)
	}
}

// A WRITE THAT LOSES EVERY ROUND IS TOLD SO, and is not retried for ever.
func TestAWriteThatKeepsLosingIsRefusedRatherThanRetriedForEver(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil, []byte("peer"))
	if err != nil {
		t.Fatalf("the peer's write: %v", err)
	}
	// A stale anchor that never moves: this node cannot catch up, which
	// is what a write storm on one object looks like from here.
	h.anchorAt(probeSubject("a"), peer-1)
	h.applier.mu.Lock()
	h.applier.frozen = true
	h.applier.mu.Unlock()

	_, err = h.write(probeSubject("a"), "op-1", "mine")
	if !errors.Is(err, statelog.ErrConflict) {
		t.Fatalf("write = %v, want ErrConflict", err)
	}
	if got := h.rows.snapshots(); got != 16 {
		t.Fatalf("took %d snapshots, want 16 — each round must decide again "+
			"from a fresh one, or the retry is the same decision repeated", got)
	}
}

// A RECORD THE TRANSPORT REFUSES FOR ITS SIZE IS REFUSED ON ITS FIRST APPEND,
// naming the limit.
//
// Through the real appender on a real broker, from both sides that refuse one:
// the client, against the max_payload its server announced, and the broker,
// against a max_msg_size somebody set on the stream. Either way nothing was
// stored and the same record is refused the same way every time, so another
// round only repeats the refusal. Read as no answer, the client's refusal would
// be decided again round after round, to a conflict that names neither the
// size nor the setting.
func TestARecordTooLargeForTheTransportIsRefusedOnceNamingTheLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// limit narrows the broker the harness runs, where the case needs
		// a limit the default one does not have.
		limit func(t *testing.T, h *harness)
		body  int
		// says is the setting the refusal must name.
		says string
	}{
		{name: "the client refuses it against its server's max_payload",
			body: queue.MaxPayloadBytes + 1, says: "max_payload"},
		{name: "the broker refuses it against the stream's max_msg_size",
			limit: func(t *testing.T, h *harness) { streamMaxMsgSize(t, h, 1<<10) },
			body:  2 << 10, says: "max_msg_size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if tc.limit != nil {
				tc.limit(t, h)
			}
			_, err := h.write(probeSubject("a"), "op-1", strings.Repeat("x", tc.body))
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) {
				t.Fatalf("a record too large for the transport returned %v, want "+
					"an Unavailable", err)
			}
			if refusal.Reason != statelog.ReasonTooLarge {
				t.Fatalf("reason = %q, want %q: %v", refusal.Reason,
					statelog.ReasonTooLarge, err)
			}
			if !strings.Contains(refusal.Detail, tc.says) {
				t.Errorf("the refusal does not name %s, the setting that refused "+
					"the record: %v", tc.says, err)
			}
			if refusal.OpID != "op-1" {
				t.Errorf("the refusal carries op id %q, want the write's own", refusal.OpID)
			}
			if got := h.appends.appends.Load(); got != 1 {
				t.Errorf("the publisher appended %d times, want 1 — a size "+
					"refusal is the same on every attempt, so the first one is "+
					"the answer", got)
			}
		})
	}
}

// streamMaxMsgSize sets a max_msg_size on the harness's log, which the engine
// never does, so the broker refuses a larger record itself.
func streamMaxMsgSize(t *testing.T, h *harness, limit int32) {
	t.Helper()
	updateProbeStream(t, h, "set its max_msg_size", func(cfg *jetstream.StreamConfig) {
		cfg.MaxMsgSize = limit
	})
}

// updateProbeStream changes the harness's log's configuration as the broker
// holds it, which is how a case puts the log into a state only an operator's
// hand on the stream reaches.
func updateProbeStream(t *testing.T, h *harness, what string, change func(*jetstream.StreamConfig)) {
	t.Helper()
	broker, err := jetstream.New(h.q.Conn())
	if err != nil {
		t.Fatalf("reach the JetStream API: %v", err)
	}
	s, err := broker.Stream(t.Context(), probeStream)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	info, err := s.Info(t.Context())
	if err != nil {
		t.Fatalf("read the log's configuration: %v", err)
	}
	cfg := info.Config
	change(&cfg)
	if _, err := broker.UpdateStream(t.Context(), cfg); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// AN EMPTY SCOPE IS REFUSED, because it is the one claim a record no build
// may be able to read cannot make.
func TestARecordMustDeclareWhatItMakesStale(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, err := h.pub.Publish(t.Context(), statelog.Request{
		Subject:  probeSubject("a"),
		OpID:     "op-1",
		MintedAt: time.Now(),
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return statelog.Decision{Payload: []byte("x")}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("write with no scope = %v, want a refusal naming the scope", err)
	}
}

// A DECISION THAT CHANGES NOTHING IS A SUCCESS AND PUBLISHES NOTHING.
func TestADecisionWithNothingToSayPublishesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res, err := h.pub.Publish(t.Context(), statelog.Request{
		Subject:  probeSubject("a"),
		Scope:    statelog.ScopeSet{Paths: []string{"object.a"}},
		OpID:     "op-1",
		MintedAt: time.Now(),
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return statelog.Decision{Version: 7}, nil
		},
	})
	if err != nil {
		t.Fatalf("a no-op write: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Version != 7 {
		t.Fatalf("result = %+v, want an applied no-op carrying the version it read", res)
	}
	if got := h.appends.appends.Load(); got != 0 {
		t.Fatalf("a no-op appended %d record(s)", got)
	}
}

// pubUnreachable makes the discriminator itself fail, which is what turns an
// ambiguous publish into the fourth arm.
func (h *harness) pubUnreachable() {
	h.appends.inner = unreachable{}
}

type unreachable struct{}

func (unreachable) Append(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
	return 0, false, errors.New("no response from stream")
}

func (unreachable) LastSeq(context.Context, string) (uint64, bool, error) {
	return 0, false, fmt.Errorf("no response from stream")
}

func (unreachable) At(context.Context, uint64) (string, []byte, time.Time, bool, error) {
	return "", nil, time.Time{}, false, fmt.Errorf("no response from stream")
}

// THE WAIT FOR A PEER'S POSITION IS BOUNDED, AND EXPIRY IS A REASON.
//
// # The failure this exists to catch
//
// A rejected append means a peer wrote in this generation and this node has
// not applied it, so re-deciding needs the subject's true last position. The
// wait for it ran on the CALLER'S OWN CONTEXT with no budget — and the state
// that produces it is an applier that has not caught up, which is unbounded by
// construction. A request with no deadline waited for ever, holding the
// caller's goroutine and its own snapshot; a request with a deadline got a
// bare cancellation where it needed the reason and the position.
//
// This is the one shape that catches it. A frozen applier answers instantly
// and never moves, which spends the round budget and ends in a conflict — the
// case above — and never touches the wait's own budget at all. Only an applier
// that does not ANSWER does.
func TestTheWaitForAPeersPositionIsBoundedAndSaysWhy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil, []byte("peer"))
	if err != nil {
		t.Fatalf("the peer's write: %v", err)
	}
	// The anchor is below what the log holds, so this write is behind a
	// peer's record — and the applier never answers, which is a node that
	// has stopped catching up rather than one losing races.
	h.anchorAt(probeSubject("a"), peer-1)
	h.applier.mu.Lock()
	h.applier.stalled = true
	h.applier.mu.Unlock()

	started := time.Now()
	// NO DEADLINE ON THE CALLER'S CONTEXT, deliberately: the budget under
	// test is the write path's own, and a context deadline here would be
	// the test supplying the bound it is meant to be checking.
	_, err = h.pub.Publish(context.Background(), statelog.Request{
		Subject:  probeSubject("a"),
		Scope:    statelog.ScopeSet{Paths: []string{"p/a"}},
		OpID:     "op-1",
		MintedAt: time.Now(),
		Pattern:  statelog.PatternArbitrated,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			return statelog.Decision{
				Payload:  []byte("mine"),
				Envelope: statelog.Envelope{Kind: "object", OpID: "op-1"},
			}, nil
		},
	})
	waited := time.Since(started)

	var unavailable *statelog.Unavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("the write returned %v, and a caller behind a peer needs the "+
			"typed refusal it reads a retry position out of", err)
	}
	if unavailable.Reason != statelog.ReasonBehind {
		t.Errorf("the refusal's reason is %q, want %q — the remedy differs: "+
			"behind is waited out, and every other reason is not",
			unavailable.Reason, statelog.ReasonBehind)
	}
	if unavailable.Position.Seq != peer {
		t.Errorf("the refusal names position %d and the peer's record is at "+
			"%d — a caller told it is behind with no number has nothing to "+
			"wait for", unavailable.Position.Seq, peer)
	}
	if waited > 5*time.Second {
		t.Fatalf("the write waited %s before refusing, and the budget is %s — "+
			"an unbounded wait blocks the caller for as long as this node "+
			"stays behind", waited, 250*time.Millisecond)
	}
}

// AN EVICTION THAT LANDS WHILE A WRITE DECIDES IS ANSWERED AS ONE, AT ONCE.
//
// The fence runs again before every append, because a write that has spent
// rounds losing races has been running for as long as they took. What it
// refuses is the answer: read as no answer at all, the write probes the
// subject, finds nothing of its own, retakes its snapshot and decides again —
// round after round, to a conflict a model reads as a colleague editing the
// same object, counted as one.
//
// Mutation: hand the fence's refusal to classify with the broker's errors and
// the write ends in ErrConflict after sixteen rounds and sixteen probes.
func TestAnEvictionMidWriteIsRefusedAsOneAtOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// AN ESTABLISHED OBJECT, so the expectation comes from its anchor and
	// the write asks the broker nothing before it appends.
	first, err := h.write(probeSubject("a"), "op-0", "one")
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	h.anchorAt(probeSubject("a"), first.Position.Seq)
	appended := h.appends.appends.Load()
	h.appends.probes.Store(0)

	res, err := h.pub.Publish(t.Context(), statelog.Request{
		Subject:  probeSubject("a"),
		Scope:    statelog.ScopeSet{Paths: []string{"object.a"}},
		OpID:     "op-1",
		MintedAt: time.Now(),
		Pattern:  statelog.PatternArbitrated,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			// THE EVICTION LANDS HERE, after the fence at the top of
			// the write has already passed.
			h.fence.mu.Lock()
			h.fence.evicted = true
			h.fence.mu.Unlock()
			return statelog.Decision{Payload: []byte("two"), Version: 1}, nil
		},
	})
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonEvicted {
		t.Fatalf("a write evicted while it decided answered %v, want an evicted "+
			"refusal", err)
	}
	if res.Rounds != 1 {
		t.Errorf("the refusal came after %d round(s), want 1 — the fence's answer "+
			"is the write's", res.Rounds)
	}
	if got := h.appends.probes.Load(); got != 0 {
		t.Errorf("the write probed the subject %d time(s) — a refusal this node "+
			"made is not an append whose outcome anybody has to discover", got)
	}
	if got := h.appends.appends.Load(); got != appended {
		t.Errorf("the write appended %d record(s) after its eviction", got-appended)
	}
}

// A BROKER REFUSAL THIS FRAMEWORK HAS NO REMEDY FOR IS ANSWERED IN THE BROKER'S
// OWN WORDS, once.
//
// A sealed stream refuses every append with an API error that is neither a lost
// race, nor a size, nor a store limit. Filed with the full log it carries the
// full log's remedy — raise a byte ceiling, unblock the trim — for a refusal
// neither setting causes; the broker's own words say which refusal it was.
//
// Mutation: file every other API error with the full log and the reason reads
// log_full.
func TestABrokerRefusalIsAnsweredInItsOwnWords(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sealProbeStream(t, h)

	_, err := h.write(probeSubject("a"), "op-1", "hello")
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("a write on a sealed stream answered %v, want an Unavailable", err)
	}
	if refusal.Reason != statelog.ReasonRefused {
		t.Fatalf("reason = %q, want %q: %v", refusal.Reason, statelog.ReasonRefused, err)
	}
	if !strings.Contains(refusal.Detail, "sealed") {
		t.Errorf("the refusal does not carry the broker's words, which name the "+
			"sealed stream: %v", err)
	}
	if refusal.OpID != "op-1" {
		t.Errorf("the refusal carries op id %q, want the write's own", refusal.OpID)
	}
	if got := h.appends.appends.Load(); got != 1 {
		t.Errorf("the publisher appended %d times, want 1 — the broker's decision "+
			"is the same on every attempt", got)
	}
}

// sealProbeStream seals the harness's log, which the engine never does, so the
// broker refuses every append with an error of its own.
func sealProbeStream(t *testing.T, h *harness) {
	t.Helper()
	updateProbeStream(t, h, "seal the log", func(cfg *jetstream.StreamConfig) {
		cfg.Sealed = true
	})
}

// brokerSays is an API error in the shape the client hands back for a
// refused acknowledgement.
func brokerSays(code jetstream.ErrorCode, description string) error {
	return fmt.Errorf("nats: %w", &jetstream.APIError{
		Code: 400, ErrorCode: code, Description: description,
	})
}

// The two answers the broker gives a publish that are not a decision about the
// record, as the server words them. A full ingest queue drops the message
// before it is stored; a message id still being proposed belongs to a record
// that may yet commit.
var (
	tooManyRequests   = brokerSays(10167, "too many requests")
	idStillInProgress = brokerSays(10158, "duplicate message id is in process")
)

// scripted answers each append from a script, in order, and delegates to the
// real log once the script is spent — which is how a case puts one broker
// answer the embedded broker cannot be driven into in front of a real one.
type scripted struct {
	log  statelog.Appender
	mu   sync.Mutex
	next []func(ctx context.Context, subject, msgID string, expect *uint64,
		body []byte) (uint64, bool, error)
	// always, when set, answers every append the script does not.
	always error
}

func (s *scripted) Append(ctx context.Context, subject, msgID string, expect *uint64,
	body []byte) (uint64, bool, error) {

	s.mu.Lock()
	var step func(context.Context, string, string, *uint64, []byte) (uint64, bool, error)
	if len(s.next) > 0 {
		step, s.next = s.next[0], s.next[1:]
	}
	always := s.always
	s.mu.Unlock()
	switch {
	case step != nil:
		return step(ctx, subject, msgID, expect, body)
	case always != nil:
		return 0, false, always
	}
	return s.log.Append(ctx, subject, msgID, expect, body)
}

func (s *scripted) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	return s.log.LastSeq(ctx, subject)
}

func (s *scripted) At(ctx context.Context, seq uint64) (string, []byte, time.Time, bool, error) {
	return s.log.At(ctx, seq)
}

// answer is a script step that stores nothing and says err.
func answer(err error) func(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
	return func(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
		return 0, false, err
	}
}

// established writes the object this case writes over, so the next write forms
// its expectation from the anchor and asks the broker nothing before its own
// append — which is what lets a case count the broker calls a refusal makes.
func established(t *testing.T, h *harness) statelog.Result {
	t.Helper()
	first, err := h.write(probeSubject("a"), "op-0", "one")
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	h.anchorAt(probeSubject("a"), first.Position.Seq)
	return first
}

// A FULL INGEST QUEUE IS WAITED OUT, NOT ANSWERED.
//
// The broker drops a record its stream's ingest queue cannot take and says so,
// and the same record is taken once the queue drains. So the write pauses and
// retakes its snapshot, and the caller hears only that it landed. The pause is
// the half that is easy to lose: retaken at once, the write meets the same
// queue in the same millisecond, and a queue that stays full spends every round
// on nothing and ends in a conflict a model reads as a colleague editing.
//
// Mutation: file the queue's answer as a refusal and the write is refused on
// its first attempt; drop the pause and it lands inside the first beat.
func TestAFullIngestQueueIsWaitedOutBeforeTheWriteRetakes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	established(t, h)
	appended := h.appends.appends.Load()
	h.appends.probes.Store(0)
	h.appends.fail(tooManyRequests, true)

	started := time.Now()
	res, err := h.write(probeSubject("a"), "op-1", "two")
	took := time.Since(started)
	if err != nil {
		t.Fatalf("a write the queue refused once answered %v, want it landed", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("outcome = %q, want applied", res.Outcome)
	}
	if got := h.appends.appends.Load() - appended; got != 2 {
		t.Errorf("the write appended %d times, want 2 — the refused one and the "+
			"retake that landed", got)
	}
	// THE PAUSE HAPPENED: its first beat is ApplyRetryBeat, jittered by a
	// fifth either way.
	if floor := statelog.ApplyRetryBeat * 4 / 5; took < floor {
		t.Errorf("the write retook after %s, inside the %s the first pause takes "+
			"at least — so it met the same full queue", took, floor)
	}
	if got := h.appends.probes.Load(); got != 0 {
		t.Errorf("the write probed the subject %d time(s) — a record the broker "+
			"said it did not store is not one whose outcome anybody has to "+
			"discover", got)
	}
}

// A QUEUE THAT STAYS FULL PAST THE BUDGET IS REFUSED AS BUSY, NAMING IT.
//
// Nothing was stored, and the refusal says so and says why — rather than
// holding the caller for as long as the broker stays saturated, or ending in a
// conflict after sixteen immediate retakes.
//
// Mutation: retake without pausing and the write ends in ErrConflict.
func TestAQueueThatStaysFullIsRefusedBusyWithinTheBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	established(t, h)
	appended := h.appends.appends.Load()
	h.appends.inner = &scripted{log: h.log, always: tooManyRequests}

	started := time.Now()
	res, err := h.write(probeSubject("a"), "op-1", "two")
	took := time.Since(started)
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("a write against a queue that stayed full answered %v (%+v), want "+
			"an Unavailable", err, res)
	}
	if refusal.Reason != statelog.ReasonBusy {
		t.Fatalf("reason = %q, want %q: %v", refusal.Reason, statelog.ReasonBusy, err)
	}
	for _, says := range []string{"stored nothing", "ingest queue", "too many requests"} {
		if !strings.Contains(refusal.Detail, says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
	if refusal.OpID != "op-1" {
		t.Errorf("the refusal carries op id %q, want the write's own", refusal.OpID)
	}
	if got := h.appends.appends.Load() - appended; got < 2 {
		t.Errorf("the write appended %d time(s) — a full queue is retaken after a "+
			"pause, not answered on its first refusal", got)
	}
	// THE HARNESS'S BUDGET IS A QUARTER OF A SECOND; a write held for
	// several of them is one the budget did not bound.
	if took > 2*time.Second {
		t.Errorf("the write was held %s against a full queue", took)
	}
}

// A QUEUE THAT REFUSES A WRITE ONE OF WHOSE ATTEMPTS WENT UNANSWERED LEAVES IT
// UNKNOWN, NOT REFUSED.
//
// The unanswered attempt's record may still arrive, so "stored nothing" is not
// something this write can say about itself: the honest answer is the third
// value, with the op id the retry collapses it by.
//
// Mutation: answer busy whatever came before, and a write whose record may yet
// land is reported as one that never will.
func TestAFullQueueAfterAnUnansweredAttemptLeavesTheWriteUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	established(t, h)
	h.appends.inner = &scripted{log: h.log,
		next:   []func(context.Context, string, string, *uint64, []byte) (uint64, bool, error){answer(errors.New("nats: timeout"))},
		always: tooManyRequests,
	}

	res, err := h.write(probeSubject("a"), "op-1", "two")
	if err != nil {
		t.Fatalf("write = %v, want the third value rather than a refusal", err)
	}
	if res.Outcome != statelog.OutcomeUnknown || res.OpID != "op-1" {
		t.Fatalf("result = %+v, want unknown carrying op-1", res)
	}
}

// A WRITE WHOSE PROPOSAL WAS STILL IN FLIGHT, AND WHOSE RETAKES MEET A FULL
// QUEUE UNTIL THE BUDGET RUNS OUT, IS UNKNOWN — NOT BUSY.
//
// The broker answered the first attempt that a record under this op id was
// still being proposed, and nothing had landed when the write looked. That
// proposal may still commit after every later attempt was refused, so "stored
// nothing" is not something the write can say about itself: it answers the
// third value, with the op id a retry collapses it by.
//
// Mutation: drop the in-flight branch's mark on the write's pacing and the
// write is refused busy, a record that may yet land reported as one that never
// will.
func TestAnInFlightProposalThenAFullQueueLeavesTheWriteUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	established(t, h)
	h.appends.inner = &scripted{log: h.log,
		next:   []func(context.Context, string, string, *uint64, []byte) (uint64, bool, error){answer(idStillInProgress)},
		always: tooManyRequests,
	}

	res, err := h.write(probeSubject("a"), "op-1", "two")
	if err != nil {
		t.Fatalf("write = %v, want the third value rather than a refusal", err)
	}
	if res.Outcome != statelog.OutcomeUnknown || res.OpID != "op-1" {
		t.Fatalf("result = %+v, want unknown carrying op-1", res)
	}
	if res.Position.Seq != 0 {
		t.Errorf("an unknown write names position %d, and nothing was "+
			"established about one", res.Position.Seq)
	}
}

// A MESSAGE ID STILL BEING PROPOSED IS RESOLVED, NOT REFUSED — BY THE LEDGER,
// OR BY THE DUPLICATE ACKNOWLEDGEMENT A LATER APPEND GETS.
//
// A clustered leader answers a publish carrying a message id it is still
// proposing with a conflict, and the first proposal may still commit. Filed
// as a refusal, the caller is told nothing landed about a record that will.
func TestAMessageIDStillBeingProposedIsResolvedRatherThanRefused(t *testing.T) {
	t.Parallel()

	// Mutation: skip the resolution in the in-flight branch and go straight
	// to the pause — the next round's snapshot answers the same position
	// from the ledger, so only the probe count tells the two paths apart.
	t.Run("the ledger, when the first record has landed", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		established(t, h)
		appended := h.appends.appends.Load()
		h.appends.probes.Store(0)
		// THE FIRST PROPOSAL COMMITTED AND THIS NODE APPLIED IT; this
		// attempt's own answer was the conflict.
		h.appends.fail(idStillInProgress, false)

		res, err := h.write(probeSubject("a"), "op-1", "two")
		if err != nil {
			t.Fatalf("write = %v, want it resolved from the ledger", err)
		}
		if res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("outcome = %q, want applied", res.Outcome)
		}
		last, _, err := h.log.LastSeq(t.Context(), probePrefix+".object.a")
		if err != nil {
			t.Fatalf("read the subject: %v", err)
		}
		if res.Position.Seq != last {
			t.Errorf("the write answered position %d and its record is at %d",
				res.Position.Seq, last)
		}
		// WHAT ONLY THE RESOLUTION DOES: it reads the subject back in the
		// round that met the conflict, and appends nothing more.
		if got := h.appends.appends.Load() - appended; got != 1 {
			t.Errorf("the write appended %d time(s), want 1 — the in-flight "+
				"record is its own and nothing else was sent", got)
		}
		if got := h.appends.probes.Load(); got != 1 {
			t.Errorf("the write probed the subject %d time(s), want 1 — the "+
				"in-flight answer is resolved by reading the subject back in the "+
				"round that met it, not by pausing for a later round", got)
		}
		if res.Rounds != 1 {
			t.Errorf("the write took %d round(s), want 1", res.Rounds)
		}
	})

	t.Run("the duplicate acknowledgement, when it lands during the pause", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first := established(t, h)
		subject := probePrefix + ".object.a"
		landedAt := uint64(0)
		h.appends.inner = &scripted{log: h.log,
			next: []func(context.Context, string, string, *uint64, []byte) (uint64, bool, error){
				// THE PROPOSAL IS STILL IN FLIGHT: nothing has landed.
				answer(idStillInProgress),
				// AND IT COMMITS BEFORE THE RETAKE ARRIVES, which then
				// meets it.
				func(ctx context.Context, subj, msgID string, expect *uint64, body []byte) (uint64, bool, error) {
					at := first.Position.Seq
					seq, _, err := h.log.Append(ctx, subj, msgID, &at, body)
					if err != nil {
						return 0, false, err
					}
					landedAt = seq
					return h.log.Append(ctx, subj, msgID, expect, body)
				},
			},
		}

		res, err := h.write(probeSubject("a"), "op-1", "two")
		if err != nil {
			t.Fatalf("write = %v, want it resolved to the first record", err)
		}
		if res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("outcome = %q, want applied", res.Outcome)
		}
		if landedAt == 0 {
			t.Fatal("the in-flight record never landed, so this case is not the " +
				"shape it names")
		}
		if res.Position.Seq != landedAt {
			t.Errorf("the write answered position %d, want %d — the record the "+
				"first proposal committed, which the duplicate acknowledgement names",
				res.Position.Seq, landedAt)
		}
		last, _, err := h.log.LastSeq(t.Context(), subject)
		if err != nil {
			t.Fatalf("read the subject: %v", err)
		}
		if last != landedAt {
			t.Errorf("the subject's last record is %d and the operation's is %d — "+
				"one operation landed twice", last, landedAt)
		}
	})

	t.Run("the third value, when it is still pending at the budget", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		established(t, h)
		h.appends.inner = &scripted{log: h.log, always: idStillInProgress}

		res, err := h.write(probeSubject("a"), "op-1", "two")
		if err != nil {
			t.Fatalf("write = %v, want the third value rather than a refusal", err)
		}
		if res.Outcome != statelog.OutcomeUnknown || res.OpID != "op-1" {
			t.Fatalf("result = %+v, want unknown carrying op-1 — a record under "+
				"this op id may still commit", res)
		}
		if res.Position.Seq != 0 {
			t.Errorf("an unknown write names position %d, and nothing was "+
				"established about one", res.Position.Seq)
		}
	})
}

// shortWindow is the probe domain on a log whose duplicate window is the
// broker's own minimum, so a case can outlive it.
type shortWindow struct{ probeDomain }

// shortWindowDuplicates is the broker's floor for a stream's duplicate window:
// it refuses a smaller one.
const shortWindowDuplicates = 100 * time.Millisecond

func (shortWindow) Stream() statelog.StreamSpec {
	spec := probeDomain{}.Stream()
	spec.Duplicates = shortWindowDuplicates
	return spec
}

// A RETRY OF AN OPERATION THIS NODE HAS APPLIED PUBLISHES NOTHING, HOWEVER LATE
// IT COMES.
//
// Inside the broker's duplicate window a retry under the same op id is
// collapsed by the broker; past it the broker has forgotten the id, and a
// retry that decided again would land a second record of one operation and
// hand its caller that second decision's values while every node also holds
// the first's. The snapshot reads this node's ledger first, so the retry is
// answered at the first record's position whatever the window says.
//
// Mutation: drop the publisher's answer from the ledger and the retry appends
// a second record.
func TestARetryPastTheDuplicateWindowPublishesNothing(t *testing.T) {
	t.Parallel()
	h := newHarnessFor(t, shortWindow{})
	subject := probePrefix + ".object.a"

	first, err := h.write(probeSubject("a"), "op-1", "one")
	if err != nil {
		t.Fatalf("the first attempt: %v", err)
	}
	if first.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the first attempt answered %q, want applied", first.Outcome)
	}
	appended := h.appends.appends.Load()
	h.appends.probes.Store(0)

	// PAST THE WINDOW: the broker purges an id once it is a window old, on a
	// timer of the same period, so several windows is past it.
	time.Sleep(5 * shortWindowDuplicates)

	retried, err := h.write(probeSubject("a"), "op-1", "one, decided again")
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if retried.Outcome != statelog.OutcomeApplied || retried.Position != first.Position {
		t.Fatalf("the retry answered %q at %s, want applied at the first "+
			"attempt's %s", retried.Outcome, retried.Position, first.Position)
	}
	if retried.Version != first.Version {
		t.Errorf("the retry answered version %d and the first attempt %d — one "+
			"operation has one version", retried.Version, first.Version)
	}
	if got := h.appends.appends.Load() - appended; got != 0 {
		t.Errorf("the retry appended %d record(s) of an operation this node had "+
			"already applied", got)
	}
	if got := h.appends.probes.Load(); got != 0 {
		t.Errorf("the retry asked the broker %d time(s) about a write this "+
			"node's own ledger answers", got)
	}
	last, _, err := h.log.LastSeq(t.Context(), subject)
	if err != nil {
		t.Fatalf("read the subject: %v", err)
	}
	if last != first.Position.Seq {
		t.Errorf("the subject's last record is %d and the operation's is %d — "+
			"one operation landed twice", last, first.Position.Seq)
	}

	// THE CONTROL: the broker really has forgotten the id, or the case above
	// is the window's work rather than the ledger's.
	_, duplicate, err := h.log.Append(t.Context(), probePrefix+".object.control",
		"op-1", nil, []byte("control"))
	if err != nil {
		t.Fatalf("append the control: %v", err)
	}
	if duplicate {
		t.Fatal("the broker still remembers op-1, so this case never left the " +
			"duplicate window it is named for")
	}
}

// enveloped is a probe record whose envelope names its own operation, which is
// what [statelog.Publisher.Landed] checks a record against. The value rides
// the one field nothing in these cases reads.
func enveloped(t *testing.T, opID, value string) string {
	t.Helper()
	body, err := json.Marshal(statelog.Envelope{
		V: 1, Kind: "object", Op: value, OpID: opID,
		Subject: probeSubject("a"),
		Scope:   statelog.ScopeSet{Paths: []string{"object/a"}},
	})
	if err != nil {
		t.Fatalf("encode a probe record: %v", err)
	}
	return string(body)
}

// A WRITE'S CALLER READS BACK THE RECORD ITS RESULT NAMES, AND ONLY ITS OWN.
//
// A value a decision computed is recovered from the record that landed, never
// from the closure, because a write answered from the ledger ran no decide
// that landed. So the read-back has to refuse every result it cannot vouch
// for: one with no position, one whose record is some other operation's, one
// the log no longer holds, and one from a generation the log has left.
//
// Mutation: drop the operation check and another write's record is handed
// back as this one's; drop the held check and a trimmed record fails as an
// undecodable envelope rather than as unavailable.
func TestLandedReadsBackTheRecordTheWriteResolvedTo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := enveloped(t, "op-1", "the first")
	first, err := h.write(probeSubject("a"), "op-1", body)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := h.pub.Landed(t.Context(), first)
	if err != nil {
		t.Fatalf("read back an applied write: %v", err)
	}
	if string(got) != body {
		t.Fatalf("read back %q, want the record the write published, %q", got, body)
	}

	for _, tc := range []struct {
		name string
		res  statelog.Result
	}{
		{"a result with no position", statelog.Result{
			Outcome: statelog.OutcomeUnknown, OpID: "op-1"}},
		{"a result naming another operation's record", statelog.Result{
			Outcome: statelog.OutcomeApplied, Position: first.Position, OpID: "op-2"}},
		{"a result from a generation the log has left", statelog.Result{
			Outcome: statelog.OutcomeApplied, OpID: "op-1",
			Position: statelog.Position{Stream: probeStream,
				Generation: first.Position.Generation + 1, Seq: first.Position.Seq}}},
	} {
		if payload, err := h.pub.Landed(t.Context(), tc.res); err == nil {
			t.Errorf("%s read back %q", tc.name, payload)
		}
	}

	// A RECORD THE LOG NO LONGER HOLDS is unavailable, which a caller can
	// tell from a broker that did not answer.
	if _, err := h.write(probeSubject("a"), "op-2", enveloped(t, "op-2", "the second")); err != nil {
		t.Fatalf("a second write: %v", err)
	}
	if err := h.log.Purge(t.Context(), first.Position.Seq+1); err != nil {
		t.Fatalf("trim the first record: %v", err)
	}
	if _, err := h.pub.Landed(t.Context(), first); !errors.Is(err, statelog.ErrUnavailable) {
		t.Errorf("a trimmed record read back as %v, want it unavailable", err)
	}
}

// THE SNAPSHOT READS THE OPERATION LEDGER IN ITS OWN TRANSACTION, AND AN
// APPLIED OPERATION IS NOT DECIDED AGAIN.
//
// Over the framework's own read seam and a real estate, because the fakes the
// publisher's cases run on are the publisher's seams and this is the seam's
// own contract: a ledger hit answers where the operation applied and never
// runs the domain's decide, and a miss decides exactly as a first attempt
// does.
//
// Mutation: drop the ledger read from the snapshot and the applied operation
// is decided again.
func TestTheSnapshotAnswersAnAppliedOperationFromItsLedger(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	if err := h.run(1); err != nil {
		t.Fatalf("apply op-1: %v", err)
	}
	rows, err := statelog.NewRows(h.db, probeDomain{}, nil)
	if err != nil {
		t.Fatalf("NewRows: %v", err)
	}
	scope := statelog.ScopeSet{Paths: []string{"object/a"}}
	decided := 0
	decide := func(*sql.Tx) (statelog.Decision, error) {
		decided++
		return statelog.Decision{Payload: []byte("again"), Version: 1}, nil
	}

	applied, err := rows.Snapshot(t.Context(), probeSubject("a"), scope, "op-1", decide)
	if err != nil {
		t.Fatalf("snapshot for op-1: %v", err)
	}
	if !applied.AlreadyApplied || applied.Applied.Seq != 1 {
		t.Fatalf("the snapshot for an applied operation reports %+v, want it "+
			"applied at sequence 1", applied)
	}
	if decided != 0 || !applied.Decision.Empty() {
		t.Fatalf("the domain decided %d time(s) about an operation that had "+
			"already landed", decided)
	}

	fresh, err := rows.Snapshot(t.Context(), probeSubject("a"), scope, "op-2", decide)
	if err != nil {
		t.Fatalf("snapshot for op-2: %v", err)
	}
	if fresh.AlreadyApplied || decided != 1 || fresh.Decision.Empty() {
		t.Fatalf("an operation this node never applied reads as applied=%v with "+
			"%d decision(s), want it decided once", fresh.AlreadyApplied, decided)
	}
}
