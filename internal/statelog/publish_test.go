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
// Three outcomes a caller is told and two refusals that say no record happened.
// Collapsing any pair is the failure under test, most sharply `applied` and
// `gated`, which differ only in a durable local table.
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
		// This node's applier never reaches the record, which is DURABLE and every
		// other node applies: the answer is neither success nor failure.
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
		// THE ORDINARY BRANCH, where the wait SUCCEEDS: the node applies its own
		// record and drops it under its own eviction gate, so an answer read from the
		// wait alone would say applied.
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
		if refusal.CopyWriter != "" {
			t.Errorf("a refusal of this node's own record names %q as the copy's "+
				"writer — the reason is this node's standing", refusal.CopyWriter)
		}
		// THE GATE READER IS HANDED THE BODY THIS CALL APPENDED, the decision's own
		// bytes rather than the signed frame, so a gate keyed on what the payload names
		// (the identity estate's claims, about a person the subject does not name) can
		// say which gate dropped the record.
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

		// Then the broker stops answering: the append may or may not have landed
		// and the discriminator cannot say which, the third value.
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

		// The append fails BEFORE the record reaches the stream, so nothing above
		// the anchor needs resolving: the write decides again from a fresh snapshot.
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
// Not only on the expectation-zero branch: the broker refuses a stale
// expectation but knows nothing of the counted set, so an evicted node whose
// row is current would publish and be acknowledged.
func TestEvictedNodeRefusesBeforeTheAppend(t *testing.T) {
	t.Parallel()
	for name, pattern := range map[string]statelog.Pattern{
		"arbitrated": statelog.PatternArbitrated,
		"create":     statelog.PatternCreate,
		"additive":   statelog.PatternAdditive,
		"sequenced":  statelog.PatternSequenced,
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
					return statelog.Decision{Payload: probeRecord(stamp, probeSubject("a"),
						statelog.ScopeSet{Paths: []string{"object.a"}}, "op-1", "x")}, nil
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

	// AND AN EVICTION THAT CANNOT BE READ BLOCKS: it is the third value, and
	// publishing under it produces durable records every node drops.
	//
	// BUT IT IS NOT AN EVICTION, and the refusal says so: an eviction lasts until
	// an operator readmits the node, a coordination blip passes in seconds, and
	// `evicted` (a 503 with no Retry-After) is the signal a client gives up on.
	t.Run("an unreadable eviction blocks and says to come back", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.fence.evictErr = errors.New("open /var/lib/crewlet/replicated.db: database is locked")
		_, err := h.write(probeSubject("a"), "op-1", "hello")
		var refusal *statelog.Unavailable
		if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonEvictionUnknown {
			t.Fatalf("write = %v, want an eviction_unknown refusal", err)
		}
		// IN THIS PACKAGE'S WORDS: a refusal's detail reaches the caller, and the
		// fence's error is the store's own (a database path), which belongs in the log.
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
// Not only the retry at zero the floor theorem names: on a rebuilt log an
// ordinary expectation is an old-history sequence the broker ACCEPTS where the
// two happen to be equal, and an additive record is resolved against an
// old-history checkpoint that reads a low sequence as already applied.
func TestARebuiltLogRefusesEveryWriteBeforeTheAppend(t *testing.T) {
	t.Parallel()
	for name, pattern := range map[string]statelog.Pattern{
		"arbitrated": statelog.PatternArbitrated,
		"create":     statelog.PatternCreate,
		"additive":   statelog.PatternAdditive,
		"sequenced":  statelog.PatternSequenced,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			// A CURRENT ROW, so the ordinary branch forms an expectation the broker
			// accepts: without the fence this write lands.
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
					return statelog.Decision{Payload: probeRecord(stamp, probeSubject("a"),
						statelog.ScopeSet{Paths: []string{"object.a"}}, "op-1", "x"), Version: 1}, nil
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
// Fence 0 runs again before every append because the log can be rebuilt, or
// this node evicted, after the check at the top of the write. Its refusal is
// this node's own decision, taken before anything reached the broker, so it is
// not an ambiguous publish: read as one, the write retakes snapshots until its
// rounds run out and reports a CONFLICT for a refusal whose remedy is an
// operator's.
func TestFenceZeroAtTheAppendRefusesAsItselfInTheSameRound(t *testing.T) {
	t.Parallel()
	// Each trip lands inside the decision: after the check at the top of the
	// write, before the one at the append.
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
		"sequenced":  statelog.PatternSequenced,
	}
	// A WRITTEN SUBJECT reaches the append expecting the anchor and a FRESH one
	// expecting zero, past a zero fence that cleared; the additive pattern forms no
	// expectation on either.
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
							return statelog.Decision{Payload: probeRecord(stamp, probeSubject("a"),
								statelog.ScopeSet{Paths: []string{"object.a"}}, "op-1", "x"), Version: 1}, nil
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
// reason, which clears on the next read, rather than `evicted`, which tells the
// caller to give up on this node.
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
// Fence 0 at the top of a write answers from the heartbeat's last reading of
// the stream's instant, so a log rebuilt since passes it, and at zero that needs
// no coincidence to do harm: a rebuilt log holds nothing on any subject. The
// zero fence's read of the log carries the creation instant to the applier, and
// the fence 0 before the append asks after that read. Without either, both
// cases below land at zero on the rebuilt log.
//
// The identity asked over the zero fence's own REFUSAL is
// [TestARebuildOutranksTheZeroFencesEndCheck]'s.
func TestARebuildTheZeroFencesOwnReadFindsIsRefusedInThatRound(t *testing.T) {
	t.Parallel()

	// THE CONTROL: the same writes land at zero on a log nobody rebuilt.
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
		// Never written, so the write reaches the fence with no append attempted,
		// and the fence's read of the log finds the rebuild.
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
		// The row names a record at 7 the log does not hold: a rebuilt log's
		// shape, and a trimmed anchor's.
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
// The floor theorem's first clause (package doc, "THE FLOOR THEOREM"): rows an
// undecodable record touched are permanently stale here, and were its position
// trimmed the retry-at-zero branch would silently and irreversibly overwrite
// them.
func TestADeferredScopeRefusesBeforeAnyExpectation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.rows.set(func(s *statelog.Snap) {
		s.Deferred = true
		s.Deferral = statelog.Deferral{
			Position: statelog.Position{Stream: probeStream, Generation: 1, Seq: 42},
			Version:  9,
			// The deferred record's SUBJECT is b and the write is to a, so a's own
			// subject has nothing to probe: hence the probe over the scope.
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
// A trimmed claim's subject holds nothing, so zero is expected again and the
// broker enforces no uniqueness. The deletion half: a purge deletes the
// guarding row while the marker is permanent, so without it a writer is told
// "it already exists" above the floor and "it succeeded" below it.
func TestACreateIsGuardedByTheRowAndByTheDeletionMarker(t *testing.T) {
	t.Parallel()

	create := func(h *harness) (statelog.Result, error) {
		return h.pub.Publish(h.t.Context(), statelog.Request{
			Subject: probeSubject("a"),
			Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
			OpID:    "op-1",
			Pattern: statelog.PatternCreate,
			Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				return statelog.Decision{Payload: probeRecord(stamp, probeSubject("a"),
					statelog.ScopeSet{Paths: []string{"object.a"}}, "op-1", "x")}, nil
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
// Elsewhere a stale floor costs a refused write or a needless snapshot; on the
// zero branch it costs a silent lost update. Both halves are asserted so the
// fence moves neither onto the hot path nor off the cold one.
func TestTheOrdinaryWritePathTakesNoCoordinationRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// An anchor at a live sequence, as an ordinary second write has.
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
// Without the wait it re-reads an anchor the applier has not advanced, sixteen
// times, to a conflict a model reads as a colleague editing the object.
func TestALostRaceWaitsForTheWinnerBeforeItRedecides(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A peer writes the object while this node has applied nothing: its anchor
	// is empty and the subject is not, the state a lost race leaves.
	peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil,
		signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-b"}, probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}}, "peer-op", "peer")))
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
	peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil,
		signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-b"}, probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}}, "peer-op", "peer")))
	if err != nil {
		t.Fatalf("the peer's write: %v", err)
	}
	// A stale anchor that never moves: this node cannot catch up, as under a
	// write storm on one object.
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

// A SEQUENCED WRITE IS ORDERED BY THE LOG AND NEVER ARBITRATED: on the very
// subject where an arbitrated write formed from a stale anchor loses every
// round, it lands in ONE round with no expectation, taking one snapshot, and is
// placed after the peer's record for the applier to decide.
//
// Arbitrated, a shared sheet's writers on one subject would spend sixteen
// compare-and-set rounds behind a burst of clicks to be told "it kept
// changing". Mutation: make [statelog.PatternSequenced] form an expectation
// and the write ends in ErrConflict as the arbitrated case above does.
func TestASequencedWriteIsOrderedByTheLogAndNeverArbitrated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil,
		signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-b"}, probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}}, "peer-op", "peer")))
	if err != nil {
		t.Fatalf("the peer's write: %v", err)
	}
	// THE STATE THAT EXHAUSTS AN ARBITRATED WRITE: an anchor that never
	// reaches the peer's record, because this node cannot catch up.
	h.anchorAt(probeSubject("a"), peer-1)
	h.applier.mu.Lock()
	h.applier.frozen = true
	h.applier.mu.Unlock()

	res, err := h.pub.Publish(t.Context(), statelog.Request{
		Subject: probeSubject("a"),
		Scope:   statelog.ScopeSet{Paths: []string{probeSubject("a").String()}},
		OpID:    "op-1",
		Pattern: statelog.PatternSequenced,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{Payload: probeRecord(stamp, probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}}, "op-1", "mine")}, nil
		},
	})
	if errors.Is(err, statelog.ErrConflict) {
		t.Fatalf("a sequenced write was refused as a lost race (%v): it carries "+
			"no expectation, so there is no race for it to lose", err)
	}
	if res.Rounds != 1 {
		t.Fatalf("the write ran %d round(s), want 1: a sequenced record is never "+
			"rejected by the broker, so it never retakes its snapshot", res.Rounds)
	}
	if got := h.rows.snapshots(); got != 1 {
		t.Fatalf("took %d snapshots, want 1", got)
	}
	if res.Position.Seq <= peer {
		t.Fatalf("the write landed at %d, at or below the peer's %d: the log "+
			"orders it after every record already there", res.Position.Seq, peer)
	}
	// AND IT WAS NOT A ZERO PUBLISH: no expectation is not an expectation
	// of zero, which would have needed the zero fence's coordination read.
	if got := h.fence.zeroes.Load(); got != 0 {
		t.Fatalf("cleared %d zero publish(es) for a write that forms no "+
			"expectation at all", got)
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
			return statelog.Decision{Payload: probeRecord(stamp, probeSubject("a"),
				statelog.ScopeSet{}, "op-1", "x")}, nil
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
// The ledger answers a retry from an id's first row before the domain decides,
// so a write to b under a's id would be answered `applied` at a's position with
// nothing written to b. The id is the caller's (a seat carrying one forward, a
// route's `?op_id=`), so this is a caller's mistake to refuse, not confirm.
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
// A snapshot taken before a's record landed finds no row, decides, and appends
// under the same id, which the broker's duplicate window collapses onto a's
// record: resolved from the ledger alone, b is `applied` at a's position. The
// resolution judges the row exactly as the snapshot does.
func TestAReusedOperationIDTheBrokerCollapsedIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var fired atomic.Bool
	var first statelog.Result
	var firstErr error
	h.rows.mu.Lock()
	h.rows.afterSnapshot = func() {
		// a's WRITE LANDS BETWEEN b's SNAPSHOT AND b's APPEND, the one ordering the
		// snapshot's own check cannot see. Once only, and not under a sync.Once: a's
		// own snapshot runs this hook too.
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
// After a rejected append, re-deciding needs the subject's true last position,
// and waiting for an applier that has not caught up is unbounded: on the
// caller's own context, a request with no deadline would wait for ever and one
// with a deadline would get a bare cancellation instead of the reason and the
// position.
//
// Only an applier that does not ANSWER reaches this budget: a frozen one
// answers at once and spends the round budget instead (the case above).
func TestTheWaitForAPeersPositionIsBoundedAndSaysWhy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil,
		signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-b"}, probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}}, "peer-op", "peer")))
	if err != nil {
		t.Fatalf("the peer's write: %v", err)
	}
	// The anchor is below what the log holds and the applier never answers: a
	// node that has stopped catching up, not one losing races.
	h.anchorAt(probeSubject("a"), peer-1)
	h.applier.mu.Lock()
	h.applier.stalled = true
	h.applier.mu.Unlock()

	started := time.Now()
	// NO DEADLINE ON THE CALLER'S CONTEXT: the bound under test is the write
	// path's own.
	_, err = h.pub.Publish(context.Background(), statelog.Request{
		Subject: probeSubject("a"),
		Scope:   statelog.ScopeSet{Paths: []string{"p/a"}},
		OpID:    "op-1",
		Pattern: statelog.PatternArbitrated,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{
				Payload: probeRecord(stamp, probeSubject("a"),
					statelog.ScopeSet{Paths: []string{"p/a"}}, "op-1", "mine"),
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
// Both pendings are durable and unresolved here; this one never asked. It is
// for a sign-in: the cookie carries the record's position and every node
// validates the bearer against its own applier, so nothing in the answer reads
// the row this node would wait for.
func TestAFlaggedWriteAnswersPendingWithoutWaiting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// THE APPLIER IS LEFT RUNNING: stopped, this would pass on a wait that timed
	// out, and the claim is that no wait happened, visible only while a wait WOULD
	// succeed.
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

	// THE CONTROL, and the whole case: without the flag the same write resolves
	// as applied against the same running applier, so the applier was going to
	// arrive.
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

// AND THE PENDING THAT DID WAIT SAYS SO: the two are one outcome and
// opposite facts about this node, and reported as one a deliberate skip reads
// to an operator as a node falling behind under load.
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

// A DURABLE WRITE IS NEVER ANSWERED AS A FAILURE BECAUSE ITS WAIT ENDED.
//
// Past the broker's acknowledgement the record is on the log and every node
// applies it; a context error would tell every caller the write did not land,
// and lose its position and operation id. It is answered pending at its
// position where the record there is this operation's, and unknown with no
// position where the ambiguous path found a record nobody here could read as
// this operation's (as is a wait that ran out of its own budget), since naming
// another writer's record as this one's is what [Result.Position] forbids.
func TestAWriteWhoseWaitEndsIsAnsweredByWhatLanded(t *testing.T) {
	t.Parallel()
	// stoppedCaller ends while this node's applier is still not there, long
	// before the publisher's own budget: the CALLER stopped waiting.
	stoppedCaller := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		t.Cleanup(cancel)
		return statelog.WithResolveBudget(ctx, time.Minute)
	}
	publish := func(ctx context.Context, h *harness, subject statelog.Subject,
		opID string) (statelog.Result, error) {
		return h.pub.Publish(ctx, statelog.Request{
			Subject: subject,
			Scope:   statelog.ScopeSet{Paths: []string{subject.String()}},
			OpID:    opID,
			Pattern: statelog.PatternArbitrated,
			Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				return statelog.Decision{Payload: probeRecord(stamp, subject,
					statelog.ScopeSet{Paths: []string{subject.String()}}, opID, "body"), Version: 1}, nil
			},
		})
	}
	behind := func(h *harness) {
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()
	}

	t.Run("its own acknowledged append is pending at its position", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		behind(h)
		res, err := publish(stoppedCaller(t), h, probeSubject("a"), "op-1")
		if err != nil {
			t.Fatalf("a write the broker acknowledged answered %v — a caller "+
				"reads that as a write that did not land", err)
		}
		if res.Outcome != statelog.OutcomePending || res.Position.Seq == 0 ||
			res.Position.Seq != uint64(h.appends.lastSeq.Load()) {
			t.Fatalf("answered %+v, want pending at the position the broker "+
				"acknowledged (%d)", res, h.appends.lastSeq.Load())
		}
		if res.OpID != "op-1" || !res.Waited {
			t.Errorf("answered %+v, want its own operation id and a wait that was "+
				"made", res)
		}
	})

	t.Run("a record found newest is unknown, naming no position", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		behind(h)
		// THE RECORD LANDS AND ITS ACKNOWLEDGEMENT IS LOST, so the write resolves
		// the record it FINDS newest on its subject, which, its caller having stopped,
		// nobody here can read as its own.
		h.appends.fail(errors.New("no response from stream"), false)
		res, err := publish(stoppedCaller(t), h, probeSubject("a"), "op-1")
		if err != nil {
			t.Fatalf("a write whose record landed answered %v", err)
		}
		if res.Outcome != statelog.OutcomeUnknown || !res.Position.IsZero() {
			t.Fatalf("answered %+v, want unknown with no position", res)
		}
		// WAITED: the wait was reached, so this is the found record's
		// answer and not the one a subject nobody could probe gives.
		if res.OpID != "op-1" || !res.Waited {
			t.Errorf("answered %+v, want the operation id to retry under, "+
				"from a wait that was made", res)
		}
	})

	// A READ AFTER THE WAIT THAT FAILS fails the read and not the write,
	// whether the caller stopped or the store was busy or closed while this node
	// adopted a snapshot.
	for _, c := range []struct {
		name string
		// fail is what the ledger read after the record landed meets:
		// the caller's end, or an error of the store's own.
		fail func(cancel context.CancelFunc) error
	}{
		{"its caller's end", func(cancel context.CancelFunc) error { cancel(); return nil }},
		{"the store", func(context.CancelFunc) error {
			return errors.New("the replicated estate is closed")
		}},
	} {
		t.Run("a read after the wait that "+c.name+" fails is pending", func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			// THIS NODE APPLIES THE RECORD AT ONCE, so the wait succeeds,
			// and the read that says what it produced fails.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h.applier.mu.Lock()
			h.applier.beforeOp = func() error {
				// ONLY ONCE THE RECORD HAS LANDED: the ledger is read before
				// the decision too, and a write stopped there wrote nothing.
				if h.appends.lastSeq.Load() > 0 {
					return c.fail(cancel)
				}
				return nil
			}
			h.applier.mu.Unlock()
			res, err := publish(ctx, h, probeSubject("a"), "op-1")
			if err != nil {
				t.Fatalf("a write this node applied answered %v — a caller reads "+
					"that as a write that did not land", err)
			}
			if res.Outcome != statelog.OutcomePending || res.Position.Seq == 0 ||
				res.Position.Seq != uint64(h.appends.lastSeq.Load()) {
				t.Fatalf("answered %+v, want pending at the position the broker "+
					"acknowledged (%d)", res, h.appends.lastSeq.Load())
			}
		})
	}

	t.Run("a wait that runs out under a peer's newer record is unknown", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first, err := h.write(probeSubject("a"), "op-0", "one")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		h.anchorAt(probeSubject("a"), first.Position.Seq)
		behind(h)
		h.appends.fail(errors.New("no response from stream"), false)
		// A PEER'S WRITE LANDS ABOVE THIS ONE before the subject is asked
		// what it holds, so the record found newest is another operation's.
		var peer atomic.Uint64
		h.appends.mu.Lock()
		h.appends.beforeLastSeq = func() {
			seq, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil,
				signedByPeer(t, h, probeRecord(statelog.Stamp{Gen: 1, Writer: "node-b"},
					probeSubject("a"), statelog.ScopeSet{Paths: []string{probeSubject("a").String()}},
					"peer-op", "peer")))
			if err != nil {
				t.Errorf("the peer's write: %v", err)
			}
			peer.Store(seq)
		}
		h.appends.mu.Unlock()

		res, err := publish(statelog.WithResolveBudget(t.Context(), 300*time.Millisecond),
			h, probeSubject("a"), "op-1")
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if peer.Load() == 0 {
			t.Fatal("the peer's record never landed, so this case staged nothing")
		}
		if res.Outcome != statelog.OutcomeUnknown || !res.Position.IsZero() {
			t.Fatalf("answered %+v, want unknown with no position — the record "+
				"at %d is the peer's, and pending there tells this caller its own "+
				"write is durable at another writer's record", res, peer.Load())
		}
	})
}

// THE FLAG DOES NOT REACH THE SESSION WAIT, which is the one it must never
// skip.
//
// The session wait asks "may I decide yet", the apply wait "may I report what
// my decision produced". Skipping the first lets a two-step gesture decide from
// a state below its own previous write: a correctness loss, not latency.
func TestTheSessionWaitIsNeverSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// STALLED rather than merely not auto-applying: `stalled` stops the session
	// wait, the state it exists to refuse a decision in.
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
			return statelog.Decision{Payload: probeRecord(stamp, probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}}, "op-1", "hello"), Version: 1}, nil
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

// A PUBLISHER MISSING A SEAM IS REFUSED AT CONSTRUCTION, NAMING IT.
//
// Every seam is on the write path (the rows are the decision's snapshot, the
// fence the eviction check every append makes, the gates what a create's
// deletion marker is read through), so a missing one fails the first append or,
// for the fence, collects acknowledgements for records every node drops.
// [statelog.NewPublisher] names the field for whoever wires a new domain's
// register entry. The complete set is the control: a refusal that always fired
// would satisfy every row below.
func TestAPublisherMissingASeamIsRefusedByName(t *testing.T) {
	t.Parallel()
	complete := func() statelog.Deps {
		a := newApplier()
		return statelog.Deps{
			Domain: probeDomain{},
			Spec:   specOf(probeDomain{}),
			Log:    refusingAppender{err: errors.New("never asked")},
			Signer: testSigner(t, probeDomain{}),
			Records: statelog.VerifiedLog{Log: struct{ statelog.LogReader }{},
				Verifier: testVerifier(t, probeDomain{})},
			Rows:       &fakeRows{applier: a},
			Fence:      &fakeFence{},
			Gates:      &fakeGates{},
			Waiter:     a,
			Voids:      a,
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
		{"Spec", func(d *statelog.Deps) { d.Spec = statelog.StreamSpec{} }, "stream spec has no name"},
		{"Log", func(d *statelog.Deps) { d.Log = nil }, "no appender"},
		{"Signer", func(d *statelog.Deps) { d.Signer = nil }, "no signer"},
		{"Records", func(d *statelog.Deps) { d.Records = nil }, "no reader of its log"},
		{"Rows", func(d *statelog.Deps) { d.Rows = nil }, "no rows"},
		{"Fence", func(d *statelog.Deps) { d.Fence = nil }, "no fence"},
		{"Gates", func(d *statelog.Deps) { d.Gates = nil }, "no gates"},
		{"Waiter", func(d *statelog.Deps) { d.Waiter = nil }, "no waiter"},
		{"Voids", func(d *statelog.Deps) { d.Voids = nil }, "no reanchor rules"},
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
// Neither changes between rounds, so neither is retried. A full log is the
// operator's retention (raise the ceiling, unblock the trim); a record too
// large is the writer's change, on a log with room to spare. Both are staged
// on a REAL broker: a record past its domain's declared largest is refused
// before anything is sent, and a log created at a few kilobytes fills after a
// handful of records.
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
		// WHAT TO CHANGE: the domain's own declared limit and the writer's
		// remedy, never the ceiling or the trim.
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
// Fence 0 runs before the first snapshot and before every append, since rounds
// lost to races take time an eviction may land in. Classified as an unanswered
// append, the second check's refusal would re-decide every round and report
// contention from a node that KNEW it had been removed.
//
// The fence answers "not evicted" to the check before the snapshot and the
// other answer to the one before the append.
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
			deps := h.deps
			deps.Fence = fence
			pub, err := statelog.NewPublisher(deps)
			if err != nil {
				t.Fatalf("NewPublisher: %v", err)
			}
			res, err := pub.Publish(t.Context(), statelog.Request{
				Subject: probeSubject("a"),
				Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
				OpID:    "op-1",
				Pattern: statelog.PatternArbitrated,
				Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
					return statelog.Decision{Payload: probeRecord(stamp, probeSubject("a"), statelog.ScopeSet{Paths: []string{"object.a"}}, "op-1", "x"), Version: 1}, nil
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
// A clustered leader answers 10158 "duplicate message id is in process" to a
// second append under an id it has proposed but not applied (the id is the op
// id, so the append is this write's own), and 10077 "store is closed" for an
// entry raft already committed. Both records apply moments later, so a refusal
// would have a caller re-file under a fresh op id and write the change twice.
// Unsettled by the probe, it is `unknown` with the op id: a fresh snapshot
// would decide against a state about to hold this very write. The rows are the
// server's own answers; the append either never reaches the stream or lands
// with its answer lost.
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
// A sealed stream or a JetStream store out of resources is `broker_refused`,
// with the broker's code and description as the remedy, not `log_full`, which
// sends an operator to a ceiling and trim neither involves. This pins the
// PUBLISHER's arm, not the classification: it refuses at round one after one
// append, keeps the broker's words and tells nobody to come back. The control
// is a full log answered through the same fake, which stays `log_full`.
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
// On a broker restored from an older copy, this node's rows are still the
// log's history, but a write from here is one the newer peer's restored
// reanchor applies nowhere: it refuses `log_truncated` before the broker. A
// node gate is still published: evicting that peer is one of the operator's
// ways out.
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
// Everything it decides from rows the log no longer continues is refused, but
// an eviction is decided from none of them, and evicting the decommissioned
// peer that stranded a fleet is what releases it. A RECREATED stream is never
// excused: this node's expectations there are sequences on another stream.
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

// AN AMBIGUOUS PUBLISH IS JUDGED AGAINST THIS GENERATION'S ANCHOR, never one a
// reanchor left in the generation before.
//
// A restored log's reanchor opens a new generation at the log's end and keeps
// every row, so an anchor can sit in the old generation ABOVE anything the log
// holds. Compared with it, a record that DID land with its acknowledgement
// lost reads as "nothing landed above the anchor", and the write decides again
// behind its own record, to a conflict.
func TestAnAmbiguousPublishIsJudgedAgainstThisGenerationsAnchor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	subject := probeSubject("a")
	first, err := h.write(subject, "op-0", "one")
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}

	// THE REANCHOR: generation 2, its checkpoint at the log's end, and the
	// row's anchor left above it by the generation before.
	h.gen.Store(2)
	h.applier.advance(statelog.Position{Stream: probeStream, Generation: 2, Seq: first.Position.Seq})
	h.rows.pin(subject, statelog.Position{Stream: probeStream, Generation: 1, Seq: 1_000})

	// The record LANDS and the acknowledgement does not, and this node's
	// applier never reaches it.
	h.applier.mu.Lock()
	h.applier.auto, h.applier.frozen = false, true
	h.applier.mu.Unlock()
	h.appends.fail(errors.New("no response from stream"), false)
	before := h.appends.appends.Load()

	res, err := h.write(subject, "op-1", "two")
	if err != nil {
		t.Fatalf("a write whose record landed = %v — the subject's last record "+
			"is above this generation's anchor, so it is resolved rather than "+
			"decided again", err)
	}
	if res.Outcome != statelog.OutcomePending {
		t.Fatalf("outcome = %q, want pending: the record is on the log and this "+
			"node has not applied it", res.Outcome)
	}
	if want := first.Position.Seq + 1; res.Position.Seq != want || res.Position.Generation != 2 {
		t.Fatalf("pending named %s, want generation 2 at %d — the record this "+
			"write appended", res.Position, want)
	}
	if res.Rounds != 1 {
		t.Fatalf("the write took %d rounds, want 1 — a landed record is not a "+
			"reason to decide again", res.Rounds)
	}
	if got := h.appends.appends.Load() - before; got != 1 {
		t.Fatalf("the write appended %d time(s), want once", got)
	}
}
