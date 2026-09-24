package statelog_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY RECORD A PUBLISHER APPENDS NAMES THE NODE THAT WROTE IT AND THE
// GENERATION OF THE ROWS IT WAS DECIDED FROM — handed to the decision by the
// framework, and on the bytes the broker holds.
//
// The writer is what every applier's eviction gate compares against an
// eviction, on a node that may not be able to decode the payload at all. No
// write path in the tree set it: each domain's envelope declared the field,
// the publisher held the node id, and every record reached every applier
// naming nobody — so the gate that is the floor theorem's last clause dropped
// nothing, for any node, ever.
func TestEveryRecordNamesItsWriterAndTheGenerationItWasDecidedIn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// A GENERATION OTHER THAN THE HARNESS'S FIRST, so a stamp that was a
	// constant, or the zero value, could not pass.
	const gen = 3
	h.gen.Store(gen)
	h.applier.advance(statelog.Position{Stream: probeStream, Generation: gen})

	var handed []statelog.Stamp
	res, err := h.pub.Publish(t.Context(), statelog.Request{
		Subject: probeSubject("a"),
		Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
		OpID:    "op-stamp",
		Pattern: statelog.PatternArbitrated,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			handed = append(handed, stamp)
			return statelog.Decision{Payload: probeRecord(stamp, "op-stamp", "x")}, nil
		},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	want := statelog.Stamp{Writer: "node-a", Gen: gen}
	if len(handed) == 0 || handed[len(handed)-1] != want {
		t.Fatalf("the decision was handed %+v, want %+v — the node's own id and "+
			"the generation of the checkpoint its snapshot read", handed, want)
	}

	_, payload, _, ok, err := h.log.At(t.Context(), res.Position.Seq)
	if err != nil || !ok {
		t.Fatalf("read the record back at %s: ok %v, err %v", res.Position, ok, err)
	}
	env, err := probeDomain{}.Envelope(payload)
	if err != nil {
		t.Fatalf("decode the record the broker holds: %v", err)
	}
	if env.Writer != want.Writer || env.Gen != want.Gen {
		t.Fatalf("the broker holds a record naming writer %q at generation %d, "+
			"want %q at %d — this is the only copy any applier's eviction gate "+
			"ever reads", env.Writer, env.Gen, want.Writer, want.Gen)
	}
}

// A RECORD THAT DOES NOT CARRY ITS STAMP IS REFUSED BEFORE ANYTHING IS
// APPENDED, naming what it lacks.
//
// Checked by the publisher on the encoded bytes, through the domain's own
// envelope reader, because a stamp a domain was merely HANDED is one it can
// forget to write — which is what every domain in the tree did. A refusal on
// the first write is the loud form of that mistake; the quiet form is a fleet
// whose evictions drop nothing.
func TestARecordWithoutItsStampIsRefusedBeforeItIsAppended(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		record func(stamp statelog.Stamp) []byte
		names  string
	}{
		"no writer": {
			record: func(s statelog.Stamp) []byte {
				s.Writer = ""
				return probeRecord(s, "op-1", "x")
			},
			names: "writer",
		},
		"another node's id": {
			record: func(s statelog.Stamp) []byte {
				s.Writer = "node-b"
				return probeRecord(s, "op-1", "x")
			},
			names: "writer",
		},
		"another generation": {
			record: func(s statelog.Stamp) []byte {
				s.Gen++
				return probeRecord(s, "op-1", "x")
			},
			names: "generation",
		},
		"another operation": {
			record: func(s statelog.Stamp) []byte {
				return probeRecord(s, "op-someone-else", "x")
			},
			names: "operation",
		},
		"bytes its own domain cannot read": {
			record: func(statelog.Stamp) []byte { return []byte("not a record") },
			names:  "decode",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			_, err := h.pub.Publish(t.Context(), statelog.Request{
				Subject: probeSubject("a"),
				Scope:   statelog.ScopeSet{Paths: []string{"object.a"}},
				OpID:    "op-1",
				Pattern: statelog.PatternArbitrated,
				Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
					return statelog.Decision{Payload: tc.record(stamp)}, nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("publish = %v, want a refusal naming the %s", err, tc.names)
			}
			if got := h.appends.appends.Load(); got != 0 {
				t.Fatalf("appended %d time(s) a record the publisher refused — "+
					"what reaches the broker cannot be taken back", got)
			}
		})
	}
}
