package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

var _ coord.BrowserControls = (*Backend)(nil)

// TakeBrowserControl takes a launch's browser for t's owner: a claim of its
// lease whose meta is the take's record, judged under the lock on the twin's
// clock. Before it, the twin forgets the ended records of the class past the
// duty bucket's age (the package doc).
func (b *Backend) TakeBrowserControl(ctx context.Context, t coord.BrowserControlTake,
	ttl time.Duration) (coord.BrowserControl, coord.BrowserControlTaken, error) {

	if err := unavailable(ctx); err != nil {
		return coord.BrowserControl{}, "", err
	}
	if err := t.Validate(); err != nil {
		return coord.BrowserControl{}, "", err
	}
	resource := coord.BrowserControlResource(t.Launch)
	if err := validateTTL(resource, t.Owner.Key(), ttl); err != nil {
		return coord.BrowserControl{}, "", err
	}
	b.forgetEndedControls()
	c, err := b.tryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: t.Owner.Key(), TTL: ttl,
		// UNGATED: the gate guards what holding a seat means, and a person's
		// gesture holds none; it must work through a rolling upgrade.
		Ungated: true,
	}, takeMeta(t))
	switch {
	case err != nil:
		return coord.BrowserControl{}, "", err
	case c.lease != nil:
		return coord.BrowserControlGranted(t.Launch, *c.lease, c.fresh)
	case c.refused == coord.RefusedHeld && c.refuser.live(c.now):
		control, decodeErr := coord.DecodeBrowserControl(t.Launch, c.refuser.meta, judged(&c.refuser, c.now))
		if decodeErr != nil {
			return coord.BrowserControl{}, "", decodeErr
		}
		return control, coord.ControlHeld, nil
	case c.refused == coord.RefusedHeld:
		// Refused for another owner's claim still minting: unknown, never held
		// naming nobody, as the KV store answers its claiming record.
		return coord.BrowserControl{}, "", fmt.Errorf("%w: the browser control of launch %s is being "+
			"taken by another owner", coord.ErrUnavailable, t.Launch)
	}
	return coord.BrowserControl{}, "", fmt.Errorf("%w: a browser control's claim was refused %q, which "+
		"an ungated claim never is", coord.ErrUnavailable, c.refused)
}

// RenewBrowserControl extends the owner's live tenure at epoch, answered from
// the row as renewed.
func (b *Backend) RenewBrowserControl(ctx context.Context, launch uuid.UUID, owner coord.BrowserControlOwner,
	epoch int64, ttl time.Duration) (coord.BrowserControl, bool, error) {

	if err := unavailable(ctx); err != nil {
		return coord.BrowserControl{}, false, err
	}
	if err := coord.CheckBrowserControlTenure(launch, owner, epoch); err != nil {
		return coord.BrowserControl{}, false, err
	}
	resource := coord.BrowserControlResource(launch)
	if err := validateTTL(resource, owner.Key(), ttl); err != nil {
		return coord.BrowserControl{}, false, err
	}
	row := b.renew(resource, owner.Key(), epoch, ttl)
	if row == nil {
		return b.refusedWith(launch)
	}
	return answered(coord.DecodeBrowserControl(launch, row.meta, coord.BrowserControlJudged{
		Epoch: row.epoch, TakenAt: row.acquiredAt, Live: true,
	}))
}

// EndBrowserControl ends the owner's live tenure at epoch as by: a release of
// its lease marked with the cause, ended at the release's instant.
func (b *Backend) EndBrowserControl(ctx context.Context, launch uuid.UUID, owner coord.BrowserControlOwner,
	epoch int64, by coord.BrowserControlEnd) (coord.BrowserControl, bool, error) {

	if err := unavailable(ctx); err != nil {
		return coord.BrowserControl{}, false, err
	}
	if err := coord.CheckBrowserControlEnd(by); err != nil {
		return coord.BrowserControl{}, false, err
	}
	if err := coord.CheckBrowserControlTenure(launch, owner, epoch); err != nil {
		return coord.BrowserControl{}, false, err
	}
	row, err := b.release(coord.BrowserControlResource(launch), owner.Key(), epoch, endMark(by))
	switch {
	case err != nil:
		return coord.BrowserControl{}, false, err
	case row == nil:
		return b.refusedWith(launch)
	}
	return answered(coord.DecodeBrowserControl(launch, row.meta, ended(row)))
}

// BrowserControl reads a launch's row, its tenure judged on the twin's clock.
func (b *Backend) BrowserControl(ctx context.Context, launch uuid.UUID) (coord.BrowserControl, bool, error) {
	if err := unavailable(ctx); err != nil {
		return coord.BrowserControl{}, false, err
	}
	if err := coord.CheckBrowserControlLaunch(launch); err != nil {
		return coord.BrowserControl{}, false, err
	}
	return b.readControl(launch)
}

// readControl is [Backend.BrowserControl] past its checks.
func (b *Backend) readControl(launch uuid.UUID) (coord.BrowserControl, bool, error) {
	b.mu.Lock()
	row := b.rows[coord.BrowserControlResource(launch)]
	if row == nil {
		b.mu.Unlock()
		return coord.BrowserControl{}, false, nil
	}
	now := b.now()
	read := row.copied()
	b.mu.Unlock()
	return answered(coord.DecodeBrowserControl(launch, read.meta, judged(read, now)))
}

// refusedWith answers a renewal or an end the twin refused with the row as it
// reads now.
func (b *Backend) refusedWith(launch uuid.UUID) (coord.BrowserControl, bool, error) {
	control, _, err := b.readControl(launch)
	return control, false, err
}

// answered is a renewal's, an end's or a read's true answer from the control
// it holds, or no answer where the record does not decode.
func answered(control coord.BrowserControl, err error) (coord.BrowserControl, bool, error) {
	if err != nil {
		return coord.BrowserControl{}, false, err
	}
	return control, true, nil
}

// WatchBrowserControls watches the class's leases ([Backend.WatchLive]) for
// the launch each write names.
func (b *Backend) WatchBrowserControls(ctx context.Context) (<-chan coord.BrowserControlWrite, error) {
	writes, err := b.WatchLive(ctx, coord.ClassBrowserControl)
	if err != nil {
		return nil, err
	}
	return coord.BrowserControlWrites(ctx, writes), nil
}

// takeMeta is a take's rule for the meta its claim writes: the owner's own live
// tenure goes on with its row as it is, and a new one replaces the row's
// record, carrying the tenure it held forward as an end, as the twin judged it.
func takeMeta(t coord.BrowserControlTake) metaRule {
	return func(prior *record, held bool) (map[string]any, error) {
		switch {
		case held:
			return prior.meta, nil
		case prior == nil:
			return coord.BrowserControlTakeMeta(t, nil, coord.BrowserControlJudged{})
		}
		return coord.BrowserControlTakeMeta(t, prior.meta, ended(prior))
	}
}

// endMark marks the row an end releases with its cause.
func endMark(by coord.BrowserControlEnd) releaseMark {
	return func(row *record) error {
		meta, err := coord.BrowserControlEndMeta(row.meta, by)
		if err != nil {
			return err
		}
		row.meta = meta
		return nil
	}
}

// judged is how the twin holds row's tenure at now: live while its lease is,
// else as [ended] says.
func judged(row *record, now time.Time) coord.BrowserControlJudged {
	if row.epoch != 0 && row.live(now) {
		return coord.BrowserControlJudged{Epoch: row.epoch, TakenAt: row.acquiredAt, Live: true}
	}
	return ended(row)
}

// ended is how a row whose tenure is not live ended: it holds none before its
// first token; lapsed at its deadline while its owner is set; else ended at
// the release that cleared it, which set its expiry to that instant.
func ended(row *record) coord.BrowserControlJudged {
	if row.epoch == 0 {
		return coord.BrowserControlJudged{}
	}
	return coord.BrowserControlJudged{Epoch: row.epoch, TakenAt: row.acquiredAt, Lapsed: row.owner != "",
		At: row.expiresAt}
}

// forgetEndedControls removes every browser control row that holds nothing
// live and claims nothing, once its end, its expiry, is coord.MaxDutyTTL past
// on the twin's clock: the KV duty bucket's age, so the two stores keep an end
// for as long, within a TTL (the package doc). The class's counter is its own.
func (b *Backend) forgetEndedControls() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for resource, row := range b.rows {
		if coord.ClassBrowserControl.Holds(resource) && row.claiming == nil &&
			!now.Before(row.expiresAt.Add(coord.MaxDutyTTL)) {
			delete(b.rows, resource)
		}
	}
}
