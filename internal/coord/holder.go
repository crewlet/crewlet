package coord

import (
	"context"
	"time"
)

// LeaseGetter reads one resource's lease: the half of [Backend] a reader that
// asks who holds a resource needs.
type LeaseGetter interface {
	Get(ctx context.Context, resource string) (*Lease, error)
}

// Holder is the owner of resource's lease on the store's word, or "" while
// nothing holds it, and an error is UNKNOWN: it says nothing about who holds
// the resource. It reads no clock. The lease Get hands out was live by the
// store's clock when read, and judged again on the reader's own it reads as
// lapsed on any node whose clock runs ahead (the package doc, "A lease a read
// hands out is the store's word").
func Holder(ctx context.Context, leases LeaseGetter, resource string) (string, error) {
	lease, err := leases.Get(ctx, resource)
	if err != nil || lease == nil {
		return "", err
	}
	return lease.Owner, nil
}

// StoreClock reads the store's clock: the half of [Backend] a reader that says
// how long a lease has left needs.
type StoreClock interface {
	Now(ctx context.Context, class Class) (time.Time, error)
}

// TimeLeft reads the store's clock for class once ([Backend.Now]) and answers
// how long a lease of that class had left by it: its ExpiresAt less that
// reading, both the store's instants, and never below zero. An error is
// UNKNOWN, never "no time left". The one reading of a deadline outside a
// backend: held against the reader's own clock instead, every lease reads as
// run out on a node whose clock runs ahead of the store's (the package doc,
// "A lease a read hands out is the store's word").
func TimeLeft(ctx context.Context, clock StoreClock, class Class) (func(Lease) time.Duration, error) {
	now, err := clock.Now(ctx, class)
	if err != nil {
		return nil, err
	}
	return func(lease Lease) time.Duration { return max(lease.ExpiresAt.Sub(now), 0) }, nil
}
