package engine

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A seat's extra grants beside its level: the agents' stored requests, and the
// audit trail of every grant asked for, granted, declined, removed, lapsed or
// refused (seat_grant_changed).

// GrantRequests is the agents' stored grant requests, on the fleet's
// coordination store.
func (e *Engine) GrantRequests() (*seataccess.CoordRequests, error) {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil, errors.New("engine: this engine holds no coordination store to keep the " +
			"agents' grant requests in; it was not built by engine.New")
	}
	return seataccess.NewCoordRequests(e.backends.Fleet)
}

// grantEvent is one change to one seat's grant, as the audit trail records it.
type grantEvent struct {
	kind    integration.Kind
	lane    seataccess.Lane
	handle  string
	change  string
	grant   seataccess.Grant
	by      string
	request string
	detail  string
	at      time.Time
}

// publishGrantChanged records one grant change. LOGGED, NOT RAISED: the change
// itself is already the company's (a revision, a stored request, a token), and
// failing it over its announcement would undo nothing.
func (e *Engine) publishGrantChanged(ctx context.Context, g grantEvent) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	ev := events.NewFrom(&types.SeatGrantChanged{
		Integration: string(g.kind), Lane: string(g.lane), AgentHandle: g.handle, Change: g.change,
		Grant: grantSetting(g.grant), By: g.by, Request: g.request, Detail: g.detail, At: g.at,
	}, events.TraceContext{})
	if ev == nil {
		return
	}
	if company := e.Company(); company != nil {
		if seat := company.Org.AgentSeatByHandle(g.handle); seat != nil {
			ev.Source = seat.Name
		}
	}
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "seat_grant_publish_failed", "integration", g.kind.String(),
			"lane", g.lane.String(), "seat", g.handle, "change", g.change, "error", err)
	}
}

// grantSetting is a grant as an event names it.
func grantSetting(g seataccess.Grant) types.GrantSetting {
	return types.GrantSetting{ID: g.ID, Permission: g.Permission, Scope: g.Scope, ExpiresAt: g.ExpiresAt,
		Target: g.Target}
}

// grantSettings is [grantSetting] over a list, nil for none.
func grantSettings(grants []seataccess.Grant) []types.GrantSetting {
	if len(grants) == 0 {
		return nil
	}
	out := make([]types.GrantSetting, 0, len(grants))
	for _, g := range grants {
		out = append(out, grantSetting(g))
	}
	return out
}

// GrantRequest implements setupapi.Access.
func (a *SeatAccessAPI) GrantRequest(ctx context.Context, id string) (seataccess.GrantRequest, error) {
	requests, err := a.engine.GrantRequests()
	if err != nil {
		return seataccess.GrantRequest{}, err
	}
	return requests.Request(ctx, id)
}

// GrantRequests implements setupapi.Access.
func (a *SeatAccessAPI) GrantRequests(ctx context.Context, kind integration.Kind) (
	[]seataccess.GrantRequest, error) {

	requests, err := a.engine.GrantRequests()
	if err != nil {
		return nil, err
	}
	open, err := requests.Open(ctx, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(open, func(r seataccess.GrantRequest) bool { return r.Kind != kind }), nil
}

// RecordGrants implements setupapi.Access: one event per grant a save added or
// removed, as the saver, and each answered request dropped, since its grant
// now carries it.
func (a *SeatAccessAPI) RecordGrants(ctx context.Context, kind integration.Kind, by iam.Actor,
	changes []seataccess.GrantChange) {

	ctx = context.WithoutCancel(ctx)
	now := time.Now().UTC()
	requests, reqErr := a.engine.GrantRequests()
	for _, c := range changes {
		for _, g := range c.Added {
			a.engine.publishGrantChanged(ctx, grantEvent{kind: kind, lane: c.Lane, handle: c.Handle,
				change: types.GrantGranted, grant: g, by: by.Name, request: g.Request, at: now})
			if g.Request == "" || reqErr != nil {
				continue
			}
			if err := requests.Drop(ctx, g.Request); err != nil {
				log.WarnContext(ctx, "seat_grant_request_undropped", "request", g.Request, "error", err,
					"detail", "the sweep drops it once it lapses")
			}
		}
		for _, g := range c.Removed {
			a.engine.publishGrantChanged(ctx, grantEvent{kind: kind, lane: c.Lane, handle: c.Handle,
				change: types.GrantRemoved, grant: g, by: by.Name, at: now})
		}
	}
}

// GrantDesk is the engine's half of request_access's integration form and of
// its answer ([builtin.GrantDesk]).
func (e *Engine) GrantDesk() builtin.GrantDesk { return grantDesk{e: e} }

// grantDesk is [Engine.GrantDesk].
type grantDesk struct{ e *Engine }

// Offer implements builtin.GrantDesk: the seat's setting in the running
// document, and the lane's offer read live (never from the offer cache, so a
// permission added to the provisioner a minute ago is offered).
func (d grantDesk) Offer(ctx context.Context, handle string, lane seataccess.Lane) (
	seataccess.Desired, seataccess.Options, error) {

	company := d.e.Company()
	if company == nil {
		return seataccess.Desired{}, seataccess.Options{}, errors.New("no company is running")
	}
	var want seataccess.Desired
	for _, seat := range seataccess.SeatsOf(company.Config) {
		if seat.Handle == handle {
			want, _ = seataccess.DesiredOf(seat.Role, lane)
		}
	}
	for _, o := range d.e.seatOffers(ctx, lane.Kind(), company.Config, company.Org, "", d.e.companyManaged) {
		if o.Lane == lane {
			return want, o, nil
		}
	}
	return want, seataccess.Options{Lane: lane}, nil
}

// File implements builtin.GrantDesk.
func (d grantDesk) File(ctx context.Context, req seataccess.GrantRequest) error {
	requests, err := d.e.GrantRequests()
	if err != nil {
		return err
	}
	if err := requests.Put(ctx, req); err != nil {
		return err
	}
	d.e.publishGrantChanged(ctx, grantEvent{kind: req.Kind, lane: req.Lane, handle: req.Handle,
		change: types.GrantRequested, grant: req.Grant(false, req.AskedAt), request: req.ID, at: req.AskedAt})
	return nil
}

// Withdraw implements builtin.GrantDesk.
func (d grantDesk) Withdraw(ctx context.Context, id string) error {
	requests, err := d.e.GrantRequests()
	if err != nil {
		return err
	}
	return requests.Drop(ctx, id)
}

// Granted implements builtin.GrantDesk, against the fleet's active revision
// the way an access request's answer is (ADR-0118).
func (d grantDesk) Granted(ctx context.Context, handle, id string, permanent bool) (bool, error) {
	company, err := d.e.servedFleetRevision(ctx)
	if err != nil {
		return false, err
	}
	if company == nil || company.Config == nil {
		return false, errors.New("no company is active")
	}
	now := time.Now().UTC()
	for _, seat := range seataccess.SeatsOf(company.Config) {
		if seat.Handle != handle {
			continue
		}
		for _, lane := range seataccess.GrantLanes() {
			want, _ := seataccess.DesiredOf(seat.Role, lane)
			for _, g := range want.Grants {
				if g.Request == id && g.Live(now) && g.ExpiresAt.IsZero() == permanent {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// GrantSaver saves a chosen request's grant (ADR-0118): the setup surface's.
type GrantSaver func(ctx context.Context, asker, id string, permanent bool,
	admit func(raise bool) error) error

// UseGrantSaver installs the grant save, as [Engine.UseConfigWriter] is: the setup
// surface is built after the engine; until then a choice is refused.
func (e *Engine) UseGrantSaver(save GrantSaver) { e.grantSaver.Store(&save) }

// ErrNoGrantSaver reports a node whose setup surface installed no grant save.
var ErrNoGrantSaver = errors.New("engine: this node cannot save a grant yet; its setup surface is not serving")

// Grant implements builtin.GrantDesk through the installed [GrantSaver].
func (d grantDesk) Grant(ctx context.Context, asker, id string, permanent bool,
	admit func(raise bool) error) error {

	save := d.e.grantSaver.Load()
	if save == nil || *save == nil {
		return ErrNoGrantSaver
	}
	return (*save)(ctx, asker, id, permanent, admit)
}

// Declined implements builtin.GrantDesk.
func (d grantDesk) Declined(ctx context.Context, id, by string) error {
	requests, err := d.e.GrantRequests()
	if err != nil {
		return err
	}
	req, err := requests.Request(ctx, id)
	if errors.Is(err, seataccess.ErrNoRequest) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := requests.Drop(ctx, id); err != nil {
		return err
	}
	now := time.Now().UTC()
	d.e.publishGrantChanged(ctx, grantEvent{kind: req.Kind, lane: req.Lane, handle: req.Handle,
		change: types.GrantDeclined, grant: req.Grant(false, now), by: by, request: id, at: now})
	return nil
}

// LapsedGrants is every lapsed grant of the running company that no agent's
// credential still holds, per the access ledger: what the sweep drops from the
// document. One still held waits for the pass that removes it, so the document
// never forgets a grant a token carries.
func (e *Engine) LapsedGrants(ctx context.Context, now time.Time) ([]seataccess.GrantChange, error) {
	company := e.Company()
	if company == nil {
		return nil, nil
	}
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		return nil, err
	}
	var out []seataccess.GrantChange
	for _, lane := range seataccess.GrantLanes() {
		records, err := ledger.Records(ctx, lane.Kind())
		if err != nil {
			return nil, err
		}
		for _, seat := range seataccess.SeatsOf(company.Config) {
			want, _ := seataccess.DesiredOf(seat.Role, lane)
			held := records[seat.Handle].Lanes[lane].Observed
			var lapsed []seataccess.Grant
			for _, g := range want.Grants {
				if g.Live(now) || (held.Exists && slices.ContainsFunc(held.Grants, func(h seataccess.Grant) bool {
					return seataccess.GrantKey(h) == seataccess.GrantKey(g)
				})) {
					continue
				}
				lapsed = append(lapsed, g)
			}
			if len(lapsed) > 0 {
				out = append(out, seataccess.GrantChange{Handle: seat.Handle, Lane: lane, Removed: lapsed})
			}
		}
	}
	return out, nil
}

// RecordLapsed records each grant a sweep dropped from the document as
// expired, the engine's change rather than a person's.
func (e *Engine) RecordLapsed(ctx context.Context, changes []seataccess.GrantChange, at time.Time) {
	for _, c := range changes {
		for _, g := range c.Removed {
			e.publishGrantChanged(ctx, grantEvent{kind: c.Lane.Kind(), lane: c.Lane, handle: c.Handle,
				change: types.GrantExpired, grant: g, request: g.Request, at: at})
		}
	}
}
