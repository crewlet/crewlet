package configapi

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// WHO MAY MAKE A WRITE: the admission every /config write path asks, between
// building the document it proposes and validating it.
//
// # Admin, or a lead inside their own subtree
//
// Admin may change anything. Anybody else may write only when EVERY change is
// inside a subtree they lead, judged on both sides: [config.DiffOrg] states
// the places each change reaches, each asked as `config.org.write` against the
// document it was read in (where an object was: the revision replaced; where
// it lands and what its new references name: the one proposed). Settings,
// credentials, a `${VAR}`, a key another system finds a seat or unit by and a
// seat role's grants (`health_tools`, deployment reads) are `config.write`.

// WIDENING AN MCP SERVER IS ADMIN'S TOO. A lead writes `mcp_limits` only to
// hold a server back from their teams: every limit a write lifts or loosens
// for an agent seat, by editing a block or moving a seat out from under one
// ([config.OrgDiff.Widened]), is `config.write`.
//
// A WRITE THAT CHANGES NOTHING IS NOT A LEAD'S: it still makes a revision and
// re-activates it on every node, the gesture of `POST /config/reload`, so
// anybody bound to a seat could force a fleet-wide rebuild with
// `{"vision": null}`.
//
// FROM THE TWO DOCUMENTS, NEVER THE RUNNING COMPANY, so a node behind on its
// applies decides a write as a current one does.

// ITS CHECK IS A LEAD'S, at the address of a seat or unit [Service.mayReach]
// admitted them to: a dry run stores nothing, and it is the one way a lead
// reads what the engine derives of their unit and the warnings their change is
// compared against. A check of the whole document is decided at no address,
// and would hand the hierarchy and every warning to anybody sent `{}`.

// # The charter, on its own verb
//
// A charter write (`PUT /config/charter`) is admitted on
// `config.principles.write` ONLY WHILE EVERY CHANGE IS A
// [config.CharterSettings] SETTING and no seat or unit is touched. It differs
// from `config.write` only in the Dashboard Assistant's column (ADR-0052), so
// it must never stretch further; a draft reaching past the charter is decided
// by the document's own rules, which the assistant is never admitted to.

// # What a refusal says
//
// THE ADDRESS IS DECIDED FIRST: a seat or unit route is decided where the id
// sits in the active revision (the root for an unknown id) before the id is
// looked up or the body decoded ([Service.mayReach]), so another team's seat,
// a root seat and a missing id are refused in the same bytes.
//
// The chart's SHAPE is no secret from anybody bound to a seat: a refusal names
// the place each refused part reaches, because that is what the caller is
// refused on and everybody's overview is served the chart whole. Only a read
// serves a seat's or unit's fields.

// THE ANSWER FOLDS EVERY REFUSED PART ([authz.Combine]): a bar on the caller
// names no permission and lists no part; a refusal on authority names the
// parts' shared reason or `no_grant`, the permission admitting every part, and
// each part's reason and grants; a lapsed proof alone is `403 step_up_required`.

// # Before validation, and before the seat-holder check
//
// A refused write is refused before the document is validated, so a refusal
// says what the caller may not change rather than what is wrong in parts they
// cannot read, and before the directory is asked who holds a seat the write
// removes, which a caller who could not remove it must not learn.
//
// # Not asked of the engine's own writes
//
// [Service.Apply], [Service.ApplyEntity] and [Service.Reload] are the node's
// own writers and carry no principal; only a request does ([draft.principal]).

// RefusedChange is one part of a write its caller may not make, as a refusal
// names it.
type RefusedChange struct {
	// Kind is `seat`, `unit`, `setting` for a key outside the org chart, or
	// `document` for a write that changes nothing.
	Kind string `json:"kind"`
	// ID is the seat's handle, the unit's key, or the setting's top-level
	// key; empty for the document.
	ID string `json:"id"`
	// Op is what the write does to the seat or unit: added, removed, moved
	// or changed.
	Op string `json:"op,omitempty"`
	// Side is which document the place was read in: `before` (the revision
	// replaced) or `after` (the one proposed).
	Side string `json:"side,omitempty"`
	// Place is the key of the unit the change reaches, empty for the
	// company root.
	Place string `json:"place"`
	// Why is what about the change reaches it — `place`, `self`, `lead`,
	// `manages`, `named`, `duplicate` — `credential` for a credential the
	// change sets, clears or alters, `key` for a key another system finds
	// the object by, `grant` for a grant the seat's role carries, `widens` for
	// an MCP limit the change lifts or loosens for the seat, or `unchanged` for
	// a write that changes nothing.
	Why string `json:"why,omitempty"`
	// Value is the reference, the id, the credential's path, the key's
	// field, or the limit as the seat is held to it
	// (`mcp_limits.<server>.enabled`, `mcp_limits.<server>.tools.<tool>`).
	Value string `json:"value,omitempty"`
	// Reason is the authority table's own reason for refusing it.
	Reason authz.Reason `json:"reason"`
	// Grants are the permissions any one of which would admit this part —
	// the part's own [authz.Decision.Grants], so each part names its own
	// remedy beside the write's.
	Grants []iam.Grant `json:"grants"`
}

// AdmissionError reports a write its caller may not make: the decision the
// whole write was refused by, and every part a permission would lift.
type AdmissionError struct {
	// Decision is the write's own, folded from every part refused
	// ([authz.Combine]): the rule that refused it and the permission that
	// would admit all of it.
	Decision authz.Decision

	// Refused are the parts refused on AUTHORITY ([authz.Reason.Authority])
	// — a part only a fresher proof would admit is admitted by one, and a
	// bar on the caller is about who is asking rather than any part — and
	// empty where the write was refused by a bar alone.
	Refused []RefusedChange
}

func (e *AdmissionError) Error() string {
	if len(e.Refused) == 0 {
		return "configapi: this change was refused (" + string(e.Decision.Reason) + ")"
	}
	parts := make([]string, 0, len(e.Refused))
	for _, r := range e.Refused {
		parts = append(parts, r.Kind+" "+r.ID)
	}
	return "configapi: this change reaches outside what you lead: " +
		strings.Join(parts, ", ")
}

// refusedPart is one part of a write and the decision it was refused by.
type refusedPart struct {
	change   RefusedChange
	decision authz.Decision
}

// refusal is the error for a write whose parts were refused: their decision
// folded into the write's own, and the parts a permission would lift listed.
func refusal(parts []refusedPart) *AdmissionError {
	decisions := make([]authz.Decision, 0, len(parts))
	var listed []RefusedChange
	for _, part := range parts {
		decisions = append(decisions, part.decision)
		if part.decision.Reason.Authority() {
			change := part.change
			change.Reason = part.decision.Reason
			change.Grants = slices.Clone(part.decision.Grants)
			if change.Grants == nil {
				change.Grants = []iam.Grant{}
			}
			listed = append(listed, change)
		}
	}
	decision := authz.Combine(decisions...)
	if !decision.Reason.Authority() {
		// A BAR ON THE CALLER, or a proof gone stale: no part is what
		// shut the write, so none is named as though a permission would
		// open it.
		listed = nil
	}
	return &AdmissionError{Decision: decision, Refused: listed}
}

// principalOf is who a request was resolved to, answering the request itself
// when there is nobody to decide for; ok is false when it has. Every route
// here is guarded, so a request reaching a handler carries one — and
// [auth.Caller] keeps the third answer apart: a node that could not tell who
// the caller is answers 503, where reading that as nobody refused a working
// credential as though it named no lead.
func principalOf(w http.ResponseWriter, r *http.Request) (*iam.Principal, bool) {
	p, ok := auth.Caller(w, r)
	return &p, ok
}

// admit refuses next when p may not turn prior into it — see the file's header.
// addressedCheck is a dry run of one seat or unit whose address the caller was
// admitted to; charter is a write of the charter alone.
//
// AT THE INSTANT THE ROUTE DECIDED ([authz.DecidedAt]): a proof that lapsed
// while the body was read would otherwise refuse every part as stale.
//
// THE ORG CHART'S VERB IS ASKED OF THE WHOLE DOCUMENT TOO, beside
// `config.write`, when the write changes the chart: it bars an agent's seat
// whatever it holds ([authz.ActionOrgWrite] is human-only), while entity routes
// outside the chart decide only `config.write`, which bars nobody.
func admit(ctx context.Context, p iam.Principal, prior, next *config.Company,
	addressedCheck, charter bool) error {

	at := authz.DecidedAt(ctx)
	if charter {
		if diff := config.DiffOrg(prior, next); charterOnly(diff) {
			d := authz.Decide(ctx, p, authz.ActionPrinciplesWrite,
				authz.Object{Kind: authz.KindCompany}, authz.NoChart{}, at)
			if d.Allowed {
				return nil
			}
			refused := make([]RefusedChange, 0, len(diff.Settings))
			for _, key := range diff.Settings {
				refused = append(refused, RefusedChange{Kind: "setting", ID: key,
					Reason: d.Reason})
			}
			return &AdmissionError{Refused: refused}
		}
	}
	company := authz.Decide(ctx, p, authz.ActionConfigWrite,
		authz.Object{Kind: authz.KindCompany}, authz.NoChart{}, at)
	diff := config.DiffOrg(prior, next)
	if len(diff.Changes) == 0 && len(diff.Settings) == 0 {
		if company.Allowed || addressedCheck {
			return nil
		}
		return refusal([]refusedPart{{change: RefusedChange{Kind: "document",
			Why: "unchanged"}, decision: company}})
	}
	if company.Allowed {
		if len(diff.Changes) == 0 {
			return nil
		}
		// AT THE ROOT, where the admin path is asked before any chart and
		// a refusal of the caller is a bar about who they are.
		chart := authz.Decide(ctx, p, authz.ActionOrgWrite,
			authz.Object{Kind: authz.KindUnit}, authz.NoChart{}, at)
		if chart.Allowed {
			return nil
		}
		return &AdmissionError{Decision: authz.Combine(company, chart)}
	}
	charts := map[config.OrgSide]authz.Chart{
		config.OrgBefore: orgchart.Of(diff.Before),
		config.OrgAfter:  orgchart.Of(diff.After),
	}
	var refused []refusedPart
	for _, key := range diff.Settings {
		refused = append(refused, refusedPart{change: RefusedChange{Kind: "setting",
			ID: key}, decision: company})
	}
	for _, w := range diff.Widened {
		refused = append(refused, refusedPart{change: RefusedChange{Kind: string(config.OrgSeat),
			ID: w.Seat, Why: "widens", Value: w.Field()}, decision: company})
	}
	for _, change := range diff.Changes {
		for _, path := range change.Credentials {
			refused = append(refused, refusedPart{change: RefusedChange{
				Kind: string(change.Kind), ID: change.ID, Op: string(change.Op),
				Why: "credential", Value: path}, decision: company})
		}
		for _, field := range change.Keys {
			refused = append(refused, refusedPart{change: RefusedChange{
				Kind: string(change.Kind), ID: change.ID, Op: string(change.Op),
				Why: "key", Value: field}, decision: company})
		}
		for _, field := range change.Grants {
			refused = append(refused, refusedPart{change: RefusedChange{
				Kind: string(change.Kind), ID: change.ID, Op: string(change.Op),
				Why: "grant", Value: field}, decision: company})
		}
		for _, touch := range change.Touches {
			d := authz.Decide(ctx, p, authz.ActionOrgWrite,
				authz.Object{Kind: authz.KindUnit, ID: change.ID, Container: touch.Unit},
				charts[touch.Side], at)
			if d.Unknown() {
				// A FAULT, NOT A WAIT: both trees are in hand, so nothing
				// a retry brings could answer what these could not.
				return fmt.Errorf("configapi: decide %s %s at %q: %w",
					change.Kind, change.ID, touch.Unit, d.Err)
			}
			if !d.Allowed {
				refused = append(refused, refusedPart{change: RefusedChange{
					Kind: string(change.Kind), ID: change.ID, Op: string(change.Op),
					Side: string(touch.Side), Place: touch.Unit, Why: touch.Why,
					Value: touch.Value}, decision: d})
			}
		}
	}
	if len(refused) > 0 {
		return refusal(refused)
	}
	return nil
}

// charterOnly reports a diff that changes the charter and nothing else: at
// least one of [config.CharterSettings], no other setting, and no seat or unit.
func charterOnly(diff config.OrgDiff) bool {
	if len(diff.Changes) > 0 || len(diff.Settings) == 0 {
		return false
	}
	for _, key := range diff.Settings {
		if !slices.Contains(config.CharterSettings, key) {
			return false
		}
	}
	return true
}

// refuseAdmission answers a write its caller may not make by the rule that
// refused it, in the status and code the authority table's own mapping gives
// that rule ([authz.RefusalFields]) — and, for a refusal on authority, every
// part a permission would lift.
//
// FROM THE PARTS' OWN DECISIONS, never a permission written here: it named
// admin beside the first part's reason whatever had decided, so an agent's
// seat was told it needed admin, a write refused for a lead relation and for
// a credential named the relation alone, and a proof that had lapsed was a
// list of parts each "needing" admin rather than the `403 step_up_required`
// a client re-proves and replays on.
func refuseAdmission(w http.ResponseWriter, err *AdmissionError) {
	status, code, detail := authz.RefusalFields(err.Decision)
	if code == httpjson.CodeUnauthorized && len(err.Refused) > 0 {
		detail["refused"] = err.Refused
		detail["hint"] = "Leads can change seats and units in their own units " +
			"only, and can hold an MCP server back from them but never give one " +
			"back. Settings, credentials, ${VAR} references, projects, spaces, " +
			"channels, addresses, contact ids, a seat's health_tools grant and " +
			"lifting an MCP limit need admin."
	}
	httpjson.FailWithFields(w, status, code, detail)
}

// mayReach decides a — a read or a write of one seat or unit of the active
// document — answering the refusal itself; ok is false when the request has
// been answered. A seat is decided where it sits and a unit as itself, so a
// lead reads and writes what they lead and nothing else; and it is decided
// BEFORE anything looks the id up, so an id outside the caller's subtree and
// one the revision does not hold are refused in the same bytes.
func (s *Service) mayReach(w http.ResponseWriter, r *http.Request, a authz.Action,
	company *config.Company, kind, id string) (ok bool) {

	o, err := company.Organization()
	if err != nil {
		s.fail(w, r, "build the organization the request is decided on", err)
		return false
	}
	p, ok := principalOf(w, r)
	if !ok {
		return false
	}
	d := authz.Decide(r.Context(), *p, a,
		authz.Object{Kind: authz.KindUnit, ID: id, Container: placeOf(o, kind, id)},
		orgchart.Of(o), authz.DecidedAt(r.Context()))
	if d.Unknown() || !d.Allowed {
		authz.EnvelopeRefusal(w, r, authz.Policy{Action: a}, d)
		return false
	}
	return true
}

// placeOf is the unit a request for one seat or unit is decided on: the unit
// a seat sits in, or a unit itself; empty for a seat at the root and for an
// id the revision does not hold, which is decided as the root is.
func placeOf(o *org.Organization, kind, id string) string {
	if kind == EntityUnits {
		if unit := o.Unit(id); unit != nil {
			return unit.Key()
		}
		return ""
	}
	if seat := o.Role(id); seat != nil {
		return o.UnitFor(seat).Key()
	}
	return ""
}
