// Package spendguardtest is the one contract suite every ad product's
// [spendguard.Capacity] runs: a raise over per_change, a raise over daily,
// unset caps and a currency mismatch are refused before a person is asked,
// and two allowed raises applied at once cannot both pass the daily cap.
package spendguardtest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/spendguard"
)

// Harness is one adapter's Capacity over its fake vendor.
type Harness[Op any] struct {
	// Kind is the product's integration kind.
	Kind string
	// New is a fresh account, its capacity daily micros, in currency, over a
	// fake vendor nothing else touches, with the capacity reading it.
	New func(t *testing.T, daily int64) (capacity spendguard.Capacity[Op], account spendguard.Account)
	// Raise is an operation adding delta micros to account's daily capacity.
	Raise func(t *testing.T, account string, delta int64) Op
	// Apply makes op at the fake vendor, so Daily reads its effect after.
	Apply func(ctx context.Context, account string, op Op) error
}

// micros is n whole units in micros.
func micros(n int64) int64 { return n * 1_000_000 }

func caps(currency string, perChange, daily int64) spendguard.Caps {
	return spendguard.Caps{Currency: currency, PerChangeMicros: &perChange, DailyMicros: &daily}
}

// Run is the suite.
func Run[Op any](t *testing.T, h Harness[Op]) {
	t.Helper()
	refusedAs := func(t *testing.T, err error, want spendguard.Code) {
		t.Helper()
		var r *spendguard.Refusal
		if !errors.As(err, &r) || r.Code != want {
			t.Fatalf("got %v, want a %s refusal", err, want)
		}
	}
	ctx := context.Background()
	t.Run("a raise within both caps passes", func(t *testing.T) {
		capacity, acct := h.New(t, micros(100))
		f, err := spendguard.New(h.Kind, capacity, &Locks{}).Check(ctx, acct, caps(acct.Currency, micros(50), micros(200)),
			h.Raise(t, acct.ID, micros(50)))
		if err != nil || f.Daily != micros(100) || f.Delta != micros(50) || f.DailyAfter != micros(150) {
			t.Fatalf("got %+v, %v; want daily 100, delta 50, after 150 units", f, err)
		}
	})
	t.Run("over per_change is refused before the ask", func(t *testing.T) {
		capacity, acct := h.New(t, micros(10))
		_, err := spendguard.New(h.Kind, capacity, &Locks{}).Check(ctx, acct, caps(acct.Currency, micros(50), micros(1000)),
			h.Raise(t, acct.ID, micros(50)+1))
		refusedAs(t, err, spendguard.CodeOverPerChange)
	})
	t.Run("over daily is refused", func(t *testing.T) {
		capacity, acct := h.New(t, micros(180))
		_, err := spendguard.New(h.Kind, capacity, &Locks{}).Check(ctx, acct, caps(acct.Currency, micros(50), micros(200)),
			h.Raise(t, acct.ID, micros(21)))
		refusedAs(t, err, spendguard.CodeOverDaily)
	})
	t.Run("one cap unset is refused", func(t *testing.T) {
		capacity, acct := h.New(t, micros(10))
		perChange := micros(50)
		_, err := spendguard.New(h.Kind, capacity, &Locks{}).Check(ctx, acct,
			spendguard.Caps{Currency: acct.Currency, PerChangeMicros: &perChange}, h.Raise(t, acct.ID, micros(1)))
		refusedAs(t, err, spendguard.CodeCapsUnset)
	})
	t.Run("a currency mismatch is refused", func(t *testing.T) {
		capacity, acct := h.New(t, micros(10))
		other := "JPY"
		if acct.Currency == other {
			other = "EUR"
		}
		_, err := spendguard.New(h.Kind, capacity, &Locks{}).Check(ctx, acct, caps(other, micros(50), micros(200)),
			h.Raise(t, acct.ID, micros(1)))
		refusedAs(t, err, spendguard.CodeCurrencyMismatch)
	})
	t.Run("two allowed raises cannot both pass the daily cap", func(t *testing.T) {
		capacity, acct := h.New(t, micros(100))
		guard := spendguard.New(h.Kind, capacity, &Locks{})
		limits := caps(acct.Currency, micros(80), micros(200))
		// EACH PASSES ALONE, read on the same capacity: 100 + 80 is within 200,
		// and 100 + 80 + 80 is not.
		ops := []Op{h.Raise(t, acct.ID, micros(80)), h.Raise(t, acct.ID, micros(80))}
		for _, op := range ops {
			if _, err := guard.Check(ctx, acct, limits, op); err != nil {
				t.Fatalf("a raise asked alone was refused: %v", err)
			}
		}
		var wg sync.WaitGroup
		errs := make([]error, len(ops))
		for i, op := range ops {
			wg.Go(func() {
				_, errs[i] = guard.Apply(ctx, acct.ID, op,
					func(context.Context) (spendguard.Account, spendguard.Caps, error) { return acct, limits, nil },
					func(ctx context.Context) error { return h.Apply(ctx, acct.ID, op) })
			})
		}
		wg.Wait()
		applied := 0
		for _, err := range errs {
			if err == nil {
				applied++
				continue
			}
			refusedAs(t, err, spendguard.CodeOverDaily)
		}
		if daily, err := capacity.Daily(ctx, acct.ID); applied != 1 || err != nil || daily != micros(180) {
			t.Fatalf("%d raises applied and daily reads %d (%v); want one, and 180 units", applied, daily, err)
		}
	})
}

// Locks is an in-process [spendguard.Leaser]: one holder per resource.
type Locks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

// Hold takes resource's lock, waiting while another holds it or until ctx
// ends; an error took nothing, as [spendguard.Leaser] says.
func (l *Locks) Hold(ctx context.Context, resource string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]chan struct{}{}
	}
	slot := l.locks[resource]
	if slot == nil {
		slot = make(chan struct{}, 1)
		l.locks[resource] = slot
	}
	l.mu.Unlock()
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
