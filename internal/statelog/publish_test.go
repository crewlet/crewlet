package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
