package spendguard_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/spendguard"
	"github.com/crewlet/crewlet/internal/spendguard/spendguardtest"
)

// twin is a vendor of one number per account: its daily capacity.
type twin struct {
	mu    sync.Mutex
	daily map[string]int64
	trace *[]string
}

type raise struct{ delta int64 }

func (v *twin) Daily(_ context.Context, account string) (int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.note("daily")
	return v.daily[account], nil
}

func (v *twin) Delta(_ context.Context, _ string, op raise) (int64, error) {
	v.note("delta")
	return op.delta, nil
}

func (v *twin) apply(account string, op raise) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.note("apply")
	v.daily[account] += op.delta
}

func (v *twin) note(s string) {
	if v.trace != nil {
		*v.trace = append(*v.trace, s)
	}
}

// The memory twin runs the suite every ad product's Capacity runs.
func TestTheTwinKeepsTheSpendContract(t *testing.T) {
	t.Parallel()
	vendors := map[string]*twin{}
	var mu sync.Mutex
	spendguardtest.Run(t, spendguardtest.Harness[raise]{
		Kind: "twin",
		New: func(t *testing.T, daily int64) (spendguard.Capacity[raise], spendguard.Account) {
			v := &twin{daily: map[string]int64{t.Name(): daily}}
			mu.Lock()
			vendors[t.Name()] = v
			mu.Unlock()
			return v, spendguard.Account{ID: t.Name(), Currency: "EUR", Serving: true}
		},
		Raise: func(_ *testing.T, _ string, delta int64) raise { return raise{delta: delta} },
		Apply: func(_ context.Context, account string, op raise) error {
			mu.Lock()
			v := vendors[account]
			mu.Unlock()
			v.apply(account, op)
			return nil
		},
	})
}

// recordingLocks notes when the lease is taken and released.
type recordingLocks struct{ trace *[]string }

func (l recordingLocks) Hold(context.Context, string) (func(), error) {
	*l.trace = append(*l.trace, "hold")
	return func() { *l.trace = append(*l.trace, "release") }, nil
}

// An apply reads capacity, checks and applies inside the account's lease, so a
// second applier reads the first's effect.
//
// Mutation: read Daily before taking the lease, or apply after releasing it,
// and two raises read on one stale capacity both pass the daily cap.
func TestAnApplyReadsAndAppliesInsideTheLease(t *testing.T) {
	t.Parallel()
	var trace []string
	v := &twin{daily: map[string]int64{"a": 0}, trace: &trace}
	perChange, daily := int64(10), int64(100)
	acct := spendguard.Account{ID: "a", Currency: "EUR", Serving: true}
	_, err := spendguard.New("twin", v, recordingLocks{&trace}).Apply(context.Background(), "a", raise{delta: 5},
		func(context.Context) (spendguard.Account, spendguard.Caps, error) {
			return acct, spendguard.Caps{Currency: "EUR", PerChangeMicros: &perChange, DailyMicros: &daily}, nil
		},
		func(context.Context) error { v.apply("a", raise{delta: 5}); return nil })
	want := []string{"hold", "daily", "delta", "apply", "release"}
	if err != nil || !slices.Equal(trace, want) {
		t.Fatalf("an apply went %v (%v), want %v", trace, err, want)
	}
}

// A RECHECK READS THE ACCOUNT AND CAPACITY AGAIN AND TAKES NO LEASE: it runs
// inside the one lease its product holds around every write on the account,
// so the caps read now refuse what the ask's read let through.
//
// Mutation: check the caps the ask read, or take the spend lease inside the
// product's, and a lowered cap passes or the act holds two leases.
func TestARecheckReadsAgainInsideTheProductsLease(t *testing.T) {
	t.Parallel()
	var trace []string
	v := &twin{daily: map[string]int64{"a": 90}, trace: &trace}
	perChange, daily := int64(10), int64(100)
	acct := spendguard.Account{ID: "a", Currency: "EUR", Serving: true}
	_, err := spendguard.New("twin", v, recordingLocks{&trace}).Recheck(context.Background(), raise{delta: 5},
		func(context.Context) (spendguard.Account, spendguard.Caps, error) {
			trace = append(trace, "read")
			lowered := daily - 10
			return acct, spendguard.Caps{Currency: "EUR", PerChangeMicros: &perChange, DailyMicros: &lowered}, nil
		})
	var r *spendguard.Refusal
	if !errors.As(err, &r) || r.Code != spendguard.CodeOverDaily ||
		!slices.Equal(trace, []string{"read", "daily", "delta"}) {
		t.Fatalf("a recheck went %v (%v), want a read and an over_daily refusal under no lease", trace, err)
	}
}

// An account that cannot serve refuses spend, and a change adding nothing to
// capacity already above the daily cap still passes the caps.
//
// Mutation: refuse every change while capacity is above the cap, and an agent
// cannot lower a budget a person let stand above it.
func TestAChangeThatAddsNothingIsNotRefusedByCapacity(t *testing.T) {
	t.Parallel()
	v := &twin{daily: map[string]int64{"a": 500}}
	perChange, daily := int64(10), int64(100)
	caps := spendguard.Caps{Currency: "EUR", PerChangeMicros: &perChange, DailyMicros: &daily}
	g := spendguard.New("twin", v, &spendguardtest.Locks{})
	acct := spendguard.Account{ID: "a", Currency: "EUR", Serving: true}
	if _, err := g.Check(context.Background(), acct, caps, raise{delta: 0}); err != nil {
		t.Errorf("enabling a keyword above the cap was refused: %v", err)
	}
	acct.Serving, acct.State = false, "SUSPENDED"
	var r *spendguard.Refusal
	if _, err := g.Check(context.Background(), acct, caps, raise{delta: 0}); !errors.As(err, &r) ||
		r.Code != spendguard.CodeNotServing {
		t.Errorf("a suspended account answered %v, want not_serving", err)
	}
}

// THE CONTRACT SUITE'S LOCKS KEEP THE LEASER'S WORD: a hold whose context
// ends takes nothing, so the resource stays free for the next holder, and a
// waiter whose context ends stops waiting.
//
// Mutation: take the lock before reading the context, and the next hold
// waits forever.
func TestALockWhoseContextEndedTakesNothing(t *testing.T) {
	t.Parallel()
	locks := &spendguardtest.Locks{}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if release, err := locks.Hold(ended, "spend/twin/a"); err == nil || release != nil {
		t.Fatalf("a hold with an ended context took the lock (%v)", err)
	}
	release, err := locks.Hold(t.Context(), "spend/twin/a")
	if err != nil {
		t.Fatalf("the lock was left taken: %v", err)
	}
	waiting, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := locks.Hold(waiting, "spend/twin/a")
		done <- err
	}()
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("a waiter whose context ended answered %v", err)
	}
	release()
}

// failingLocks fails every lease with err.
type failingLocks struct{ err error }

func (l failingLocks) Hold(context.Context, string) (func(), error) { return nil, l.err }

// EVERY FAILURE BEFORE APPLY IS NOT APPLIED, and apply's own is not: a lease
// the store could not take and a read again that failed both say the change
// was never sent, so a product tells its caller nothing was changed; an
// apply that failed may have changed something, and says nothing of it.
//
// Mutation: drop ErrNotApplied from the lease's failure or the recheck's, or
// wrap apply's error in it, and this fails.
func TestOnlyAFailureBeforeApplyIsNotApplied(t *testing.T) {
	t.Parallel()
	v := &twin{daily: map[string]int64{"a": 0}, trace: &[]string{}}
	perChange, daily := int64(10), int64(100)
	acct := spendguard.Account{ID: "a", Currency: "EUR", Serving: true}
	read := func(context.Context) (spendguard.Account, spendguard.Caps, error) {
		return acct, spendguard.Caps{Currency: "EUR", PerChangeMicros: &perChange, DailyMicros: &daily}, nil
	}
	applied := false
	apply := func(context.Context) error { applied = true; return nil }
	unavailable := errors.New("the store did not answer")
	_, err := spendguard.New("twin", v, failingLocks{unavailable}).Apply(t.Context(), "a", raise{delta: 5}, read,
		apply)
	if !errors.Is(err, spendguard.ErrNotApplied) || !errors.Is(err, unavailable) || applied {
		t.Errorf("a lease the store could not take = %v (applied %v), want not applied, naming the store", err,
			applied)
	}
	unread := errors.New("the vendor did not answer the read")
	_, err = spendguard.New("twin", v, recordingLocks{&[]string{}}).Apply(t.Context(), "a", raise{delta: 5},
		func(context.Context) (spendguard.Account, spendguard.Caps, error) {
			return spendguard.Account{}, spendguard.Caps{}, unread
		}, apply)
	if !errors.Is(err, spendguard.ErrNotApplied) || !errors.Is(err, unread) || applied {
		t.Errorf("a read again that failed = %v (applied %v), want not applied, naming the read", err, applied)
	}
	sent := errors.New("the vendor's answer was cut off")
	_, err = spendguard.New("twin", v, recordingLocks{&[]string{}}).Apply(t.Context(), "a", raise{delta: 5}, read,
		func(context.Context) error { return sent })
	if !errors.Is(err, sent) || errors.Is(err, spendguard.ErrNotApplied) {
		t.Errorf("an apply that failed = %v, want its own error, never not applied", err)
	}
}
