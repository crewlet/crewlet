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
