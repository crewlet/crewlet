package kv

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// coveredEarlier are the positions classes whose widest records their own
// tests hold (coord.TestEveryPositionsClassesWidestRecordFitsItsBound and the
// writers' own), beside coordtest.WidestPositionsRecords.
var coveredEarlier = []string{coord.MCPListingClass, coord.MCPDefinitionClass, coord.MCPCheckClass,
	coord.AccessReviewClass, coord.SeatAccessClass, coord.HomeBoardClass, coord.AtlasWakeClass,
	coord.DeviceSignInClass, coord.AssistantInstructionsClass}

// EVERY POSITIONS FAMILY IS DECLARED AT THE BOUND ITS WIDEST RECORD FITS: the
// register's holding gives each family's keys a class of their own, never its
// rest, held to the bound its record's widest is proved against
// (coord.TestEveryPositionsFamilysWidestRecordFitsItsClass), and declares no
// class nothing writes.
//
// Mutation: drop the seat pause's entry from positionsHolding, or hold the
// OAuth grants to coord.SmallValueBytes, and this is red.
func TestEveryPositionsFamilyIsDeclaredAtTheBoundItsWidestRecordFits(t *testing.T) {
	t.Parallel()
	widest := coordtest.WidestPositionsRecords()
	for _, w := range widest {
		k, declared := declaredClass(positionsHolding, w.Key)
		switch {
		case !declared:
			t.Errorf("%s's key %q falls to the register's rest", w.Class, w.Key)
		case k.class.bound != w.Bound:
			t.Errorf("%s is declared at %d bytes, want the %d its widest record is proved against",
				w.Class, k.class.bound, w.Bound)
		}
	}
	for _, k := range positionsHolding.classes {
		if !slices.ContainsFunc(widest, func(w coordtest.PositionsWidest) bool {
			return server.SubjectMatchesFilter(w.Key, k.keys)
		}) && !slices.ContainsFunc(coveredEarlier, func(class string) bool {
			return k.keys == coord.DocumentFilter(class)
		}) {
			t.Errorf("the register declares %q, which no widest record is written under", k.keys)
		}
	}
}

// declaredClass is the keyClass of h key matches, false where it falls to
// h's rest.
func declaredClass(h holding, key string) (keyClass, bool) {
	for _, k := range h.classes {
		if server.SubjectMatchesFilter(key, k.keys) {
			return k, true
		}
	}
	return keyClass{}, false
}

// positionsWrites records every key the KV backend writes to the positions
// registers it watches, from the write itself as the broker carries it: a
// put's subject ($KV.<bucket>.<key>), never a removal.
type positionsWrites struct {
	mu   sync.Mutex
	keys map[string]bool
	nc   *nats.Conn
}

// recordPositionsWrites starts a recorder on srv, watching the registers
// [positionsWrites.watch] names.
func recordPositionsWrites(t *testing.T, srv *server.Server) *positionsWrites {
	t.Helper()
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("connect the recorder: %v", err)
	}
	t.Cleanup(nc.Close)
	return &positionsWrites{keys: map[string]bool{}, nc: nc}
}

// watch records the positions register of the fleet opened under prefix,
// from before its first write.
func (w *positionsWrites) watch(t *testing.T, prefix string) {
	t.Helper()
	subject := "$KV." + prefix + positionsSuffix + "."
	if _, err := w.nc.Subscribe(subject+">", func(m *nats.Msg) {
		if m.Header.Get("KV-Operation") != "" {
			return
		}
		w.mu.Lock()
		w.keys[strings.TrimPrefix(m.Subject, subject)] = true
		w.mu.Unlock()
	}); err != nil {
		t.Fatalf("subscribe the recorder: %v", err)
	}
	if err := w.nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

// written is every key recorded: after a round trip, every write the broker
// took is queued to the recorder, and after a barrier queued behind each
// subscription's pending messages, every one of them has been handled.
func (w *positionsWrites) written(t *testing.T) []string {
	t.Helper()
	if err := w.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{})
	if err := w.nc.Barrier(func() { close(handled) }); err != nil {
		t.Fatal(err)
	}
	<-handled
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Sorted(func(yield func(string) bool) {
		for k := range w.keys {
			if !yield(k) {
				return
			}
		}
	})
}

// checkPositionsWritesDeclared fails t for every key w recorded that falls to
// the register's rest, and for a declared class nothing wrote.
func checkPositionsWritesDeclared(t *testing.T, w *positionsWrites) {
	t.Helper()
	keys := w.written(t)
	hit := map[string]bool{}
	for _, key := range keys {
		k, declared := declaredClass(positionsHolding, key)
		if !declared {
			t.Errorf("the store wrote %q to the positions register, which falls to its rest", key)
			continue
		}
		hit[k.keys] = true
	}
	for _, k := range positionsHolding.classes {
		if !hit[k.keys] {
			t.Errorf("premise: the suite wrote no key the positions register declares %q for", k.keys)
		}
	}
}
