package configapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

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
// # The company's grant, or a lead inside their own subtree
//
// Whoever holds `config:write` may change anything in the document. Anybody
// else may write only when EVERY change is inside a subtree they lead, judged
// on both sides of the write: [config.DiffOrg] states the places each change
// reaches, and each is asked of the authority table as `config.org.write`
// against the document it was read in — where an object was, against the
// revision being replaced; where it lands and what its new references name,
// against the revision proposed. A setting, a credential, a `${VAR}` in any
// field and a key another system finds a seat or unit by — its project, page
// space, channel, address or contact ids — are never a lead's: all four are
// asked as `config.write`, the company's grant.
//
// A WRITE THAT CHANGES NOTHING IS NOT A LEAD'S EITHER. Storing the document
// unchanged still makes a new revision and re-activates it on every node,
// rebuilding every seat's tools, providers and MCP children — the gesture
// `POST /config/reload` makes, which is `config:write`'s. Admitted because no
// part of it was refused, anybody bound to a seat could force that fleet-wide
// rebuild at will with `{"vision": null}`.
//
// FROM THE TWO DOCUMENTS, NEVER THE RUNNING COMPANY, so a node behind on its
// applies decides a write exactly as a current one does.
//
// # What a refusal says
//
// A refused READ of one seat or unit says nothing about it: one in a team the
// caller does not lead, one at the root and an id the revision does not hold
// are refused in the same bytes, because a missing id is decided at the root
// before it is looked for and the root is refused as another team's unit is.
// A write naming a missing id is refused the same way, before it is called
// missing. Only the company's grant is told an id is not there.
//
// A refused WRITE says more, and that is the price of the rule rather than an
// oversight: each refused part names the place it reaches — the unit a named
// seat sits in — because that place is what the caller is refused on, and
// whether a reference is refused at all already depends on what sits outside
// their subtree. So anybody bound to a seat can learn the chart's SHAPE by
// naming things in a write of their own; never a seat's or a unit's fields,
// which only a read serves.
//
// # Before validation, and before the seat-holder check
//
// A write its caller may not make is refused before the document is validated,
// so a refusal says what the caller may not change rather than what is wrong
// with parts of the company they cannot read; and before the identity
// directory is asked who holds a seat the write removes, which is not the
// caller's to learn about a seat they could not have removed.
//
// # Not asked of the engine's own writes
//
// [Service.Apply], [Service.ApplyEntity] and [Service.Reload] are the node's
// own writers — the setup surface after its own grant check, the reconcile
// loop — and carry no principal; only a request does ([draft.principal]).

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
	// the object by, or `unchanged` for a write that changes nothing.
	Why string `json:"why,omitempty"`
	// Value is the reference, the id, the credential's path or the key's
	// field.
	Value string `json:"value,omitempty"`
	// Reason is the authority table's own reason for refusing it.
	Reason authz.Reason `json:"reason"`
}

// AdmissionError reports a write its caller may not make, naming every part.
type AdmissionError struct {
	Refused []RefusedChange
}

func (e *AdmissionError) Error() string {
	parts := make([]string, 0, len(e.Refused))
	for _, r := range e.Refused {
		parts = append(parts, r.Kind+" "+r.ID)
	}
	return "configapi: this write changes what its caller may not: " +
		strings.Join(parts, ", ")
}

// principalOf is who a request was resolved to. Every route here is guarded,
// so a request reaching a handler carries one; the zero principal a handler
// mounted outside the guard would read is refused everything.
func principalOf(r *http.Request) *iam.Principal {
	p, _ := iam.From(r.Context())
	return &p
}

// admit refuses next when p may not turn prior into it — see the file's header.
func admit(ctx context.Context, p iam.Principal, prior, next *config.Company) error {
	now := time.Now()
	company := authz.Decide(ctx, p, authz.ActionConfigWrite,
		authz.Object{Kind: authz.KindCompany}, authz.NoChart{}, now)
	if company.Allowed {
		return nil
	}
	diff := config.DiffOrg(prior, next)
	if len(diff.Changes) == 0 && len(diff.Settings) == 0 {
		return &AdmissionError{Refused: []RefusedChange{{Kind: "document",
			Why: "unchanged", Reason: company.Reason}}}
	}
	charts := map[config.OrgSide]authz.Chart{
		config.OrgBefore: orgchart.Of(diff.Before),
		config.OrgAfter:  orgchart.Of(diff.After),
	}
	var refused []RefusedChange
	for _, key := range diff.Settings {
		refused = append(refused, RefusedChange{Kind: "setting", ID: key,
			Reason: company.Reason})
	}
	for _, change := range diff.Changes {
		for _, path := range change.Credentials {
			refused = append(refused, RefusedChange{Kind: string(change.Kind),
				ID: change.ID, Op: string(change.Op), Why: "credential", Value: path,
				Reason: company.Reason})
		}
		for _, field := range change.Keys {
			refused = append(refused, RefusedChange{Kind: string(change.Kind),
				ID: change.ID, Op: string(change.Op), Why: "key", Value: field,
				Reason: company.Reason})
		}
		for _, touch := range change.Touches {
			d := authz.Decide(ctx, p, authz.ActionOrgWrite,
				authz.Object{Kind: authz.KindUnit, ID: change.ID, Container: touch.Unit},
				charts[touch.Side], now)
			if d.Unknown() {
				// A FAULT, NOT A WAIT: both trees are in hand, so nothing
				// a retry brings could answer what these could not.
				return fmt.Errorf("configapi: decide %s %s at %q: %w",
					change.Kind, change.ID, touch.Unit, d.Err)
			}
			if !d.Allowed {
				refused = append(refused, RefusedChange{Kind: string(change.Kind),
					ID: change.ID, Op: string(change.Op), Side: string(touch.Side),
					Place: touch.Unit, Why: touch.Why, Value: touch.Value,
					Reason: d.Reason})
			}
		}
	}
	if len(refused) > 0 {
		return &AdmissionError{Refused: refused}
	}
	return nil
}

// refuseAdmission answers a write its caller may not make: the authority
// table's 403, naming the company's grant — the one that would have admitted
// all of it — and every part refused.
func refuseAdmission(w http.ResponseWriter, err *AdmissionError) {
	detail := authz.RefusalDetail(err.Refused[0].Reason, []iam.Grant{iam.GrantConfigWrite})
	detail["refused"] = err.Refused
	detail["hint"] = "a lead may change only the seats and units inside a unit " +
		"they lead, on both sides of the write, and no setting, credential, " +
		"${VAR}, project, space, channel, address or contact id; a write that " +
		"changes nothing re-publishes the company and is config:write's; each " +
		"refused part names the place it reaches"
	httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeUnauthorized, detail)
}

// mayRead decides a read of one seat or unit of the active document,
// answering the refusal itself; ok is false when the request has been
// answered. A seat is read where it sits and a unit as itself, so a lead
// reads what they may write and nothing else.
func (s *Service) mayRead(w http.ResponseWriter, r *http.Request,
	company *config.Company, kind, id string) (ok bool) {

	o, err := company.Organization()
	if err != nil {
		s.fail(w, "build the organization the read is decided on", err)
		return false
	}
	return mayAt(w, r, authz.ActionOrgRead, o, id, placeOf(o, kind, id))
}

// mayAt decides a on one place of the org chart o — container empty for the
// root, which is decided without a tree — answering the refusal itself; ok is
// false when the request has been answered.
func mayAt(w http.ResponseWriter, r *http.Request, a authz.Action,
	o *org.Organization, id, container string) (ok bool) {

	d := authz.Decide(r.Context(), *principalOf(r), a,
		authz.Object{Kind: authz.KindUnit, ID: id, Container: container},
		orgchart.Of(o), time.Now())
	if d.Unknown() || !d.Allowed {
		authz.EnvelopeRefusal(w, r, authz.Policy{Action: a}, d)
		return false
	}
	return true
}

// placeOf is the unit a read of one seat or unit is decided on: the unit a
// seat sits in, or a unit itself; empty for a seat at the root and for an id
// the revision does not hold, which is decided as the root is.
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
