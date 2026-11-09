package setupapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
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

	// HandIn checks a credential a person created at the vendor and pasted
	// for one agent's lane, then has the seat's sync seal it. [ErrNoHandIn]
	// where the vendor takes no pasted credential, [ErrNotAwaitingHandIn]
	// where the agent is not on there, an [*AccessRefusal] where the check
	// refuses the value, and [ErrAccessSyncInFlight] while a sync runs.
	HandIn(ctx context.Context, q AccessQuery, handle string, lane seataccess.Lane,
		value string, by iam.Actor) (seataccess.HandedIn, error)
}

// ErrNotAwaitingInstall refuses an install link for an agent whose Slack app is
// not waiting for a person to approve its install. The engine's implementation
// wraps it.
var ErrNotAwaitingInstall = errors.New("setupapi: this agent's app is not waiting for an install")

// ErrNoHandIn refuses a pasted credential for a vendor whose credentials the
// engine mints or a person's install hands over.
var ErrNoHandIn = errors.New("setupapi: this integration takes no pasted credential")

// ErrNotAwaitingHandIn refuses a pasted credential for an agent that is not on
// at the integration.
var ErrNotAwaitingHandIn = errors.New("setupapi: this agent is not waiting for a credential")

// ErrNoSuchAccessSeat refuses a pasted credential for a handle that is no
// agent seat of the company.
var ErrNoSuchAccessSeat = errors.New("setupapi: no such agent seat")

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

	// Then runs once the sync has finished, whatever it concluded, while the
	// integration still reads as syncing: the check route's own pass, which
	// observes the whole integration again and records what it found
	// ([Service.recheck]). Nil runs nothing.
	//
	// A SYNC ANSWERS FOR THE SEATS IT TOUCHED and records nothing else. The
	// integration's status row (its issues, its notes, its card's phase) is
	// written only by a pass, and the pass a save's revision brings forward
	// loses the surface's lease to the very sync the save started, so the
	// row stayed as the last pass left it until somebody pressed Check now.
	// Not taken where the sync never ran (another writer held the surface,
	// the store could not say, the ask was refused): that writer's own pass
	// records, or nothing changed.
	Then func(ctx context.Context)
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
	s.writeAnswer(w, answer)
}

// writeAnswer writes an access answer with its revision as the entity tag.
func (s *Service) writeAnswer(w http.ResponseWriter, answer seataccess.Answer) {
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
	if !checkDraft(w, kind, q.Company, current, draft) {
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
	// here is, and so is its check: a check that answered a plan for a save
	// that would be refused is the one thing a check must never do.
	if !s.mayWriteAccess(w, r) {
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
	if !checkDraft(w, kind, q.Company, current, draft) {
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
	stated := strings.TrimSpace(r.Header.Get("If-Match"))
	expect := stated
	if expect == "" {
		expect = draft.Revision
	}
	if expect == "" {
		expect = q.Revision
	}
	// A HEADER IS A PRECONDITION, a body field the domain's base: a save that
	// loses to a revision its If-Match named is 412, one built on the body's
	// or the read's revision is 409 (RFC 9110 §13.1.1).
	req := configapi.ApplySeatsRequest{
		Seats: edits, Summary: accessSummary(kind, changed, draft.Summary),
		By: attributionOf(r), Expect: expect, Stated: stated != "",
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
	s.writeSaved(w, r, current, changed, applied.RevisionID)
}

// writeSaved answers a save that landed: the tab read again at the revision it
// wrote, or, where that read fails, the tab as it was read before the save with
// the saved settings in place, marked as syncing. NEVER A FAILURE: the revision
// is the company's now, and an error here would have the person save again.
func (s *Service) writeSaved(w http.ResponseWriter, r *http.Request,
	before seataccess.Answer, changed []accessChange, revision string) {

	fc, err := s.readCompany(r.Context())
	if err == nil {
		q := AccessQuery{Kind: before.Kind, Company: fc.company, Roster: fc.roster,
			Revision: fc.revision}
		answer, answerErr := s.access.Answer(r.Context(), q)
		if answerErr == nil {
			s.writeAnswer(w, answer)
			return
		}
		err = answerErr
	}
	log.WarnContext(r.Context(), "setup_access_reread_failed", "kind", before.Kind,
		"revision", revision, "error", err.Error(),
		"detail", "the save landed; the answer is the tab as read before it, with the saved settings")
	saved := before
	saved.Revision, saved.Syncing = revision, true
	saved.Seats = slices.Clone(before.Seats)
	for i, seat := range saved.Seats {
		lanes := maps.Clone(seat.Lanes)
		for _, c := range changed {
			if c.handle != seat.Handle {
				continue
			}
			la := lanes[c.lane]
			la.Desired, la.Plan = c.after, []seataccess.Step{}
			lanes[c.lane] = la
		}
		saved.Seats[i].Lanes = lanes
	}
	s.writeAnswer(w, saved)
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

// accessCredential serves POST
// /setup/integrations/{kind}/access/seats/{handle}/{lane}/credential: a
// credential a person created at the vendor for one agent, checked there and
// then sealed by the seat's sync. The value is never logged or echoed.
func (s *Service) accessCredential(w http.ResponseWriter, r *http.Request) {
	kind, ok := accessKind(w, r)
	if !ok {
		return
	}
	lane := seataccess.Lane(r.PathValue("lane"))
	handle := r.PathValue("handle")
	if !slices.Contains(seataccess.LanesOf(kind), lane) {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSeatAccess, map[string]string{
			"detail": string(kind) + " grants no lane " + string(lane),
			"hint":   "one of " + laneList(kind),
		})
		return
	}
	// A PASTE SEALS A CREDENTIAL, so it is the credential write a rotate is.
	if !s.mayWriteCredentials(w, r) {
		return
	}
	body, err := httpjson.ReadBody(w, r, MaxBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := decode(body, &req); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": plainjson.Unreadable("the body", err),
		})
		return
	}
	if problem := handInProblem(req.Value); problem != "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
			"detail": problem, "field": "value",
		})
		return
	}
	q, ok := s.accessQuery(w, r, kind)
	if !ok {
		return
	}
	handed, err := s.access.HandIn(r.Context(), q, handle, lane, req.Value, attributionOf(r))
	var refusal *AccessRefusal
	switch {
	case errors.Is(err, ErrNoHandIn):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSeatAccess, map[string]string{
			"detail": string(kind) + " takes no pasted credential",
		})
		return
	case errors.Is(err, ErrNoSuchAccessSeat):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchSeat, map[string]string{
			"detail": "no agent seat " + handle + " on " + string(kind),
		})
		return
	case errors.Is(err, ErrNotAwaitingHandIn):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeInvalidInput, map[string]string{
			"detail": "this agent is not waiting for a token",
			"hint":   "turn the agent on for " + string(kind) + " and save, then paste its token",
		})
		return
	case errors.Is(err, ErrAccessSyncInFlight):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodePassInFlight, map[string]string{
			"hint": "a sync of this integration's agents is running; try again when it finishes",
		})
		return
	case errors.As(err, &refusal):
		httpjson.FailWith(w, refusal.Status, httpjson.CodeInvalidInput, map[string]string{
			"detail": refusal.Reason, "hint": refusal.Remedy, "field": "value",
		})
		return
	case err != nil:
		s.accessFault(w, r, kind, "setup_access_credential_unchecked", err)
		return
	}
	// THE AUDIT NAMES THE ACCOUNT AND WHAT THE VENDOR SHOWS OF THE CREDENTIAL,
	// never the value.
	log.InfoContext(r.Context(), "setup_access_credential_sealed", "kind", kind, "seat", handle,
		"lane", lane, "rotate", handed.Rotate, "account", handed.Account, "account_id", handed.AccountID,
		"credential", handed.Label, "by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
	answer, err := s.access.Answer(r.Context(), q)
	if err != nil {
		s.accessFault(w, r, kind, "setup_access_unread", err)
		return
	}
	if s.config.Managed() {
		answer.ManagedBy = seataccess.ManagedByOperator
	}
	w.Header().Set("ETag", `"`+answer.Revision+`"`)
	httpjson.Write(w, http.StatusOK, seataccess.CredentialAnswer{Verdict: handed.Verdict, Access: answer})
}

// handInProblem is why a pasted value cannot be a credential, "" where it can
// be one: empty, a `${VAR}` rather than the value, or holding stray characters
// by the rule every pasted credential is held to.
func handInProblem(value string) string {
	switch {
	case strings.TrimSpace(value) == "":
		return "Paste the token to check."
	case envref.Has(value):
		return "Paste the token itself, not a ${VAR} reference: Crewlet seals it under its own name."
	default:
		return secrets.StrayProblem("token", value)
	}
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
	"gitlab, datadog, cloudflare, expo, digitalocean, scaleway, aws, google_cloud, ssh); connect one with POST /setup/integrations/{kind}/inputs first"

// accessAct serves POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}.
//
// Every action but `keep` is an ask carried by a sync, which none of them can
// come from otherwise: a rotate, a remove, a turn back on, a retry, a re-read
// after a person's step at the vendor, and a person keeping an account there. `keep` writes the vendor's state
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
			"hint":   "one of rotate, remove, keep, restore, retry, done, release",
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
		s.keepVendorSetting(w, r, q, current, seat, lane, held)
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
		if refusedOutside(w, current, kind, lane, seat.Handle) {
			return
		}
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
		if refusedOutside(w, current, kind, lane, seat.Handle) {
			return
		}
		ask.Reenable = true
	case seataccess.ActionRelease:
		// ONLY A LANE WHOSE ACCOUNT IS WORTH KEEPING, and only a step waiting
		// for its deletion: the person's choice, recorded below.
		if !offerOf(current, lane).Releasable {
			httpjson.FailWith(w, http.StatusConflict, httpjson.CodeInvalidInput, map[string]string{
				"detail": string(lane) + " keeps nothing at the vendor a person may choose to keep",
				"hint":   "delete the old credential at the vendor; the row clears once it stops working",
			})
			return
		}
		if sync := held.Sync; sync == nil || sync.Outcome != seataccess.OutcomeWaiting ||
			sync.Error == nil || sync.Error.Step != seataccess.VerbRemove {
			httpjson.FailWith(w, http.StatusConflict, httpjson.CodeInvalidInput, map[string]string{
				"detail": seat.Handle + " is not waiting for its account to be deleted at the vendor",
				"hint":   "keep an account only when the row asks you to delete it",
			})
			return
		}
		ask.Release = true
	}
	// A RETRY AND A RE-READ ASK NOTHING: they run the sync for the one seat.
	var asks []SeatAsk
	if !ask.Zero() {
		ask.Lane = lane
		asks = []SeatAsk{{Handle: seat.Handle, Lane: lane, Ask: ask}}
	}
	err := s.access.Sync(r.Context(), AccessSync{Kind: kind, Revision: q.Revision,
		Asks: asks, Handles: []string{seat.Handle}, By: attributionOf(r),
		Then: s.recheckAfter(kind)})
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
		"account", held.Observed.Account, "account_id", held.Observed.AccountID,
		"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
	s.writeAccess(w, r, q)
}

// keepVendorSetting is the `keep` action: the vendor's state for one seat's
// lane written into the document as a revision ("Keep the vendor's setting"
// after drift, D2; "Keep off" after an account was disabled at the vendor, D3),
// so the next sync finds nothing to restore.
func (s *Service) keepVendorSetting(w http.ResponseWriter, r *http.Request, q AccessQuery,
	current seataccess.Answer, seat seataccess.SeatAnswer, lane seataccess.Lane,
	held seataccess.LaneAnswer) {

	// THE SAVE'S OWN VERBS: keep writes the document as a save does, so it
	// is asked setup.access.write, and the raise on top where what the
	// vendor shows is an administration grant the document did not hold
	// (D10). The route's own verb is the one-off asks', which write nothing.
	if !authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionSetupAccessWrite}) ||
		!s.mayWriteAccess(w, r) {
		return
	}
	if seat.Departed || !held.Observed.Exists {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeInvalidInput, map[string]string{
			"detail": "the vendor shows no account for " + seat.Handle + " on " + string(lane) +
				", so there is no setting of its to keep",
		})
		return
	}
	kept, err := seataccess.KeptAccess(lane, held.Desired, held.Observed)
	if err != nil {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeInvalidInput, map[string]string{
			"detail": seat.Handle + ": " + err.Error(),
		})
		return
	}
	if seataccess.AsksStepUp(offerOf(current, lane), held.Desired, kept) &&
		!authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionSetupAccessRaise}) {
		return
	}
	stated := strings.TrimSpace(r.Header.Get("If-Match"))
	expect := stated
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
		By: attributionOf(r), Expect: expect, Stated: stated != "",
	})
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	log.InfoContext(r.Context(), "setup_access_kept",
		"kind", q.Kind, "seat", seat.Handle, "lane", lane, "revision", applied.RevisionID,
		"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
	// A KEEP IS A SAVE, and ends as one does: the sync on the revision it
	// wrote (which finds the vendor already matching) and then the check, so
	// the tab and the integration's status read the kept setting.
	s.startSync(r, AccessSync{Kind: q.Kind, Revision: applied.RevisionID, By: attributionOf(r),
		Handles: []string{seat.Handle}, AfterSave: true})
	s.writeSaved(w, r, current, []accessChange{change}, applied.RevisionID)
}

// offerOf is what lane offers on an access answer: an offer nobody read where
// the answer holds none, so a judgement on it fails closed.
func offerOf(answer seataccess.Answer, lane seataccess.Lane) seataccess.Options {
	for _, o := range answer.Options {
		if o.Lane == lane {
			return o
		}
	}
	return seataccess.UnreadOffers(answer.Kind, false)[lane]
}

// startSync asks the engine to sync after a save, and logs rather than refuses
// when it cannot: the revision is the company's either way, and the reconcile
// loop takes it on its next pass. The sync ends in the check, as every access
// sync does ([AccessSync.Then]).
func (s *Service) startSync(r *http.Request, req AccessSync) {
	req.Then = s.recheckAfter(req.Kind)
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
func checkDraft(w http.ResponseWriter, kind integration.Kind, company *config.Company,
	current seataccess.Answer, draft accessDraft) bool {

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
			if refusal := notOffered(kind, lane, offers[lane], within(company, lane),
				draft.Seats[handle][lane]); refusal != nil {
				refusal.Seat, refusal.Lane = handle, lane
				httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeAccessNotOffered,
					refusalFields(*refusal))
				return false
			}
		}
	}
	return true
}

// notOffered is why want is not something o offers on lane, as a refusal
// naming the part of the setting it is about, or nil where it is. under is the path every scope entry must sit
// under where the connection does not list its reach, empty where any entry
// the vendor accepts is within it ([within]).
//
// A LEVEL IS JUDGED ON WHAT IS LISTED: a fixed set always, and a live-listed
// one only when it was read live just now. A live-listed lane the vendor did
// not answer for holds no list to judge by, and an id it does not hold is the
// document rules' and the sync's finding rather than a refusal on a guess. A
// scope entry is judged the same way, only where the scope was read
// ([seataccess.Options.ScopeLive]); an empty scope where the lane requires one
// is refused whatever was read.
func notOffered(kind integration.Kind, lane seataccess.Lane, o seataccess.Options,
	under string, want seataccess.Desired) *seataccess.OfferRefusal {

	refuse := func(field seataccess.RefusalField, detail string) *seataccess.OfferRefusal {
		return &seataccess.OfferRefusal{Field: field, Detail: detail}
	}

	if want.Level != "" && len(o.Levels) > 0 && (o.Live || !liveLevels(kind, lane)) {
		// A SET OF LEVELS where the lane takes one (Confluence's space
		// permissions by operation): every one of them must be offered.
		if _, listed := o.LevelsOf(want.Level); !listed {
			offered := make([]string, 0, len(o.Levels))
			for _, level := range o.Levels {
				offered = append(offered, level.ID+" ("+level.Name+")")
			}
			r := refuse(seataccess.FieldLevel, fmt.Sprintf("level %q is not one the vendor lists", want.Level))
			r.Offered = offered
			return r
		}
	}
	if required, missing := o.MissingRequired(want.Level); missing {
		r := refuse(seataccess.FieldLevel, fmt.Sprintf("%s grants nothing else without %s "+
			"(%s); add it to the set", seataccess.LaneName(lane), required.Name, required.ID))
		r.Required = required.ID
		return r
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
			r := refuse(seataccess.FieldPermissions, fmt.Sprintf("permission %q is not one an "+
				"agent's app can hold", key))
			r.Offered = keys
			return r
		case !grant.Valid():
			return refuse(seataccess.FieldPermissions, fmt.Sprintf("permission %q is %q, and "+
				"GitHub grants read or write", key, grant))
		case o.Permissions[i].WriteOnly && grant != seataccess.AccessWrite:
			return refuse(seataccess.FieldPermissions, fmt.Sprintf("GitHub grants %s as write "+
				"or not at all", o.Permissions[i].Name))
		}
	}
	if o.ScopeRequired && want.Enabled && len(want.Scope) == 0 {
		return refuse(seataccess.FieldScope, fmt.Sprintf("an agent turned on at %s must have at least "+
			"one scope entry chosen, since one with none can reach nothing", seataccess.LaneName(lane)))
	}
	if o.ScopeLive {
		// A SCOPE IS JUDGED ON WHAT WAS READ, as a live-listed level is: an
		// unread list holds nothing to judge by, and the sync judges the
		// entry against the vendor instead of a refusal on a guess.
		for _, entry := range want.Scope {
			if !o.Reaches(entry) {
				r := refuse(seataccess.FieldScope, fmt.Sprintf("%q is outside what the "+
					"company's connection reaches", entry))
				r.Offered = o.Scope
				return r
			}
		}
	}
	if o.ScopeOpen && under != "" {
		prefix := strings.ToLower(strings.Trim(under, "/")) + "/"
		for _, entry := range want.Scope {
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry)), prefix) {
				return refuse(seataccess.FieldScope, fmt.Sprintf("%q is outside %s, the group "+
					"the company's connection reaches", entry, under))
			}
		}
	}
	return nil
}

// refusalFields is r as the refusal envelope's fields, under r's own JSON names
// (a test holds the two together), each optional one only where it is set.
func refusalFields(r seataccess.OfferRefusal) httpjson.Detail {
	out := httpjson.Detail{"seat": r.Seat, "lane": string(r.Lane), "field": string(r.Field), "detail": r.Detail}
	if len(r.Offered) > 0 {
		out["offered"] = r.Offered
	}
	if r.Required != "" {
		out["required"] = r.Required
	}
	return out
}

// within is the path a lane's scope entries must sit under where its
// connection does not list its reach: a GitLab project inside the group the
// company's connection joins agents to, since the engine grants a seat its
// projects as a member of that group. Empty for every other lane.
func within(company *config.Company, lane seataccess.Lane) string {
	if lane != seataccess.LaneGitLab || company == nil || company.Integrations.GitLab == nil ||
		company.Integrations.GitLab.Provisioning == nil {
		return ""
	}
	return strings.TrimSpace(company.Integrations.GitLab.Provisioning.Group)
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

// refusedOutside refuses a remove or a turn back on of an account managed
// outside Crewlet, and reports whether it did. The outside removes it or turns
// it back on: the engine opens no session there and records nothing, so an ask
// would change nothing while the card said it had.
func refusedOutside(w http.ResponseWriter, current seataccess.Answer, kind integration.Kind,
	lane seataccess.Lane, handle string) bool {

	if !managedOutside(current, lane) {
		return false
	}
	httpjson.FailWith(w, http.StatusConflict, httpjson.CodeAccessExternal, map[string]string{
		"detail": handle + "'s " + string(lane) + " account is managed outside Crewlet",
		"hint":   "remove it or turn it back on at " + seataccess.KindName(kind) + " itself",
	})
	return true
}

// managedOutside reports a lane the engine does not provision for the company
// ([seataccess.ManagedExternal]).
func managedOutside(current seataccess.Answer, lane seataccess.Lane) bool {
	for _, o := range current.Options {
		if o.Lane == lane && o.Managed == seataccess.ManagedExternal {
			return true
		}
	}
	return false
}

// external reports a lane whose credential was handed in from outside Crewlet:
// one the engine does not provision for the company, or a credential the
// ledger records as provided rather than minted.
func external(current seataccess.Answer, lane seataccess.Lane, held seataccess.LaneAnswer) bool {
	if managedOutside(current, lane) {
		return true
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

	return seatEdits(current, draft.Seats)
}

// DraftSeatEdits is the seat edits a save of seats writes, against current:
// what a plan measures a draft by, so its count follows the save's own rules.
func DraftSeatEdits(current seataccess.Answer,
	seats map[string]map[seataccess.Lane]seataccess.Desired) []configapi.SeatEdit {

	edits, _ := seatEdits(current, seats)
	return edits
}

// seatEdits is [draftEdits] over the draft's seats.
func seatEdits(current seataccess.Answer,
	seats map[string]map[seataccess.Lane]seataccess.Desired) ([]configapi.SeatEdit, []accessChange) {

	var edits []configapi.SeatEdit
	var changed []accessChange
	for _, handle := range sortedHandles(seats) {
		seat, _ := seatOf(current, handle)
		var lanes []seataccess.Lane
		for _, lane := range sortedLanes(seats[handle]) {
			before := seat.Lanes[lane].Desired
			after := seats[handle][lane].Normalized()
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
		wants := seats[handle]
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
	return words.Or(seataccess.LanesOf(kind))
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
