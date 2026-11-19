package coord_test

// leaseDeadlineFixture is a module whose verdict is known, for
// TestTheLeaseDeadlineWalkJudgesAKnownTree: a coord package declaring Lease and
// TimeLeft, with a reader of its own; a backend and the contract suite inside
// internal/coord/... reading a deadline; every shape a reader outside them
// takes (/fleet's and fleet_health's took the first: a lease ListLive listed),
// a reader in cmd/ and one at the module root; and what the walk must leave
// alone or cannot type.
var leaseDeadlineFixture = map[string]string{
	"internal/coord/coord.go": `package coord

import (
	"context"
	"time"
)

type Lease struct {
	Resource  string
	Owner     string
	ExpiresAt time.Time
}

type Backend interface {
	Get(ctx context.Context, resource string) (*Lease, error)
	ListLive(ctx context.Context, class string) ([]Lease, error)
	Now(ctx context.Context, class string) (time.Time, error)
}

func TimeLeft(ctx context.Context, b Backend, class string) (func(Lease) time.Duration, error) {
	now, err := b.Now(ctx, class)
	if err != nil {
		return nil, err
	}
	return func(l Lease) time.Duration { return l.ExpiresAt.Sub(now) }, nil
}
`,

	"internal/coord/view.go": `package coord

import "time"

type view struct {
	leases []Lease
}

func (v view) stale(now time.Time) bool { return v.leases[0].ExpiresAt.Before(now) }

func (v view) TimeLeft(now time.Time) time.Duration { return v.leases[0].ExpiresAt.Sub(now) }
`,

	"internal/coord/memory/memory.go": `package memory

import (
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

func judged(l coord.Lease, now time.Time) bool { return l.ExpiresAt.After(now) }
`,

	"internal/coord/coordtest/cases.go": `package coordtest

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

func control(ctx context.Context, b coord.Backend) bool {
	listed, _ := b.ListLive(ctx, "seat")
	return listed[0].ExpiresAt.After(time.Now())
}
`,

	"internal/atlasdomain/expiry.go": `package atlasdomain

import "time"

var ExpiresAt = time.Unix(0, 0)
`,

	"internal/readers/readers.go": `package readers

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/coord"
)

type heldBy struct {
	coord.Lease
}

type local = coord.Lease

type row struct {
	lease coord.Lease
}

func first(leases []coord.Lease) coord.Lease { return leases[0] }

var cached atomic.Pointer[coord.Lease]

func holder(ctx context.Context, b coord.Backend, resource string, now time.Time,
	byName map[string]coord.Lease, ch chan coord.Lease, x any) (string, bool) {

	held, err := b.Get(ctx, resource)
	if err != nil || held == nil || !held.ExpiresAt.After(time.Now()) {
		return "", false
	}
	leases, _ := b.ListLive(ctx, "seat")
	for _, listed := range leases {
		if listed.ExpiresAt.Before(now) {
			return "", false
		}
	}
	for _, named := range byName {
		_ = named.ExpiresAt
	}
	embedded := heldBy{Lease: *held}
	var aliased local = *held
	r := row{lease: *held}
	get := func() coord.Lease { return *held }
	_ = atlasdomain.ExpiresAt
	left := time.Until(embedded.ExpiresAt) + time.Until(aliased.ExpiresAt) +
		time.Until(r.lease.ExpiresAt) + time.Until(leases[0].ExpiresAt) +
		time.Until(first(leases).ExpiresAt) + time.Until(byName["seat"].ExpiresAt) +
		time.Until((<-ch).ExpiresAt) + time.Until(x.(coord.Lease).ExpiresAt) +
		time.Until((*held).ExpiresAt) + time.Until(coord.Lease(*held).ExpiresAt) +
		time.Until(get().ExpiresAt) + time.Until(cached.Load().ExpiresAt)
	return held.Owner, left > 0 && switched(*held, now)
}

func deadline(lease coord.Lease) time.Time { return lease.ExpiresAt }

func moved(lease *coord.Lease) { lease.ExpiresAt = lease.ExpiresAt.Add(time.Hour) }

func switched(x any, now time.Time) bool {
	switch v := x.(type) {
	case coord.Lease:
		return v.ExpiresAt.After(now)
	}
	return false
}
`,

	"internal/readers/tokens.go": `package readers

import "time"

type token struct {
	ExpiresAt time.Time
}

type grant struct{}

func (grant) ExpiresAt() time.Time { return time.Time{} }

func fresh(t token, g grant) bool { return t.ExpiresAt.After(time.Now()) && g.ExpiresAt().IsZero() }
`,

	"internal/readers/readers_test.go": `package readers

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

func TestJudged(t *testing.T) {
	if (coord.Lease{}).ExpiresAt.After(time.Now()) {
		t.Fatal("live")
	}
}
`,

	"internal/readers/testdata/judge.go": `package testdata

import (
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

func judged(l coord.Lease) bool { return l.ExpiresAt.After(time.Now()) }
`,

	"cmd/tool/main.go": `package main

import (
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

func main() {
	var lease coord.Lease
	_ = lease.ExpiresAt.After(time.Now())
}
`,

	"root.go": `package crewlet

import (
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

func judged(lease coord.Lease) bool { return lease.ExpiresAt.After(time.Now()) }
`,
}

// leaseDeadlineFixtureVerdicts is what the walk must report of
// leaseDeadlineFixture, by kind, each selection as "file: receiver".
var leaseDeadlineFixtureVerdicts = map[string]map[string]int{
	"selection of coord.Lease.ExpiresAt": {
		"internal/coord/view.go: v.leases[0]":             2,
		"internal/readers/readers.go: held":               1,
		"internal/readers/readers.go: listed":             1,
		"internal/readers/readers.go: named":              1,
		"internal/readers/readers.go: embedded":           1,
		"internal/readers/readers.go: aliased":            1,
		"internal/readers/readers.go: r.lease":            1,
		"internal/readers/readers.go: leases[0]":          1,
		"internal/readers/readers.go: first(leases)":      1,
		`internal/readers/readers.go: byName["seat"]`:     1,
		"internal/readers/readers.go: (<-ch)":             1,
		"internal/readers/readers.go: x.(coord.Lease)":    1,
		"internal/readers/readers.go: (*held)":            1,
		"internal/readers/readers.go: coord.Lease(*held)": 1,
		"internal/readers/readers.go: get()":              1,
		"internal/readers/readers.go: lease":              3,
		"cmd/tool/main.go: lease":                         1,
		"root.go: lease":                                  1,
	},
	"untyped receiver": {
		"internal/readers/readers.go: cached.Load()": 1,
		"internal/readers/readers.go: v":             1,
	},
}
