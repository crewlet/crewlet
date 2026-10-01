package estate

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Which partition an operation addresses.
//
// # Every operation says, and the answer is a function of the layout
//
// A request goes to a node that serves the partition holding what it reads or
// writes, so every operation declares how its arguments resolve to that
// partition — once, beside its server half, where the two can never drift.
// Under layout 0 the answer is always the one partition, `estate.000`; under a
// layout that divides a domain it is the domain's own partition function,
// which is the domain's to state: a task by the id it was minted with, a page
// by its container. An operation that can only address a domain as ONE
// partition says so ([wholeDomain]), and under a layout that divides the
// domain it answers [ErrUnaddressed] — the same answer the domain itself gives
// ([statelog.Layout.OnlyPartition]) rather than a guess that sends the request
// to a node holding a different part of it.
//
// # A bare id resolves THROUGH the resolver
//
// An operation that names one object by id — a task, a page — asks the
// [Resolver], never the layout directly, because under a divided layout the
// partition of an id is a READ: the id's birth partition, then any forward a
// move left there. Under a layout that gives the domain one log it is that
// log's partition, which is all [layoutResolver] answers.

// Resolver is what a partition function may read, through the router, to
// resolve an object's id to the partition holding it.
type Resolver interface {
	// TaskPartition is the partition holding the task a key or an id
	// names.
	TaskPartition(ctx context.Context, idOrKey string) (statelog.PartitionID, error)

	// PagePartition is the partition holding the page an id names.
	PagePartition(ctx context.Context, id string) (statelog.PartitionID, error)
}

// ErrUnaddressed reports an operation that has no partition under the layout
// the fleet runs: it addresses its domain as one partition, and the layout
// divides the domain.
var ErrUnaddressed = errors.New("estate: the operation addresses no partition under the running layout")

// ErrPartitionUnserved is a partition no node served for an operation: every
// holder the asker's view named — this node among them where it holds it —
// answered that it does not serve it, could not run it, was behind, or did not
// answer at all.
//
// NEVER A FALSE "NOT FOUND". An operation on a partition nobody answered for
// is refused naming the partition, because a read reported as empty would say
// the company has none of what it asked about.
type ErrPartitionUnserved struct {
	// Partition is the partition, by its id (`estate.000`).
	Partition string

	// Detail is who was asked and what each said, in order.
	Detail string
}

func (e *ErrPartitionUnserved) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("estate: no node serves %s right now", e.Partition)
	}
	return fmt.Sprintf("estate: no node serves %s right now: %s", e.Partition, e.Detail)
}

// layoutResolver resolves an id by the layout alone: the domain's one
// partition, under a layout that gives the domain one log. It is every
// resolution this build makes — a divided layout's ids carry the partition
// they were minted in, and resolving one is a read this build's domains do not
// yet stamp — so under such a layout it answers [ErrUnaddressed].
type layoutResolver struct{ layout statelog.Layout }

// TaskPartition implements [Resolver].
func (r layoutResolver) TaskPartition(_ context.Context, idOrKey string) (statelog.PartitionID, error) {
	p, err := onlyPartition(r.layout, tracker.Domain{}.Name())
	if err != nil {
		return statelog.PartitionID{}, fmt.Errorf("resolve task %q: %w", idOrKey, err)
	}
	return p, nil
}

// PagePartition implements [Resolver].
func (r layoutResolver) PagePartition(_ context.Context, id string) (statelog.PartitionID, error) {
	p, err := onlyPartition(r.layout, pages.Domain{}.Name())
	if err != nil {
		return statelog.PartitionID{}, fmt.Errorf("resolve page %q: %w", id, err)
	}
	return p, nil
}

// onlyPartition is the named domain's one partition under l, or
// [ErrUnaddressed] where l divides it.
func onlyPartition(l statelog.Layout, domain string) (statelog.PartitionID, error) {
	p := l.OnlyPartition(domain)
	if !p.Valid() {
		return statelog.PartitionID{}, fmt.Errorf("%w: layout %d gives the %s domain %d logs",
			ErrUnaddressed, l.Number, domain, len(l.LogsOf(domain)))
	}
	return p, nil
}

// address is how an operation finds what it touches: the DOMAIN whose log it
// reads or writes, and the partitions its arguments address under a layout.
//
// # The domain is what a floor is on
//
// A request carries this node's floors on the logs it depends on, and the
// holder that answers waits to have applied them first ([ready]). Those logs
// are the operation's OWN DOMAIN's in the partition, never every log the
// partition carries: no operation reads another domain's rows — each domain's
// tables are written by its own applier alone, and under a layout that divides
// the estate they are not even in the same file — so a floor on another
// domain's log buys nothing, and costs a refusal whenever THAT log's applier
// lags. Under layout 0, where one partition carries the tracker's, the
// knowledge base's and the vectors' logs together, the seat a page comment
// woke would otherwise hold every tracker read and write its node routes until
// the pages applier reached the comment, and refuse them `behind` while that
// applier was faulted — the coupling a floor on another PARTITION's log was
// never carried for, refused within one partition for the same reason.
//
// The ZERO address addresses no partition: an operation any data node
// answers ([opSpec.partitions] nil).
type address[A any] struct {
	// domain is the domain whose log the operation depends on — its
	// floors' log in each partition it addresses. Empty only for an
	// operation that carries no floor ([opSpec.floorless]), which a gate
	// over the registry holds.
	domain string

	// partitions resolves the arguments to the partitions they address.
	partitions partitionsFunc[A]
}

// wholeDomain is the address of an operation that addresses the named domain
// as one partition: a query over all of it, or a write keyed by a container
// rather than an object's id.
func wholeDomain[A any](domain string) address[A] {
	return address[A]{domain: domain, partitions: func(_ context.Context, l statelog.Layout, _ Resolver,
		_ A) ([]statelog.PartitionID, error) {
		p, err := onlyPartition(l, domain)
		if err != nil {
			return nil, err
		}
		return []statelog.PartitionID{p}, nil
	}}
}

// byTask is the address of an operation that names one task, by the id or key
// id picks out of its arguments.
func byTask[A any](id func(A) string) address[A] {
	return address[A]{domain: trackerDomain, partitions: func(ctx context.Context, _ statelog.Layout,
		r Resolver, args A) ([]statelog.PartitionID, error) {
		p, err := r.TaskPartition(ctx, id(args))
		if err != nil {
			return nil, err
		}
		return []statelog.PartitionID{p}, nil
	}}
}

// byPage is the address of an operation that names one page, by the id id
// picks out of its arguments.
func byPage[A any](id func(A) string) address[A] {
	return address[A]{domain: pagesDomain, partitions: func(ctx context.Context, _ statelog.Layout,
		r Resolver, args A) ([]statelog.PartitionID, error) {
		p, err := r.PagePartition(ctx, id(args))
		if err != nil {
			return nil, err
		}
		return []statelog.PartitionID{p}, nil
	}}
}

// streamsOf is the stream of every log partition p carries under l — the cut
// a read of p reports ([statelog.Coverage.At]): where this copy has applied
// each of them.
func streamsOf(l statelog.Layout, p statelog.PartitionID) []string {
	var out []string
	for _, log := range l.Logs(p) {
		if name, _ := l.Stream(log); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// floorStreams is the stream of each log of the operation's own domain in p
// under l — the logs a request on p carries this node's floors on, and the
// ones a holder of p waits to have applied ([address]). None for an operation
// that carries no floor.
func (s *opSpec) floorStreams(l statelog.Layout, p statelog.PartitionID) []string {
	if s.floorless {
		return nil
	}
	var out []string
	for _, log := range l.Logs(p) {
		if log.Domain != s.domain {
			continue
		}
		if name, _ := l.Stream(log); name != "" {
			out = append(out, name)
		}
	}
	return out
}
