package setupapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// An agent's own identity at a vendor (ADR-0179):
//
//	POST   /setup/integrations/{kind}/access/seats/{handle}/{lane}/identity  check and keep one
//	DELETE /setup/integrations/{kind}/access/seats/{handle}/{lane}/identity  back to the company's
//
// Each writes the seat's `identity` as a revision where it changes, has the
// seat's sync carry the ask, and answers the check's verdict and the tab.
// Writing who signs is an administrator's at a fresh step-up, as a level is,
// and keeping a key is a credential write.

// Identities is the engine half of the identity routes, beside [Access].
type Identities interface {
	// Identity has the seat's sync keep or give up its identity on lane and
	// answers what the check read. [ErrNoIdentity] where the lane offers
	// none, [ErrNotAwaitingHandIn] where the agent is not on there.
	Identity(ctx context.Context, q AccessQuery, handle string, lane seataccess.Lane,
		ask seataccess.IdentityAsk, by iam.Actor) (seataccess.IdentityCheck, error)
}

// ErrNoIdentity refuses an identity for a lane whose vendor offers none.
var ErrNoIdentity = errors.New("setupapi: this integration offers no identity per agent")

// identityRequest is the POST body: how the identity was made, and what was
// handed in where a person made it.
type identityRequest struct {
	Way      seataccess.IdentityWay `json:"way"`
	Value    string                 `json:"value,omitempty"`
	FileName string                 `json:"file_name,omitempty"`
	ID       string                 `json:"id,omitempty"`
	// Detail is the optional value the guide asks for beside a file.
	Detail string `json:"detail,omitempty"`
}

// accessIdentity serves both identity routes.
func (s *Service) accessIdentity(w http.ResponseWriter, r *http.Request) {
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
	ids, ok := s.access.(Identities)
	if !ok {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSeatAccess, map[string]string{
			"detail": string(kind) + " offers no identity per agent on this node",
		})
		return
	}
	keep := r.Method == http.MethodPost
	ask := seataccess.IdentityAsk{Keep: keep, Drop: !keep}
	if keep {
		req, read := readIdentity(w, r)
		if !read {
			return
		}
		ask.Way, ask.Value, ask.FileName, ask.ID, ask.Detail = req.Way, req.Value, req.FileName, req.ID,
			strings.TrimSpace(req.Detail)
	}
	if !s.mayWriteAccess(w, r) || !s.mayWriteCredentials(w, r) ||
		!authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionSetupAccessRaise}) {
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
	la := seat.Lanes[lane]
	if !found || la.Identity == nil || !la.Identity.Offered {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSeatAccess, map[string]string{
			"detail": string(kind) + " offers " + handle + " no identity of its own",
		})
		return
	}
	if !keep && !sharesCompanyIdentity(kind, lane) {
		// ITS OWN BY CONSTRUCTION (a Google growth identity): there is no
		// company's to go back to, and turning the agent off gives it up.
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, map[string]string{
			"detail": string(kind) + " has no company identity for " + handle + " to go back to",
			"hint":   "turn the agent off on this lane to give its identity up",
		})
		return
	}
	mode := seataccess.IdentityCompany
	if keep {
		mode = seataccess.IdentityOwn
	}
	if la.Identity.Mode != mode {
		if q, ok = s.writeIdentity(w, r, q, current, handle, lane, mode); !ok {
			return
		}
	}
	check, err := ids.Identity(r.Context(), q, handle, lane, ask, attributionOf(r))
	if !s.identityRefused(w, r, kind, handle, err) {
		return
	}
	// THE AUDIT NAMES THE IDENTITY AND ITS VERDICT, never what was handed in.
	log.InfoContext(r.Context(), "setup_access_identity", "kind", kind, "seat", handle, "lane", lane,
		"keep", keep, "way", ask.Way, "identity", check.ID, "status", check.Status,
		"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
	answer, err := s.access.Answer(r.Context(), q)
	if err != nil {
		s.accessFault(w, r, kind, "setup_access_unread", err)
		return
	}
	if s.config.Managed() {
		answer.ManagedBy = seataccess.ManagedByOperator
	}
	w.Header().Set("ETag", `"`+answer.Revision+`"`)
	httpjson.Write(w, http.StatusOK, seataccess.CredentialAnswer{
		Verdict: IdentityVerdict(check, la.Identity.Guide), Access: answer})
}

// readIdentity reads and holds a POST body to its shape.
func readIdentity(w http.ResponseWriter, r *http.Request) (identityRequest, bool) {
	body, err := httpjson.ReadBody(w, r, MaxBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return identityRequest{}, false
	}
	var req identityRequest
	if err := decode(body, &req); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": plainjson.Unreadable("the body", err),
		})
		return req, false
	}
	problem, field := "", "value"
	switch {
	case !req.Way.Valid():
		problem, field = "way is handed_in, invited or made", "way"
	case req.Way == seataccess.WayHandedIn && strings.TrimSpace(req.Value) == "":
		problem = "Choose the file the vendor offered, or paste what it gave."
	case req.Way != seataccess.WayHandedIn && (req.Value != "" || req.Detail != ""):
		problem = "Only a handed-in identity carries a value."
	case envref.Has(req.Value):
		problem = "Hand in the value itself, not a ${VAR} reference: Crewlet seals it under its own name."
	}
	if problem != "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
			"detail": problem, "field": field,
		})
		return req, false
	}
	return req, true
}

// writeIdentity writes the seat's `identity` on lane as mode, one revision on
// the revision the tab was read at, and answers the company as it now runs.
func (s *Service) writeIdentity(w http.ResponseWriter, r *http.Request, q AccessQuery, current seataccess.Answer,
	handle string, lane seataccess.Lane, mode seataccess.IdentityMode) (AccessQuery, bool) {

	seat, _ := seatOf(current, handle)
	want := seat.Lanes[lane].Desired
	want.Identity = mode
	edits, changed := seatEdits(current, map[string]map[seataccess.Lane]seataccess.Desired{handle: {lane: want}})
	applied, err := s.config.ApplySeats(r.Context(), configapi.ApplySeatsRequest{
		Seats: edits, Summary: accessSummary(q.Kind, changed, ""), By: attributionOf(r), Expect: q.Revision,
	})
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return q, false
	}
	fc, err := s.readCompany(r.Context())
	if err != nil {
		s.accessFault(w, r, q.Kind, "setup_access_unread", err)
		return q, false
	}
	log.InfoContext(r.Context(), "setup_access_written", "kind", q.Kind, "revision", applied.RevisionID,
		"epoch", applied.Epoch, "seats", handle, "identity", mode, "by", attributionOf(r).Name,
		"operator", attributionOf(r).OperatorID)
	return AccessQuery{Kind: q.Kind, Company: fc.company, Roster: fc.roster, Revision: fc.revision}, true
}

// identityRefused answers err, true where there is none to answer.
func (s *Service) identityRefused(w http.ResponseWriter, r *http.Request, kind integration.Kind, handle string,
	err error) bool {
	var refusal *AccessRefusal
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrNoIdentity):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSeatAccess, map[string]string{
			"detail": "this integration offers no identity per agent",
		})
	case errors.Is(err, ErrNoSuchAccessSeat):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchSeat, map[string]string{
			"detail": "no agent seat " + handle,
		})
	case errors.Is(err, ErrNotAwaitingHandIn):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeInvalidInput, map[string]string{
			"detail": "this agent is off on this integration",
			"hint":   "turn the agent on and save, then set up its identity",
		})
	case errors.Is(err, ErrAccessSyncInFlight):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodePassInFlight, map[string]string{
			"hint": "a sync of this integration's agents is running; try again when it finishes",
		})
	case errors.As(err, &refusal):
		httpjson.FailWith(w, refusal.Status, httpjson.CodeInvalidInput, map[string]string{
			"detail": refusal.Reason, "hint": refusal.Remedy, "field": "value",
		})
	case errors.Is(err, ErrAccessUnavailable):
		s.accessFault(w, r, kind, "setup_access_identity_unchecked", err)
	case vendorRefusal(err) != nil:
		// THE VENDOR ANSWERED: its refusal is the person's to act on, never Crewlet's fault.
		said := vendorRefusal(err)
		log.InfoContext(r.Context(), "setup_access_identity_refused", "kind", kind, "seat", handle,
			"error", err.Error())
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeInvalidInput, map[string]string{
			"detail": said.Said, "hint": said.Remedy,
		})
		s.accessFault(w, r, kind, "setup_access_identity_unchecked", err)
	case vendorFault(err):
		log.WarnContext(r.Context(), "setup_access_identity_unchecked", "kind", kind, "seat", handle,
			"error", err.Error())
		httpjson.FailWith(w, http.StatusBadGateway, httpjson.CodeInvalidInput, map[string]string{
			"detail": "the vendor did not answer, so the identity was not checked. Nothing was kept.",
			"hint":   "try again in a moment",
		})
	default:
		log.ErrorContext(r.Context(), "setup_access_identity_unchecked", "kind", kind, "seat", handle,
			"error", err.Error())
		httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeInternalError, map[string]string{
			"detail": "Crewlet could not finish the check.",
			"hint": "try again; if it fails again, the engine's log line setup_access_identity_unchecked " +
				"says why",
		})
	}
	return false
}

// vendorUnavailable is a vendor's error that says whether the vendor failed
// to serve the call (a 5xx, a rate limit), declared by each vendor's own type.
type vendorUnavailable interface{ VendorUnavailable() bool }

// vendorRefusal is err as the vendor's definite refusal, nil where it is none.
func vendorRefusal(err error) *seataccess.Refusal {
	r, _ := seataccess.RefusalOf(err)
	return r
}

// vendorFault reports err as the vendor's: no answer (a network error or a
// deadline; the fleet's own store faults are answered before this) or an
// answer saying it is unavailable. Anything else is Crewlet's own.
func vendorFault(err error) bool {
	var netErr net.Error
	var unavailable vendorUnavailable
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) ||
		(errors.As(err, &unavailable) && unavailable.VendorUnavailable())
}

// IdentityVerdict is a check as the sheet's verdict reads it: what works, or
// what failed and what to do, with what each read found and, from guide's
// steps, the one a refused check stopped at.
func IdentityVerdict(c seataccess.IdentityCheck, guide *seataccess.IdentityGuide) setup.Verdict {
	v := setup.Verdict{Outcome: setup.VerifyRefused, Summary: c.Said}
	switch c.Status {
	case seataccess.IdentityReady, seataccess.IdentityUnchecked, seataccess.IdentityReplacing, seataccess.IdentityCompanys:
		v.Outcome = setup.VerifyWorks
	}
	if guide != nil {
		if v.Outcome == setup.VerifyRefused {
			v.FailedAt = failedAt(c.Reads, guide.Checks)
		}
		for _, read := range c.Reads {
			if id := checkID(read, guide.Checks); id != "" && (read.Said != "" || read.Detail != "") {
				v.Steps = append(v.Steps, setup.StepNote{ID: id, Said: read.Said, Detail: read.Detail})
			}
		}
	}
	if v.Summary == "" {
		v.Summary = c.Status.Label() + "."
	}
	for _, f := range []setup.Fact{{Label: "Identity", Value: c.ID, Identifier: true},
		{Label: "Role found", Value: c.Role}, {Label: "Role needed", Value: c.Needs},
		{Label: "Team", Value: c.Team, Identifier: true}} {
		if f.Value != "" {
			v.Facts = append(v.Facts, f)
		}
	}
	// A READ THE GUIDE LISTS IS SAID ON ITS STEP (Steps); only one it does not
	// list is a fact, so nothing is said twice.
	for _, read := range c.Reads {
		if guide != nil && checkID(read, guide.Checks) != "" {
			continue
		}
		value := setup.PartFailed
		if read.OK {
			value = setup.PartPassed
		}
		if read.Said != "" {
			value += ": " + strings.TrimSuffix(read.Said, ".")
		}
		v.Facts = append(v.Facts, setup.Fact{Label: read.Label, Value: value})
	}
	if c.Remedy != "" {
		v.Warnings = append(v.Warnings, setup.Warning{Action: c.Remedy})
	}
	if c.Blocked != "" {
		v.Warnings = append(v.Warnings, setup.Warning{Title: c.Blocked,
			Action: "The agent's calls stay on the company's identity until the vendor allows one more."})
	}
	return v
}

// failedAt is the step a refused check stopped at: the first read that
// failed, or the first step where it read nothing at all (the vendor refused
// before any step passed); "" where every read passed.
func failedAt(reads []seataccess.IdentityRead, checks []seataccess.GuideCheck) string {
	if len(reads) == 0 && len(checks) > 0 {
		return checks[0].ID
	}
	for _, read := range reads {
		if !read.OK {
			return checkID(read, checks)
		}
	}
	return ""
}

// checkID is the step read reports on, "" where the guide lists none: a
// read carries its step's label, as the sheet lists it.
func checkID(read seataccess.IdentityRead, checks []seataccess.GuideCheck) string {
	for _, c := range checks {
		if c.Label == read.Label {
			return c.ID
		}
	}
	return ""
}

// sharesCompanyIdentity reports whether kind's lane has a company identity a
// seat may go back to.
func sharesCompanyIdentity(kind integration.Kind, lane seataccess.Lane) bool {
	v, ok := seataccess.Lookup(kind)
	if !ok {
		return false
	}
	spec, ok := v.Catalog().Lane(lane)
	return ok && spec.Identity.Company
}
