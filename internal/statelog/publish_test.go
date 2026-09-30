package statelog_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
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
		// THE GATE READER IS HANDED THE BODY THIS CALL APPENDED — the
		// decision's own bytes, not the signed frame — which is what lets a
		// reader whose gate is keyed on what the payload names (the
		// identity estate's claims, about a person their subject does not
		// name) say which gate dropped the record.
		h.gates.mu.Lock()
		bodies := h.gates.bodies
		h.gates.mu.Unlock()
		if len(bodies) == 0 || !bytes.Contains(bodies[len(bodies)-1], []byte("hello")) {
			t.Fatalf("the gate reader was handed %q, want the body this call appended",
				bodies)
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
				Subject: probeSubject("a"),
				Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
				OpID:    "op-1",
				Pattern: pattern,
				Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
					return statelog.Decision{Payload: probeRecord(stamp, "op-1", "x")}, nil
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
	//
	// BUT IT IS NOT AN EVICTION, and the refusal says so: an eviction is
	// permanent until an operator readmits the node, and a coordination
	// blip is gone in seconds. Answered as `evicted`, every write during
	// the blip was a 503 with no Retry-After — "waiting cannot clear
	// this" — which is the signal a client gives up on.
	t.Run("an unreadable eviction blocks and says to come back", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.fence.evictErr = errors.New("open /var/lib/crewlet/replicated.db: database is locked")
		_, err := h.write(probeSubject("a"), "op-1", "hello")
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonEvictionUnknown {
			t.Fatalf("write = %v, want an eviction_unknown refusal", err)
		}
		// IN THIS PACKAGE'S WORDS: every surface sends a refusal's detail
		// to the caller, and the fence's error is the store's own — a
		// database path here — which belongs in the log.
		if strings.Contains(err.Error(), "/var/lib") {
			t.Errorf("the refusal carries the store's own words to the caller: %q", err)
		}
		if got := h.appends.appends.Load(); got != 0 {
			t.Fatalf("appended %d time(s) under an unreadable eviction", got)
		}
		if got := statelog.RetryAfter(err, 2*time.Second); got == 0 {
			t.Error("a write refused on an eviction state nobody could read " +
				"carries no Retry-After, which tells a client waiting cannot " +
				"clear a refusal the next read clears")
		}
	})
}

// A NODE WHOSE LOG WAS REBUILT UNDER IT REFUSES EVERY WRITE, before the append
// and whatever the pattern.
//
// Fencing only the retry at zero is the tempting version, because it is the
// branch the floor theorem names. Every other pattern is as meaningless on a
// rebuilt log: an ordinary expectation is a sequence from the old history the
// broker compares with the new one — and ACCEPTS where the two happen to be
// equal — and an additive record is resolved against a checkpoint from the
// old history, which reads a low sequence as already applied.
func TestARebuiltLogRefusesEveryWriteBeforeTheAppend(t *testing.T) {
	t.Parallel()
	for name, pattern := range map[string]statelog.Pattern{
		"arbitrated": statelog.PatternArbitrated,
		"create":     statelog.PatternCreate,
		"additive":   statelog.PatternAdditive,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			// A CURRENT ROW, so the ordinary branch would form an
			// expectation the broker accepts: without the fence this
			// write lands, which is what makes the refusal the fence's.
			first, err := h.write(probeSubject("a"), "op-0", "before")
			if err != nil {
				t.Fatalf("the write before the rebuild: %v", err)
			}
			h.applier.rebuilt()
			before := h.appends.appends.Load()

			_, err = h.pub.Publish(t.Context(), statelog.Request{
				Subject: probeSubject("a"),
				Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
				OpID:    "op-1",
				Pattern: pattern,
				Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
					return statelog.Decision{Payload: probeRecord(stamp, "op-1", "x"), Version: 1}, nil
				},
			})
			requireWrongStream(t, err)
			if got := h.appends.appends.Load() - before; got != 0 {
				t.Fatalf("appended %d time(s) onto a log this node's rows are not "+
					"keyed to", got)
			}
			if _, last, err := h.log.Bounds(t.Context()); err != nil ||
				last != first.Position.Seq {
				t.Fatalf("the log ends at %d (err %v), want %d — a record landed",
					last, err, first.Position.Seq)
			}
		})
	}
}

// A REFUSAL FENCE 0 MAKES AT THE APPEND IS RETURNED AS ITSELF, IN THE ROUND IT
// WAS MADE — whatever the pattern, and whichever expectation the round formed.
//
// Fence 0 runs again before every append because a write is not instant: the
// heartbeat can find the log rebuilt, and a peer can commit this node's
// eviction, after the check at the top of the write has passed. What it finds
// there is this node's own decision, taken before anything reached the broker,
// so it is not an ambiguous publish. Read as one — every error that is not the
// broker's own is "no answer" to the classifier — the write asked the log what
// landed, found nothing, took a fresh snapshot, was refused again, and spent
// every round to report a CONFLICT: a colleague editing the object, told to a
// caller whose write this node refused for a reason of its own and whose remedy
// is an operator's.
func TestFenceZeroAtTheAppendRefusesAsItselfInTheSameRound(t *testing.T) {
	t.Parallel()
	// Each trip lands inside the decision, which runs after the check at
	// the top of the write and before the append — the window the check at
	// the append exists for.
	trips := map[string]struct {
		trip    func(*harness)
		require func(*testing.T, error)
	}{
		"the log was rebuilt": {
			trip:    func(h *harness) { h.applier.rebuilt() },
			require: requireWrongStream,
		},
		"the node was evicted": {
			trip: func(h *harness) {
				h.fence.mu.Lock()
				defer h.fence.mu.Unlock()
				h.fence.evicted = true
			},
			require: requireEvicted,
		},
		"the eviction became unreadable": {
			trip: func(h *harness) {
				h.fence.mu.Lock()
				defer h.fence.mu.Unlock()
				h.fence.evictErr = errors.New("coordination unreachable")
			},
			require: requireEvictionUnknown,
		},
	}
	patterns := map[string]statelog.Pattern{
		"arbitrated": statelog.PatternArbitrated,
		"create":     statelog.PatternCreate,
		"additive":   statelog.PatternAdditive,
	}
	// A WRITTEN SUBJECT reaches the append with the anchor as its
	// expectation, and a FRESH one with zero — past a zero fence that
	// cleared — so both roads to the append are covered. The additive
	// pattern forms no expectation on either.
	for tripName, tc := range trips {
		for patternName, pattern := range patterns {
			for _, written := range []bool{true, false} {
				name := tripName + "/" + patternName + "/fresh subject"
				if written {
					name = tripName + "/" + patternName + "/written subject"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					h := newHarness(t)
					if written {
						if _, err := h.write(probeSubject("a"), "op-0", "before"); err != nil {
							t.Fatalf("the write before the trip: %v", err)
						}
					}
					snapshots, appends := h.rows.snapshots(), h.appends.appends.Load()
					_, end, err := h.log.Bounds(t.Context())
					if err != nil {
						t.Fatalf("read the log's end: %v", err)
					}

					res, err := h.pub.Publish(t.Context(), statelog.Request{
						Subject: probeSubject("a"),
						Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
						OpID:    "op-1",
						Pattern: pattern,
						Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
							tc.trip(h)
							return statelog.Decision{Payload: probeRecord(stamp, "op-1", "x"), Version: 1}, nil
						},
					})
					tc.require(t, err)
					if errors.Is(err, statelog.ErrConflict) {
						t.Fatalf("write = %v answers ErrConflict — a caller retries "+
							"that against a colleague who does not exist", err)
					}
					if res.Rounds != 1 {
						t.Fatalf("the write ran %d round(s), want 1 — a refusal this "+
							"node made is final in the round it was made", res.Rounds)
					}
					if got := h.rows.snapshots() - snapshots; got != 1 {
						t.Fatalf("the write took %d snapshot(s), want 1 — it was retaken "+
							"as though nothing had answered", got)
					}
					if got := h.appends.appends.Load() - appends; got != 0 {
						t.Fatalf("appended %d time(s) past a fence that refused", got)
					}
					if _, last, err := h.log.Bounds(t.Context()); err != nil || last != end {
						t.Fatalf("the log ends at %d (err %v), want %d — a record landed",
							last, err, end)
					}
				})
			}
		}
	}
}

// requireEvicted is the refusal an evicted node earns, and an unreadable
// eviction with it.
func requireEvicted(t *testing.T, err error) {
	t.Helper()
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonEvicted {
		t.Fatalf("write = %v, want a %s refusal", err, statelog.ReasonEvicted)
	}
}

// requireEvictionUnknown is an eviction state that could not be read: its own
// reason, which clears when the state is read again, rather than `evicted`,
// which tells the caller to give up on this node.
func requireEvictionUnknown(t *testing.T, err error) {
	t.Helper()
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonEvictionUnknown {
		t.Fatalf("write = %v, want a %s refusal", err, statelog.ReasonEvictionUnknown)
	}
}

// A REBUILD THE ZERO FENCE'S OWN READ FINDS IS REFUSED IN THAT ROUND, before
// anything is appended at zero.
//
// The fence 0 at the top of a write answers from the last reading of the
// stream's instant, which on a running node is the heartbeat's — so a log
// rebuilt since the last beat passes it. On an ordinary expectation that window
// needs a coincidence to do harm; on an expectation of zero it needs nothing,
// because a rebuilt log holds nothing on any subject and the floor the fence
// clears against is a number from the old history. The zero fence's read of the
// log is where a real node first sees the rebuild (it carries the creation
// instant to the applier), and the fence 0 the attempt runs before its append
// asks the identity after that read — so the write is refused in the round that
// found it. Without the read reaching the applier, or without the check at the
// append, both cases below land at zero on the rebuilt log.
//
// What this does NOT exercise is the identity the publisher asks over the zero
// fence's own REFUSAL: here the fence clears, and that ask is what
// [TestARebuildOutranksTheZeroFencesEndCheck] holds.
func TestARebuildTheZeroFencesOwnReadFindsIsRefusedInThatRound(t *testing.T) {
	t.Parallel()

	// THE CONTROL: the same writes on a log nobody rebuilt land at zero,
	// so a fence that refused everything would not pass the cases below.
	t.Run("control: an unrebuilt log is written at zero", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.fence.reads = func() {}
		res, err := h.write(probeSubject("fresh"), "op-1", "hello")
		if err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("write = %+v, %v; want applied", res, err)
		}
	})

	t.Run("an expectation of zero", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// The subject was never written, so the write reaches the fence
		// with no append attempted — and the fence's read of the log is
		// what finds the rebuild.
		h.fence.reads = h.applier.rebuilt
		res, err := h.write(probeSubject("fresh"), "op-1", "hello")
		requireWrongStream(t, err)
		if got := h.fence.zeroes.Load(); got != 1 {
			t.Fatalf("the zero fence ran %d time(s), want 1 — this case is about "+
				"what happens after it", got)
		}
		requireRefusedInTheRoundThatFoundIt(t, h, res)
		if got := h.appends.appends.Load(); got != 0 {
			t.Fatalf("appended %d time(s) at zero onto a log the fence's own "+
				"read found rebuilt", got)
		}
	})

	t.Run("a retry at zero", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// The row names a record at 7 that the log does not hold — the
		// rebuilt log's shape, and a trimmed anchor's.
		h.anchorAt(probeSubject("quiet"), 7)
		h.fence.reads = h.applier.rebuilt
		res, err := h.write(probeSubject("quiet"), "op-1", "again")
		requireWrongStream(t, err)
		requireRefusedInTheRoundThatFoundIt(t, h, res)
		for _, expect := range h.appends.expectations() {
			if expect != nil && *expect == 0 {
				t.Fatal("the write was retried at zero after the fence's own read " +
					"found the log rebuilt — the append that overwrites a " +
					"subject from nothing")
			}
		}
		if _, last, err := h.log.Bounds(t.Context()); err != nil || last != 0 {
			t.Fatalf("the log ends at %d (err %v), want 0", last, err)
		}
	})
}

// requireRefusedInTheRoundThatFoundIt is a refusal made in the first round,
// from its one snapshot — never one the write reached after retaking it.
func requireRefusedInTheRoundThatFoundIt(t *testing.T, h *harness, res statelog.Result) {
	t.Helper()
	if res.Rounds != 1 {
		t.Fatalf("the write ran %d round(s), want 1 — the rebuild was found in "+
			"the first", res.Rounds)
	}
	if got := h.rows.snapshots(); got != 1 {
		t.Fatalf("the write took %d snapshot(s), want 1", got)
	}
}

// requireWrongStream is the refusal a rebuilt log earns, in all three forms a
// caller can recognise it by.
func requireWrongStream(t *testing.T, err error) {
	t.Helper()
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonWrongStream {
		t.Fatalf("write = %v, want a %s refusal", err, statelog.ReasonWrongStream)
	}
	if !errors.Is(err, statelog.ErrStreamRecreated) {
		t.Fatalf("write = %v, which does not answer errors.Is(%v) — the refusal "+
			"carries its cause so a caller need not switch on a reason",
			err, statelog.ErrStreamRecreated)
	}
	if !errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("write = %v, which does not answer errors.Is(%v)",
			err, statelog.ErrUnavailable)
	}
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
			Subject: probeSubject("a"),
			Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
			OpID:    "op-1",
			Pattern: statelog.PatternCreate,
			Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				return statelog.Decision{Payload: probeRecord(stamp, "op-1", "x")}, nil
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
		Subject: probeSubject("a"),
		OpID:    "op-1",
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{Payload: probeRecord(stamp, "op-1", "x")}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("write with no scope = %v, want a refusal naming the scope", err)
	}
}

// AN OPERATION ID CARRIED TO A SECOND OBJECT IS REFUSED, NOT ANSWERED WITH THE
// FIRST OBJECT'S RECORD — and the same id on the same object is still the same
// operation.
//
// The ledger keeps an id's first row and answers a retry from it, before the
// domain decides anything. A write to b under the id a write to a had used
// found a's row there and was answered `applied` at a's position — a write to
// b reported done, with nothing written to b at all. An operation id is the
// caller's — a seat carrying one forward, a route taking `?op_id=` — so this
// is a caller's mistake the framework has to refuse rather than confirm.
func TestAnOperationIDReusedOnAnotherObjectIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	first, err := h.write(probeSubject("a"), "op-shared", "one")
	if err != nil || first.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the first write: %v (%+v)", err, first)
	}
	appended := h.appends.appends.Load()

	res, err := h.write(probeSubject("b"), "op-shared", "two")
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonOpReused {
		t.Fatalf("a write to b under a's operation id answered (%+v, %v), want an "+
			"op_reused refusal", res, err)
	}
	if res.Outcome == statelog.OutcomeApplied {
		t.Fatalf("a write to b was reported applied at %s, a's record", res.Position)
	}
	if n := h.appends.appends.Load(); n != appended {
		t.Fatalf("the refused write reached the broker %d time(s) — the ledger "+
			"answers it before anything is decided", n-appended)
	}

	again, err := h.write(probeSubject("a"), "op-shared", "one")
	if err != nil || again.Outcome != statelog.OutcomeApplied ||
		again.Position != first.Position || !again.Collapsed {
		t.Fatalf("the same operation on the same object answered (%+v, %v), want "+
			"applied at %s and collapsed — a retry is the same write",
			again, err, first.Position)
	}
}

// AND SO IS ONE WHOSE REUSE ONLY THE BROKER'S ANSWER SHOWS.
//
// A write whose snapshot was taken before the first object's record landed
// finds no row, decides, and appends under the same id — which the broker's
// duplicate window collapses onto the first record, acknowledging a's
// sequence. Resolved from the ledger, that answered `applied` at a's position
// too. The resolution judges the row exactly as the snapshot does.
func TestAReusedOperationIDTheBrokerCollapsedIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var fired atomic.Bool
	var first statelog.Result
	var firstErr error
	h.rows.mu.Lock()
	h.rows.afterSnapshot = func() {
		// a's WRITE LANDS BETWEEN b's SNAPSHOT AND b's APPEND, which is
		// the one ordering the snapshot's own check cannot see. Once only,
		// and not under a sync.Once: a's own snapshot runs this hook too.
		if fired.CompareAndSwap(false, true) {
			first, firstErr = h.write(probeSubject("a"), "op-shared", "one")
		}
	}
	h.rows.mu.Unlock()

	res, err := h.write(probeSubject("b"), "op-shared", "two")
	if firstErr != nil || first.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the first write: %v (%+v)", firstErr, first)
	}
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonOpReused {
		t.Fatalf("a write to b the broker collapsed onto a's record answered "+
			"(%+v, %v), want an op_reused refusal", res, err)
	}
	if refusal.Position != first.Position {
		t.Errorf("the refusal names %s, want a's record at %s", refusal.Position,
			first.Position)
	}
}

// A DECISION THAT CHANGES NOTHING IS A SUCCESS AND PUBLISHES NOTHING.
func TestADecisionWithNothingToSayPublishesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res, err := h.pub.Publish(t.Context(), statelog.Request{
		Subject: probeSubject("a"),
		Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
		OpID:    "op-1",
		Decide: func(*sql.Tx, statelog.Stamp) (statelog.Decision, error) {
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
		Subject: probeSubject("a"),
		Scope:   statelog.ScopeSet{Paths: []string{"p/a"}},
		OpID:    "op-1",
		Pattern: statelog.PatternArbitrated,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{
				Payload: probeRecord(stamp, "op-1", "mine"),
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

// A NO-WAIT WRITE ANSWERS PENDING WITHOUT ENTERING THE WAIT AT ALL.
//
// The difference from the ordinary pending above is not the outcome — both are
// durable at a position and unresolved here — it is that this one never asked.
// A sign-in is what it exists for: the cookie minted from the record carries
// its position, every node validates the bearer against its own applier, and
// the row this node would be waiting for is a row nothing in the answer reads.
func TestAFlaggedWriteAnswersPendingWithoutWaiting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// THE APPLIER IS LEFT RUNNING, deliberately. Stopping it would make
	// this pass for the ordinary reason — a wait that timed out — and the
	// claim is that no wait happened, which is only visible while a wait
	// WOULD have succeeded.
	res, err := h.writeNoWait(probeSubject("a"), "op-1", "hello")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.Outcome != statelog.OutcomePending {
		t.Fatalf("outcome = %q, want pending", res.Outcome)
	}
	if res.Position.Seq == 0 {
		t.Error("a no-wait write has no position, so its caller has nothing " +
			"to carry to whoever reads next")
	}
	if res.Waited {
		t.Error("the write reports that it waited; a caller reading this as " +
			"a node falling behind would go and investigate a skip somebody " +
			"asked for")
	}

	// THE CONTROL, and it is the whole case: the same write without the
	// flag resolves as applied, against the same running applier. Without
	// it the assertions above would pass on a harness whose applier was
	// simply never going to arrive.
	ordinary, err := h.write(probeSubject("b"), "op-2", "hello")
	if err != nil {
		t.Fatalf("the ordinary write: %v", err)
	}
	if ordinary.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the unflagged write resolved %q, want applied: this node's "+
			"applier is not arriving, so the case above proves nothing",
			ordinary.Outcome)
	}
	if !ordinary.Waited {
		t.Error("an ordinary write reports that it did not wait")
	}
}

// AND THE PENDING THAT DID WAIT SAYS SO, which is the other half: the two are
// one outcome and opposite facts about this node, and reported as one a
// deliberate skip reads to an operator as a node falling behind under load.
func TestThePendingThatWaitedIsDistinguishable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
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
	if !res.Waited {
		t.Error("a write whose apply wait timed out reports that it did not " +
			"wait, so a node that is genuinely behind is indistinguishable " +
			"from a caller that declined to find out")
	}
}

// THE FLAG DOES NOT REACH THE SESSION WAIT, which is the one it must never
// skip.
//
// The two waits sit at opposite ends of one write and answer opposite
// questions: the session wait is "may I decide yet" and the apply wait is "may
// I report what my decision produced". Skipping the first makes a two-step
// gesture decide from a state below its own previous write — which is a
// correctness property, not a latency one, and collapsing them would trade the
// first away for the second.
func TestTheSessionWaitIsNeverSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// STALLED rather than merely not auto-applying, because the session
	// wait is what `stalled` stops: a node that has stopped catching up,
	// which is the state the wait exists to refuse a decision in.
	h.applier.mu.Lock()
	h.applier.stalled = true
	h.applier.mu.Unlock()

	// A session mark this node's applier will never reach. The write must
	// refuse rather than decide from a state below it — flag or no flag.
	ahead := statelog.Position{
		Stream: probeStream, Generation: h.gen.Load(), Seq: 9999,
	}
	_, err := h.pub.Publish(context.Background(), statelog.Request{
		Subject: probeSubject("a"),
		Scope:   statelog.ScopeSet{Paths: []string{probeSubject("a").String()}},
		OpID:    "op-1",
		Pattern: statelog.PatternArbitrated,
		Session: ahead,
		NoWait:  true,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{Payload: probeRecord(stamp, "op-1", "hello"), Version: 1}, nil
		},
	})
	if err == nil {
		t.Fatal("a no-wait write decided from a state below its caller's own " +
			"previous write")
	}
	var unavailable *statelog.Unavailable
	if !errors.As(err, &unavailable) || unavailable.Reason != statelog.ReasonBehind {
		t.Errorf("the refusal is not the session wait's: %v", err)
	}
}

// A SUBJECT'S END IS ITS OWN LAST RECORD, and nobody else's.
//
// It is what a gesture waits for when its decision turns on an object it does
// not publish on: the position the broker holds for THAT subject, so a node
// behind on it is made to catch up before it decides. A position from a
// neighbouring subject, or the stream's own end, would either wait for nothing
// or wait for every write in the domain. Mutation: answer the stream's last
// sequence and the first subject's end moves when its neighbour is written.
func TestASubjectsEndIsItsOwnLastRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, write := range []struct{ subject, op string }{
		{"a", "op-a1"}, {"b", "op-b1"}, {"a", "op-a2"}, {"b", "op-b2"},
	} {
		if _, err := h.write(probeSubject(write.subject), write.op, write.op); err != nil {
			t.Fatalf("write %s: %v", write.op, err)
		}
	}
	for subject, want := range map[string]uint64{"a": 3, "b": 4} {
		end, found, err := h.pub.SubjectEnd(t.Context(), probeSubject(subject))
		if err != nil || !found {
			t.Fatalf("the end of %s: %v (found %v)", subject, err, found)
		}
		if end.Seq != want || end.Stream != probeStream || end.Generation != h.gen.Load() {
			t.Errorf("the end of %s is %s, want sequence %d on %s", subject, end,
				want, probeStream)
		}
	}
	// A SUBJECT NOTHING WAS WRITTEN TO holds no end, which is a fact rather
	// than a failure.
	if end, found, err := h.pub.SubjectEnd(t.Context(), probeSubject("c")); err != nil ||
		found || !end.IsZero() {
		t.Errorf("an unwritten subject answered %s (found %v, %v)", end, found, err)
	}
}

// A PUBLISHER MISSING A SEAM IS REFUSED AT CONSTRUCTION, NAMING IT.
//
// Every seam is on the write path: the rows are the snapshot a decision is
// taken in, the fence is the eviction check every append makes, and the gates
// are what a create's permanent deletion marker is read through. Built without
// one, the write authority would fail at its first append — or, for a fence
// nothing asked, collect acknowledgements for records every node drops — so
// [statelog.NewPublisher] refuses the set, and the refusal names the missing
// field because it is read by whoever wired a new domain's register entry.
//
// The complete set is the control: a refusal that fired whatever was passed
// would satisfy every row below.
func TestAPublisherMissingASeamIsRefusedByName(t *testing.T) {
	t.Parallel()
	complete := func() statelog.Deps {
		a := newApplier()
		return statelog.Deps{
			Domain:     probeDomain{},
			Log:        refusingAppender{err: errors.New("never asked")},
			Signer:     testSigner(t, probeDomain{}),
			Rows:       &fakeRows{applier: a},
			Fence:      &fakeFence{},
			Gates:      &fakeGates{},
			Waiter:     a,
			Identity:   a,
			Admission:  noCeiling(t),
			NodeID:     "node-a",
			Generation: func() uint32 { return 1 },
		}
	}
	if _, err := statelog.NewPublisher(complete()); err != nil {
		t.Fatalf("a complete dependency set is refused: %v", err)
	}
	for _, c := range []struct {
		field string
		drop  func(*statelog.Deps)
		names string
	}{
		{"Domain", func(d *statelog.Deps) { d.Domain = nil }, "no domain"},
		{"Log", func(d *statelog.Deps) { d.Log = nil }, "no appender"},
		{"Signer", func(d *statelog.Deps) { d.Signer = nil }, "no signer"},
		{"Rows", func(d *statelog.Deps) { d.Rows = nil }, "no rows"},
		{"Fence", func(d *statelog.Deps) { d.Fence = nil }, "no fence"},
		{"Gates", func(d *statelog.Deps) { d.Gates = nil }, "no gates"},
		{"Waiter", func(d *statelog.Deps) { d.Waiter = nil }, "no waiter"},
		{"Identity", func(d *statelog.Deps) { d.Identity = nil }, "no stream identity"},
		{"Admission", func(d *statelog.Deps) { d.Admission = nil }, "no admission"},
		{"Generation", func(d *statelog.Deps) { d.Generation = nil }, "no generation source"},
		{"NodeID", func(d *statelog.Deps) { d.NodeID = "" }, "no node id"},
	} {
		t.Run(c.field, func(t *testing.T) {
			t.Parallel()
			deps := complete()
			c.drop(&deps)
			_, err := statelog.NewPublisher(deps)
			if err == nil {
				t.Fatalf("a publisher with no %s was built — it fails at its "+
					"first append instead of at the boot", c.field)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("the refusal of a publisher with no %s says %q, which does "+
					"not name what is missing (%q)", c.field, err, c.names)
			}
		})
	}
}

// A RECORD TOO LARGE AND A LOG AT ITS CEILING ARE TWO REFUSALS, with two
// remedies — and both are decided at round one.
//
// Neither changes between rounds, so neither may be retried. But the remedies
// are different people's: a full log is the operator's retention (raise the
// ceiling, unblock the trim), and a record too large is the writer's change,
// with a log that has room to spare. Both were `log_full`, so a record too
// large told whoever read it to raise a ceiling nothing was near.
//
// Both are staged on a REAL broker: a record past its domain's declared
// largest is refused before anything is sent, naming the declaration, and a
// log created at a few kilobytes fills after a handful of records.
func TestATooLargeRecordAndAFullLogAreRefusedApart(t *testing.T) {
	t.Parallel()

	t.Run("a record too large", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		body := strings.Repeat("x", 8<<20+1)
		res, err := h.write(probeSubject("big"), "op-big", body)
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonRecordTooLarge {
			t.Fatalf("an oversized record = %v, want a %s refusal", err,
				statelog.ReasonRecordTooLarge)
		}
		if res.Rounds != 1 || h.appends.appends.Load() != 0 {
			t.Errorf("an oversized record took %d round(s) and %d append(s), want "+
				"one round and no append — this node refuses it before the "+
				"broker is asked, and nothing a retry does makes it smaller",
				res.Rounds, h.appends.appends.Load())
		}
		// WHAT TO CHANGE: the limit that refused it, which is the
		// domain's own declaration, and the writer's remedy. Never the
		// ceiling or the trim, which is the remedy this refusal used to
		// carry.
		for _, want := range []string{strconv.Itoa(probeMaxRecord),
			"declared largest record", "split the change"} {
			if !strings.Contains(refusal.Detail, want) {
				t.Errorf("the refusal does not say %q: %s", want, refusal.Detail)
			}
		}
		if !errors.Is(err, queue.ErrTooLarge) {
			t.Errorf("an oversized record = %v, which does not answer "+
				"errors.Is(queue.ErrTooLarge)", err)
		}
		for _, wrong := range []string{"byte ceiling", "trim"} {
			if strings.Contains(refusal.Detail, wrong) {
				t.Errorf("the refusal of a record too large names %q, a full "+
					"log's remedy: %s", wrong, refusal.Detail)
			}
		}
		if got := statelog.RetryAfter(err, 2*time.Second); got != 0 {
			t.Errorf("a record too large says come back in %s — it is refused "+
				"the same on every attempt and every node", got)
		}
	})

	t.Run("a log at its byte ceiling", func(t *testing.T) {
		t.Parallel()
		h := newHarnessFor(t, tinyLogDomain{})
		body := strings.Repeat("x", 4<<10)
		var (
			res statelog.Result
			err error
		)
		for i := range 64 {
			res, err = h.write(probeSubject(fmt.Sprintf("o%d", i)),
				fmt.Sprintf("op-%d", i), body)
			if err != nil {
				break
			}
		}
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonLogFull {
			t.Fatalf("a write to a log at its ceiling = %v, want a %s refusal", err,
				statelog.ReasonLogFull)
		}
		if res.Rounds != 1 {
			t.Errorf("a full log's refusal took %d rounds, want one", res.Rounds)
		}
		for _, want := range []string{"byte ceiling", "crewlet retention status"} {
			if !strings.Contains(refusal.Detail, want) {
				t.Errorf("the refusal does not say %q: %s", want, refusal.Detail)
			}
		}
	})
}

// tinyLogDomain is the probe domain on a log whose ceiling a handful of
// records fills.
type tinyLogDomain struct{ probeDomain }

func (tinyLogDomain) Stream() statelog.StreamSpec {
	spec := probeDomain{}.Stream()
	spec.MaxBytes = 32 << 10
	return spec
}

// AN EVICTION THAT LANDS WHILE A WRITE IS IN FLIGHT REFUSES IT AT THE NEXT
// APPEND, as an eviction.
//
// Fence 0 runs before the first snapshot and again before every append,
// because a write that has spent rounds losing races has run for as long as
// they took and the eviction may have landed inside that window. The second
// check's refusal went through the append's error classification, which read
// it as an append nobody answered: it asked the broker for the subject's last
// sequence, found nothing of its own there, re-decided — and met the same
// refusal every round until the budget ran out, when the caller was told the
// rows kept changing under the write. A node that KNEW it had been removed
// reported contention.
//
// The fence here answers "not evicted" to the check before the snapshot and
// the other answer to the one before the append.
func TestAnEvictionLandingMidWriteRefusesAsAnEviction(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		answer func() (bool, error)
		want   statelog.Reason
	}{
		{"evicted", func() (bool, error) { return true, nil }, statelog.ReasonEvicted},
		{"unreadable", func() (bool, error) {
			return false, errors.New("coordination unreachable")
		}, statelog.ReasonEvictionUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			fence := &laterFence{fakeFence: h.fence, after: 1, answer: c.answer}
			pub, err := statelog.NewPublisher(statelog.Deps{
				Domain: probeDomain{}, Log: h.appends,
				Signer: testSigner(t, probeDomain{}), Rows: h.rows, Fence: fence,
				Gates: h.gates, Waiter: h.applier, Identity: h.applier,
				Admission: noCeiling(t), NodeID: "node-a",
				Generation: h.gen.Load, ResolveBudget: 250 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("NewPublisher: %v", err)
			}
			res, err := pub.Publish(t.Context(), statelog.Request{
				Subject: probeSubject("a"),
				Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
				OpID:    "op-1",
				Pattern: statelog.PatternArbitrated,
				Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
					return statelog.Decision{Payload: probeRecord(stamp, "op-1", "x"), Version: 1}, nil
				},
			})
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != c.want {
				t.Fatalf("a write whose fence answered %s at the append = %v "+
					"after %d round(s), want a %s refusal", c.name, err,
					res.Rounds, c.want)
			}
			if res.Rounds != 1 || h.appends.appends.Load() != 0 {
				t.Errorf("the refusal took %d round(s) and %d append(s), want one "+
					"round and no append", res.Rounds, h.appends.appends.Load())
			}
		})
	}
}

// laterFence answers "not evicted" for its first `after` questions and
// answer's value from then on.
type laterFence struct {
	*fakeFence
	after  int64
	calls  atomic.Int64
	answer func() (bool, error)
}

func (f *laterFence) Evicted(ctx context.Context) (bool, error) {
	if f.calls.Add(1) <= f.after {
		return f.fakeFence.Evicted(ctx)
	}
	return f.answer()
}

// AN APPEND THE BROKER SAYS MAY YET LAND IS NEVER A REFUSAL, AND NEVER A
// RETAKE.
//
// A clustered leader stages a record's message id when it PROPOSES it, and a
// second append under the same id before that proposal applies is answered
// 10158, "duplicate message id is in process". The id is the op id, so the
// second append is this write's own: an earlier round's append went unanswered
// while its commit was slow, or a caller retried an `unknown` under the same
// operation. A leader whose store closes under an entry raft already committed
// answers 10077 "store is closed" for a record every member then applies.
//
// Both were `broker_refused` — "asking again changes nothing" — about a record
// that applied a moment later, and a caller that re-filed under a fresh op id
// wrote the change twice. What the probe cannot settle is `unknown` with the
// op id, and nothing is re-decided: a fresh snapshot would decide against a
// state about to hold this very write.
//
// The rows are the server's own answers, built by its constructors; the
// append either never reaches the stream (the proposal is still in flight) or
// lands with its answer lost.
func TestAnAppendTheBrokerSaysMayYetLandIsNeverRefused(t *testing.T) {
	t.Parallel()
	wire := func(e *server.ApiError) error {
		return fmt.Errorf("nats: %w", &jetstream.APIError{
			Code: e.Code, ErrorCode: jetstream.ErrorCode(e.ErrCode),
			Description: e.Description,
		})
	}
	inFlight := wire(server.NewJSStreamDuplicateMessageConflictError())
	closed := wire(server.NewJSStreamStoreFailedError(server.ErrStoreClosed,
		server.Unless(server.ErrStoreClosed)))
	for _, c := range []struct {
		name   string
		err    error
		landed bool
		want   statelog.Outcome
	}{
		{"its own record in flight, not yet visible", inFlight, false,
			statelog.OutcomeUnknown},
		{"a store closed under it, not yet visible", closed, false,
			statelog.OutcomeUnknown},
		// RESOLVED WHEN IT CAN BE: the record is on the stream and this
		// node applied it, so the ledger answers for it.
		{"its own record in flight, already visible", inFlight, true,
			statelog.OutcomeApplied},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			first, err := h.write(probeSubject("a"), "op-0", "one")
			if err != nil {
				t.Fatalf("the first write: %v", err)
			}
			h.anchorAt(probeSubject("a"), first.Position.Seq)
			appends, snapshots := h.appends.appends.Load(), h.rows.snapshots()

			h.appends.fail(c.err, !c.landed)
			res, err := h.write(probeSubject("a"), "op-1", "two")
			if err != nil {
				t.Fatalf("a write the broker answered %v = %v, want outcome %q — "+
					"the record may yet land, so no refusal is true of it",
					c.err, err, c.want)
			}
			if res.Outcome != c.want {
				t.Fatalf("outcome = %q, want %q", res.Outcome, c.want)
			}
			if res.OpID != "op-1" {
				t.Errorf("op id = %q, want op-1 — the same op id is the only safe "+
					"retry, and it is what gets the broker's duplicate "+
					"acknowledgement once the record lands", res.OpID)
			}
			if got := h.appends.appends.Load() - appends; res.Rounds != 1 || got != 1 {
				t.Errorf("the write took %d round(s) and %d append(s), want one of "+
					"each", res.Rounds, got)
			}
			if got := h.rows.snapshots() - snapshots; got != 1 {
				t.Errorf("the write took %d snapshots, want one — a retake decides "+
					"against a state about to hold this very write", got)
			}
		})
	}
}

// A REFUSAL THE BROKER NAMED AND THIS BUILD HAS NO REMEDY FOR IS FINAL, AND
// CARRIES THE BROKER'S WORDS.
//
// A sealed stream, a JetStream store out of resources: they were `log_full`,
// whose detail sends an operator to a ceiling and a trim neither involves, and
// are now `broker_refused` with the broker's code and description as the
// remedy. What this pins is the PUBLISHER's arm rather than the classification
// beneath it — that it refuses at round one after one append, keeps the
// broker's code and words, and tells nobody to come back: the arm could
// return `log_full`, drop the words or fall through to the lost-race path and
// retry for the whole round budget with every classification test still
// green. The control is a full log answered through the same fake, which
// stays `log_full`.
func TestABrokerRefusalIsFinalAndCarriesItsWords(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		err  error
		want statelog.Reason
		says []string
	}{
		{"a sealed stream", &jetstream.APIError{Code: 400, ErrorCode: 10109,
			Description: "invalid operation on sealed stream"},
			statelog.ReasonBrokerRefused,
			[]string{"code 10109", "invalid operation on sealed stream"}},
		{"a full log, the control", &jetstream.APIError{Code: 503, ErrorCode: 10077,
			Description: "maximum bytes exceeded"},
			statelog.ReasonLogFull,
			[]string{"maximum bytes exceeded", "byte ceiling"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.appends.fail(fmt.Errorf("nats: %w", c.err), true)
			res, err := h.write(probeSubject("a"), "op-1", "hello")
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != c.want {
				t.Fatalf("a write the broker answered %v = %v, want a %s refusal",
					c.err, err, c.want)
			}
			if got := h.appends.appends.Load(); res.Rounds != 1 || got != 1 {
				t.Errorf("the refusal took %d round(s) and %d append(s), want one "+
					"of each — the broker will answer the next append the same",
					res.Rounds, got)
			}
			for _, want := range c.says {
				if !strings.Contains(refusal.Detail, want) {
					t.Errorf("the refusal does not say %q: %s", want, refusal.Detail)
				}
			}
			if refusal.OpID != "op-1" {
				t.Errorf("the refusal carries op id %q, want op-1", refusal.OpID)
			}
			if got := statelog.RetryAfter(err, 2*time.Second); got != 0 {
				t.Errorf("a %s refusal says come back in %s — waiting changes "+
					"nothing about it", c.want, got)
			}
		})
	}
}

// A LOG THAT LOST WHAT A PEER'S ROWS HOLD REFUSES THIS NODE'S WRITES — BUT NOT
// THE EVICTION OF THAT PEER.
//
// This node's rows are the log's own history, so nothing about its identity is
// wrong; what is wrong is that a write from here, on a broker restored from an
// older copy, is one the restored reanchor of the newer peer applies nowhere.
// So an ordinary write refuses `log_truncated`, before anything reaches the
// broker, naming the finding a caller can recognise — and a node gate is
// published, because evicting that peer is one of the operator's ways out and a
// fence that refused it would leave the fleet with one fewer.
func TestALogThatLostWhatAPeerHoldsRefusesWritesButNotTheEviction(t *testing.T) {
	t.Parallel()
	h := newHarnessFor(t, gatingDomain{})
	h.applier.truncatedBy("node-newer")

	before := h.appends.appends.Load()
	_, err := h.write(probeSubject("a"), "op-1", "decided from the log's own history")
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonLogTruncated ||
		!errors.Is(err, statelog.ErrLogTruncated) || errors.Is(err, statelog.ErrConflict) {
		t.Fatalf("an ordinary write = %v, want a %s refusal over the truncation",
			err, statelog.ReasonLogTruncated)
	}
	if n := h.appends.appends.Load() - before; n != 0 {
		t.Fatalf("appended %d time(s) onto a log a peer's rows hold more than", n)
	}

	before = h.appends.appends.Load()
	_, err = h.gate("node-newer", "op-gate", "evict", evictionRecord)
	if n := h.appends.appends.Load() - before; err != nil || n != 1 {
		t.Fatalf("the eviction = %v after %d append(s), want it published", err, n)
	}

	// AND A NODE WHOSE OWN ROWS ARE NOT THE LOG'S HISTORY is refused for
	// that, the truer finding.
	h.applier.rebuilt()
	_, err = h.write(probeSubject("b"), "op-2", "decided from these rows")
	requireWrongStream(t, err)
}

// A NODE GATE IS THE ONE WRITE A NODE THE FLEET RE-ANCHORED PAST STILL MAKES.
//
// Such a node's rows are a history the log no longer continues, so everything
// it decides from them is refused — but an eviction is decided from nothing in
// them, and the eviction of the decommissioned peer that stranded a fleet is
// the gesture that releases it. Refused, no stranded node could ever make it.
// A RECREATED stream is never excused: this node's expectations there are
// sequences on another stream.
func TestANodeGateIsTheOneWriteAPassedNodeStillMakes(t *testing.T) {
	t.Parallel()
	gate := func(h *harness) error {
		_, err := h.gate("node-decommissioned", "op-gate", "evict", evictionRecord)
		return err
	}
	for _, c := range []struct {
		name    string
		trip    func(h *harness)
		excused bool
	}{
		{"a generation the fleet moved past", func(h *harness) { h.applier.passedBy() }, true},
		{"a stream rebuilt under the node", func(h *harness) { h.applier.rebuilt() }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarnessFor(t, gatingDomain{})
			c.trip(h)
			_, err := h.write(probeSubject("a"), "op-1", "decided from these rows")
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonWrongStream {
				t.Fatalf("an ordinary write = %v, want a %s refusal", err,
					statelog.ReasonWrongStream)
			}
			before := h.appends.appends.Load()
			err = gate(h)
			appended := h.appends.appends.Load() - before
			switch {
			case c.excused && (err != nil || appended != 1):
				t.Fatalf("the node gate = %v after %d append(s), want it published", err, appended)
			case !c.excused:
				requireWrongStream(t, err)
				if appended != 0 {
					t.Fatalf("appended %d time(s) onto a log this node's rows are not "+
						"keyed to", appended)
				}
			}
		})
	}
}
