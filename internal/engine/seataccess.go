package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// PER-SEAT INTEGRATION ACCESS, run by the engine. Each vendor registers with
// internal/seataccess, so nothing here names one: a setup pass runs its
// company-level work then [seataccess.Sync] ([Engine.withSeatAccess]), an access
// route syncs one seat's action ([Engine.SyncSeatAccess]), and a disconnect
// turns every seat off and, when asked, removes the accounts
// ([Engine.tearDownSeatAccess]).
//
// ONE WRITER AT A TIME, through the kind's provisioning lease: a pass already
// holds it (the loop's worker or [setup.Runner]), a seat action takes it here.
// The ledger relies on that ([seataccess.Ledger]).

// ErrSeatAccessBusy is a seat action asked while another writer (a pass, a
// disconnect, another action) holds the integration; the caller answers 409.
var ErrSeatAccessBusy = errors.New("engine: a sync for this integration is running")

// SeatAccessLedger is the fleet's account ledger, or an error on an engine
// built with no fleet store, which [New] never builds.
func (e *Engine) SeatAccessLedger() (seataccess.Ledger, error) {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil, errors.New("engine: this engine holds no coordination store to " +
			"keep the seat access ledger in; it was not built by engine.New")
	}
	ledger, err := seataccess.NewCoordLedger(e.backends.Fleet)
	if err != nil {
		return nil, err
	}
	// A WRITE NAMING A SEAT MEASURES ITS ACCESS REVIEW (ADR-0111).
	return reviewingLedger{Ledger: ledger, kick: e.kickAccessReviews}, nil
}

// seatAccessAccounts is each seat's account the ledger records as held for
// kind, by the seat's handle: the agents' accounts the access sync manages,
// which a company-level pass leaves to it or touches only as its own.
func (e *Engine) seatAccessAccounts(ctx context.Context, kind integration.Kind) (map[string]string, error) {
	records, err := e.seatAccessHeld(ctx, kind)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(records))
	for handle, rec := range records {
		out[handle] = rec.Account.ID
	}
	return out, nil
}

// seatAccessHeld is every ledger record of kind that holds an account, by
// seat handle.
func (e *Engine) seatAccessHeld(ctx context.Context, kind integration.Kind) (map[string]seataccess.Record, error) {
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		return nil, err
	}
	records, err := ledger.Records(ctx, kind)
	if err != nil {
		return nil, err
	}
	out := make(map[string]seataccess.Record, len(records))
	for handle, rec := range records {
		if rec.Held() {
			out[handle] = rec
		}
	}
	return out, nil
}

// SyncSeatAccess runs one integration's seat access sync for a person's
// action on the access routes (save, rotate, remove, restore, retry, done),
// under the integration's provisioning lease, sealing as by. A lease already
// held answers [ErrSeatAccessBusy] rather than waiting: the sync in flight
// carries the document's latest setting anyway.
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

	// reads is what the pass's company half read at the vendor, for its
	// session to read no second time ([seataccess.OpenInput.Reads]).
	reads seataccess.PassReads
}

// syncSeatAccess runs the sync for kind and flushes its sink, so what it
// sealed or cleared reaches this node's snapshot and every seat waiting on it.
// [integration.ErrNotConfigured] is an integration with no registered vendor,
// no company document, one the company does not connect, or one the vendor
// says it does not provision. One whose credentials are handed in from outside
// Crewlet (D4b) is one advisory finding ([seataccess.ExternalFinding]).
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
	// EXTERNAL MODE IS BOUND, NEVER MANAGED (D4b): where the engine lacks the
	// authority to create an agent's account, the sync opens no session and
	// writes no ledger record; each seat's tool reads the credential it names
	// ([config.Company.SeatCredentialMode]). A disconnect still runs: it acts
	// only on accounts the ledger records this engine made.
	if !in.disconnect && company.Config.SeatCredentialMode(string(kind), e.companyManaged) !=
		config.SeatCredentialEngine {
		return e.externalSeatAccess(company.Config, kind)
	}
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		return seataccess.SyncReport{}, err
	}
	if in.reads == nil {
		// A SYNC NO PASS STARTED (a change on the Agents tab) offers what
		// this node's product last read, as the access routes do.
		in.reads = e.seatReads(ctx, kind)
	}
	report, err := seataccess.Sync(ctx, seataccess.SyncInput{
		Vendor: vendor,
		Open: seataccess.OpenInput{
			Config: company.Config, Org: company.Org, Env: e.resolver(), Operator: in.operator,
			Derive: e.deriveKey, Tokens: seatTokens{engine: e},
			// A DRY RUN HANDS NEITHER, so a session rotates and registers
			// nothing (vendor.go).
			Sink: in.sink, WebhookBase: in.webhookBase,
			Reads: in.reads,
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
		// migrated, its seats say nothing per seat and, read as off, would
		// have every account turned off. A disconnect is not gated: it acts
		// only on accounts the ledger records. An unconnected integration is
		// not configured (the session's open says so) and waits on nothing.
		Unmigrated:       !in.disconnect && company.Config.SeatAccessPending(string(kind)),
		UnmigratedReason: e.seatAccessPendingReason(kind),
		// THE SHARED CREDENTIAL'S TEST, read off the sealed store the
		// integration's finding reads ([Engine.SharedCredentialFindings]).
		Written: writtenBy(fleetsecrets.New(e.backends.Fleet, e.Cipher())),
	})
	if in.sink != nil {
		// FLUSHED WHATEVER THE SYNC CONCLUDED: what it recorded is durable,
		// and a seat waiting for one value must not wait on the rest converging.
		if ferr := in.sink.Flush(ctx); ferr != nil {
			err = errors.Join(err, fmt.Errorf("engine: %s access sync: %w", kind, ferr))
		}
	}
	return report, err
}

// withSeatAccess runs a pass's company-level work and then its integration's
// seat access sync, and reports both in one finding list.
//
// A company-level [integration.ErrNotConfigured] skips the seats: a removed
// block touches no vendor (internal/integration's package doc). A vendor that
// does not provision seats leaves the company-level findings as they are.
func (e *Engine) withSeatAccess(
	ctx context.Context, kind integration.Kind, in setup.PassInput,
	company func(context.Context, setup.PassInput) ([]integration.Finding, error),
) ([]integration.Finding, error) {
	return e.withSeatAccessReads(ctx, kind, in,
		func(ctx context.Context, in setup.PassInput) ([]integration.Finding, seataccess.PassReads, error) {
			findings, err := company(ctx, in)
			return findings, nil, err
		})
}

// withSeatAccessReads is [Engine.withSeatAccess] for a company half that
// hands its session what it already read at the vendor, so a pass reads a
// listing once (Cloudflare's zones and permission groups).
func (e *Engine) withSeatAccessReads(
	ctx context.Context, kind integration.Kind, in setup.PassInput,
	company func(context.Context, setup.PassInput) ([]integration.Finding, seataccess.PassReads, error),
) ([]integration.Finding, error) {
	// ONE WRITER PER SEAT CREDENTIAL: the company half writes through a sink
	// that refuses every seat's own sealed name, so only the sync below can
	// seal, rotate or forget one ([seatCredentialGuard]).
	half := in
	half.Sink = e.seatCredentialGuard(kind, in.Sink)
	findings, reads, err := company(ctx, half)
	if err != nil {
		return nil, err
	}
	// A CONNECTION STILL WAITING ON ITS SETUP has no agents' accounts to move
	// onto per-seat access yet: its own finding names the person's next step,
	// and its seats wait on it rather than reporting a migration.
	if c := e.Company(); c != nil && c.Config.SeatAccessPending(string(kind)) &&
		slices.ContainsFunc(findings, integration.Finding.AwaitsSetup) {
		return findings, nil
	}
	report, err := e.syncSeatAccess(ctx, kind, seatSync{
		sink: in.Sink, by: in.By, operator: in.Operator, webhookBase: in.WebhookBase, reads: reads,
	})
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		return findings, nil
	case err != nil:
		return nil, fmt.Errorf("engine: %s access sync: %w", kind, err)
	}
	// AND THE COMPANY CREDENTIALS A SEAT ONCE SHARED (D12), until each is
	// replaced: the finding the integration's Agents tab draws.
	shared, err := e.SharedCredentialFindings(ctx, kind, report.Records)
	if err != nil {
		return nil, fmt.Errorf("engine: %s shared credentials: %w", kind, err)
	}
	findings = append(findings, report.Findings...)
	return append(findings, shared...), nil
}

// externalSeatAccess is a sync's answer for an integration whose agents'
// credentials are handed in from outside Crewlet: not configured where the
// company does not connect it, its unfinished migration where pending (the
// binding waits on it too), nothing where no agent is on there, and otherwise
// the one advisory that its agents' accounts are managed outside Crewlet.
func (e *Engine) externalSeatAccess(c *config.Company, kind integration.Kind) (seataccess.SyncReport, error) {
	var said []integration.Finding
	switch {
	case !c.SeatAccessConnected(string(kind)):
		return seataccess.SyncReport{}, integration.ErrNotConfigured
	case c.SeatAccessPending(string(kind)):
		said = append(said, seataccess.UnmigratedFinding(kind, e.seatAccessPendingReason(kind)))
	case slices.ContainsFunc(seataccess.SeatsOf(c), func(s seataccess.Seat) bool { return s.OnAt(kind) }):
		said = append(said, seataccess.ExternalFinding(kind))
	}
	return seataccess.SyncReport{Findings: said}, nil
}

// errSeatCredentialOwned is a pass's company half writing a seat's own
// credential, which only the access sync writes.
var errSeatCredentialOwned = errors.New("only the access sync writes an agent's own credential")

// seatCredentialGuard is the sink a pass's company half writes through: sink
// itself, except that a Record or Forget naming any agent seat's own sealed
// name at kind is refused with [errSeatCredentialOwned]: the access sync is a
// seat credential's one writer, and a second could undo it on any pass, so a
// company half reaching for one fails its pass rather than racing the sync. A
// nil sink stays nil: a dry run is a nil sink, and a guard around nothing would
// read as permission to write.
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
		// products of the Atlassian account, which Atlassian's disconnect
		// turns off.
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
	case err == nil && remove:
		return accountsLeft(ctx, ledger, kind)
	case err == nil:
		return nil
	case !errors.Is(err, integration.ErrNotConfigured):
		return fmt.Errorf("engine: %s teardown: %w", kind, err)
	case remove && held > 0:
		// REMOVAL WAS ASKED FOR AND CANNOT BE DONE: nil here would drop the
		// block, the only credential reaching these accounts, while each stays
		// live at the vendor and the operator is told remove_seats succeeded.
		return fmt.Errorf("engine: %s teardown: the ledger records %d agent "+
			"account(s) to remove and the vendor cannot be reached to remove them: %w; "+
			"fix the connection, or disconnect without removing accounts and remove "+
			"them at the vendor by hand", kind, held, err)
	default:
		// KEPT AS THEY ARE: removing nothing needs no vendor, and the ledger
		// keeps every account for a reconnect.
		if held > 0 {
			log.WarnContext(ctx, "seat_access_teardown_unreachable",
				"integration", kind.String(), "accounts", held, "error", err,
				"detail", "the agents' accounts stay as they are at the vendor; "+
					"the ledger keeps them for a reconnect")
		}
		return nil
	}
}

// accountsLeft refuses a disconnect that asked for the agents' accounts removed
// while the ledger still holds one (a removal waiting on a person, or one the
// vendor refused). Dropping the block then would drop the only connection to
// those accounts and the sealed credentials that confirm their removal, with
// the operator told it finished. The loop keeps the surface disconnecting and
// retries; the Agents tab names what each removal waits on.
func accountsLeft(ctx context.Context, ledger seataccess.Ledger, kind integration.Kind) error {
	records, err := ledger.Records(ctx, kind)
	if err != nil {
		return fmt.Errorf("engine: %s teardown: read what is left to remove: %w", kind, err)
	}
	var left []string
	for handle, rec := range records {
		if rec.Held() {
			left = append(left, handle)
		}
	}
	if len(left) == 0 {
		return nil
	}
	slices.Sort(left)
	return fmt.Errorf("engine: %s teardown: %d agent account(s) are not removed yet (%s), "+
		"so the integration stays connected until each removal finishes; the Agents tab "+
		"names what each one waits on", kind, len(left), strings.Join(left, ", "))
}

// seatAccessPlan is the provisioning plan a seat's tool waits on for kind:
// every agent seat that is on at one of kind's lanes, with the engine-owned
// names its credential is sealed under. [seatMinter.seatPlan] answers it for
// every pass that mints a seat's credential, so the name a sync seals and the
// name a seat's server waits for are one derivation.
func (e *Engine) seatAccessPlan(c *Company, kind integration.Kind) (*provision.Plan, error) {
	vendor, ok := seataccess.Lookup(kind)
	if !ok || c == nil || c.Config == nil {
		return nil, integration.ErrNotConfigured
	}
	cat := vendor.Catalog()
	plan := &provision.Plan{}
	// NOTHING IS ON ITS WAY where the sync seals nothing: credentials handed
	// in from outside (D4b), an integration not migrated onto per-seat access,
	// or every lane's credential issued at use ([seatIssuer.variable]). A
	// seat's child naming a sealed name there waits on no pass and is
	// reported as the missing secret it is.
	if c.Config.SeatCredentialMode(string(kind), e.companyManaged) != config.SeatCredentialEngine ||
		!c.Config.SeatAccessMigratedFor(string(kind)) || len(cat.Credentials) == 0 {
		return plan, nil
	}
	for _, seat := range seataccess.SeatsOf(c.Config) {
		if !seat.OnAt(kind) {
			continue
		}
		sinks := seataccess.SinkNames(cat, seat.Handle)
		s := provision.Seat{Handle: seat.Handle, Role: seat.Name, TokenVar: sinks[cat.Credentials[0]]}
		// EVERY OTHER VALUE THE SEAT'S TOOL READS that this sync seals beside
		// the credential ([provision.Seat.AlsoVars]), such as Atlassian's
		// address; one issued at use (GitHub's installation token) is sealed
		// by nobody.
		for _, field := range config.SeatToolFields(string(kind)) {
			if name := sinks[field]; name != "" && field != cat.Credentials[0] {
				s.AlsoVars = append(s.AlsoVars, name)
			}
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
		// LOGGED, NOT RAISED: the restore landed at the vendor and in the
		// ledger; losing the announcement costs only the activity row, and
		// failing the sync would retry a restore that already happened.
		log.WarnContext(ctx, "seat_access_drift_publish_failed",
			"integration", d.Kind.String(), "lane", d.Lane.String(),
			"seat", d.Handle, "error", err)
	}
}

func accessSetting(d seataccess.Desired) types.AccessSetting {
	out := types.AccessSetting{Level: d.Level, Scope: d.Scope, Grants: grantSettings(d.Grants)}
	if len(d.Permissions) > 0 {
		out.Permissions = make(map[string]string, len(d.Permissions))
		for key, access := range d.Permissions {
			out.Permissions[key] = string(access)
		}
	}
	return out
}

// grantHeld reads whether the engine's grant to the seat on lane is in place,
// as the seat access ledger last observed it at the vendor; known is false
// where the ledger cannot be read.
func (e *Engine) grantHeld(ctx context.Context, handle, lane string) (held, known bool) {
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		return false, false
	}
	l := seataccess.Lane(lane)
	rec, found, err := ledger.SeatRecord(ctx, l.Kind(), handle)
	if err != nil {
		return false, false
	}
	if !found {
		return false, true
	}
	observed := rec.Lanes[l].Observed
	return observed.Exists && observed.Enabled, true
}
