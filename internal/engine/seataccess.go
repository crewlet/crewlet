package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// PER-SEAT INTEGRATION ACCESS, run by the engine.
//
// Every vendor's half registers itself with internal/seataccess, so nothing
// here names a vendor: a setup pass runs its integration's company-level work
// and then [seataccess.Sync] for the integration's seats ([Engine.withSeatAccess]),
// the access routes run a sync for one seat's action ([Engine.SyncSeatAccess]),
// and a disconnect turns every seat off, and removes the accounts when asked
// ([Engine.tearDownSeatAccess]).
//
// ONE WRITER AT A TIME, through the kind's provisioning lease: a pass holds it
// already (the loop's worker or [setup.Runner]), and a seat action takes it
// here. The ledger relies on that ([seataccess.Ledger]).

// ErrSeatAccessBusy is a seat action asked while another writer holds the
// integration: a pass, a disconnect, or another action. The caller answers
// 409 and the person tries again when the sync finishes.
var ErrSeatAccessBusy = errors.New("engine: a sync for this integration is running")

// SeatAccessLedger is the fleet's account ledger, or an error on an engine
// built with no fleet store, which [New] never builds.
func (e *Engine) SeatAccessLedger() (seataccess.Ledger, error) {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil, errors.New("engine: this engine holds no coordination store to " +
			"keep the seat access ledger in; it was not built by engine.New")
	}
	return seataccess.NewCoordLedger(e.backends.Fleet)
}

// SyncSeatAccess runs one integration's seat access sync for a person's
// action on the access routes (save, rotate, remove, restore, retry, done),
// under the integration's provisioning lease, sealing as by.
//
// A writer already holding the lease answers [ErrSeatAccessBusy] rather than
// waiting: the person's gesture is answered now, and the sync in flight will
// carry the document's latest setting anyway.
func (e *Engine) SyncSeatAccess(
	ctx context.Context, kind integration.Kind, by iam.Actor,
	asks map[string]seataccess.Ask, handles []string,
) (seataccess.SyncReport, error) {
	held, release, ok, err := e.holdSurface(ctx, kind)
	if err != nil {
		return seataccess.SyncReport{}, fmt.Errorf("engine: %s access sync: %w", kind, err)
	}
	if !ok {
		return seataccess.SyncReport{}, ErrSeatAccessBusy
	}
	defer release()
	sink, err := e.SetupSink(by)
	if err != nil {
		return seataccess.SyncReport{}, fmt.Errorf("engine: %s access sync: %w", kind, err)
	}
	return e.syncSeatAccess(held, kind, seatSync{
		sink: sink, by: by, asks: asks, handles: handles, webhookBase: e.webhookBase,
	})
}

// SecretResolver is the resolver a vendor session reads the document's
// `${VAR}` references through, the sealed store included: what the access
// routes hand [seataccess.Vendor.Open] for a preview.
func (e *Engine) SecretResolver() *config.Resolver { return e.resolver() }

// seatSync is one sync's inputs beyond the integration.
type seatSync struct {
	sink        provision.TokenSink
	by          iam.Actor
	operator    string
	webhookBase string
	asks        map[string]seataccess.Ask
	handles     []string
	disconnect  bool
}

// syncSeatAccess runs the sync for kind and flushes its sink, so what it
// sealed or cleared reaches this node's snapshot and every seat waiting on it.
// [integration.ErrNotConfigured] is an integration with no registered vendor,
// no company document, or one the vendor says it does not provision.
func (e *Engine) syncSeatAccess(
	ctx context.Context, kind integration.Kind, in seatSync,
) (seataccess.SyncReport, error) {
	vendor, ok := seataccess.Lookup(kind)
	if !ok {
		return seataccess.SyncReport{}, integration.ErrNotConfigured
	}
	company := e.Company()
	if company == nil {
		return seataccess.SyncReport{}, integration.ErrNotConfigured
	}
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		return seataccess.SyncReport{}, err
	}
	report, err := seataccess.Sync(ctx, seataccess.SyncInput{
		Vendor: vendor,
		Open: seataccess.OpenInput{
			Config: company.Config, Org: company.Org, Env: e.resolver(), Operator: in.operator,
			// A DRY RUN HANDS NEITHER, so a session rotates and registers
			// nothing (vendor.go).
			Sink: in.sink, WebhookBase: in.webhookBase,
		},
		Ledger:     ledger,
		Sink:       in.sink,
		Pass:       uuid.NewString(),
		By:         in.by.Name,
		Asks:       in.asks,
		Handles:    in.handles,
		Disconnect: in.disconnect,
		OnDrift:    e.publishDriftRestored,
		// THE MIGRATION'S GATE: until the document marks the integration
		// migrated onto per-seat access, its seats say nothing per seat,
		// and read as off they would have every account turned off.
		//
		// A DISCONNECT IS NOT GATED: it turns every seat off by request, and
		// acts only on accounts the ledger records, never on a document's
		// unmigrated seats.
		Unmigrated: !in.disconnect && !company.Config.SeatAccessMigratedFor(string(kind)),
	})
	if in.sink != nil {
		// FLUSHED WHATEVER THE SYNC CONCLUDED: every value it recorded is
		// already durable, and a seat waiting for one must not wait on the
		// rest of the integration converging.
		if ferr := in.sink.Flush(ctx); ferr != nil {
			err = errors.Join(err, fmt.Errorf("engine: %s access sync: %w", kind, ferr))
		}
	}
	return report, err
}

// withSeatAccess runs a pass's company-level work and then its integration's
// seat access sync, and reports both in one finding list.
//
// A company-level answer of [integration.ErrNotConfigured] skips the seats:
// the block is gone, and a removed block forgets the integration's state and
// touches no vendor (see internal/integration's package doc). A vendor that
// says it does not provision seats leaves the company-level findings as they
// are.
func (e *Engine) withSeatAccess(
	ctx context.Context, kind integration.Kind, in setup.PassInput,
	company func(context.Context, setup.PassInput) ([]integration.Finding, error),
) ([]integration.Finding, error) {
	// ONE WRITER PER SEAT CREDENTIAL: the company half writes through a sink
	// that refuses every seat's own sealed name, so only the sync below can
	// seal, rotate or forget one ([seatCredentialGuard]).
	half := in
	half.Sink = e.seatCredentialGuard(kind, in.Sink)
	findings, err := company(ctx, half)
	if err != nil {
		return nil, err
	}
	report, err := e.syncSeatAccess(ctx, kind, seatSync{
		sink: in.Sink, by: in.By, operator: in.Operator, webhookBase: in.WebhookBase,
	})
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		return findings, nil
	case err != nil:
		return nil, fmt.Errorf("engine: %s access sync: %w", kind, err)
	}
	// AND THE COMPANY CREDENTIALS A SEAT ONCE SHARED (D12), until each is
	// replaced: the integration's own finding, which its Agents tab draws.
	shared, err := e.SharedCredentialFindings(ctx, kind, report.Records)
	if err != nil {
		return nil, fmt.Errorf("engine: %s shared credentials: %w", kind, err)
	}
	findings = append(findings, report.Findings...)
	return append(findings, shared...), nil
}

// errSeatCredentialOwned is a pass's company half writing a seat's own
// credential, which only the access sync writes.
var errSeatCredentialOwned = errors.New("only the access sync writes an agent's own credential")

// seatCredentialGuard is the sink a pass's company half writes through: sink
// itself, except that a Record or Forget naming any agent seat's own sealed
// name at kind is refused with [errSeatCredentialOwned].
//
// THE INCIDENT IT CLOSES is two writers of one credential: a vendor's company
// pass still handed a seat plan and the sink minted and revoked the same key
// the access sync sealed, so either could undo the other on any pass. With the
// guard, a company half that reaches for a seat's credential fails its pass
// loudly instead of racing the sync. A nil sink stays nil: a dry run is a nil
// sink, and a guard around nothing would read as permission to write.
func (e *Engine) seatCredentialGuard(kind integration.Kind, sink provision.TokenSink) provision.TokenSink {
	vendor, ok := seataccess.Lookup(kind)
	company := e.Company()
	if sink == nil || !ok || company == nil {
		return sink
	}
	owned := map[string]string{}
	for _, seat := range seataccess.SeatsOf(company.Config) {
		for _, name := range seataccess.SinkNames(vendor.Catalog(), seat.Handle) {
			owned[name] = seat.Handle
		}
	}
	return &guardedSink{TokenSink: sink, kind: kind, owned: owned}
}

// guardedSink is [Engine.seatCredentialGuard]'s sink.
type guardedSink struct {
	provision.TokenSink
	kind  integration.Kind
	owned map[string]string
}

// Record implements [provision.TokenSink].
func (s *guardedSink) Record(ctx context.Context, name, value string) error {
	if err := s.refuse(name); err != nil {
		return err
	}
	return s.TokenSink.Record(ctx, name, value)
}

// Forget implements [provision.TokenSink].
func (s *guardedSink) Forget(ctx context.Context, names ...string) error {
	for _, name := range names {
		if err := s.refuse(name); err != nil {
			return err
		}
	}
	return s.TokenSink.Forget(ctx, names...)
}

// refuse is the error for writing name, or nil where name is no seat's.
func (s *guardedSink) refuse(name string) error {
	handle, owned := s.owned[name]
	if !owned {
		return nil
	}
	return fmt.Errorf("engine: the %s pass wrote ${%s}, agent %s's own credential: %w",
		s.kind, name, handle, errSeatCredentialOwned)
}

// tearDownSeatAccess is a disconnect's seat half: every seat of kind turned
// off (each account disabled and its credential revoked), and every account
// removed where remove is set. Run inside the disconnect's own lease.
func (e *Engine) tearDownSeatAccess(
	ctx context.Context, kind integration.Kind, by iam.Actor, remove bool,
) error {
	if _, ok := seataccess.Lookup(kind); !ok {
		// NO PER-SEAT ACCESS AT THIS SURFACE: Jira and Confluence are
		// products of the Atlassian account, which Atlassian's own
		// disconnect turns off.
		return nil
	}
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		return fmt.Errorf("engine: %s teardown: %w", kind, err)
	}
	records, err := ledger.Records(ctx, kind)
	if err != nil {
		return fmt.Errorf("engine: %s teardown: %w", kind, err)
	}
	var held int
	asks := make(map[string]seataccess.Ask, len(records))
	for handle, rec := range records {
		if rec.Held() {
			held++
		}
		if remove {
			asks[handle] = seataccess.Ask{Remove: true}
		}
	}
	sink, err := e.SetupSink(by)
	if err != nil {
		return fmt.Errorf("engine: %s teardown: %w", kind, err)
	}
	_, err = e.syncSeatAccess(ctx, kind, seatSync{
		sink: sink, by: by, asks: asks, disconnect: true, webhookBase: e.webhookBase,
	})
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, integration.ErrNotConfigured):
		return fmt.Errorf("engine: %s teardown: %w", kind, err)
	case remove && held > 0:
		// REMOVAL WAS ASKED FOR AND CANNOT BE DONE. Answering nil here lets
		// the block, and with it the only credential that reaches these
		// accounts, be dropped while every one stays live at the vendor and
		// the operator is told remove_seats succeeded.
		return fmt.Errorf("engine: %s teardown: the ledger records %d agent "+
			"account(s) to remove and the vendor cannot be reached to remove them: %w; "+
			"fix the connection, or disconnect without removing accounts and remove "+
			"them at the vendor by hand", kind, held, err)
	default:
		// KEPT AS THEY ARE: a disconnect that removes nothing needs no
		// vendor, and the ledger keeps every account for a reconnect to
		// reach.
		if held > 0 {
			log.WarnContext(ctx, "seat_access_teardown_unreachable",
				"integration", kind.String(), "accounts", held, "error", err,
				"detail", "the agents' accounts stay as they are at the vendor; "+
					"the ledger keeps them for a reconnect")
		}
		return nil
	}
}

// seatAccessPlan is the provisioning plan a seat's tool waits on for kind:
// every agent seat that is on at one of kind's lanes, with the engine-owned
// names its credential is sealed under. It is what [seatMinter.seatPlan]
// answers for every pass that mints a seat's credential, so the name a sync
// seals and the name a seat's server waits for are one derivation.
func (e *Engine) seatAccessPlan(c *Company, kind integration.Kind) (*provision.Plan, error) {
	vendor, ok := seataccess.Lookup(kind)
	if !ok || c == nil || c.Config == nil {
		return nil, integration.ErrNotConfigured
	}
	cat := vendor.Catalog()
	plan := &provision.Plan{}
	// NOTHING IS ON ITS WAY where the access sync seals nothing: an
	// integration whose credentials are handed in from outside (D4b), or one
	// the document has not migrated onto per-seat access, whose sync changes
	// nothing. A seat's child naming one of its sealed names there waits on
	// no pass and is reported as the missing secret it is.
	if c.Config.SeatCredentialMode(string(kind), e.companyManaged) != config.SeatCredentialEngine ||
		!c.Config.SeatAccessMigratedFor(string(kind)) {
		return plan, nil
	}
	for _, seat := range seataccess.SeatsOf(c.Config) {
		on := false
		for _, lane := range seataccess.LanesOf(kind) {
			on = on || seat.Role.SeatAccessEnabled(string(lane))
		}
		if !on {
			continue
		}
		sinks := seataccess.SinkNames(cat, seat.Handle)
		s := provision.Seat{Handle: seat.Handle, Role: seat.Name, TokenVar: sinks[cat.Credentials[0]]}
		if len(cat.Credentials) > 1 && kind == integration.KindAtlassian {
			// ATLASSIAN ASSIGNS THE ADDRESS, and its product APIs refuse
			// the token without it ([provision.Seat.EmailVar]).
			s.EmailVar = sinks[cat.Credentials[1]]
		}
		plan.Add(s)
	}
	return plan, nil
}

// publishDriftRestored announces one restored setting (D2).
func (e *Engine) publishDriftRestored(ctx context.Context, d seataccess.DriftRestored) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	ev := events.NewFrom(&types.SeatAccessDriftRestored{
		Integration: string(d.Kind), Lane: string(d.Lane), AgentHandle: d.Handle,
		Before: accessSetting(d.Before), After: accessSetting(d.After), RestoredAt: d.At,
	}, events.TraceContext{})
	if ev == nil {
		return
	}
	if company := e.Company(); company != nil {
		if seat := company.Org.AgentSeatByHandle(d.Handle); seat != nil {
			ev.Source = seat.Name
		}
	}
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		// LOGGED, NOT RAISED: the restore itself landed at the vendor and
		// in the ledger, whose lane record carries the step; losing the
		// announcement costs the activity row, and failing the sync over it
		// would retry a restore that already happened.
		log.WarnContext(ctx, "seat_access_drift_publish_failed",
			"integration", d.Kind.String(), "lane", d.Lane.String(),
			"seat", d.Handle, "error", err)
	}
}

func accessSetting(d seataccess.Desired) types.AccessSetting {
	out := types.AccessSetting{Level: d.Level, Scope: d.Scope}
	if len(d.Permissions) > 0 {
		out.Permissions = make(map[string]string, len(d.Permissions))
		for key, access := range d.Permissions {
			out.Permissions[key] = string(access)
		}
	}
	return out
}
