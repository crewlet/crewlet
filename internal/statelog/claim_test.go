package statelog_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A CLAIM IS HELD BY ITS OWN EARLIER ATTEMPT AND REFUSED TO A RIVAL.
//
// A transition that crashed after its claim landed is re-run, and the re-run
// meets its own record on the subject — which the write path cannot tell from a
// rival's until this node's applier has applied it, and a generation claim is
// made exactly while that applier is stopped. The record's op id says whose it
// is, both for the re-run and for a rival that arrives second.
//
// Mutation: answer from the publish alone and the re-run on a stopped applier
// is refused `behind`; accept any holder and the rival reads the first
// claimant's record as its own.
func TestAClaimIsHeldByItsOwnEarlierAttemptAndRefusedToARival(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	subject := statelog.Subject{Kind: "generation", ID: "2"}
	claim := func(opID string) error {
		t.Helper()
		payload, err := json.Marshal(statelog.Envelope{
			V: 1, Kind: subject.Kind, Subject: subject, Op: "generation", OpID: opID,
			Scope: statelog.ScopeSet{Paths: []string{subject.String()}},
		})
		if err != nil {
			t.Fatalf("encode the record: %v", err)
		}
		return h.pub.Claim(t.Context(), statelog.Request{
			Subject: subject, OpID: opID, MintedAt: time.Now(),
			Scope:   statelog.ScopeSet{Paths: []string{subject.String()}},
			Pattern: statelog.PatternArbitrated,
			Decide: func(*sql.Tx) (statelog.Decision, error) {
				return statelog.Decision{Payload: payload, Version: 1}, nil
			},
		})
	}

	// THIS NODE'S APPLIER IS STOPPED: its own record lands and is never
	// applied here, which is the state a reanchor claims in.
	h.applier.mu.Lock()
	h.applier.auto, h.applier.stalled = false, true
	h.applier.mu.Unlock()

	if err := claim("reanchor:2:node-a"); err != nil {
		t.Fatalf("the first claim: %v", err)
	}
	if err := claim("reanchor:2:node-a"); err != nil {
		t.Fatalf("the same claimant's re-run was refused: %v — its own record "+
			"on the subject is its claim, whether or not this node applied it", err)
	}
	var elsewhere *statelog.ClaimedElsewhere
	if err := claim("reanchor:2:node-b"); !errors.As(err, &elsewhere) ||
		elsewhere.Holder != "reanchor:2:node-a" {
		t.Fatalf("a rival's claim = %v, want it refused naming the holder", err)
	}
	if got := h.appends.appends.Load(); got != 1 {
		t.Errorf("the three claims appended %d record(s), want the first alone", got)
	}
}
