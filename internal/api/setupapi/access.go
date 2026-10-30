package setupapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// What each agent seat may do on one integration: the Agents tab's four routes.
//
//	GET  /setup/integrations/{kind}/access          every seat: desired, observed, last sync, plan, tool
//	POST /setup/integrations/{kind}/access/plan     what a draft would do at the vendor, writing nothing
//	PUT  /setup/integrations/{kind}/access          a draft saved as one revision, then synced
//	POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}
//
// # The save is a revision like any other
//
// Each seat's setting lives in the company document (`roles[].integrations`),
// so a save is one revision of it, written through the config surface's own
// several-seat write ([configapi.Service.ApplySeats]): attributed, validated as
// the whole document, held to the revision the tab was read at, and reverted
// like any other change. Nothing here writes the document a second way.
//
// # The plan is the sync's
//
// What a draft would do is answered by the engine's own plan, the function the
// sync takes its steps from ([Access.Plan]), so a preview cannot promise what a
// sync will not do. This surface composes nothing about a vendor itself.
//
// # A seat's credential is never named here
//
// A seat's credential is sealed under the name the engine derives for it
// ([seataccess.SinkName]), and no body this surface reads can carry a variable
// name: the draft is on or off, the vendor's level, its permissions and its
// scope, decoded strictly. The answers name the sealed variable and when it was
// minted, never its value.

// Access is the engine's per-seat access, as the access routes need it.
//
// THE CONSUMER'S OWN INTERFACE, for the reason [Status] is one: the routes need
// the engine's answer about each seat (the ledger, what the vendor offers now,
// the plan, whether a sync runs), and the engine is what holds the vendors'
// sessions and the fleet's lease. A test drives the routes with something that
// is not an engine.
type Access interface {
	// Answer is one integration's Agents tab, against the company q names:
	// every agent seat with what the document asks, what the vendor last
	// showed, the last sync and the plan, what each lane offers now, and
	// the integration's notice.
	Answer(ctx context.Context, q AccessQuery) (seataccess.Answer, error)

	// Plan is what draft would do at the vendor on q's company, writing
	// nothing: every seat's lane the draft changes, with its steps and
	// whether saving it needs a fresh step-up. The SAME function the sync
	// takes its steps from.
	Plan(ctx context.Context, q AccessQuery, draft seataccess.Draft) (seataccess.PlanAnswer, error)

	// Sync starts a sync of one integration's seats, detached from the
	// request, and returns once it has started. A sync with asks is refused
	// [ErrAccessSyncInFlight] while another runs, since an ask is one
	// person's request about one seat; one without asks is taken once the
	// running one finishes, since a save must reach the vendor.
	Sync(ctx context.Context, req AccessSync) error

	// InstallLink mints a fresh, single-use link a person follows to approve
	// one agent's Slack app install, signed for that seat and its app. It is
	// a bearer credential for its short life: never stored, never logged,
	// never answered on a read. [ErrNotAwaitingInstall] where the seat's app
	// is not waiting for its install, and an [*AccessRefusal] where the
	// vendor's own rules refuse one.
	InstallLink(ctx context.Context, q AccessQuery, handle string) (string, error)
}

// ErrNotAwaitingInstall refuses an install link for an agent whose Slack app is
// not waiting for a person to approve its install. The engine's implementation
// wraps it.
var ErrNotAwaitingInstall = errors.New("setupapi: this agent's app is not waiting for an install")

// AccessRefusal is a refusal the engine's access half names in the vendor's
// own terms (a Slack app with no OAuth client yet, a deployment with no public
// address to land an install on): the status it is answered with, what is
// wrong and what to do.
type AccessRefusal struct {
	Status         int
	Reason, Remedy string

	// RetryAfter is how long to wait where the refusal is transient (a
	// store the vendor's step needs could not be read): answered as a 503
	// with a Retry-After, and never set otherwise.
	RetryAfter time.Duration
}

func (r *AccessRefusal) Error() string { return r.Reason + " (" + r.Remedy + ")" }

// AccessQuery is the company an access answer or plan is about: the fleet's,
// with the revision it is ([fleetCompany]).
type AccessQuery struct {
	Kind     integration.Kind
	Company  *config.Company
	Roster   *org.Organization
	Revision string
}

// AccessSync is one sync asked for.
type AccessSync struct {
	Kind integration.Kind

	// Revision is the fleet revision the sync takes the document from, or a
	// later one: the revision a save just activated, which this node may not
	// have applied yet.
	Revision string

	// Asks are the one-offs a person asked for, by seat and lane: a rotate,
	// a remove, a turn back on. Empty for a save or a retry.
	Asks []SeatAsk

	// Handles are the seats the sync is for: the seats a save changed, or
	// the one seat a person acted on. Empty is every seat of the kind.
	Handles []string

	// AfterSave is the sync a save starts: taken once this node runs the
	// revision the save wrote, rather than at once, since the sync reads the
	// company the node runs.
	AfterSave bool

	// By is who asked, which the sync records against what it seals and
	// the ledger rows it writes.
	By iam.Actor
}

// SeatAsk is one person's ask on one seat's lane.
type SeatAsk struct {
	Handle string
	Lane   seataccess.Lane
	Ask    seataccess.Ask
}

// ErrAccessSyncInFlight reports an ask refused because a sync of the same
// integration is running. The engine's implementation wraps it.
var ErrAccessSyncInFlight = errors.New("setupapi: a sync of this integration's agents is running")

// accessDraft is the body of the plan and save routes.
//
// DECODED STRICTLY, as every submission here is ([decode]): a field this
// surface does not take is refused rather than ignored, so a body that tried to
// name a seat's credential variable is refused by name rather than quietly
// saved without it.
type accessDraft struct {
	// Seats are each seat's lanes as the person left them. A seat or lane
	// it leaves out keeps its setting.
	Seats map[string]map[seataccess.Lane]seataccess.Desired `json:"seats"`

	// Revision is the revision the tab was read at, the access answer's
	// `revision`: what the save is held to where no If-Match header is sent.
	Revision string `json:"revision,omitempty"`

	// Summary is an optional history line, after the one this surface
	// writes ([accessSummary]).
	Summary string `json:"summary,omitempty"`
}

// accessCheck is a dry run's answer: the plan, and the warnings the write would
// introduce, as every route's check answers them ([configapi.CheckedWarning]).
type accessCheck struct {
	seataccess.PlanAnswer
	Warnings []configapi.CheckedWarning `json:"warnings"`
}

// accessKind is the integration a request names, or the refusal answered.
//
// THE KINDS THAT MANAGE AN ACCOUNT PER AGENT ([seataccess.Kinds]), and no
// other. Jira and Confluence are lanes of `atlassian`, whose one account per
// agent both products grant, so the two are named there rather than answered
// twice.
func accessKind(w http.ResponseWriter, r *http.Request) (integration.Kind, bool) {
	kind := integration.Kind(r.PathValue("kind"))
	if slices.Contains(seataccess.Kinds(), kind) {
		return kind, true
	}
	detail := map[string]string{"hint": accessElsewhere}
	if kind == integration.KindJira || kind == integration.KindConfluence {
		detail["detail"] = "Jira and Confluence are set per agent on atlassian, " +
			"whose one account per agent both products grant"
	}
	httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSeatAccess, detail)
	return "", false
}

// accessQuery is the fleet's company for a request, as an [AccessQuery].
func (s *Service) accessQuery(w http.ResponseWriter, r *http.Request,
	kind integration.Kind) (AccessQuery, bool) {

	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok {
		return AccessQuery{}, false
	}
	return AccessQuery{Kind: kind, Company: fc.company, Roster: fc.roster,
		Revision: fc.revision}, true
}

// writeAccess reads q's answer and writes it with the revision as its entity
// tag, which a save sends back as If-Match.
func (s *Service) writeAccess(w http.ResponseWriter, r *http.Request, q AccessQuery) {

	answer, err := s.access.Answer(r.Context(), q)
	if err != nil {
		s.accessFault(w, r, q.Kind, "setup_access_unread", err)
		return
	}
	if s.config.Managed() {
		answer.ManagedBy = seataccess.ManagedByOperator
	}
	w.Header().Set("ETag", `"`+answer.Revision+`"`)
	httpjson.Write(w, http.StatusOK, answer)
}

// mayWriteAccess refuses a change to the company document on a deployment
// whose document the operator writes (Enforce mode), naming where the change
// is made instead, and reports whether the handler may go on. The refusal is
// the config surface's own ([configapi.RefuseManaged]) with this surface's
// remedy, since what a person changes there is the agent's Seat object.
func (s *Service) mayWriteAccess(w http.ResponseWriter, r *http.Request) bool {
	err := s.config.Authorize(attributionOf(r))
	var managed *configapi.ManagedError
	switch {
	case err == nil:
		return true
	case errors.As(err, &managed):
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeConfigManaged,
			httpjson.Detail{
				"detail":     "managed by the operator: change the Seat object",
				"managed_by": managed.Writers,
				"hint": "an agent's access is set on its Seat object, which the operator " +
					"renders into the company configuration; a change here would be " +
					"overwritten",
			})
	default:
		s.refuse(w, r, err, setup.Result{})
	}
	return false
}

// accessRead serves GET /setup/integrations/{kind}/access.
func (s *Service) accessRead(w http.ResponseWriter, r *http.Request) {
	kind, ok := accessKind(w, r)
	if !ok {
		return
	}
	q, ok := s.accessQuery(w, r, kind)
	if !ok {
		return
	}
	s.writeAccess(w, r, q)
}

// accessPlan serves POST /setup/integrations/{kind}/access/plan.
//
// A READ SENT AS A POST, because a draft is a body: it writes nothing, so it is
// the read verb and asks for no recent proof.
func (s *Service) accessPlan(w http.ResponseWriter, r *http.Request) {
	kind, ok := accessKind(w, r)
	if !ok {
		return
	}
	q, ok := s.accessQuery(w, r, kind)
	if !ok {
		return
	}
	draft, ok := readAccessDraft(w, r)
	if !ok {
		return
	}
	current, ok := s.accessCurrent(w, r, q)
	if !ok {
		return
	}
	if !checkDraft(w, kind, current, draft) {
		return
	}
	plan, err := s.access.Plan(r.Context(), q, seataccess.Draft{Seats: draft.Seats})
	if err != nil {
		s.accessFault(w, r, kind, "setup_access_unplanned", err)
		return
	}
	httpjson.Write(w, http.StatusOK, plan)
}

// accessSave serves PUT /setup/integrations/{kind}/access, and its check
// (`?dry_run=true`, ADR-0055).
//
// ONE REVISION, THEN A SYNC. The draft is written as one revision held to the
// revision the tab was read at, and the sync for this integration is started
// on it. A sync that cannot start is not a failed save: the revision is the
// company's now, and the reconcile loop takes it on its next pass.
//
// A RAISE TO AN ADMINISTRATION ROLE ASKS MORE (D10): once the body shows one,
// [authz.ActionSetupAccessRaise] is asked on top of the route's verb, which a
// machine token is refused. A check asks it of nobody, since it writes nothing;
// it answers `step_up` so the card says so before the person approves.
func (s *Service) accessSave(w http.ResponseWriter, r *http.Request) {
	kind, ok := accessKind(w, r)
	if !ok {
		return
	}
	check := authz.IsCheck(r, accessSavePolicy())
	// A MANAGED DOCUMENT IS REFUSED BEFORE ANYTHING ELSE, as every write
	// here is: see the package documentation.
	if !check && !s.mayWriteAccess(w, r) {
		return
	}
	q, ok := s.accessQuery(w, r, kind)
	if !ok {
		return
	}
	draft, ok := readAccessDraft(w, r)
	if !ok {
		return
	}
	current, ok := s.accessCurrent(w, r, q)
	if !ok {
		return
	}
	if !checkDraft(w, kind, current, draft) {
		return
	}
	edits, changed := draftEdits(current, draft)
	if len(edits) == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "the draft changes no agent's access on " + string(kind),
			"hint":   "change at least one agent's setting before saving",
		})
		return
	}
	plan, err := s.access.Plan(r.Context(), q, seataccess.Draft{Seats: draft.Seats})
	if err != nil {
		s.accessFault(w, r, kind, "setup_access_unplanned", err)
		return
	}
	if plan.StepUp && !check &&
		!authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionSetupAccessRaise}) {
		return
	}
	// THE REVISION THE TAB WAS READ AT, from the header a client forwards
	// or the answer's own `revision`; the fleet's revision this request
	// read where the caller named none, so the draft lands on the document
	// it was judged against or is refused as stale.
	expect := strings.TrimSpace(r.Header.Get("If-Match"))
	if expect == "" {
		expect = draft.Revision
	}
	if expect == "" {
		expect = q.Revision
	}
	req := configapi.ApplySeatsRequest{
		Seats: edits, Summary: accessSummary(kind, changed, draft.Summary),
		By: attributionOf(r), Expect: expect,
	}
	if check {
		checked, checkErr := s.config.CheckSeats(r.Context(), req)
		if checkErr != nil {
			s.refuse(w, r, checkErr, setup.Result{})
			return
		}
		httpjson.Write(w, http.StatusOK, accessCheck{PlanAnswer: plan,
			Warnings: checked.Warnings})
		return
	}
	applied, err := s.config.ApplySeats(r.Context(), req)
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	// HANDLES AND LANES, NEVER VALUES: a seat's access is not a secret, and
	// the log says which seats a revision changed so an operator can find it.
	log.InfoContext(r.Context(), "setup_access_written",
		"kind", kind, "revision", applied.RevisionID, "epoch", applied.Epoch,
		"seats", strings.Join(changedHandles(changed), ","), "step_up", plan.StepUp,
		"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
	s.startSync(r, AccessSync{Kind: kind, Revision: applied.RevisionID, By: attributionOf(r),
		Handles: changedHandles(changed), AfterSave: true})
	after, ok := s.accessQuery(w, r, kind)
	if !ok {
		return
	}
	s.writeAccess(w, r, after)
}

// accessInstall serves
// POST /setup/integrations/slack/access/seats/{handle}/{lane}/install: a fresh
// install link for one agent's Slack app, minted on request.
//
// ONLY WHILE THE SEAT WAITS FOR ITS INSTALL, by Slack's own rule (its app is
// recorded and its last sync stopped at the install only a person approves):
// anything else is refused, so a link is never handed out for an app nobody is
// installing.
// A deployment whose document the operator writes still mints one: the link
// writes nothing to the document.
func (s *Service) accessInstall(w http.ResponseWriter, r *http.Request) {
	lane := seataccess.Lane(r.PathValue("lane"))
	handle := r.PathValue("handle")
	if lane != seataccess.LaneSlack {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSeatAccess, map[string]string{
			"detail": "slack grants no lane " + string(lane),
			"hint":   "one of slack",
		})
		return
	}
	q, ok := s.accessQuery(w, r, integration.KindSlack)
	if !ok {
		return
	}
	current, ok := s.accessCurrent(w, r, q)
	if !ok {
		return
	}
	seat, found := seatOf(current, handle)
	if !found || seat.Departed {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchSeat, map[string]string{
			"detail": "no agent seat " + handle + " on slack",
		})
		return
	}
	url, err := s.access.InstallLink(r.Context(), q, seat.Handle)
	var refusal *AccessRefusal
	switch {
	case errors.Is(err, ErrNotAwaitingInstall):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeInvalidInput, map[string]string{
			"detail": "this agent is not waiting for an install",
			"hint": "an install link is offered while the agent is on in Slack and its app " +
				"waits for a person to approve the install",
		})
		return
	case errors.As(err, &refusal) && refusal.RetryAfter > 0:
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
			int(math.Ceil(refusal.RetryAfter.Seconds())),
			httpjson.Detail{"detail": refusal.Reason, "hint": refusal.Remedy})
		return
	case errors.As(err, &refusal):
		httpjson.FailWith(w, refusal.Status, httpjson.CodeInvalidInput, map[string]string{
			"detail": refusal.Reason, "hint": refusal.Remedy,
		})
		return
	case err != nil:
		s.accessFault(w, r, q.Kind, "setup_access_install_unminted", err)
		return
	}
	// THE LINK IS A BEARER CREDENTIAL until it is spent or expires: the log
	// names the seat and who asked, never the link.
	log.InfoContext(r.Context(), "setup_access_install_minted", "seat", seat.Handle,
		"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
	w.Header().Set("Cache-Control", "no-store")
	httpjson.Write(w, http.StatusOK, seataccess.InstallLink{URL: url})
}

// accessSavePolicy is the save route's policy, declared once so the mount and
// the handler's own check read one value.
//
// IT NEVER CREATES WHAT ITS ADDRESS NAMES: the address is an integration, and
// an integration is connected through its own setup, never by saving its
// agents. A kind that manages no account per agent is refused with
// [accessElsewhere], the sentence a preview that finds nothing there is told.
func accessSavePolicy() authz.Policy {
	return authz.Policy{Action: authz.ActionSetupAccessWrite}.Checked().
		WhenAbsent(authz.AddedElsewhere(accessElsewhere))
}

// accessElsewhere is where an integration's agents are reached from when the
// address names no integration that manages an account per agent: what the
// route refuses such a kind with, and what it declares ([authz.Absent]).
const accessElsewhere = "an agent's access is set on an integration that manages one " +
	"account per agent (slack, mattermost, atlassian for Jira and Confluence, github, " +
	"gitlab, datadog); connect one with POST /setup/integrations/{kind}/inputs first"

// accessAct serves POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}.
//
// Every action but `keep` is an ask carried by a sync, which none of them can
// come from otherwise: a rotate, a remove, a turn back on, a retry and a
// re-read after a person's step at the vendor. `keep` writes the vendor's state
// into the document as a revision, so nothing is restored.
func (s *Service) accessAct(w http.ResponseWriter, r *http.Request) {
	kind, ok := accessKind(w, r)
	if !ok {
		return
	}
	action := seataccess.SeatAction(r.PathValue("action"))
	lane := seataccess.Lane(r.PathValue("lane"))
	handle := r.PathValue("handle")
	switch {
	case !action.Valid():
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "no seat action " + string(action),
			"hint":   "one of rotate, remove, keep, restore, retry, done",
		})
		return
	case !slices.Contains(seataccess.LanesOf(kind), lane):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSeatAccess, map[string]string{
			"detail": string(kind) + " grants no lane " + string(lane),
			"hint":   "one of " + laneList(kind),
		})
		return
	}
	// A ROTATE SEALS A FRESH CREDENTIAL, so it is a credential write as a
	// provisioning pass is, asked before anything is minted.
	if action == seataccess.ActionRotate && !s.mayWriteCredentials(w, r) {
		return
	}
	q, ok := s.accessQuery(w, r, kind)
	if !ok {
		return
	}
	current, ok := s.accessCurrent(w, r, q)
	if !ok {
		return
	}
	seat, found := seatOf(current, handle)
	if !found {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchSeat, map[string]string{
			"detail": "no agent seat " + handle + " on " + string(kind),
		})
		return
	}
	held := seat.Lanes[lane]
	if action == seataccess.ActionKeep {
		s.keepVendorSetting(w, r, q, seat, lane, held)
		return
	}
	var ask seataccess.Ask
	switch action {
	case seataccess.ActionRotate:
		// A CREDENTIAL HANDED IN FROM OUTSIDE CREWLET is the outside's to
		// rotate: the engine binds it and never writes it.
		if external(current, lane, held) {
			httpjson.FailWith(w, http.StatusConflict, httpjson.CodeCredentialExternal,
				map[string]string{
					"detail": seat.Handle + "'s " + string(lane) + " credential was " +
						"provided from outside Crewlet",
					"hint": "rotate it where it was issued, then write the new value to " +
						"the variable the seat names",
				})
			return
		}
		ask.Rotate = true
	case seataccess.ActionRemove:
		// OFF FIRST (D1): remove is a separate, confirmed action, allowed
		// only once the seat is off here. A seat that left the document is
		// off by definition.
		if !seat.Departed && held.Desired.Enabled {
			httpjson.FailWith(w, http.StatusConflict, httpjson.CodeSeatStillOn, map[string]string{
				"detail": seataccess.ErrStillOn.Error(),
				"hint":   "turn the agent off on this integration and save, then remove its account",
			})
			return
		}
		ask.Remove = true
	case seataccess.ActionRestore:
		ask.Reenable = true
	}
	// A RETRY AND A RE-READ ASK NOTHING: they run the sync for the one seat.
	var asks []SeatAsk
	if !ask.Zero() {
		ask.Lane = lane
		asks = []SeatAsk{{Handle: seat.Handle, Lane: lane, Ask: ask}}
	}
	err := s.access.Sync(r.Context(), AccessSync{Kind: kind, Revision: q.Revision,
		Asks: asks, Handles: []string{seat.Handle}, By: attributionOf(r)})
	if errors.Is(err, seataccess.ErrStillOn) {
		// ANOTHER LANE OF THE SAME ACCOUNT IS STILL ON: an Atlassian
		// account serves Jira and Confluence both, and is removed only once
		// neither is on.
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeSeatStillOn, map[string]string{
			"detail": seataccess.ErrStillOn.Error(),
			"hint":   "turn the agent off on every lane of this account and save, then remove it",
		})
		return
	}
	if errors.Is(err, ErrAccessSyncInFlight) {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodePassInFlight, map[string]string{
			"hint": "a sync of this integration's agents is running; try again when it finishes",
		})
		return
	}
	if err != nil {
		s.accessFault(w, r, kind, "setup_access_unsynced", err)
		return
	}
	log.InfoContext(r.Context(), "setup_access_asked",
		"kind", kind, "seat", seat.Handle, "lane", lane, "action", action,
		"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
	s.writeAccess(w, r, q)
}

// keepVendorSetting is the `keep` action: the vendor's state for one seat's
// lane written into the document as a revision ("Keep the vendor's setting"
// after drift, D2; "Keep off" after an account was disabled at the vendor, D3),
// so the next sync finds nothing to restore.
func (s *Service) keepVendorSetting(w http.ResponseWriter, r *http.Request, q AccessQuery,
	seat seataccess.SeatAnswer, lane seataccess.Lane, held seataccess.LaneAnswer) {

	if !s.mayWriteAccess(w, r) {
		return
	}
	if seat.Departed || !held.Observed.Exists {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeInvalidInput, map[string]string{
			"detail": "the vendor shows no account for " + seat.Handle + " on " + string(lane) +
				", so there is no setting of its to keep",
		})
		return
	}
	kept := held.Observed.Access()
	expect := strings.TrimSpace(r.Header.Get("If-Match"))
	if expect == "" {
		expect = q.Revision
	}
	change := accessChange{handle: seat.Handle, name: seat.Name, lane: lane,
		before: held.Desired, after: kept}
	applied, err := s.config.ApplySeats(r.Context(), configapi.ApplySeatsRequest{
		Seats: []configapi.SeatEdit{{Handle: seat.Handle, Edit: func(role *config.Role) error {
			return seataccess.SetDesired(role, lane, kept)
		}}},
		Summary: accessSummary(q.Kind, []accessChange{change},
			"keep the vendor's setting"),
		By: attributionOf(r), Expect: expect,
	})
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	log.InfoContext(r.Context(), "setup_access_kept",
		"kind", q.Kind, "seat", seat.Handle, "lane", lane, "revision", applied.RevisionID,
		"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
	after, ok := s.accessQuery(w, r, q.Kind)
	if !ok {
		return
	}
	s.writeAccess(w, r, after)
}

// startSync asks the engine to sync after a save, and logs rather than refuses
// when it cannot: the revision is the company's either way, and the reconcile
// loop takes it on its next pass.
func (s *Service) startSync(r *http.Request, req AccessSync) {
	if err := s.access.Sync(context.WithoutCancel(r.Context()), req); err != nil {
		log.WarnContext(r.Context(), "setup_access_sync_deferred",
			"kind", req.Kind, "revision", req.Revision, "error", err.Error(),
			"detail", "the save landed; the reconcile loop syncs it on its next pass")
	}
}

// accessCurrent is the answer a draft is judged against: who the seats are,
// what each lane holds and offers now.
func (s *Service) accessCurrent(w http.ResponseWriter, r *http.Request,
	q AccessQuery) (seataccess.Answer, bool) {

	answer, err := s.access.Answer(r.Context(), q)
	if err != nil {
		s.accessFault(w, r, q.Kind, "setup_access_unread", err)
		return seataccess.Answer{}, false
	}
	return answer, true
}

// accessFault answers an error from the engine's access half: a fault in this
// node or the store, never the caller's, so it is logged and answered as such.
func (s *Service) accessFault(w http.ResponseWriter, r *http.Request, kind integration.Kind,
	event string, err error) {

	log.ErrorContext(r.Context(), event, "kind", kind, "error", err.Error(),
		"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
	if errors.Is(err, ErrAccessUnavailable) {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
			httpjson.Detail{"reason": "the fleet's record of the agents' accounts could " +
				"not be read"})
		return
	}
	httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
}

// ErrAccessUnavailable reports the fleet's record of the agents' accounts as
// unreadable right now, which is transient: a 503 with a Retry-After rather
// than a 500. The engine's implementation wraps it.
var ErrAccessUnavailable = errors.New("setupapi: the agents' account record is unavailable")

// readAccessDraft reads and strictly decodes a plan or save body.
func readAccessDraft(w http.ResponseWriter, r *http.Request) (accessDraft, bool) {
	body, err := httpjson.ReadBody(w, r, MaxBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return accessDraft{}, false
	}
	var draft accessDraft
	if err := decode(body, &draft); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": err.Error(),
			"hint": "send {\"seats\": {\"<handle>\": {\"<lane>\": {\"enabled\": true, " +
				"\"level\": \"...\", \"permissions\": {...}, \"scope\": [...]}}}}",
		})
		return accessDraft{}, false
	}
	return draft, true
}

// checkDraft refuses a draft naming a seat the tab does not list, a lane the
// integration does not grant, or access the vendor does not offer now, and
// reports whether it may go on.
//
// WHAT THE VENDOR OFFERS NOW, from the same answer the tab drew its pickers
// from. Where a lane's levels could not be read live, the static list is all
// there is, and a live-listed id it does not hold is left to the document's
// rules and the sync's finding rather than refused on a guess.
func checkDraft(w http.ResponseWriter, kind integration.Kind, current seataccess.Answer,
	draft accessDraft) bool {

	if len(draft.Seats) == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "the draft names no agent",
		})
		return false
	}
	offers := map[seataccess.Lane]seataccess.Options{}
	for _, o := range current.Options {
		offers[o.Lane] = o
	}
	for _, handle := range sortedHandles(draft.Seats) {
		seat, found := seatOf(current, handle)
		if !found || seat.Departed {
			httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchSeat, map[string]string{
				"detail": "no agent seat " + handle + " to set " + string(kind) + " access for",
				"hint":   "a person's seat holds no agent account, and a seat that left the company is only removed",
			})
			return false
		}
		for _, lane := range sortedLanes(draft.Seats[handle]) {
			if !slices.Contains(seataccess.LanesOf(kind), lane) {
				httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeNoSeatAccess,
					map[string]string{
						"detail": string(kind) + " grants no lane " + string(lane),
						"hint":   "one of " + laneList(kind),
					})
				return false
			}
			if refusal := notOffered(kind, lane, offers[lane],
				draft.Seats[handle][lane]); refusal != nil {
				refusal["seat"] = handle
				refusal["lane"] = string(lane)
				httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeAccessNotOffered,
					refusal)
				return false
			}
		}
	}
	return true
}

// notOffered is why want is not something o offers on lane, as a refusal's
// detail, or nil where it is.
//
// A LEVEL IS JUDGED ON WHAT IS LISTED: a fixed set always, and a live-listed
// one only when it was read live just now. A live-listed lane the vendor did
// not answer for holds no list to judge by, and an id it does not hold is the
// document rules' and the sync's finding rather than a refusal on a guess.
func notOffered(kind integration.Kind, lane seataccess.Lane, o seataccess.Options,
	want seataccess.Desired) httpjson.Detail {

	if want.Level != "" && len(o.Levels) > 0 && (o.Live || !liveLevels(kind, lane)) {
		// A SET OF LEVELS where the lane takes one (Confluence's space
		// permissions by operation): every one of them must be offered.
		if _, listed := o.LevelsOf(want.Level); !listed {
			offered := make([]string, 0, len(o.Levels))
			for _, level := range o.Levels {
				offered = append(offered, level.ID+" ("+level.Name+")")
			}
			return httpjson.Detail{"detail": fmt.Sprintf("level %q is not one the vendor "+
				"lists", want.Level), "offered": offered}
		}
	}
	for key, grant := range want.Permissions {
		i := slices.IndexFunc(o.Permissions, func(p seataccess.PermissionSpec) bool {
			return p.Key == key
		})
		switch {
		case i < 0:
			keys := make([]string, 0, len(o.Permissions))
			for _, p := range o.Permissions {
				keys = append(keys, p.Key)
			}
			return httpjson.Detail{"detail": fmt.Sprintf("permission %q is not one an "+
				"agent's app can hold", key), "offered": keys}
		case !grant.Valid():
			return httpjson.Detail{"detail": fmt.Sprintf("permission %q is %q, and "+
				"GitHub grants read or write", key, grant)}
		case o.Permissions[i].WriteOnly && grant != seataccess.AccessWrite:
			return httpjson.Detail{"detail": fmt.Sprintf("GitHub grants %s as write "+
				"or not at all", o.Permissions[i].Name)}
		}
	}
	if !o.ScopeOpen && len(want.Scope) > 0 {
		for _, entry := range want.Scope {
			if !slices.Contains(o.Scope, strings.TrimSpace(entry)) {
				return httpjson.Detail{"detail": fmt.Sprintf("%q is outside what the "+
					"company's connection reaches", entry), "offered": o.Scope}
			}
		}
	}
	return nil
}

// liveLevels reports a lane whose levels the vendor lists live rather than
// from a fixed set ([seataccess.LaneSpec.LiveLevels]).
func liveLevels(kind integration.Kind, lane seataccess.Lane) bool {
	vendor, ok := seataccess.Lookup(kind)
	if !ok {
		return false
	}
	spec, ok := vendor.Catalog().Lane(lane)
	return ok && spec.LiveLevels
}

// external reports a lane whose credential was handed in from outside Crewlet:
// one the engine does not provision for the company, or a credential the
// ledger records as provided rather than minted.
func external(current seataccess.Answer, lane seataccess.Lane, held seataccess.LaneAnswer) bool {
	for _, o := range current.Options {
		if o.Lane == lane && o.Managed == seataccess.ManagedExternal {
			return true
		}
	}
	c := held.Observed.Credential
	return c != nil && c.Source == seataccess.CredentialExternal
}

// accessChange is one seat's lane a save changes.
type accessChange struct {
	handle, name  string
	lane          seataccess.Lane
	before, after seataccess.Desired
}

// draftEdits are the seat edits a draft makes, and the changes they are, for
// the history line. A lane the draft states exactly as the document holds it
// is not a change, so a draft that resends the whole tab writes only what
// moved.
func draftEdits(current seataccess.Answer, draft accessDraft) ([]configapi.SeatEdit,
	[]accessChange) {

	var edits []configapi.SeatEdit
	var changed []accessChange
	for _, handle := range sortedHandles(draft.Seats) {
		seat, _ := seatOf(current, handle)
		var lanes []seataccess.Lane
		for _, lane := range sortedLanes(draft.Seats[handle]) {
			before := seat.Lanes[lane].Desired
			after := draft.Seats[handle][lane].Normalized()
			if before.Enabled == after.Enabled && before.SameAccess(after) {
				continue
			}
			lanes = append(lanes, lane)
			changed = append(changed, accessChange{handle: handle, name: seat.Name,
				lane: lane, before: before, after: after})
		}
		if len(lanes) == 0 {
			continue
		}
		wants := draft.Seats[handle]
		edits = append(edits, configapi.SeatEdit{Handle: handle,
			Edit: func(role *config.Role) error {
				for _, lane := range lanes {
					if err := seataccess.SetDesired(role, lane, wants[lane].Normalized()); err != nil {
						return err
					}
				}
				return nil
			}})
	}
	return edits, changed
}

// accessSummary is the history line a save records: what it did to whom, in
// words a person reads in the revision list.
func accessSummary(kind integration.Kind, changed []accessChange, supplied string) string {
	parts := make([]string, 0, len(changed))
	for _, c := range changed {
		who := cmpOr(c.name, c.handle)
		switch {
		case c.after.Enabled && !c.before.Enabled:
			parts = append(parts, "turn on "+who+" on "+string(c.lane))
		case !c.after.Enabled && c.before.Enabled:
			parts = append(parts, "turn off "+who+" on "+string(c.lane))
		default:
			parts = append(parts, "change "+who+"'s "+string(c.lane)+" access")
		}
	}
	line := "agents' access on " + string(kind)
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	if supplied = strings.Join(strings.Fields(supplied), " "); supplied != "" {
		line += " (" + supplied + ")"
	}
	return line
}

func cmpOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// changedHandles are the seats a save changed, each once, in order.
func changedHandles(changed []accessChange) []string {
	var out []string
	for _, c := range changed {
		if !slices.Contains(out, c.handle) {
			out = append(out, c.handle)
		}
	}
	return out
}

// seatOf is one seat on an access answer.
func seatOf(answer seataccess.Answer, handle string) (seataccess.SeatAnswer, bool) {
	for _, seat := range answer.Seats {
		if seat.Handle == handle {
			return seat, true
		}
	}
	return seataccess.SeatAnswer{}, false
}

// laneList names the lanes one integration grants, for a refusal's hint.
func laneList(kind integration.Kind) string {
	lanes := seataccess.LanesOf(kind)
	names := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		names = append(names, string(lane))
	}
	return strings.Join(names, ", ")
}

func sortedHandles[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func sortedLanes[V any](m map[seataccess.Lane]V) []seataccess.Lane {
	out := make([]seataccess.Lane, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
