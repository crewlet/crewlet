package coordtest_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// THE CONTROLS. Each hands a check a twin built to get one thing wrong and
// asserts it is caught, because a check that has only ever been run against
// correct backends is a claim rather than coverage — and the invariants here
// are precisely the ones a passing suite hid before: the claim that lapsed on
// a horizon other than the one it asked for, and the refusal that came back as
// "somebody else has it".
//
// The first arm of each is the CONTROL SET's other half: the twin as it
// actually is, expected to come back clean. Without it a check that objected
// to everything would look exactly like a check that works.

// wholeFleet names the shared-state contract by an alias, because embedding
// coord.Fleet directly names the field "Fleet" and shadows the interface's own
// Fleet method — the same dodge internal/engine's fakes take.
type wholeFleet = coord.Fleet

// oneHorizon is a claims store that keeps every claim for ONE horizon of its
// own, whatever ttl its caller asked for — the shape of the defect the ttl
// parameter once hid: the KV validated the argument and let its bucket's age
// decide, so a claim lived exactly as long as the bucket did.
type oneHorizon struct {
	wholeFleet

	horizon time.Duration

	mu   sync.Mutex
	made map[string]time.Time
}

func (o *oneHorizon) Claim(_ context.Context, key string, _ time.Duration, now time.Time) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if at, held := o.made[key]; held && now.Sub(at) < o.horizon {
		return false, nil
	}
	o.made[key] = now
	return true, nil
}

// everyTakeover honours each claim's own deadline but lets every caller past
// it win: a takeover written as a plain put rather than a compare-and-set.
type everyTakeover struct {
	wholeFleet

	mu    sync.Mutex
	until map[string]time.Time
}

func (e *everyTakeover) Claim(_ context.Context, key string, ttl time.Duration, now time.Time) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if until, held := e.until[key]; held && now.Before(until) {
		return false, nil
	}
	if _, held := e.until[key]; !held {
		e.until[key] = now.Add(ttl)
	}
	// The lapsed record is never replaced, so every caller after the
	// deadline is told it won.
	return true, nil
}

func TestTheSuiteCatchesAClaimThatDoesNotLapseAtItsOwnTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)

	// THE CONTROL: the twin as it is.
	if errs := coordtest.CheckClaimLapsesAtItsOwnTTL(ctx, memory.NewFleet(), at); len(errs) != 0 {
		t.Fatalf("the check objected to a correct twin, so it is measuring itself: %v", errs)
	}

	for _, c := range []struct {
		name    string
		horizon time.Duration
		want    string
	}{
		// Every claim held for the claims bucket's age: a webhook replay
		// swallowed for the chat socket's window.
		{"held for the bucket's age", coord.MaxClaimTTL, "still claimed a second past its own"},
		// Every claim held for the shortest horizon anybody asks for: the
		// socket's claim lapses while a reconnecting seat still replays.
		{"held for the shortest horizon", time.Minute, "lets the duplicate through"},
	} {
		lying := &oneHorizon{wholeFleet: memory.NewFleet(), horizon: c.horizon,
			made: map[string]time.Time{}}
		errs := coordtest.CheckClaimLapsesAtItsOwnTTL(ctx, lying, at)
		if len(errs) == 0 {
			t.Errorf("%s: the check passed a store that keeps every claim for %v "+
				"whatever its ttl", c.name, c.horizon)
			continue
		}
		if !containing(errs, c.want) {
			t.Errorf("%s: the check objected, but to something else: %v", c.name, errs)
		}
	}

	errs := coordtest.CheckClaimLapsesAtItsOwnTTL(ctx,
		&everyTakeover{wholeFleet: memory.NewFleet(), until: map[string]time.Time{}}, at)
	if !containing(errs, "taken over by two callers") {
		t.Errorf("a store whose takeover every caller wins came back %v, want the "+
			"double takeover named", errs)
	}
}

// mute answers an empty key the way a bool-returning registry does: as a race
// somebody else won.
type mute struct{ wholeFleet }

func (mute) Claim(context.Context, string, time.Duration, time.Time) (bool, error) {
	return false, nil
}

func TestTheSuiteCatchesARegistryThatRefusesInsteadOfErroring(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)

	if errs := coordtest.CheckUnnamedRecordsAreRefused(ctx, memory.NewFleet(), at); len(errs) != 0 {
		t.Fatalf("the check objected to a correct twin, so it is measuring itself: %v", errs)
	}

	errs := coordtest.CheckUnnamedRecordsAreRefused(ctx, mute{memory.NewFleet()}, at)
	if len(errs) == 0 {
		t.Fatal("the check passed a registry that answers an unnamed record with false: " +
			"a caller that named nothing has not lost a race to anybody, and every " +
			"caller of a claim reads false as somebody else having the record")
	}
	if !containing(errs, "an empty key reached Claim,") {
		t.Errorf("the check objected, but not about the claim: %v", errs)
	}
}

// containing reports whether any error names what the case expected.
func containing(errs []error, want string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), want) {
			return true
		}
	}
	return false
}

// markerLoser is the KV backend as it WAS on a replicated stream: every writer
// after the first to reach a record just removed is told the store is down,
// because the broker's refusal of a create over the removal's marker matched
// neither sentinel the create looked for — and so is every charger after the
// first to a counter nothing had written, the same refusal over no marker.
// Only the delivery claims and the token counters are made to lie — two verbs
// are enough to show the check reads what each verb answered rather than
// trusting any of them.
type markerLoser struct {
	wholeFleet

	mu sync.Mutex
	// raced are the records removed since they were last written, each
	// true once the first writer after the removal has landed.
	raced map[string]bool
	// charged are the counters a charge has landed on.
	charged map[string]bool
}

func (m *markerLoser) removed(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raced[key] = false
}

// lost reports a write that reached a removed record after its first writer.
func (m *markerLoser) lost(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	landed, removed := m.raced[key]
	if removed && !landed {
		m.raced[key] = true
	}
	return removed && landed
}

func (m *markerLoser) Release(ctx context.Context, key string) error {
	m.removed("claim|" + key)
	return m.wholeFleet.Release(ctx, key)
}

func (m *markerLoser) Claim(ctx context.Context, key string, ttl time.Duration,
	now time.Time) (bool, error) {
	if m.lost("claim|" + key) {
		return false, fmt.Errorf("%w: claim the delivery: wrong last sequence",
			coord.ErrUnavailable)
	}
	return m.wholeFleet.Claim(ctx, key, ttl, now)
}

// lostFirst reports a charge that reached a counter after its first charger.
func (m *markerLoser) lostFirst(scope string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.charged == nil {
		m.charged = map[string]bool{}
	}
	lost := m.charged[scope]
	m.charged[scope] = true
	return lost
}

func (m *markerLoser) Charge(ctx context.Context, req coord.ChargeRequest) (coord.Spend, error) {
	if m.lostFirst(req.Seat) {
		return coord.Spend{}, fmt.Errorf("%w: charge the counter: wrong last sequence",
			coord.ErrUnavailable)
	}
	return m.wholeFleet.Charge(ctx, req)
}

func TestTheSuiteCatchesARaceOverARemovedRecordAnsweredAsAnOutage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)

	if errs := coordtest.CheckCreatesOverARemovedRecordAreRaces(ctx, memory.NewFleet(),
		at); len(errs) != 0 {
		t.Fatalf("the check objected to a correct twin, so it is measuring itself: %v", errs)
	}

	errs := coordtest.CheckCreatesOverARemovedRecordAreRaces(ctx,
		&markerLoser{wholeFleet: memory.NewFleet(), raced: map[string]bool{}}, at)
	if len(errs) == 0 {
		t.Fatal("the check passed a backend that tells every loser of a create " +
			"over a removed record the store is down — a delivery claim " +
			"answered unknown and processed twice, a charge refused")
	}
	if !containing(errs, "Claim racing") {
		t.Errorf("the check objected, but not to the claims: %v", errs)
	}

	// AND THE COUNTERS, which the check reaches only once the claims come
	// back clean: a verb a check stops at is one it never looked at.
	honestClaims := &markerLoser{wholeFleet: memory.NewFleet(), raced: map[string]bool{}}
	errs = coordtest.CheckCreatesOverARemovedRecordAreRaces(ctx,
		chargesOnly{honestClaims}, at)
	if !containing(errs, "Charge racing") {
		t.Errorf("a backend lying only about charges came back %v, want the "+
			"charges named", errs)
	}
}

// chargesOnly is a [markerLoser] whose delivery claims tell the truth.
type chargesOnly struct{ *markerLoser }

func (c chargesOnly) Release(ctx context.Context, key string) error {
	return c.wholeFleet.Release(ctx, key)
}

func (c chargesOnly) Claim(ctx context.Context, key string, ttl time.Duration,
	now time.Time) (bool, error) {
	return c.wholeFleet.Claim(ctx, key, ttl, now)
}
