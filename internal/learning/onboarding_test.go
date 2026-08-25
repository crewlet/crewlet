package learning_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

func onboardingOn(t *testing.T, db *store.DB) *learning.Onboarding {
	t.Helper()
	return learning.NewOnboarding(db)
}

func openStore(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("store.Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func onboarding(t *testing.T) (*learning.Onboarding, *store.DB) {
	t.Helper()
	db := openStore(t, filepath.Join(t.TempDir(), "o.db"))
	return onboardingOn(t, db), db
}

// orgWithChain builds company > units[0] > … > units[n-1] > seat. No units
// puts the seat at the org root, which is a real configuration (a founder,
// a chief of staff) and not a degenerate one.
func orgWithChain(company string, units []string, seat string) (*org.Organization, *org.Role) {
	r := &org.Role{Name: seat}
	o := &org.Organization{Name: company}
	if len(units) == 0 {
		o.Roles = []*org.Role{r}
		return o, r
	}
	u := &org.Unit{Name: units[len(units)-1], Roles: []*org.Role{r}}
	for i := len(units) - 2; i >= 0; i-- {
		u = &org.Unit{Name: units[i], Children: []*org.Unit{u}}
	}
	o.Units = []*org.Unit{u}
	return o, r
}

func hashOf(company string, units []string, seat string) string {
	return learning.ChainHash(orgWithChain(company, units, seat))
}

// --- the chain hash -------------------------------------------------------

func TestTheChainHashIsTheOrgPathAndNothingElse(t *testing.T) {
	t.Parallel()
	// The golden is computed OUTSIDE this package — `printf '4:Acme3:Ops3:eng'
	// | sha256sum` — because the property under test is that a marker written
	// by one build is still matched by the next one. A test that recomputed
	// the hash the same way the code does would pass for any construction at
	// all, including a per-process seeded one.
	const golden = "9421099d879aa059d429952458bd4bc42a639a803e19b00a55a5f99b8690a35a"
	if got := hashOf("Acme", []string{"Ops"}, "eng"); got != golden {
		t.Errorf("ChainHash = %s, want the stable golden %s", got, golden)
	}
	// Counterfactual: the golden is not a value everything hashes to.
	if got := hashOf("Acme", []string{"Ops"}, "sre"); got == golden {
		t.Errorf("a different seat hashed to the golden %s", got)
	}
}

func TestTheChainHashMovesWithTheStructureAndOnlyWithIt(t *testing.T) {
	t.Parallel()
	baseHash := hashOf("Acme", []string{"Platform", "Ops"}, "eng")

	if again := hashOf("Acme", []string{"Platform", "Ops"}, "eng"); again != baseHash {
		t.Errorf("the same structure hashed twice = %s and %s", baseHash, again)
	}

	for _, tc := range []struct {
		what    string
		company string
		units   []string
		seat    string
	}{
		{"the company is renamed", "Acme Inc", []string{"Platform", "Ops"}, "eng"},
		{"an ancestor is renamed", "Acme", []string{"Infra", "Ops"}, "eng"},
		{"the seat's own unit is renamed", "Acme", []string{"Platform", "SRE"}, "eng"},
		{"the seat moves up a level", "Acme", []string{"Platform"}, "eng"},
		{"the seat moves to the root", "Acme", nil, "eng"},
		{"an ancestor is inserted", "Acme", []string{"Platform", "Core", "Ops"}, "eng"},
		{"the units swap order", "Acme", []string{"Ops", "Platform"}, "eng"},
		{"the role is renamed", "Acme", []string{"Platform", "Ops"}, "sre"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			if got := hashOf(tc.company, tc.units, tc.seat); got == baseHash {
				// Equal here means the marker survives a restructure and
				// the seat never re-reads the pages for the team it is
				// now in.
				t.Errorf("hash unchanged after %s", tc.what)
			}
		})
	}
}

func TestJoiningTheChainWithASeparatorWouldCollide(t *testing.T) {
	t.Parallel()
	// The measurement behind hashChain's netstring encoding. These are two
	// DIFFERENT org structures: a company whose name contains a newline
	// holding a root seat, and a company holding a unit holding that seat.
	// Python hashed strings.Join(parts, "\n"), which cannot tell them apart.
	joined := func(parts ...string) string {
		sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
		return hex.EncodeToString(sum[:])
	}
	if a, b := joined("Acme\nOps", "eng"), joined("Acme", "Ops", "eng"); a != b {
		t.Fatalf("the separator form did not collide (%s vs %s) — this test's premise is gone", a, b)
	}
	flat, nested := hashOf("Acme\nOps", nil, "eng"), hashOf("Acme", []string{"Ops"}, "eng")
	if flat == nested {
		t.Errorf("ChainHash collided across two structures: %s", flat)
	}
	// And nothing in the config layer stops the name that does it.
	o, _ := orgWithChain("Acme\nOps", nil, "eng")
	o.Normalize()
	if err := o.Validate(); err != nil {
		t.Skipf("a newline in a company name is now rejected at validation: %v", err)
	}
}

func TestAChainHashNeedsBothAnOrgAndASeat(t *testing.T) {
	t.Parallel()
	o, r := orgWithChain("Acme", []string{"Ops"}, "eng")
	if got := learning.ChainHash(nil, r); got != "" {
		t.Errorf("ChainHash(nil org) = %q, want empty", got)
	}
	if got := learning.ChainHash(o, nil); got != "" {
		t.Errorf("ChainHash(nil seat) = %q, want empty", got)
	}
	// Counterfactual: the pair that IS complete hashes to something.
	if got := learning.ChainHash(o, r); got == "" {
		t.Error("a complete org and seat hashed to empty")
	}
}

// --- the marker -----------------------------------------------------------

func TestAMarkerAnswersForItsOwnChainOnly(t *testing.T) {
	t.Parallel()
	o, _ := onboarding(t)
	here, elsewhere := hashOf("Acme", []string{"Ops"}, "eng"), hashOf("Acme", []string{"Sales"}, "eng")

	if err := o.Mark(t.Context(), learning.Marker{
		AgentID: "seat-1", ChainHash: here, Handle: "eng", Role: "eng",
		Summary: "deploys go through the release channel",
	}, base); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	for _, tc := range []struct {
		what  string
		agent string
		chain string
		want  bool
	}{
		{"the chain it marked", "seat-1", here, true},
		{"a chain it never marked", "seat-1", elsewhere, false},
		{"a seat with no row at all", "seat-2", here, false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, err := o.Onboarded(t.Context(), tc.agent, tc.chain)
			if err != nil {
				t.Fatalf("Onboarded: %v", err)
			}
			if got != tc.want {
				t.Errorf("Onboarded(%s, %s) = %v, want %v", tc.agent, tc.what, got, tc.want)
			}
		})
	}
}

func TestReMarkingOverwritesInPlaceAndKeepsTheFirstOnboardingDate(t *testing.T) {
	t.Parallel()
	o, db := onboarding(t)
	first, second := hashOf("Acme", []string{"Ops"}, "eng"), hashOf("Acme", []string{"Sales"}, "eng")
	later := base.Add(72 * time.Hour)

	for _, m := range []struct {
		chain, summary string
		at             time.Time
	}{
		{first, "ops conventions", base},
		{second, "sales conventions", later},
	} {
		if err := o.Mark(t.Context(), learning.Marker{
			AgentID: "seat-1", ChainHash: m.chain, Handle: "eng", Role: "eng", Summary: m.summary,
		}, m.at); err != nil {
			t.Fatalf("Mark: %v", err)
		}
	}

	var rows int
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT count(*) FROM agent_onboarding_markers`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		// Accumulating a row per re-onboarding makes the table unbounded
		// and leaves the OLD chain hash where a read can still find it.
		t.Fatalf("rows = %d, want the marker upserted in place", rows)
	}

	m, ok, err := o.Get(t.Context(), "seat-1")
	if err != nil || !ok {
		t.Fatalf("Get: %+v ok=%v err=%v", m, ok, err)
	}
	if m.ChainHash != second || m.Summary != "sales conventions" {
		t.Errorf("marker = %q/%q, want the second mark", m.ChainHash, m.Summary)
	}
	if !m.CreatedAt.Equal(base) {
		t.Errorf("created_at = %v, want the FIRST onboarding at %v", m.CreatedAt, base)
	}
	if !m.UpdatedAt.Equal(later) {
		t.Errorf("updated_at = %v, want the second mark at %v", m.UpdatedAt, later)
	}
	// The old chain must stop answering the moment the new one is stored,
	// with no separate cleanup pass in between.
	if was, err := o.Onboarded(t.Context(), "seat-1", first); err != nil || was {
		t.Errorf("Onboarded(old chain) = %v (err %v), want false", was, err)
	}
}

func TestALookupFailureIsUnknownAndNotUnmarked(t *testing.T) {
	t.Parallel()
	o, db := onboarding(t)
	chain := hashOf("Acme", []string{"Ops"}, "eng")
	if err := o.Mark(t.Context(), learning.Marker{AgentID: "seat-1", ChainHash: chain}, base); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	// CONTROL: the read answers true while the store is healthy. Without it
	// an assertion about the failure path also passes for a store that never
	// found the marker in the first place.
	if got, err := o.Onboarded(t.Context(), "seat-1", chain); err != nil || !got {
		t.Fatalf("control read = %v (err %v), want true", got, err)
	}

	_ = db.Close()

	got, err := o.Onboarded(t.Context(), "seat-1", chain)
	if err == nil {
		t.Fatal("a failed lookup reported no error — the caller cannot tell it from 'never marked' " +
			"and re-runs a whole onboarding pass for an already-marked seat")
	}
	if got {
		t.Errorf("Onboarded = true alongside an error; the pair must be (false, err)")
	}
	if _, ok, err := o.Get(t.Context(), "seat-1"); err == nil || ok {
		t.Errorf("Get on a dead store = ok %v err %v, want (false, err)", ok, err)
	}
}

func TestAMalformedIdentityIsUnknownRatherThanUnmarked(t *testing.T) {
	t.Parallel()
	o, _ := onboarding(t)
	chain := hashOf("Acme", []string{"Ops"}, "eng")

	// A blank chain hash answering "not onboarded" would run a pass that
	// can never mark — Mark refuses a blank hash — so it would re-run every
	// turn forever. An error routes the caller to skip instead.
	for _, tc := range []struct {
		what         string
		agent, chain string
	}{
		{"no agent id", "", chain},
		{"no chain hash", "seat-1", ""},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, err := o.Onboarded(t.Context(), tc.agent, tc.chain)
			if err == nil {
				t.Errorf("Onboarded(%s) = %v with no error", tc.what, got)
			}
			if got {
				t.Errorf("Onboarded(%s) = true", tc.what)
			}
		})
	}

	if err := o.Mark(t.Context(), learning.Marker{AgentID: "", ChainHash: chain}, base); err == nil {
		t.Error("Mark accepted a blank agent id")
	}
	if err := o.Mark(t.Context(), learning.Marker{AgentID: "seat-1"}, base); err == nil {
		t.Error("Mark accepted a blank chain hash — it would read as unmarked for every chain, forever")
	}
	if _, err := o.Claim(t.Context(), "", base, time.Minute); err == nil {
		t.Error("Claim accepted a blank agent id")
	}
	if _, _, err := o.Get(t.Context(), ""); err == nil {
		t.Error("Get accepted a blank agent id")
	}
	// Counterfactual: the well-formed call on the same store succeeds.
	if err := o.Mark(t.Context(), learning.Marker{AgentID: "seat-1", ChainHash: chain}, base); err != nil {
		t.Errorf("Mark of a complete marker: %v", err)
	}
}

func TestGetOnASeatWithNoRowIsAbsenceNotAnError(t *testing.T) {
	t.Parallel()
	o, _ := onboarding(t)
	m, ok, err := o.Get(t.Context(), "seat-nobody")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Errorf("Get reported a row: %+v", m)
	}
}

// --- the pass lease -------------------------------------------------------

func TestAClaimedRowIsStillNotOnboarded(t *testing.T) {
	t.Parallel()
	o, _ := onboarding(t)
	chain := hashOf("Acme", []string{"Ops"}, "eng")

	p, err := o.Claim(t.Context(), "seat-1", base, 15*time.Minute)
	if err != nil || !p.Held() {
		t.Fatalf("Claim = %+v (err %v), want a held pass", p, err)
	}
	// The claim inserts the row for a seat that has never marked. If that
	// row read as onboarded, claiming the pass would cancel the very pass
	// it was claimed for.
	if got, err := o.Onboarded(t.Context(), "seat-1", chain); err != nil || got {
		t.Errorf("Onboarded after a bare claim = %v (err %v), want false", got, err)
	}
	m, ok, err := o.Get(t.Context(), "seat-1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if m.ChainHash != "" {
		t.Errorf("claim wrote chain_hash %q, want empty", m.ChainHash)
	}
	if !m.LeaseHeld(base) {
		t.Errorf("the claimed lease reads as free at the moment it was taken")
	}
}

func TestExactlyOneClaimantWins(t *testing.T) {
	t.Parallel()
	o, _ := onboarding(t)
	const claimants = 8

	var (
		mu   sync.Mutex
		held []learning.Pass
		bad  []error
		gate sync.WaitGroup
		done sync.WaitGroup
	)
	gate.Add(1)
	for range claimants {
		done.Add(1)
		go func() {
			defer done.Done()
			gate.Wait()
			// Every claimant passes the SAME now, so nobody can win by
			// arriving after the lease it is racing has expired.
			p, err := o.Claim(t.Context(), "seat-1", base, 15*time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				bad = append(bad, err)
			}
			if p.Held() {
				held = append(held, p)
			}
		}()
	}
	gate.Done()
	done.Wait()

	if len(bad) > 0 {
		t.Errorf("claims errored: %v", bad)
	}
	if len(held) != 1 {
		t.Fatalf("%d of %d claimants hold the pass, want exactly 1", len(held), claimants)
	}
	// Counterfactual: the single winner is not an artefact of every claim
	// failing. Give the lease back and the next claimant takes it.
	if err := o.Release(t.Context(), held[0], base.Add(time.Minute)); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if p, err := o.Claim(t.Context(), "seat-1", base.Add(time.Minute), 15*time.Minute); err != nil || !p.Held() {
		t.Errorf("claim after release = %+v (err %v), want a held pass", p, err)
	}
}

func TestTwoIndependentHandlesStillProduceOneWinner(t *testing.T) {
	t.Parallel()
	// The mutual exclusion has to live in the STATEMENT, not in a pool: the
	// lease exists for two claimants that share nothing but the row. Two
	// handles on one file is the closest this test can get to that without a
	// second process, and it is deliberately not an endorsement of running
	// two engines on one file (see the store package doc).
	path := filepath.Join(t.TempDir(), "shared.db")
	a, b := openStore(t, path), openStore(t, path)

	var (
		mu   sync.Mutex
		held []learning.Pass
		gate sync.WaitGroup
		done sync.WaitGroup
	)
	gate.Add(1)
	for _, db := range []*store.DB{a, b, a, b} {
		o := onboardingOn(t, db)
		done.Add(1)
		go func() {
			defer done.Done()
			gate.Wait()
			p, err := o.Claim(t.Context(), "seat-1", base, 15*time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("Claim: %v", err)
			}
			if p.Held() {
				held = append(held, p)
			}
		}()
	}
	gate.Done()
	done.Wait()

	if len(held) != 1 {
		t.Fatalf("%d claimants across two handles hold the pass, want exactly 1", len(held))
	}
	// The loser's handle must see the winner's lease, not its own idea of it.
	m, ok, err := onboardingOn(t, b).Get(t.Context(), "seat-1")
	if err != nil || !ok {
		t.Fatalf("Get through the other handle: ok=%v err=%v", ok, err)
	}
	if !m.LeaseUntil.Equal(held[0].Until) {
		t.Errorf("other handle sees lease until %v, winner holds %v", m.LeaseUntil, held[0].Until)
	}
}

func TestTheClaimAndTheReadAgreeOnTheExpiryBoundary(t *testing.T) {
	t.Parallel()
	const ttl = 15 * time.Minute
	until := base.Add(ttl)

	for _, tc := range []struct {
		what string
		now  time.Time
		free bool
	}{
		{"a microsecond before the deadline", until.Add(-time.Microsecond), false},
		{"exactly at the deadline", until, true},
		{"a microsecond after the deadline", until.Add(time.Microsecond), true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			o, _ := onboarding(t)
			if p, err := o.Claim(t.Context(), "seat-1", base, ttl); err != nil || !p.Held() {
				t.Fatalf("first claim = %+v (err %v)", p, err)
			}
			m, ok, err := o.Get(t.Context(), "seat-1")
			if err != nil || !ok {
				t.Fatalf("Get: ok=%v err=%v", ok, err)
			}
			p, err := o.Claim(t.Context(), "seat-1", tc.now, ttl)
			if err != nil {
				t.Fatalf("second claim: %v", err)
			}
			if p.Held() != tc.free {
				t.Errorf("claim at %s: held=%v, want %v", tc.what, p.Held(), tc.free)
			}
			// The two surfaces must never disagree: a reader that believes
			// a peer is mid-pass while any turn can take the lease is
			// reporting a pass nobody is running.
			if m.LeaseHeld(tc.now) == p.Held() {
				t.Errorf("LeaseHeld(%s) = %v and the claim %s — the read and the claim disagree",
					tc.what, m.LeaseHeld(tc.now), map[bool]string{true: "won", false: "lost"}[p.Held()])
			}
		})
	}
}

func TestTheBoundaryIsTheStoredResolutionNotTheNanosecond(t *testing.T) {
	t.Parallel()
	// A wall clock hands out nanoseconds, so a real deadline lands INSIDE a
	// microsecond: claim at …000000500ns and Pass.Until carries those 500ns
	// while the column stores …000000µs, truncated. A claimant that checks
	// whether it still holds its own lease — the natural thing to do partway
	// through a long pass — asks with the deadline it was given.
	//
	// Here the claimant asks 200ns before its Go deadline: still before it by
	// a nanosecond comparison, already at it by the stored one. The claim,
	// which can only see the stored value, hands the lease to the next
	// caller at that instant. LeaseHeld has to agree, or a seat goes on
	// believing it owns a pass another turn is already running.
	o, _ := onboarding(t)
	const ttl = 15 * time.Minute
	claimAt := base.Add(500 * time.Nanosecond)

	p, err := o.Claim(t.Context(), "seat-1", claimAt, ttl)
	if err != nil || !p.Held() {
		t.Fatalf("first claim = %+v (err %v)", p, err)
	}
	check := p.Until.Add(-200 * time.Nanosecond)
	if !check.Before(p.Until) {
		t.Fatalf("this test's premise is gone: %v is not before %v", check, p.Until)
	}

	// The claimant's own view, built from the deadline the claim returned.
	if (learning.Marker{LeaseUntil: p.Until}).LeaseHeld(check) {
		t.Error("LeaseHeld says the lease is still held at an instant the claim gives it away")
	}
	// The row's view, truncated on the way through the column, must say the
	// same thing.
	m, ok, err := o.Get(t.Context(), "seat-1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if m.LeaseHeld(check) {
		t.Error("the stored lease reads as held past its own truncated deadline")
	}
	next, err := o.Claim(t.Context(), "seat-1", check, ttl)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !next.Held() {
		t.Fatal("the claim refused a lease whose STORED deadline has passed")
	}
}

func TestReleaseIsFencedToTheClaimItTook(t *testing.T) {
	t.Parallel()
	o, _ := onboarding(t)
	const ttl = 15 * time.Minute

	slow, err := o.Claim(t.Context(), "seat-1", base, ttl)
	if err != nil || !slow.Held() {
		t.Fatalf("first claim = %+v (err %v)", slow, err)
	}
	// The first claimant did not die, it ran LONG — indistinguishable from
	// dead until it comes back. Its lease lapses and a second engine starts
	// the pass.
	afterTTL := base.Add(ttl + time.Minute)
	next, err := o.Claim(t.Context(), "seat-1", afterTTL, ttl)
	if err != nil || !next.Held() {
		t.Fatalf("claim after the ttl = %+v (err %v)", next, err)
	}

	// …and now the slow one finishes and releases.
	if err := o.Release(t.Context(), slow, afterTTL); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	m, ok, err := o.Get(t.Context(), "seat-1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if !m.LeaseHeld(afterTTL) {
		t.Fatal("a stale release cleared a live lease — a third pass can now start " +
			"on top of the one already running")
	}
	if p, err := o.Claim(t.Context(), "seat-1", afterTTL, ttl); err != nil || p.Held() {
		t.Errorf("a claim after the stale release = %+v (err %v), want it refused", p, err)
	}

	// Counterfactual: the holder's OWN release does clear it.
	if err := o.Release(t.Context(), next, afterTTL); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if p, err := o.Claim(t.Context(), "seat-1", afterTTL, ttl); err != nil || !p.Held() {
		t.Errorf("claim after the holder released = %+v (err %v), want a held pass", p, err)
	}
}

func TestReleasingAPassThatWasNeverTakenTouchesNothing(t *testing.T) {
	t.Parallel()
	o, _ := onboarding(t)
	// The skip path calls Release unconditionally, so the zero Pass must be
	// a no-op rather than a row.
	if err := o.Release(t.Context(), learning.Pass{}, base); err != nil {
		t.Fatalf("Release(zero): %v", err)
	}
	if _, ok, err := o.Get(t.Context(), "seat-1"); err != nil || ok {
		t.Errorf("Get after a zero release: ok=%v err=%v, want no row", ok, err)
	}
}

func TestMarkingEndsThePass(t *testing.T) {
	t.Parallel()
	o, _ := onboarding(t)
	chain := hashOf("Acme", []string{"Ops"}, "eng")
	if p, err := o.Claim(t.Context(), "seat-1", base, 15*time.Minute); err != nil || !p.Held() {
		t.Fatalf("Claim = %+v (err %v)", p, err)
	}
	if err := o.Mark(t.Context(), learning.Marker{AgentID: "seat-1", ChainHash: chain}, base.Add(time.Minute)); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	m, ok, err := o.Get(t.Context(), "seat-1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	// Unlike Release, this clear is unfenced on purpose: the work the lease
	// was serialising is finished, so nothing is left to protect. Leaving it
	// set would block re-onboarding for the rest of the TTL after a
	// restructure.
	if !m.LeaseUntil.IsZero() {
		t.Errorf("mark left the lease at %v", m.LeaseUntil)
	}
	if m.LeaseHeld(base.Add(time.Minute)) {
		t.Error("mark left a held lease")
	}
	if got, err := o.Onboarded(t.Context(), "seat-1", chain); err != nil || !got {
		t.Errorf("Onboarded after mark = %v (err %v), want true", got, err)
	}
}

func TestAFailedClaimIsAClaimNotTaken(t *testing.T) {
	t.Parallel()
	o, db := onboarding(t)
	// CONTROL: a claim succeeds against the healthy store, so the assertion
	// below is about the failure and not about a store that never claims.
	if p, err := o.Claim(t.Context(), "seat-control", base, 15*time.Minute); err != nil || !p.Held() {
		t.Fatalf("control claim = %+v (err %v)", p, err)
	}
	_ = db.Close()

	p, err := o.Claim(t.Context(), "seat-1", base, 15*time.Minute)
	if err == nil {
		t.Fatal("a claim against a dead store reported no error")
	}
	if p.Held() {
		// Fail-closed: a caller reading only Held() must
		// skip, because two engines running onboarding at once costs far
		// more than one turn's delay.
		t.Error("a failed claim came back HELD — the pass would run on unknown state")
	}
	if err := o.Mark(t.Context(), learning.Marker{AgentID: "seat-1", ChainHash: "abc"}, base); err == nil {
		t.Error("Mark against a dead store reported no error")
	}
	if err := o.Release(t.Context(), learning.Pass{AgentID: "seat-1", Until: base}, base); err == nil {
		t.Error("Release against a dead store reported no error")
	}
}

func TestAClaimNeedsAPositiveTTL(t *testing.T) {
	t.Parallel()
	o, _ := onboarding(t)
	for _, ttl := range []time.Duration{0, -time.Second} {
		p, err := o.Claim(t.Context(), "seat-1", base, ttl)
		if err == nil {
			// A deadline at or before now is already expired when it is
			// written, so every concurrent claimant sees a free lease and
			// they all run the pass together.
			t.Errorf("Claim(ttl=%s) = %+v with no error", ttl, p)
		}
		if p.Held() {
			t.Errorf("Claim(ttl=%s) came back held", ttl)
		}
	}
	if _, ok, err := o.Get(t.Context(), "seat-1"); err != nil || ok {
		t.Errorf("a refused claim wrote a row: ok=%v err=%v", ok, err)
	}
}

func TestWhyTheClaimIsOneStatement(t *testing.T) {
	t.Parallel()
	// The measurement behind Claim's single conditional upsert. store.Tx
	// begins DEFERRED, so two claimants doing read-then-write both take
	// their snapshot before either writes. What the drivers do next is safe
	// but unusable: one commits and the other is REFUSED — turso with
	// "database snapshot is stale", modernc.org/sqlite with "database is
	// locked (517)". The loser therefore learns it lost through an error,
	// indistinguishable from the store being down, which is the distinction
	// Onboarded and Claim exist to keep. The upsert form gives that loser a
	// definite answer instead.
	_, db := onboarding(t)
	if _, err := db.SQL().ExecContext(t.Context(),
		`INSERT INTO agent_onboarding_markers (agent_id, chain_hash, created_at, updated_at)
		 VALUES ('seat-1', '', 0, 0)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var (
		mu      sync.Mutex
		outcome []error
		read    sync.WaitGroup
		done    sync.WaitGroup
	)
	read.Add(2)
	for i := range 2 {
		done.Add(1)
		go func() {
			defer done.Done()
			err := db.Tx(t.Context(), func(tx *sql.Tx) error {
				var lease sql.NullInt64
				if err := tx.QueryRowContext(t.Context(),
					`SELECT in_progress_until FROM agent_onboarding_markers WHERE agent_id='seat-1'`).
					Scan(&lease); err != nil {
					return err
				}
				// Both readers are through before either writes — which is
				// the whole race, made deterministic.
				read.Done()
				read.Wait()
				if lease.Valid {
					return nil
				}
				_, err := tx.ExecContext(t.Context(),
					`UPDATE agent_onboarding_markers SET in_progress_until = ? WHERE agent_id='seat-1'`,
					100+i)
				return err
			})
			mu.Lock()
			outcome = append(outcome, err)
			mu.Unlock()
		}()
	}
	done.Wait()

	failures := 0
	for _, err := range outcome {
		if err != nil {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("both transactions committed a claim on the same free lease — " +
			"read-then-write in a deferred transaction is a lost update here")
	}
	if failures == 2 {
		t.Errorf("neither transaction claimed the free lease: %v", outcome)
	}
	// One winner, and one loser holding an error it cannot tell from an
	// outage. That is the whole finding.
	t.Logf("read-then-write in store.Tx: %d of 2 transactions refused (%v)", failures, outcome)
}

// --- a broken write path --------------------------------------------------

// passWriteFault wraps a certified driver and breaks the WRITE path once
// armed.
//
// It reaches two branches nothing else does. The store's own fault injector
// (internal/store/storetest) intercepts result-set ITERATION, which is the
// read path; both of a claim's failure modes are writes. One of them is not a
// transport failure at all: database/sql documents Result.RowsAffected as
// something a driver need not support, so a statement can run and still leave
// its caller unable to say whether it changed anything.
//
// (skill_test.go carries a second, SQL-matching write fault. Two wrappers in
// one test package is duplication neither file can retire on its own — the
// shared home would be internal/store/storetest, which neither owns.)
//
// What runs underneath is the real, certified driver. Only the two behaviours
// under test are fiction.
type passWriteFault struct {
	armed atomic.Bool
	kind  passFaultKind
}

type passFaultKind int

const (
	// execRefused: the statement never runs. Telling that apart from "ran,
	// then failed" is what lets a test assert a call made NO write at all.
	execRefused passFaultKind = iota
	// rowsUnknown: the statement runs and its result cannot say what it
	// did — the ambiguous write that fail-closed exists for.
	rowsUnknown
)

var errPassWrite = errors.New("injected write failure")

// arm switches the fault on. Separate from wrap because a fault armed at Open
// would fail the migrations and the database would never exist.
func (f *passWriteFault) arm() { f.armed.Store(true) }

func (f *passWriteFault) wrap(d driver.Driver) driver.Driver {
	return passFaultDriver{inner: d, fault: f}
}

type passFaultDriver struct {
	inner driver.Driver
	fault *passWriteFault
}

func (d passFaultDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return passFaultConn{Conn: conn, fault: d.fault}, nil
}

// The store's connector requires ExecerContext, and database/sql picks the
// query path from the optional interfaces a conn implements — embedding
// driver.Conn carries neither, so both are forwarded explicitly.
type passFaultConn struct {
	driver.Conn
	fault *passWriteFault
}

func (c passFaultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if c.fault.armed.Load() && c.fault.kind == execRefused {
		return nil, errPassWrite
	}
	res, err := ex.ExecContext(ctx, q, args)
	if err != nil || !c.fault.armed.Load() {
		return res, err
	}
	return unknownRowsResult{}, nil
}

func (c passFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qr.QueryContext(ctx, q, args)
}

// unknownRowsResult is a write that happened and cannot say what it did.
type unknownRowsResult struct{}

func (unknownRowsResult) LastInsertId() (int64, error) { return 0, errPassWrite }
func (unknownRowsResult) RowsAffected() (int64, error) { return 0, errPassWrite }

func faultedStore(t *testing.T, f *passWriteFault) *learning.Onboarding {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "f.db"),
		store.Options{WrapDriver: f.wrap})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return learning.NewOnboarding(db)
}

func TestReleasingAPassThatWasNeverTakenReachesNoStoreAtAll(t *testing.T) {
	t.Parallel()
	// The skip path is the overwhelmingly common one — every turn of every
	// already-onboarded seat — and a caller on it releases unconditionally.
	// On a single-writer database an UPDATE that can only ever match nothing
	// is still a write lock taken per turn per seat, and it hands that
	// caller whatever mood the store is in.
	fault := &passWriteFault{kind: execRefused}
	o := faultedStore(t, fault)

	// CONTROL: the store is healthy and a real claim goes through, so what
	// follows is about the fault and not about a store that was never up.
	p, err := o.Claim(t.Context(), "seat-1", base, 15*time.Minute)
	if err != nil || !p.Held() {
		t.Fatalf("control claim = %+v (err %v)", p, err)
	}

	fault.arm()
	if err := o.Release(t.Context(), learning.Pass{}, base); err != nil {
		t.Errorf("releasing a pass that was never taken went to the store: %v", err)
	}
	// Counterfactual: a HELD pass does reach the store, so the fault was
	// armed and would have been seen above.
	if err := o.Release(t.Context(), p, base); err == nil {
		t.Error("the armed write fault never reached a real release")
	}
}

func TestAWriteWhoseOutcomeCannotBeReadIsAClaimNotTaken(t *testing.T) {
	t.Parallel()
	// The other half of fail-closed, and the one no dead store can produce.
	// The statement RAN — the lease may well be sitting in the row — and the
	// driver cannot say whether it changed anything. Calling that a win is
	// how two engines run the pass at once; calling it a loss costs at most
	// the TTL before a lease nobody knows about expires.
	fault := &passWriteFault{kind: rowsUnknown}
	o := faultedStore(t, fault)

	if p, err := o.Claim(t.Context(), "seat-control", base, 15*time.Minute); err != nil || !p.Held() {
		t.Fatalf("control claim = %+v (err %v)", p, err)
	}

	fault.arm()
	p, err := o.Claim(t.Context(), "seat-1", base, 15*time.Minute)
	if err == nil {
		t.Fatal("a claim that could not read its own write reported success")
	}
	if p.Held() {
		t.Error("a claim whose write outcome is unknown came back HELD")
	}
}
