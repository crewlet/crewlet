package kv

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

var _ coord.BrowserControls = (*Store)(nil)

// TakeBrowserControl takes a launch's browser for t's owner: a claim of its
// lease whose Meta is the take's record, and whose give-back keeps how the
// tenure it covers ended (coord's package doc, "A person's control of a run's
// browser").
func (s *Store) TakeBrowserControl(ctx context.Context, t coord.BrowserControlTake,
	ttl time.Duration) (coord.BrowserControl, coord.BrowserControlTaken, error) {

	if err := t.Validate(); err != nil {
		return coord.BrowserControl{}, "", err
	}
	c, err := s.tryAcquire(ctx, coord.BrowserControlResource(t.Launch), coord.AcquireOptions{
		Owner: t.Owner.Key(), TTL: ttl,
		// UNGATED: the gate guards what holding a seat means, and a person's
		// gesture holds none; it must work through a rolling upgrade.
		Ungated: true,
	}, takeMeta(t), keepEnd)
	switch {
	case err != nil:
		return coord.BrowserControl{}, "", err
	case c.lease != nil:
		return coord.BrowserControlGranted(t.Launch, *c.lease, c.fresh)
	case c.refused == coord.RefusedHeld:
		return s.heldAgainst(ctx, t, c.refuser)
	}
	return coord.BrowserControl{}, "", fmt.Errorf("%w: a browser control's claim was refused %q, which "+
		"an ungated claim never is", coord.ErrUnavailable, c.refused)
}

// heldAgainst answers a take the store refused held from the record that
// refused it, judged on the clock it was judged on. Only another owner's live
// tenure is an answer: a claim of another owner still minting, or a record
// superseded inside one round trip, is unknown, never held naming nobody.
func (s *Store) heldAgainst(ctx context.Context, t coord.BrowserControlTake,
	snap snapshot) (coord.BrowserControl, coord.BrowserControlTaken, error) {

	e := snap.mine
	if e == nil || e.value.Epoch == claimingEpoch || e.value.Owner == t.Owner.Key() {
		return coord.BrowserControl{}, "", heldByNobody(t.Launch)
	}
	was, err := s.judge(ctx, *e, snap.clock)
	switch {
	case err != nil:
		return coord.BrowserControl{}, "", err
	case !was.Live:
		return coord.BrowserControl{}, "", heldByNobody(t.Launch)
	}
	control, err := coord.DecodeBrowserControl(t.Launch, e.value.Meta, was)
	if err != nil {
		return coord.BrowserControl{}, "", err
	}
	return control, coord.ControlHeld, nil
}

// heldByNobody is a take refused over a record that holds no other owner's
// live tenure: unknown, and asked again once the record settles.
func heldByNobody(launch uuid.UUID) error {
	return fmt.Errorf("%w: the browser control of launch %s is being taken by another owner, or "+
		"moved as it was read", coord.ErrUnavailable, launch)
}

// RenewBrowserControl extends the owner's live tenure at epoch: a renewal of
// its lease, answered from the record it wrote with no second read.
func (s *Store) RenewBrowserControl(ctx context.Context, launch uuid.UUID, owner coord.BrowserControlOwner,
	epoch int64, ttl time.Duration) (coord.BrowserControl, bool, error) {

	if err := coord.CheckBrowserControlTenure(launch, owner, epoch); err != nil {
		return coord.BrowserControl{}, false, err
	}
	w, err := s.renew(ctx, coord.BrowserControlResource(launch), owner.Key(), epoch, ttl)
	switch {
	case err != nil:
		return coord.BrowserControl{}, false, err
	case w == nil:
		return s.refusedWith(ctx, launch)
	}
	return answered(coord.DecodeBrowserControl(launch, w.value.Meta, coord.BrowserControlJudged{
		Epoch: w.value.Epoch, TakenAt: w.value.AcquiredAt, Live: true,
	}))
}

// EndBrowserControl ends the owner's live tenure at epoch as by: a release of
// its lease whose tombstone names the cause, answered from that tombstone read
// back by its revision, for the instant the store stamped on it.
func (s *Store) EndBrowserControl(ctx context.Context, launch uuid.UUID, owner coord.BrowserControlOwner,
	epoch int64, by coord.BrowserControlEnd) (coord.BrowserControl, bool, error) {

	if err := coord.CheckBrowserControlEnd(by); err != nil {
		return coord.BrowserControl{}, false, err
	}
	if err := coord.CheckBrowserControlTenure(launch, owner, epoch); err != nil {
		return coord.BrowserControl{}, false, err
	}
	resource := coord.BrowserControlResource(launch)
	w, err := s.release(ctx, resource, owner.Key(), epoch, endMark(by))
	switch {
	case err != nil:
		return coord.BrowserControl{}, false, err
	case w == nil:
		return s.refusedWith(ctx, launch)
	}
	return s.endedAt(ctx, launch, w.revision)
}

// endedAt answers an end that wrote launch's tombstone at revision, read back
// from the leader by that revision, as a claim reads back its start
// ([Store.claimedAt]): the stamp on it is the end's instant.
func (s *Store) endedAt(ctx context.Context, launch uuid.UUID, revision uint64) (coord.BrowserControl, bool,
	error) {

	resource := coord.BrowserControlResource(launch)
	l := s.laneFor(resource)
	kve, err := l.read.at(ctx, encodeResource(resource), revision)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		// A later take replaced the tombstone already, carrying its end.
		control, _, readErr := s.BrowserControl(ctx, launch)
		return control, readErr == nil, readErr
	case err != nil:
		return coord.BrowserControl{}, false, unavailable("read back the end of "+resource, err)
	}
	e, ok := decodeEntry(kve, l)
	if !ok {
		return coord.BrowserControl{}, false, fmt.Errorf("%w: the end of %s reads back undecodable",
			coord.ErrUnavailable, resource)
	}
	return answered(coord.DecodeBrowserControl(launch, e.value.Meta, ended(e)))
}

// refusedWith answers a renewal or an end the store refused with the record as
// it reads it now.
func (s *Store) refusedWith(ctx context.Context, launch uuid.UUID) (coord.BrowserControl, bool, error) {
	control, _, err := s.BrowserControl(ctx, launch)
	return control, false, err
}

// answered is a renewal's or an end's true answer from the control it wrote, or
// no answer where the record does not decode.
func answered(control coord.BrowserControl, err error) (coord.BrowserControl, bool, error) {
	if err != nil {
		return coord.BrowserControl{}, false, err
	}
	return control, true, nil
}

// BrowserControl reads a launch's record from the duty bucket's leader, its
// tenure judged on the clock Get judges a lease by.
func (s *Store) BrowserControl(ctx context.Context, launch uuid.UUID) (coord.BrowserControl, bool, error) {
	if err := coord.CheckBrowserControlLaunch(launch); err != nil {
		return coord.BrowserControl{}, false, err
	}
	resource := coord.BrowserControlResource(launch)
	e, err := s.readOne(ctx, s.laneFor(resource), resource)
	if err != nil || e == nil {
		return coord.BrowserControl{}, false, err
	}
	was, err := s.judge(ctx, *e, s.newClock())
	if err != nil {
		return coord.BrowserControl{}, false, err
	}
	return answered(coord.DecodeBrowserControl(launch, e.value.Meta, was))
}

// WatchBrowserControls watches the class's leases ([Store.WatchLive]) for the
// launch each write names.
func (s *Store) WatchBrowserControls(ctx context.Context) (<-chan coord.BrowserControlWrite, error) {
	writes, err := s.WatchLive(ctx, coord.ClassBrowserControl)
	if err != nil {
		return nil, err
	}
	return coord.BrowserControlWrites(ctx, writes), nil
}

// takeMeta is a take's rule for the Meta its claim writes: the owner's own live
// tenure goes on with its record as it is, and a new one replaces the record,
// carrying the tenure it held forward as an end, as the store judged it.
func takeMeta(t coord.BrowserControlTake) metaRule {
	return func(prior *entry, held bool) (map[string]any, error) {
		switch {
		case held:
			return prior.value.Meta, nil
		case prior == nil:
			return coord.BrowserControlTakeMeta(t, nil, coord.BrowserControlJudged{})
		}
		return coord.BrowserControlTakeMeta(t, prior.value.Meta, ended(*prior))
	}
}

// keepEnd is a take's give-back mark: the tombstone a failed take leaves over a
// record keeps how the record's tenure ended, and when. The give-back covers
// only a record the claim found not held, or the claim's own (no tenure).
func keepEnd(prior entry, tomb *leaseValue) error {
	meta, err := coord.BrowserControlKeptEnd(tomb.Meta, ended(prior))
	if err != nil {
		return err
	}
	tomb.Meta = meta
	return nil
}

// endMark marks the tombstone an end leaves with its cause.
func endMark(by coord.BrowserControlEnd) tombMark {
	return func(_ entry, tomb *leaseValue) error {
		meta, err := coord.BrowserControlEndMeta(tomb.Meta, by)
		if err != nil {
			return err
		}
		tomb.Meta = meta
		return nil
	}
}

// judge is how the store holds e's tenure on clk: none at the claiming epoch,
// live while [Store.tenure] says so, else ended as [ended] says.
func (s *Store) judge(ctx context.Context, e entry, clk *clock) (coord.BrowserControlJudged, error) {
	if e.value.Epoch == claimingEpoch {
		return coord.BrowserControlJudged{}, nil
	}
	live, err := s.tenure(ctx, e, clk)
	if err != nil {
		return coord.BrowserControlJudged{}, err
	}
	if live {
		return coord.BrowserControlJudged{Epoch: e.value.Epoch, TakenAt: e.value.AcquiredAt, Live: true}, nil
	}
	return ended(e), nil
}

// ended is how a record whose tenure is not live ended, on the store's clock:
// lapsed at its deadline while its owner is still set, else at the stamp of
// the write that left it. A claiming record holds no tenure, so Epoch is 0.
func ended(e entry) coord.BrowserControlJudged {
	was := coord.BrowserControlJudged{Epoch: e.value.Epoch, TakenAt: e.value.AcquiredAt, At: e.created}
	if e.value.Epoch != claimingEpoch && e.value.Owner != "" {
		was.Lapsed, was.At = true, e.created.Add(e.value.ttl())
	}
	return was
}
