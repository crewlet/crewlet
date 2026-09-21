package coordtest_test

import (
	"context"
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
// correct backends is a claim rather than coverage — and the two invariants
// here are precisely the ones a passing suite hid before: the claim that
// lapsed on a horizon nobody could state, and the refusal that came back as
// "somebody else has it".
//
// The first arm of each is the CONTROL SET's other half: the twin as it
// actually is, expected to come back clean. Without it a check that objected
// to everything would look exactly like a check that works.

// wholeFleet names the shared-state contract by an alias, because embedding
// coord.Fleet directly names the field "Fleet" and shadows the interface's own
// Fleet method — the same dodge internal/engine's fakes take.
type wholeFleet = coord.Fleet

// perWriteTTL is the twin as it WAS: a claim that lapses on a horizon of its
// own rather than on the bucket it was written to.
//
// This is not a hypothetical defect. The twin honoured the ttl its caller
// passed while the KV validated the same argument and let the bucket decide,
// and the suite certified the pair clean for as long as both existed, because
// no case ever travelled past a deadline.
type perWriteTTL struct {
	wholeFleet

	ttl time.Duration

	mu   sync.Mutex
	made map[string]time.Time
}

func (p *perWriteTTL) Claim(_ context.Context, key string, now time.Time) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if at, held := p.made[key]; held && now.Sub(at) < p.ttl {
		return false, nil
	}
	p.made[key] = now
	return true, nil
}

// mute answers an empty key the way a bool-returning registry does: as a race
// somebody else won.
type mute struct{ wholeFleet }

func (mute) Claim(context.Context, string, time.Time) (bool, error)      { return false, nil }
func (mute) ClaimSetup(context.Context, string, time.Time) (bool, error) { return false, nil }

func TestTheSuiteCatchesAClaimThatOutlivesItsBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	age := coordtest.LapseAge

	// THE CONTROL: the twin built at the age the check was told about,
	// which is what every backend the suite certifies must do.
	clean := memory.NewFleetWithAges(memory.FleetAges{Claim: age})
	if errs := coordtest.CheckClaimExpiresWithItsBucket(ctx, clean, age, at); len(errs) != 0 {
		t.Fatalf("the check objected to a correct twin, so it is measuring itself: %v", errs)
	}

	// AND THE DEFECT: a twin whose claim lapses on a TTL of its own. The
	// bucket it was built for is half a second old and the claim it keeps
	// is five minutes, which is the shape of the bug exactly — a horizon
	// longer than the bucket that is supposed to enforce it.
	lying := &perWriteTTL{
		wholeFleet: memory.NewFleetWithAges(memory.FleetAges{Claim: age}),
		ttl:        coord.ClaimTTL,
		made:       map[string]time.Time{},
	}
	errs := coordtest.CheckClaimExpiresWithItsBucket(ctx, lying, age, at)
	if len(errs) == 0 {
		t.Fatal("the check passed a backend that keeps a claim for its own TTL rather " +
			"than for the bucket's age — which is the divergence this suite certified " +
			"clean for the whole life of both backends")
	}
	if !containing(errs, "outlives its bucket") {
		t.Errorf("the check objected, but to something else: %v", errs)
	}
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
			"caller of these verbs reads false as somebody else having the record")
	}
	if !containing(errs, "Claim accepted an empty key") ||
		!containing(errs, "ClaimSetup accepted an empty key") {
		t.Errorf("the check objected, but not about both registries: %v", errs)
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
