package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/accessmigrate"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
)

// The per-seat access half of the setup surface: what the Agents tab's routes
// answer from ([setupapi.Access]), composed from the engine's own plan, ledger,
// vendor sessions and sync, so a preview, a tab and a sync are one function's
// answers. And the migration onto per-seat access ([Engine.MigrateSeatAccess]).

// optionsTTL is how long one integration's offer (each lane's levels, read live
// from the vendor where it lists them) is reused. The tab reads the answer
// again every second while a sync runs, and a vendor asked that often for a
// list that changes when somebody edits a role at the vendor is load for
// nothing.
const optionsTTL = time.Minute

// optionsDeadline bounds one vendor listing, so a slow vendor makes the tab
// read the static offer rather than hang.
const optionsDeadline = 10 * time.Second

// appliedWait is how long a save's sync waits for this node to apply the
// revision the save wrote before it gives the work to the reconcile loop.
const appliedWait = 30 * time.Second

// SecretRows is when the fleet's store last wrote a value, by name, never the
// value: what tells the D12 danger notice that a shared company credential has
// been replaced since.
type SecretRows interface {
	Describe(ctx context.Context, name string) (secrets.Record, bool, error)
}

// SeatAccessAPI is [setupapi.Access] over this engine.
type SeatAccessAPI struct {
	engine *Engine
	// fleet is the fleet's company and its revision, which a save's sync
	// waits for this node to run.
	fleet func(ctx context.Context) (*config.Company, string, error)
	rows  SecretRows
	// managed reports a company document only named systems write
	// ([config.Company.SeatCredentialMode]).
	managed func() bool
	// installLink mints one seat's Slack install link for its app
	// ([SeatAccessAPI.UseSlackInstalls]); nil where this node mints none.
	installLink func(ctx context.Context, handle, appID string) (string, error)
	// awaitsInstall is Slack's one rule for a seat waiting on its install.
	awaitsInstall func(seataccess.Record) bool

	mu sync.Mutex
	// running is each integration's syncs this node has under way, and when
	// the first of them began.
	running map[integration.Kind]syncMark
}

// syncMark is how many syncs of one integration run here, since when.
type syncMark struct {
	since time.Time
	n     int
}

// offerCache is each integration's offer by the revision it was read for
// ([Engine.seatOffers]): only an offer read whole is kept.
type offerCache struct {
	mu    sync.Mutex
	byKey map[string]cachedOffer
}

type cachedOffer struct {
	at      time.Time
	options []seataccess.Options
}

var _ setupapi.Access = (*SeatAccessAPI)(nil)

// SeatAccessAPI builds the access routes' engine half. fleet reads the fleet's
// company (configapi's FleetCompany), rows the fleet's secret store, and managed
// whether only named systems write the company document (configapi's Managed).
func (e *Engine) SeatAccessAPI(fleet func(ctx context.Context) (*config.Company, string, error),
	rows SecretRows, managed func() bool) *SeatAccessAPI {
	return &SeatAccessAPI{engine: e, fleet: fleet, rows: rows, managed: managed,
		running: map[integration.Kind]syncMark{}}
}

// UseSlackInstalls hands the adapter what mints a seat's Slack install link
// (slack's Installs.Link, its refusals given as [setupapi.AccessRefusal]),
// signed for the seat and its app and recorded as issued, so a link is
// single-use, and Slack's own rule for a seat waiting on its install
// (slack.AwaitsInstall). It returns the adapter, for wiring.
func (a *SeatAccessAPI) UseSlackInstalls(
	link func(ctx context.Context, handle, appID string) (string, error),
	awaits func(seataccess.Record) bool) *SeatAccessAPI {
	a.installLink, a.awaitsInstall = link, awaits
	return a
}

// ErrNoSlackInstalls reports a node that mints no Slack install links.
var ErrNoSlackInstalls = errors.New("engine: this node mints no Slack install links")

// InstallLink mints one agent's Slack install link, for the app the ledger
// records for the seat.
func (a *SeatAccessAPI) InstallLink(ctx context.Context, q setupapi.AccessQuery,
	handle string) (string, error) {

	if a.installLink == nil || a.awaitsInstall == nil {
		return "", ErrNoSlackInstalls
	}
	ledger, err := a.engine.SeatAccessLedger()
	if err != nil {
		return "", err
	}
	records, err := ledger.Records(ctx, integration.KindSlack)
	if errors.Is(err, coord.ErrUnavailable) {
		return "", fmt.Errorf("%w: %w", setupapi.ErrAccessUnavailable, err)
	}
	if err != nil {
		return "", err
	}
	rec, ok := records[handle]
	if !ok || !rec.Held() || !a.awaitsInstall(rec) {
		return "", fmt.Errorf("%w: %s", setupapi.ErrNotAwaitingInstall, handle)
	}
	return a.installLink(ctx, handle, rec.Account.ID)
}

// Answer composes one integration's Agents tab.
func (a *SeatAccessAPI) Answer(ctx context.Context, q setupapi.AccessQuery) (seataccess.Answer, error) {
	answer, _, err := a.compose(ctx, q, nil)
	return answer, err
}

// Plan is what a draft would do, from the same composition.
func (a *SeatAccessAPI) Plan(ctx context.Context, q setupapi.AccessQuery,
	draft seataccess.Draft) (seataccess.PlanAnswer, error) {

	_, plan, err := a.compose(ctx, q, &draft)
	return plan, err
}

// compose reads the ledger and what the vendor offers once, and answers the tab
// and, given a draft, its plan: each from [seataccess.Plan], the function the
// sync takes its steps from.
func (a *SeatAccessAPI) compose(ctx context.Context, q setupapi.AccessQuery,
	draft *seataccess.Draft) (seataccess.Answer, seataccess.PlanAnswer, error) {

	answer := seataccess.Answer{Kind: q.Kind, Revision: q.Revision, Seats: []seataccess.SeatAnswer{}}
	plan := seataccess.PlanAnswer{Kind: q.Kind, Revision: q.Revision, Plans: []seataccess.SeatPlan{}}
	ledger, err := a.engine.SeatAccessLedger()
	if err != nil {
		return answer, plan, err
	}
	records, err := ledger.Records(ctx, q.Kind)
	if errors.Is(err, coord.ErrUnavailable) {
		return answer, plan, fmt.Errorf("%w: %w", setupapi.ErrAccessUnavailable, err)
	}
	if err != nil {
		return answer, plan, err
	}
	vendor, registered := seataccess.Lookup(q.Kind)
	provisioned := q.Company.SeatCredentialMode(string(q.Kind), a.managed()) ==
		config.SeatCredentialEngine
	answer.Options = a.engine.seatOffers(ctx, q.Kind, q.Company, q.Roster, q.Revision,
		a.managed())
	// A PLAN IS DRAWN ONLY WHERE THE SYNC TAKES ITS STEPS: a kind the engine
	// provisions, through a registered vendor, once its migration has
	// finished. Anywhere else the sync takes none, so a plan would promise
	// steps that never run and hold the tab at "Next sync" for ever.
	planned := registered && provisioned && !q.Company.SeatAccessPending(string(q.Kind))
	offers := map[seataccess.Lane]seataccess.Options{}
	for _, o := range answer.Options {
		offers[o.Lane] = o
	}
	a.mu.Lock()
	running := a.running[q.Kind]
	a.mu.Unlock()
	answer.Syncing, answer.StartedAt = running.n > 0, running.since

	lanes := seataccess.LanesOf(q.Kind)
	seen := map[string]bool{}
	for _, seat := range seataccess.SeatsOf(q.Company) {
		seen[seat.Handle] = true
		rec, held := records[seat.Handle]
		want := map[seataccess.Lane]seataccess.Desired{}
		for _, lane := range lanes {
			want[lane], _ = seataccess.DesiredOf(seat.Role, lane)
		}
		sa, err := a.seatAnswer(q, vendor, planned, seat.SeatRef, want, rec, held, offers)
		if err != nil {
			return answer, plan, err
		}
		if !provisioned {
			handedIn(&sa, seat.Role, q.Kind)
		}
		answer.Seats = append(answer.Seats, sa)
		if draft == nil {
			continue
		}
		edits, named := draft.Seats[seat.Handle]
		if !named {
			continue
		}
		next := map[seataccess.Lane]seataccess.Desired{}
		for lane, d := range want {
			next[lane] = d
		}
		for lane, d := range edits {
			next[lane] = d.Normalized()
		}
		// A LANE MANAGED OUTSIDE CREWLET HAS NO PLAN and asks no step-up:
		// the engine grants nothing on it.
		if !provisioned {
			continue
		}
		// ONE WAITING ON ITS MIGRATION HAS NO STEPS YET, and still asks the
		// step-up a raise does: the saved setting is the one the sync
		// applies once the migration finishes, with nobody asked again.
		steps := map[seataccess.Lane][]seataccess.Step{}
		if planned {
			if steps, err = seataccess.Plan(vendor, seatInput(seat.SeatRef, next, rec, held,
				offers)); err != nil {
				return answer, plan, err
			}
		}
		for _, lane := range lanes {
			if _, edited := edits[lane]; !edited {
				continue
			}
			stepUp := seataccess.AsksStepUp(offers[lane], want[lane], next[lane])
			plan.StepUp = plan.StepUp || stepUp
			plan.Plans = append(plan.Plans, seataccess.SeatPlan{Handle: seat.Handle,
				Lane: lane, Steps: nonNilSteps(steps[lane]), StepUp: stepUp})
		}
	}
	// A SEAT THAT LEFT THE DOCUMENT WITH AN ACCOUNT STILL RECORDED is listed,
	// off, so a person can remove it.
	for _, handle := range sortedHandles(records) {
		if seen[handle] || !records[handle].Held() {
			continue
		}
		rec := records[handle]
		sa, err := a.seatAnswer(q, vendor, planned,
			seataccess.SeatRef{Handle: handle, Name: rec.Name},
			map[seataccess.Lane]seataccess.Desired{}, rec, true, offers)
		if err != nil {
			return answer, plan, err
		}
		sa.Departed = true
		answer.Seats = append(answer.Seats, sa)
	}
	for _, rec := range records {
		for _, lr := range rec.Lanes {
			if lr.Sync != nil && lr.Sync.At.After(answer.LastSyncAt) {
				answer.LastSyncAt = lr.Sync.At
			}
		}
	}
	answer.Notice = a.notice(ctx, q.Kind, records, answer.Seats)
	return answer, plan, nil
}

// seatAnswer is one seat on the tab. planned is a kind whose sync takes steps
// (provisioned by the engine through a registered vendor, and migrated), the
// only kind a plan is drawn for.
func (a *SeatAccessAPI) seatAnswer(q setupapi.AccessQuery, vendor seataccess.Vendor,
	planned bool, ref seataccess.SeatRef, want map[seataccess.Lane]seataccess.Desired,
	rec seataccess.Record, held bool,
	offers map[seataccess.Lane]seataccess.Options) (seataccess.SeatAnswer, error) {

	sa := seataccess.SeatAnswer{Handle: ref.Handle, Name: ref.Name,
		Lanes: map[seataccess.Lane]seataccess.LaneAnswer{}}
	steps := map[seataccess.Lane][]seataccess.Step{}
	if planned {
		var err error
		if steps, err = seataccess.Plan(vendor, seatInput(ref, want, rec, held, offers)); err != nil {
			return sa, err
		}
	}
	for _, lane := range seataccess.LanesOf(q.Kind) {
		lr := rec.Lanes[lane]
		observed := lr.Observed
		if observed.Credential == nil && rec.Credential != nil {
			observed.Credential = rec.Credential
		}
		d := want[lane]
		sa.Lanes[lane] = seataccess.LaneAnswer{
			Desired: d, Observed: observed, Sync: lr.Sync, Plan: nonNilSteps(steps[lane]),
			Tool:        seataccess.ToolStateOf(q.Company, lane, d, observed),
			Administers: seataccess.Administers(offers[lane], d),
		}
	}
	return sa, nil
}

// handedIn reports, on an external lane, the credential the seat was handed
// from outside Crewlet: the variable it names, never its value.
func handedIn(sa *seataccess.SeatAnswer, role *config.Role, kind integration.Kind) {
	variable := accessmigrate.HandedIn(role, kind)
	if variable == "" {
		return
	}
	for lane, la := range sa.Lanes {
		if la.Observed.Credential == nil {
			la.Observed.Credential = &seataccess.Credential{Variable: variable,
				Source: seataccess.CredentialExternal}
			sa.Lanes[lane] = la
		}
	}
}

// seatInput is one seat's plan input from its document and its ledger record,
// with what each lane offers now, so a step names a live role as the vendor
// does rather than by its id.
func seatInput(ref seataccess.SeatRef, want map[seataccess.Lane]seataccess.Desired,
	rec seataccess.Record, held bool,
	offers map[seataccess.Lane]seataccess.Options) seataccess.SeatInput {

	in := seataccess.SeatInput{Seat: ref, Want: want, Have: map[seataccess.Lane]seataccess.Observed{},
		Options: offers}
	for lane, lr := range rec.Lanes {
		in.Have[lane] = lr.Observed
	}
	if held {
		r := rec
		in.Record = &r
	}
	return in
}

// seatOffers is what each of kind's lanes offers now on company, read live from
// the vendor and reused for [optionsTTL] per revision: the offer the Agents
// tab draws its pickers from, and the one every write's raise is judged by
// ([seataccess.AsksStepUp]). A lane the engine does not provision is marked
// external, and offers no rotation.
//
// FAIL CLOSED, AND NEVER KEPT WHEN IT FAILED. Where the vendor does not
// answer, a lane that grants by level offers no levels, so none is vouched
// for and a turn-on or a change of level on it asks a step-up
// ([seataccess.NeedsStepUp]); and an offer any part of which was not read is
// not cached, so the next read asks again rather than judging by what failed. An empty revision (a document not yet stored)
// is read and never cached.
func (e *Engine) seatOffers(ctx context.Context, kind integration.Kind,
	company *config.Company, roster *org.Organization, revision string,
	managed bool) []seataccess.Options {

	key := string(kind) + "\x00" + revision
	if revision != "" {
		e.offers.mu.Lock()
		cached, ok := e.offers.byKey[key]
		e.offers.mu.Unlock()
		if ok && time.Since(cached.at) < optionsTTL {
			return cached.options
		}
	}
	vendor, registered := seataccess.Lookup(kind)
	provisioned := company.SeatCredentialMode(string(kind), managed) ==
		config.SeatCredentialEngine
	base := seataccess.UnreadOffers(kind, !provisioned)
	whole := !provisioned
	var session seataccess.Session
	openCtx, cancel := context.WithTimeout(ctx, optionsDeadline)
	defer cancel()
	if registered && provisioned {
		var err error
		if session, err = vendor.Open(openCtx, seataccess.OpenInput{Config: company,
			Org: roster, Env: e.resolver()}); err != nil {
			session = nil
			log.WarnContext(ctx, "seat_access_offer_unread", "kind", kind, "error", err.Error(),
				"detail", "a turn-on or a change of level asks a step-up until the vendor answers")
		}
		whole = session != nil
	}
	lanes := seataccess.LanesOf(kind)
	out := make([]seataccess.Options, 0, len(lanes))
	for _, lane := range lanes {
		o := base[lane]
		if registered {
			if spec, ok := vendor.Catalog().Lane(lane); ok {
				o.Rotatable = spec.Rotatable
			}
		}
		if session != nil {
			if live, err := session.Options(openCtx, lane); err == nil {
				live.Rotatable = o.Rotatable
				o = live
			} else {
				whole = false
				log.WarnContext(ctx, "seat_access_offer_unread", "kind", kind,
					"lane", lane, "error", err.Error())
			}
		}
		o.Lane = lane
		if !provisioned {
			o.Managed, o.Rotatable = seataccess.ManagedExternal, false
		}
		out = append(out, o)
	}
	if whole && revision != "" {
		e.offers.mu.Lock()
		if e.offers.byKey == nil {
			e.offers.byKey = map[string]cachedOffer{}
		}
		e.offers.byKey[key] = cachedOffer{at: time.Now(), options: out}
		e.offers.mu.Unlock()
	}
	return out
}

// SeatAccessRaises is every agent seat's lane a write of the company document
// from prior to next raises to an administration grant, or could not be told
// not to ([seataccess.RaisesBetween]): what the config surface asks a person's
// fresh step-up for (D10). priorRevision is prior's stored revision.
//
// JUDGED ON THE CONNECTION THE WRITE LEAVES: the offer read for prior's
// revision where the write leaves every integration's connection as it was,
// and one read for next where it changes one, since that is the vendor the
// next sync grants at. An offer that cannot be read fails closed.
func (e *Engine) SeatAccessRaises(ctx context.Context, prior *config.Company,
	priorRevision string, next *config.Company) []seataccess.Raise {

	company, revision := next, ""
	if prior != nil && reflect.DeepEqual(prior.Integrations, next.Integrations) {
		company, revision = prior, priorRevision
	}
	return seataccess.RaisesBetween(prior, next, e.companyManaged, func(kind integration.Kind) map[seataccess.Lane]seataccess.Options {
		roster, err := company.Organization()
		if err != nil {
			return seataccess.UnreadOffers(kind,
				company.SeatCredentialMode(string(kind), e.companyManaged) !=
					config.SeatCredentialEngine)
		}
		out := map[seataccess.Lane]seataccess.Options{}
		for _, o := range e.seatOffers(ctx, kind, company, roster, revision, e.companyManaged) {
			out[o.Lane] = o
		}
		return out
	})
}

// notice is the integration's danger notice: the first of its shared-credential
// findings ([accessmigrate.SharedFindings]), the same findings the pass raises
// on the integration's status, so the tab and the status say one thing. A store
// that cannot say when a variable was last written leaves the notice up rather
// than clearing it on a guess.
func (a *SeatAccessAPI) notice(ctx context.Context, kind integration.Kind,
	records map[string]seataccess.Record, seats []seataccess.SeatAnswer) *seataccess.Notice {

	names := map[string]string{}
	for _, s := range seats {
		names[s.Handle] = s.Name
	}
	written := writtenBy(a.rows)
	findings, err := accessmigrate.SharedFindings(ctx, kind, records, names,
		func(ctx context.Context, name string) (time.Time, bool, error) {
			at, held, err := written(ctx, name)
			if err != nil {
				log.WarnContext(ctx, "seat_access_notice_unread", "kind", kind,
					"variable", name, "error", err.Error())
				return time.Time{}, false, nil
			}
			return at, held, nil
		})
	if err != nil || len(findings) == 0 {
		return nil
	}
	n := accessmigrate.NoticeOf(kind, findings[0])
	return &n
}

// writtenBy is [accessmigrate.Written] over a store's rows: when each value was
// last written, never the value.
func writtenBy(rows SecretRows) accessmigrate.Written {
	return func(ctx context.Context, name string) (time.Time, bool, error) {
		row, found, err := rows.Describe(ctx, name)
		return row.UpdatedAt, found, err
	}
}

// SharedCredentialFindings are kind's shared-credential findings (D12) for the
// pass to raise on the integration's status beside its own: one per company
// credential a seat once kept its own credential in, until it is replaced
// ([accessmigrate.SharedFindings]). records are the kind's ledger records, as
// the pass read them.
func (e *Engine) SharedCredentialFindings(ctx context.Context, kind integration.Kind,
	records map[string]seataccess.Record) ([]integration.Finding, error) {

	if e.backends == nil || e.backends.Fleet == nil {
		return nil, errors.New("engine: this engine holds no secret store to read")
	}
	names := map[string]string{}
	if company := e.Company(); company != nil {
		for _, seat := range seataccess.SeatsOf(company.Config) {
			names[seat.Handle] = seat.Name
		}
	}
	return accessmigrate.SharedFindings(ctx, kind, records, names,
		writtenBy(fleetsecrets.New(e.backends.Fleet, e.Cipher())))
}

// Sync starts one integration's sync.
//
// AN ASK IS TAKEN NOW, under the kind's lease, so a person asking while another
// sync runs is told so rather than queued behind it. A SAVE'S SYNC is taken once
// this node has applied the revision the save wrote, because the sync reads
// the company this node runs: taken before, it would apply the document as it
// stood. A save that this node does not apply in time is the reconcile loop's,
// which sweeps every integration when a revision applies.
func (a *SeatAccessAPI) Sync(ctx context.Context, req setupapi.AccessSync) error {
	asks := map[string]seataccess.Ask{}
	var handles []string
	for _, s := range req.Asks {
		ask := asks[s.Handle]
		ask.Rotate = ask.Rotate || s.Ask.Rotate
		ask.Remove = ask.Remove || s.Ask.Remove
		ask.Reenable = ask.Reenable || s.Ask.Reenable
		if s.Lane != "" {
			ask.Lane = s.Lane
		}
		asks[s.Handle] = ask
		if !slices.Contains(handles, s.Handle) {
			handles = append(handles, s.Handle)
		}
	}
	for _, handle := range req.Handles {
		if !slices.Contains(handles, handle) {
			handles = append(handles, handle)
		}
	}
	if !req.AfterSave {
		return a.syncNow(context.WithoutCancel(ctx), req.Kind, req.By, asks, handles)
	}
	// MARKED BEFORE THE ANSWER IS READ, so the tab the save answers with
	// already says a sync is under way.
	a.mark(req.Kind, true)
	go func() {
		defer a.mark(req.Kind, false)
		a.syncAfterApply(context.WithoutCancel(ctx), req)
	}()
	return nil
}

// syncNow runs one sync under the kind's lease, bounded as a pass is.
func (a *SeatAccessAPI) syncNow(ctx context.Context, kind integration.Kind, by iam.Actor,
	asks map[string]seataccess.Ask, handles []string) error {

	ctx, cancel := context.WithTimeout(ctx, setup.PassDeadline)
	defer cancel()
	a.mark(kind, true)
	defer a.mark(kind, false)
	report, err := a.engine.SyncSeatAccess(ctx, kind, by, asks, handles)
	switch {
	case errors.Is(err, ErrSeatAccessBusy):
		return fmt.Errorf("%w: %w", setupapi.ErrAccessSyncInFlight, err)
	case errors.Is(err, seataccess.ErrStillOn):
		return err
	case errors.Is(err, coord.ErrUnavailable):
		return fmt.Errorf("%w: %w", setupapi.ErrAccessUnavailable, err)
	case errors.Is(err, integration.ErrNotConfigured):
		return nil
	case err != nil:
		// THE LANE'S OWN RECORD SAYS WHAT FAILED, with the vendor's reason
		// and the remedy, which the tab reads back; the request is answered
		// with it rather than refused.
		log.WarnContext(ctx, "seat_access_sync_failed", "kind", kind, "error", err.Error(),
			"by", by.Name)
		return nil
	}
	log.InfoContext(ctx, "seat_access_synced", "kind", kind, "findings",
		len(report.Findings), "by", by.Name)
	return nil
}

// syncAfterApply waits for this node to run the revision a save wrote, then
// syncs; a sync another holds is waited out once.
func (a *SeatAccessAPI) syncAfterApply(ctx context.Context, req setupapi.AccessSync) {
	if err := a.waitApplied(ctx, req.Kind); err != nil {
		log.InfoContext(ctx, "seat_access_sync_deferred", "kind", req.Kind,
			"revision", req.Revision, "detail", err.Error())
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		passCtx, cancel := context.WithTimeout(ctx, setup.PassDeadline)
		report, err := a.engine.SyncSeatAccess(passCtx, req.Kind, req.By, nil, req.Handles)
		cancel()
		switch {
		case errors.Is(err, ErrSeatAccessBusy):
			select {
			case <-ctx.Done():
				return
			case <-time.After(setup.RecordDeadline):
			}
			continue
		case err != nil && !errors.Is(err, integration.ErrNotConfigured):
			log.WarnContext(ctx, "seat_access_sync_failed", "kind", req.Kind,
				"revision", req.Revision, "error", err.Error(), "by", req.By.Name)
			return
		}
		log.InfoContext(ctx, "seat_access_synced", "kind", req.Kind,
			"revision", req.Revision, "findings", len(report.Findings), "by", req.By.Name)
		return
	}
	log.InfoContext(ctx, "seat_access_sync_deferred", "kind", req.Kind,
		"revision", req.Revision, "detail", "another sync held the integration; the "+
			"reconcile loop takes the saved revision on its next pass")
}

// waitApplied waits until this node runs the fleet's setting for every seat of
// kind, or [appliedWait] passes.
func (a *SeatAccessAPI) waitApplied(ctx context.Context, kind integration.Kind) error {
	fleet, _, err := a.fleet(ctx)
	if err != nil {
		return err
	}
	want := settingsOf(fleet, kind)
	deadline := time.Now().Add(appliedWait)
	for {
		if running := a.engine.Company(); running != nil &&
			equalSettings(settingsOf(running.Config, kind), want) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("this node has not applied the saved revision yet; the " +
				"reconcile loop takes it when it does")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// settingsOf is every agent seat's setting on kind's lanes.
func settingsOf(c *config.Company, kind integration.Kind) map[string]map[seataccess.Lane]seataccess.Desired {
	out := map[string]map[seataccess.Lane]seataccess.Desired{}
	for _, seat := range seataccess.SeatsOf(c) {
		lanes := map[seataccess.Lane]seataccess.Desired{}
		for _, lane := range seataccess.LanesOf(kind) {
			d, _ := seataccess.DesiredOf(seat.Role, lane)
			lanes[lane] = d.Normalized()
		}
		out[seat.Handle] = lanes
	}
	return out
}

func equalSettings(a, b map[string]map[seataccess.Lane]seataccess.Desired) bool {
	if len(a) != len(b) {
		return false
	}
	for handle, lanes := range a {
		other, ok := b[handle]
		if !ok || len(other) != len(lanes) {
			return false
		}
		for lane, d := range lanes {
			o := other[lane]
			if d.Enabled != o.Enabled || !d.SameAccess(o) {
				return false
			}
		}
	}
	return true
}

func (a *SeatAccessAPI) mark(kind integration.Kind, on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.running[kind]
	if on {
		if m.n == 0 {
			m.since = time.Now().UTC()
		}
		m.n++
		a.running[kind] = m
		return
	}
	if m.n--; m.n <= 0 {
		delete(a.running, kind)
		return
	}
	a.running[kind] = m
}

func nonNilSteps(steps []seataccess.Step) []seataccess.Step {
	if steps == nil {
		return []seataccess.Step{}
	}
	return steps
}

func sortedHandles[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// MigrateSeatAccess moves the fleet's company onto per-seat access, every
// integration it can ([accessmigrate.Run]): the vendors read through this
// engine's own sessions and resolver, the ledger this engine keeps, and the
// document and the secret store the caller hands in.
func (e *Engine) MigrateSeatAccess(ctx context.Context, doc accessmigrate.Document,
	store accessmigrate.Secrets, managed bool) (accessmigrate.Result, error) {

	ledger, err := e.SeatAccessLedger()
	if err != nil {
		return accessmigrate.Result{}, err
	}
	result, err := accessmigrate.Run(ctx, accessmigrate.Deps{
		Document: doc, Secrets: store, Records: ledgerRecords{ledger: ledger},
		Open: func(ctx context.Context, kind integration.Kind,
			in seataccess.OpenInput) (seataccess.Session, error) {
			vendor, ok := seataccess.Lookup(kind)
			if !ok {
				return nil, fmt.Errorf("no %s vendor is registered in this build", kind)
			}
			return vendor.Open(ctx, in)
		},
		Env: e.resolver(), Managed: managed,
	})
	e.rememberSeatAccessPending(result)
	return result, err
}

// rememberSeatAccessPending keeps why each integration waits, for its access
// pass to say: kept for the ones an attempt left pending, forgotten for the
// ones it moved.
func (e *Engine) rememberSeatAccessPending(result accessmigrate.Result) {
	for kind, why := range result.Pending {
		e.seatAccessPending.Store(kind, why.Error())
	}
	for _, kind := range result.Migrated {
		e.seatAccessPending.Delete(kind)
	}
}

// seatAccessPendingReason is why the migration left kind unmigrated on its last
// attempt on this node, or empty where it gave no reason here.
func (e *Engine) seatAccessPendingReason(kind integration.Kind) string {
	why, _ := e.seatAccessPending.Load(kind)
	reason, _ := why.(string)
	return reason
}

// ledgerRecords is the engine's ledger as [accessmigrate.Records].
type ledgerRecords struct{ ledger seataccess.Ledger }

func (r ledgerRecords) Ledger() seataccess.Ledger { return r.ledger }
