package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// The per-seat access half of the setup surface ([setupapi.Access]), composed
// from the engine's plan, ledger, vendor sessions and sync so a preview, a tab
// and a sync are one function's answers; and [Engine.MigrateSeatAccess].

// optionsTTL is how long one integration's offer (each lane's levels, read live
// from the vendor) is reused: the tab rereads every second during a sync, and
// the list changes only when somebody edits a role at the vendor.
const optionsTTL = time.Minute

// optionsDeadline bounds one vendor listing, so a slow vendor makes the tab
// read the static offer rather than hang.
const optionsDeadline = 10 * time.Second

// appliedWait is how long a save's sync waits for this node to apply the
// revision the save wrote before it gives the work to the reconcile loop.
const appliedWait = 30 * time.Second

// SecretRows is when the fleet's store last wrote a value, by name, never the
// value: how the D12 danger notice learns a shared credential was replaced.
type SecretRows interface {
	Describe(ctx context.Context, name string) (secrets.Record, bool, error)
}

// SeatAccessAPI is [setupapi.Access] over this engine.
type SeatAccessAPI struct {
	engine *Engine
	// fleet is the fleet's company and revision, which a save's sync waits
	// for this node to run.
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

// UseSlackInstalls hands the adapter Slack's install-link minter (slack's
// Installs.Link: signed for the seat and app, recorded as issued so it is
// single-use, refusing as [setupapi.AccessRefusal]) and its rule for a seat
// awaiting install (slack.AwaitsInstall). It returns the adapter.
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
	rec, ok, err := ledger.SeatRecord(ctx, integration.KindSlack, handle)
	if errors.Is(err, coord.ErrUnavailable) {
		return "", fmt.Errorf("%w: %w", setupapi.ErrAccessUnavailable, err)
	}
	if err != nil {
		return "", err
	}
	if !ok || !rec.Held() || !a.awaitsInstall(rec) {
		return "", fmt.Errorf("%w: %s", setupapi.ErrNotAwaitingInstall, handle)
	}
	return a.installLink(ctx, handle, rec.Account.ID)
}

// HandIn checks a pasted credential for one seat's lane with the vendor, then
// runs the seat's sync carrying it, which seals it and records the account.
func (a *SeatAccessAPI) HandIn(ctx context.Context, q setupapi.AccessQuery, handle string,
	lane seataccess.Lane, value string, by iam.Actor) (seataccess.HandedIn, error) {

	vendor, ok := seataccess.Lookup(q.Kind)
	if !ok {
		return seataccess.HandedIn{}, setupapi.ErrNoHandIn
	}
	var seat *seataccess.Seat
	for _, s := range seataccess.SeatsOf(q.Company) {
		if s.Handle == handle {
			seat = &s
			break
		}
	}
	if seat == nil {
		return seataccess.HandedIn{}, fmt.Errorf("%w: %s", setupapi.ErrNoSuchAccessSeat, handle)
	}
	want, _ := seataccess.DesiredOf(seat.Role, lane)
	if !want.Enabled {
		return seataccess.HandedIn{}, fmt.Errorf("%w: %s", setupapi.ErrNotAwaitingHandIn, handle)
	}
	ledger, err := a.engine.SeatAccessLedger()
	if err != nil {
		return seataccess.HandedIn{}, err
	}
	records, err := ledger.Records(ctx, q.Kind)
	if errors.Is(err, coord.ErrUnavailable) {
		return seataccess.HandedIn{}, fmt.Errorf("%w: %w", setupapi.ErrAccessUnavailable, err)
	}
	if err != nil {
		return seataccess.HandedIn{}, err
	}
	session, err := vendor.Open(ctx, seataccess.OpenInput{Config: q.Company, Org: q.Roster, Env: a.engine.resolver(),
		Derive: a.engine.deriveKey})
	if errors.Is(err, integration.ErrNotConfigured) {
		return seataccess.HandedIn{}, fmt.Errorf("%w: %w", setupapi.ErrNotAwaitingHandIn, err)
	}
	if err != nil {
		return seataccess.HandedIn{}, err
	}
	hands, ok := session.(seataccess.HandsIn)
	if !ok {
		return seataccess.HandedIn{}, setupapi.ErrNoHandIn
	}
	in := seataccess.HandInInput{Seat: seat.SeatRef, Lane: lane, Want: want, Records: records, Value: value}
	if rec, ok := records[handle]; ok {
		in.Record = &rec
	}
	handed, err := hands.CheckHandIn(ctx, in)
	if refusal, ok := seataccess.RefusalOf(err); ok {
		return seataccess.HandedIn{}, &setupapi.AccessRefusal{Status: http.StatusBadRequest,
			Reason: refusal.Said, Remedy: refusal.Remedy}
	}
	if err != nil {
		name := seataccess.KindName(q.Kind)
		return seataccess.HandedIn{}, &setupapi.AccessRefusal{Status: http.StatusBadGateway,
			Reason: name + " did not answer, so the token was not checked. Nothing was stored.",
			Remedy: "Try again in a moment"}
	}
	asks := map[string]seataccess.Ask{handle: {HandIn: value, Rotate: handed.Rotate, Lane: lane}}
	if err = a.syncNow(ctx, q.Kind, by, asks, []string{handle}, nil); err != nil {
		return seataccess.HandedIn{}, err
	}
	// THE SYNC IS WHAT SEALED IT, and it refuses what changed since the check.
	after, err := ledger.Records(ctx, q.Kind)
	if err != nil {
		return seataccess.HandedIn{}, err
	}
	sync := after[handle].Lanes[lane].Sync
	if sync != nil && sync.Outcome == seataccess.OutcomeFailed && sync.Error != nil {
		return seataccess.HandedIn{}, &setupapi.AccessRefusal{Status: http.StatusBadRequest,
			Reason: sync.Error.Said, Remedy: sync.Error.Remedy}
	}
	if after[handle].Credential == nil {
		return seataccess.HandedIn{}, fmt.Errorf("engine: %s's %s sync sealed no credential", handle, q.Kind)
	}
	// READ BACK WITH THE SEALED VALUE, so the answer shows what else the
	// account holds (a robot's older tokens). The seal stands either way; the
	// next pass rereads it.
	if reread := a.syncNow(ctx, q.Kind, by, nil, []string{handle}, nil); reread != nil {
		log.WarnContext(ctx, "seat_access_reread_deferred", "kind", q.Kind, "seat", handle,
			"error", reread.Error())
	}
	return handed, nil
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

// compose reads the ledger and the vendor's offer once and answers the tab and,
// given a draft, its plan, both from [seataccess.Plan], as the sync is.
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
	// A PLAN IS DRAWN ONLY WHERE THE SYNC TAKES STEPS (engine-provisioned,
	// registered vendor, migrated); elsewhere it would promise steps that
	// never run and hold the tab at "Next sync" for ever.
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
		// A LANE MANAGED OUTSIDE CREWLET HAS NO PLAN and no step-up: the
		// engine grants nothing on it.
		if !provisioned {
			continue
		}
		// ONE WAITING ON ITS MIGRATION HAS NO STEPS YET but still asks a
		// raise's step-up: the sync applies the saved setting afterwards
		// without asking anybody again.
		steps := map[seataccess.Lane][]seataccess.Step{}
		if planned {
			if steps, err = seataccess.Plan(vendor, seatInput(q.Company, seat.SeatRef, next, rec, held,
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
	answer.SetStatuses()
	for _, rec := range records {
		for _, lr := range rec.Lanes {
			if lr.Sync != nil && lr.Sync.At.After(answer.LastSyncAt) {
				answer.LastSyncAt = lr.Sync.At
			}
		}
	}
	answer.Notice = a.notice(ctx, q.Kind, records, answer.Seats)
	if draft != nil {
		// WHO WILL REVIEW WHAT ONCE SAVED (ADR-0111): a count it could not
		// take is logged and left out, never a zero.
		reviews, err := a.engine.draftReviews(ctx, q.Company, q.Roster, answer, *draft, a.engine.openWork)
		if err != nil {
			log.WarnContext(ctx, "access_plan_reviews_uncounted", "kind", string(q.Kind),
				"error", err.Error(), "detail", "the plan names no agent's open work to review")
		}
		plan.Reviews = reviews
	}
	return answer, plan, nil
}

// seatAnswer is one seat on the tab. planned is whether the kind's sync takes
// steps (see compose), the only case a plan is drawn for.
func (a *SeatAccessAPI) seatAnswer(q setupapi.AccessQuery, vendor seataccess.Vendor,
	planned bool, ref seataccess.SeatRef, want map[seataccess.Lane]seataccess.Desired,
	rec seataccess.Record, held bool,
	offers map[seataccess.Lane]seataccess.Options) (seataccess.SeatAnswer, error) {

	sa := seataccess.SeatAnswer{Handle: ref.Handle, Name: ref.Name,
		Lanes: map[seataccess.Lane]seataccess.LaneAnswer{}}
	steps := map[seataccess.Lane][]seataccess.Step{}
	if planned {
		var err error
		if steps, err = seataccess.Plan(vendor, seatInput(q.Company, ref, want, rec, held, offers)); err != nil {
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
		// A CREDENTIAL ISSUED AT USE is the seat's while on, though nothing
		// seals it, so its tool reads live like any other.
		if issued := seataccess.IssuedCredential(q.Kind, lane, ref.Handle, d, observed); issued != nil {
			observed.Credential = issued
		}
		tool, reason := seataccess.ToolOf(q.Company, ref.Handle, lane, d, observed, lr.Settling, time.Now())
		sa.Lanes[lane] = seataccess.LaneAnswer{
			Desired: d, Observed: observed, ObservedAt: lr.ObservedAt, Sync: lr.Sync,
			Plan: nonNilSteps(steps[lane]),
			Tool: tool, ToolReason: reason,
			Administers: seataccess.Administers(offers[lane], d),
			Box:         seataccess.BoxOf(q.Company, q.Kind, lane, ref.Handle),
			HandIn:      seataccess.HandInOf(vendor, q.Company, ref, lane, d),
			Identity:    seataccess.IdentityOf(vendor, q.Company, ref, lane, d, observed, lr.Identity),
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

// seatInput is one seat's plan input from its document and ledger record, with
// each lane's current offer so a step names a live role as the vendor does,
// not by id.
func seatInput(c *config.Company, ref seataccess.SeatRef, want map[seataccess.Lane]seataccess.Desired,
	rec seataccess.Record, held bool,
	offers map[seataccess.Lane]seataccess.Options) seataccess.SeatInput {

	// A PLAN READS ONLY LIVE GRANTS, as the sync does ([seataccess.DesiredAt]).
	now := time.Now().UTC()
	live := make(map[seataccess.Lane]seataccess.Desired, len(want))
	for lane, d := range want {
		d.Grants = seataccess.LiveGrants(d.Grants, now)
		live[lane] = d
	}
	in := seataccess.SeatInput{Seat: ref, Want: live, Have: map[seataccess.Lane]seataccess.Observed{},
		Options: offers, Company: c}
	for lane, lr := range rec.Lanes {
		in.Have[lane] = lr.Observed
	}
	if held {
		r := rec
		in.Record = &r
	}
	return in
}

// liveOffer is a vendor's answer for a lane with the catalog's facts kept from
// base and a scoped lane's scope marked as read.
func liveOffer(base, live seataccess.Options) seataccess.Options {
	live.Rotatable, live.Releasable = base.Rotatable, base.Releasable
	live.Scoped, live.ScopeRequired = base.Scoped, base.ScopeRequired
	live.ScopeLive = base.Scoped
	return live
}

// seatOffers is what each of kind's lanes offers now on company, read live and
// reused for [optionsTTL] per revision: the Agents tab's pickers and every
// write's raise ([seataccess.AsksStepUp]) judge by it. A lane the engine does
// not provision is marked external and offers no rotation.
//
// FAIL CLOSED, NEVER CACHED WHEN IT FAILED: an unanswered vendor's level lane
// offers no levels, so a turn-on or level change asks a step-up
// ([seataccess.NeedsStepUp]), and an offer not read whole is not kept. An
// empty revision (a document not yet stored) is never cached.
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
	open := seataccess.OpenInput{Config: company, Org: roster, Env: e.resolver()}
	if registered && provisioned {
		open.Reads = e.seatReads(openCtx, kind)
		var err error
		if session, err = vendor.Open(openCtx, open); err != nil {
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
				o.Rotatable, o.Releasable = spec.Rotatable, spec.Releasable
			}
		}
		read := false
		if session != nil {
			if live, err := seataccess.ReadOptions(openCtx, vendor, session, open, lane); err == nil {
				o, read = liveOffer(o, live), true
			} else {
				whole = false
				log.WarnContext(ctx, "seat_access_offer_unread", "kind", kind,
					"lane", lane, "error", err.Error())
			}
		}
		o.Lane = lane
		o.ToolServer = seataccess.ToolServerOf(company, lane)
		if registered && !read {
			// THE DOCUMENT'S REACH WITHOUT THE VENDOR: an offer read live
			// carries it already ([seataccess.ReadOptions]).
			o = seataccess.WithReach(o, vendor, company, roster)
		}
		if !provisioned {
			o.Managed, o.Rotatable, o.Releasable = seataccess.ManagedExternal, false, false
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

// SeatAccessRaises is every agent seat's lane a write from prior to next raises
// to an administration grant, or cannot be shown not to
// ([seataccess.RaisesBetween]): what the config surface asks a fresh step-up
// for (D10). priorRevision is prior's stored revision.
//
// JUDGED ON THE CONNECTION THE WRITE LEAVES: prior's offer where integrations
// are unchanged, next's where one changes, since the next sync grants there.
// An unreadable offer fails closed.
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

// notice is the integration's danger notice: the first of the shared-credential
// findings the pass also raises on its status ([accessmigrate.SharedFindings]),
// so the two agree. A store that cannot say when a variable was last written
// leaves the notice up rather than clearing it on a guess.
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
// pass to raise on the integration's status: one per company credential a seat
// once kept its own in, until replaced ([accessmigrate.SharedFindings]).
// records are the kind's ledger records as the pass read them.
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
// AN ASK IS TAKEN NOW, under the kind's lease, so a person asking during
// another sync is told so rather than queued. A SAVE'S SYNC waits until this
// node applies the saved revision, since the sync reads the company this node
// runs; one not applied in time is left to the reconcile loop, which sweeps
// every integration when a revision applies.
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
		return a.syncNow(context.WithoutCancel(ctx), req.Kind, req.By, asks, handles, req.Then)
	}
	// MARKED BEFORE THE ANSWER IS READ, so the save's answer already shows a
	// sync, and held through the follow-up.
	a.mark(req.Kind, true)
	go func() {
		defer a.mark(req.Kind, false)
		detached := context.WithoutCancel(ctx)
		if a.syncAfterApply(detached, req) && req.Then != nil {
			req.Then(detached)
		}
	}()
	return nil
}

// syncNow runs one sync under the kind's lease, bounded as a pass is, and then
// its follow-up ([setupapi.AccessSync.Then]) wherever the sync ran.
//
// THE FOLLOW-UP IS DETACHED and holds the syncing mark, so the person is
// answered once the seat's sync is done and the tab reads as syncing until the
// follow-up ends.
func (a *SeatAccessAPI) syncNow(ctx context.Context, kind integration.Kind, by iam.Actor,
	asks map[string]seataccess.Ask, handles []string, then func(context.Context)) error {

	detached := context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, setup.PassDeadline)
	defer cancel()
	a.mark(kind, true)
	defer a.mark(kind, false)
	// DEFERRED AFTER THE MARK, so it runs first and the integration never
	// reads idle between the sync and its follow-up.
	ran := false
	defer func() {
		if !ran || then == nil {
			return
		}
		a.mark(kind, true)
		go func() {
			defer a.mark(kind, false)
			then(detached)
		}()
	}()
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
		// THE LANE'S RECORD SAYS WHAT FAILED (reason and remedy), which the
		// tab reads back, so the request is answered rather than refused.
		log.WarnContext(ctx, "seat_access_sync_failed", "kind", kind, "error", err.Error(),
			"by", by.Name)
		ran = true
		return nil
	}
	log.InfoContext(ctx, "seat_access_synced", "kind", kind, "findings",
		len(report.Findings), "by", by.Name)
	ran = true
	return nil
}

// syncAfterApply waits for this node to run the saved revision, then syncs,
// waiting out another's sync once. It reports whether the sync ran, which is
// when a follow-up is owed; a sync left to the reconcile loop gets the loop's
// own pass.
func (a *SeatAccessAPI) syncAfterApply(ctx context.Context, req setupapi.AccessSync) bool {
	if err := a.waitApplied(ctx, req.Kind); err != nil {
		log.InfoContext(ctx, "seat_access_sync_deferred", "kind", req.Kind,
			"revision", req.Revision, "detail", err.Error())
		return false
	}
	for attempt := 0; attempt < 2; attempt++ {
		passCtx, cancel := context.WithTimeout(ctx, setup.PassDeadline)
		report, err := a.engine.SyncSeatAccess(passCtx, req.Kind, req.By, nil, req.Handles)
		cancel()
		switch {
		case errors.Is(err, ErrSeatAccessBusy):
			select {
			case <-ctx.Done():
				return false
			case <-time.After(setup.RecordDeadline):
			}
			continue
		case errors.Is(err, integration.ErrNotConfigured):
			return false
		case err != nil:
			log.WarnContext(ctx, "seat_access_sync_failed", "kind", req.Kind,
				"revision", req.Revision, "error", err.Error(), "by", req.By.Name)
			return true
		}
		log.InfoContext(ctx, "seat_access_synced", "kind", req.Kind,
			"revision", req.Revision, "findings", len(report.Findings), "by", req.By.Name)
		return true
	}
	log.InfoContext(ctx, "seat_access_sync_deferred", "kind", req.Kind,
		"revision", req.Revision, "detail", "another sync held the integration; the "+
			"reconcile loop takes the saved revision on its next pass")
	return false
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

// MigrateSeatAccess moves every integration it can onto per-seat access
// ([accessmigrate.Run]), through this engine's vendor sessions, resolver and
// ledger and the caller's document and secret store.
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
			in.Derive = e.deriveKey
			return vendor.Open(ctx, in)
		},
		Env: e.resolver(), Managed: managed,
	})
	e.rememberSeatAccessPending(result)
	return result, err
}

// rememberSeatAccessPending keeps why each still-pending integration waits, for
// its access pass to say, and forgets the ones moved.
func (e *Engine) rememberSeatAccessPending(result accessmigrate.Result) {
	for kind, why := range result.Pending {
		e.seatAccessPending.Store(kind, accessmigrate.Said(why))
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
