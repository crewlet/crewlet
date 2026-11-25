// Package spendguard holds every ad product's spend to the caps a company
// set, before a person is asked and again when the allowed change is applied.
//
// # Micros, never a float, and no currency table
//
// Money is int64 micros of the account's own currency. The guard compares
// micros only: each adapter converts its vendor's amounts with its vendor's
// own table (Meta's currency offsets differ from ISO 4217 for HUF, TWD and
// COP), so a shared table here would read a budget 100x wrong for one vendor.
//
// # The rules, at the ask and at the apply
//
// [Guard.Check] runs before an ask, so a person is never asked to allow what
// the engine would refuse; [Guard.Apply] runs them again against live
// capacity, under one lease per account, and makes the change inside it. In
// order: the caps' currency is the account's ([CodeCurrencyMismatch]); both
// caps are set ([CodeCapsUnset]: one cap alone fails closed); the change adds
// at most per_change ([CodeOverPerChange]); capacity after it is at most daily
// ([CodeOverDaily]); the account serves ([CodeNotServing]). A change that adds
// nothing still goes to a person: the class comes from the tool, never the
// delta.
//
// # Capacity
//
// [Capacity] is the adapter's: Daily is the daily amount of every budget of an
// enabled, unended campaign whatever its serving state, a shared budget once,
// a total budget as its remainder over its remaining days rounded up; Delta is
// what an operation adds to it on the same basis. The caps bound the budgets
// agents may set, never what a vendor delivers on a day.
//
// # One applier per account
//
// Two allowed raises read on the same capacity could both pass the daily cap.
// Apply holds the lease `spend/<kind>/<account>` ([Leaser]) while it re-reads
// Daily, re-checks and applies, so the second sees the first's effect. A
// product that holds one lease per account around every write it makes there
// (Google's, internal/googlegrowth) runs [Guard.Recheck] inside it instead, so
// no act holds two leases.
//
// spendguardtest is the one contract suite every Capacity runs.
package spendguard

import (
	"context"
	"errors"
	"fmt"
	_ "time/tzdata" // an account's zone loads on a minimal host too
)

// Caps are one account's caps as the company document sets them, in micros of
// Currency; nil is unset.
type Caps struct {
	Currency        string
	PerChangeMicros *int64
	DailyMicros     *int64
}

// Account is an ad account as its vendor answers it now.
type Account struct {
	ID string
	// Currency is the account's own, read live.
	Currency string
	// Serving is false for an account its vendor will not serve (suspended,
	// closed, unapproved); State says the vendor's words for it.
	Serving bool
	State   string
}

// Capacity is an ad account's daily spend capacity as its adapter reads it.
type Capacity[Op any] interface {
	// Daily is the account's daily capacity in micros now.
	Daily(ctx context.Context, account string) (int64, error)
	// Delta is how much op adds to Daily if applied now: 0 for a change that
	// adds nothing, negative for one that lowers it.
	Delta(ctx context.Context, account string, op Op) (int64, error)
}

// Leaser holds one lease at a time on a resource, fleet-wide.
type Leaser interface {
	// Hold takes resource's lease, waiting while another holds it, and
	// answers its release. An error took nothing.
	Hold(ctx context.Context, resource string) (release func(), err error)
}

// Code is why the guard refused a change.
type Code string

// The refusals.
const (
	CodeCurrencyMismatch Code = "currency_mismatch"
	CodeCapsUnset        Code = "caps_unset"
	CodeOverPerChange    Code = "over_per_change"
	CodeOverDaily        Code = "over_daily"
	CodeNotServing       Code = "not_serving"
)

// Figures are what a check read and computed, in micros of Currency.
type Figures struct {
	Currency                     string
	Daily, Delta, DailyAfter     int64
	PerChangeCap, DailyCap       int64
	PerChangeCapSet, DailyCapSet bool
}

// Refusal is a change the guard refused, with the figures that refused it.
type Refusal struct {
	Code    Code
	Account Account
	Caps    Caps
	Figures Figures
}

func (r *Refusal) Error() string {
	switch r.Code {
	case CodeCurrencyMismatch:
		return fmt.Sprintf("the caps for %s are in %s, but the account is in %s", r.Account.ID, r.Caps.Currency,
			r.Account.Currency)
	case CodeCapsUnset:
		return fmt.Sprintf("set both spending caps for %s first", r.Account.ID)
	case CodeOverPerChange:
		return fmt.Sprintf("the change adds %d micros a day to %s, over its per-change cap of %d", r.Figures.Delta,
			r.Account.ID, r.Figures.PerChangeCap)
	case CodeOverDaily:
		return fmt.Sprintf("%s's daily capacity would be %d micros, over its daily cap of %d", r.Account.ID,
			r.Figures.DailyAfter, r.Figures.DailyCap)
	case CodeNotServing:
		return fmt.Sprintf("%s cannot serve: %s", r.Account.ID, r.Account.State)
	}
	return string(r.Code)
}

// Guard holds one product's spend to its caps.
type Guard[Op any] struct {
	kind     string
	capacity Capacity[Op]
	leases   Leaser
}

// New is kind's guard over capacity, applying under leases.
func New[Op any](kind string, capacity Capacity[Op], leases Leaser) *Guard[Op] {
	return &Guard[Op]{kind: kind, capacity: capacity, leases: leases}
}

// Resource is the lease an account's applies are held under.
func Resource(kind, account string) string { return "spend/" + kind + "/" + account }

// Check holds op on account to caps now: the figures a person is shown, or
// a [*Refusal]. Any other error read nothing certain, and refuses too.
func (g *Guard[Op]) Check(ctx context.Context, account Account, caps Caps, op Op) (Figures, error) {
	f := Figures{Currency: account.Currency}
	if caps.Currency != account.Currency {
		return f, &Refusal{Code: CodeCurrencyMismatch, Account: account, Caps: caps, Figures: f}
	}
	if caps.PerChangeMicros == nil || caps.DailyMicros == nil {
		return f, &Refusal{Code: CodeCapsUnset, Account: account, Caps: caps, Figures: f}
	}
	f.PerChangeCap, f.DailyCap, f.PerChangeCapSet, f.DailyCapSet = *caps.PerChangeMicros, *caps.DailyMicros, true, true
	daily, err := g.capacity.Daily(ctx, account.ID)
	if err != nil {
		return f, fmt.Errorf("spendguard: reading %s's daily capacity: %w", account.ID, err)
	}
	delta, err := g.capacity.Delta(ctx, account.ID, op)
	if err != nil {
		return f, fmt.Errorf("spendguard: reading what the change adds to %s: %w", account.ID, err)
	}
	f.Daily, f.Delta, f.DailyAfter = daily, delta, daily+delta
	switch {
	case delta > f.PerChangeCap:
		return f, &Refusal{Code: CodeOverPerChange, Account: account, Caps: caps, Figures: f}
	case delta > 0 && f.DailyAfter > f.DailyCap:
		// A CHANGE THAT ADDS NOTHING is never refused by capacity a person
		// already let stand above the cap: lowering it below is the agent's
		// way back under, and enabling a keyword adds nothing.
		return f, &Refusal{Code: CodeOverDaily, Account: account, Caps: caps, Figures: f}
	case !account.Serving:
		return f, &Refusal{Code: CodeNotServing, Account: account, Caps: caps, Figures: f}
	}
	return f, nil
}

// Apply makes op on account once, under the account's lease: it reads the
// account and caps again (read), re-checks every rule against live capacity,
// and calls apply only if they pass. read answers the account as its vendor
// does now and the caps as the running document sets them. Every failure
// before apply is called, the lease's and the recheck's, wraps
// [ErrNotApplied]; apply's own error is answered as it is.
func (g *Guard[Op]) Apply(ctx context.Context, account string, op Op,
	read func(ctx context.Context) (Account, Caps, error), apply func(ctx context.Context) error) (Figures, error) {
	release, err := g.leases.Hold(ctx, Resource(g.kind, account))
	if err != nil {
		return Figures{}, fmt.Errorf("%w: spendguard: holding %s's apply lease: %w", ErrNotApplied, account, err)
	}
	defer release()
	f, err := g.Recheck(ctx, op, read)
	if err != nil {
		return f, fmt.Errorf("%w: %w", ErrNotApplied, err)
	}
	return f, apply(ctx)
}

// ErrNotApplied marks a failure of [Guard.Apply] before it called apply: the
// change was never sent, which a product tells its caller (internal/growth's
// ErrNotSent, which this package cannot import).
var ErrNotApplied = errors.New("spendguard: the change was not applied")

// Recheck is Apply's checks for a product that holds the account's one apply
// lease itself, around every write it makes there: it reads the account and
// caps again (read) and holds op to them on live capacity. Outside such a
// lease, two raises read on one capacity could both pass the daily cap.
func (g *Guard[Op]) Recheck(ctx context.Context, op Op, read func(ctx context.Context) (Account, Caps, error)) (
	Figures, error) {
	// READ AGAIN INSIDE THE LEASE: what was read before it may be the state
	// another applier was about to change.
	acct, caps, err := read(ctx)
	if err != nil {
		return Figures{}, err
	}
	return g.Check(ctx, acct, caps, op)
}
