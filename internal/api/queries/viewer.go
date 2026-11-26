// Who the caller is, which is the question every personal surface rests on.

package queries

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/speech"
)

// errNoSeat is the refusal a question about a SEAT's trail makes of a caller
// bound to no seat who named none (a Tier A token's credential, or a service
// account bound to none). It is NOT an authorization failure: its remedy is a
// row in the identity directory rather than a different credential, which is
// why it is [ErrBadParams] and not a [Refusal].
//
// THE BINDING IS NAMED WHERE IT LIVES: the identity directory, not the org
// chart, which has no field for it. And it names a HUMAN seat, the only kind
// the directory binds anybody to; a bind naming an agent's seat is refused.
var errNoSeat = fmt.Errorf("%w: this credential is not bound to a seat. Bind "+
	"its row in the identity directory to a human seat (`crewlet iam bind`), "+
	"or name a handle", ErrBadParams)

// errNoRecord is the refusal a question about a PERSONAL RECORD makes of a
// caller who has none and named nobody.
//
// RARER THAN [errNoSeat] and a different fact: every person — bound to their
// seat — every service account and every token has a record of their own — see [iam.RecordOwner] —
// so what reaches this is a principal the engine can name nothing for, and the
// remedy is to name whose record to read.
var errNoRecord = fmt.Errorf("%w: this credential names nobody whose own "+
	"record there is. Name a handle", ErrBadParams)

// viewer answers who this caller is.
//
// THE PRINCIPAL AND NOTHING ELSE: a person is a ROW in the identity estate, a
// session resolves to that row, and the seat they hold is on the chart.
//
// A CREDENTIAL BOUND TO NO SEAT IS AN ORDINARY STATE (a Tier A token's
// session, or a service account bound to none), so this answers the login with
// no seat rather than an error: the screen then says what to bind instead of
// looking broken. It is never a PERSON's: a person holds a human seat for as
// long as they exist (ADR-0032).
func (s Sources) viewer(ctx context.Context, _ Params) (any, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return nil, unresolved(ctx, "viewer")
	}
	out := map[string]any{
		"login": principal.Login,
		// WHAT THIS CALLER MAY ASK, answered once so a screen can draw a
		// locked row rather than discovering the refusal per question —
		// the EFFECTIVE set the guard composed on this node (ADR-0033):
		// every app an admin holds is listed beside `admin`, and admin is
		// left out where this node's `api.auth.admin` refuses it, so a
		// screen reads what it may do here without expanding anything.
		"grants": grantNames(principal),
		// THE SEAT'S OWN FIELDS, empty for a credential that holds none.
		// `kind` is the SEAT's — human or agent — and never the
		// principal's: the two are different vocabularies, and a screen
		// reading "person" where the chart says "human" would be reading
		// a word this company never wrote.
		"handle": "",
		"name":   "",
		"kind":   "",
		// THE NAME THE CALLER'S OWN RECORD IS KEPT UNDER — their inbox,
		// pins, priorities and personal views. The seat for a bound
		// person, and the login for everybody the directory binds to
		// none, who has a record all the same: a screen that asked for
		// "my" record by `handle` asked an unbound caller for nothing,
		// while their assistant wrote it under this. See
		// [iam.RecordOwner].
		"owner": iam.RecordOwner(principal),
		// WHAT THIS CALLER MAY DO on the act transport: the tools that
		// write which the authority table can admit them to before any
		// object is named — see [Sources.Acts]. A screen disables a
		// control nothing here names, with the reason, rather than
		// offering a press the engine refuses. ALWAYS AN ARRAY, never
		// null, so "may do nothing" is a value a reader can test rather
		// than an absence.
		"acts": []string{},
		// WHERE THIS CALLER'S CREATE LANDS when it names no project — the
		// project `create_work_item` defaults a bound person's create to,
		// from the same derivation. "" is a real answer: a caller bound to
		// no seat, and a seat whose team, and every team above it, owns no
		// project, whose create is refused until one is named.
		"project": "",
		// WHETHER THIS CALLER'S CREDENTIAL MAY CHANGE THE COMPANY DOCUMENT,
		// and who manages it (ADR-0030), so a screen draws editing controls
		// disabled with the reason. Judged on the CREDENTIAL ([iam.ActorFor]'s
		// operator id) by [config.APIAuth.MayWriteCompany], the reading the
		// config surface refuses with, so answer and refusal cannot disagree.
		// It says nothing about AUTHORITY: admin, or a lead's subtree, decides
		// whether this caller may write.
		//
		// THE WRITERS ARE NAMED TO EVERY CALLER, as the refusal does: token
		// LABELS, never values. ALWAYS AN ARRAY; empty is not managed.
		"config_writer":     s.companyWriters().MayWriteCompany(iam.ActorFor(principal)),
		"config_managed_by": slices.Clone(s.companyWriters().CompanyWriters),
		// WHICH VENDOR CHAT SURFACES THIS COMPANY DECLARES, on a company
		// whose chat is a vendor's. Whether it runs the engine's own chat
		// at all is every reader's, as the org projection's `services`; the
		// vendors' names are the one further fact a screen naming where
		// the company talks needs, and they are ANSWERED TO A HOLDER OF
		// `chatlet`, the app they describe, as every app's disclosures are
		// (ADR-0033): an empty list for anybody else, for a company on the
		// engine's own chat or on none, and for a node with no company yet.
		"chat_vendors": []string{},
		// WHETHER A FRAMED KIND'S PAGE CAN BE OPENED HERE, so a create page says
		// so before it spends a publish, rather than the frame refusing after.
		"artifact_pages": s.ArtifactPages,
		// WHETHER A MICROPHONE IN A COMPOSER CAN DICTATE HERE, and why not
		// (internal/speech's Reason), so a screen draws it disabled or uses
		// the browser's own recognition. No path, model or secret.
		"dictation": speech.Availability{Reason: speech.ReasonDisabled},
	}
	if s.Dictation != nil {
		out["dictation"] = s.Dictation()
	}
	if principal.Can(iam.GrantChatlet) {
		out["chat_vendors"] = s.chatVendors()
	}
	if out["config_managed_by"] == nil {
		out["config_managed_by"] = []string{}
	}
	// WHETHER THE ASSISTANT IS AVAILABLE TO THIS CALLER HERE, with the first
	// reason not (internal/assistant/wire's ViewerReason), so the Ask pill
	// is drawn lit, or disabled with its reason, from this one answer.
	out["assistant"] = wire.ViewerAssistant{Reason: wire.ViewerNoModel}
	if s.Assistant != nil {
		out["assistant"] = s.Assistant(ctx, principal)
	}
	if s.Acts != nil {
		acts, err := s.Acts(ctx, principal)
		if err != nil {
			// THE TABLE COULD NOT DECIDE, which is not "may do nothing":
			// answered as an empty list, a screen would lock every
			// control a person holds the authority for while this node
			// cannot read the organisation it decides on.
			return nil, fmt.Errorf("%w: this node cannot yet say what this "+
				"credential may do: %w", ErrUnavailable, err)
		}
		if acts != nil {
			out["acts"] = acts
		}
	}
	// ONE CHART READING for the seat and the project it defaults to, so the
	// two cannot describe two charts a revision landed between.
	organization := s.organization()
	seat := seatOf(organization, principal)
	if seat == nil {
		return out, nil
	}
	out["handle"] = seat.Handle()
	out["name"] = seat.Name
	// The zero value is an agent, which is what config.Role's own `kind`
	// defaults to — reporting "" would make the common case look unset.
	if kind := seat.Kind; kind != "" {
		out["kind"] = string(kind)
	} else {
		out["kind"] = string(org.KindAgent)
	}
	// THROUGH THE ONE DERIVATION the operator surface's create applies
	// (`engine.ProjectOfSeat`), so the project promised here is the project
	// the create files into.
	out["project"] = engine.ProjectOfSeat(organization, seat.Handle())
	// AND THE SPACE a page of theirs that names no container lands in, by the
	// derivation the page write applies ([engine.SpaceOfSeat]).
	out["space"] = engine.SpaceOfSeat(organization, seat.Handle())
	return out, nil
}

// grantNames renders a principal's permissions for the screen.
func grantNames(p iam.Principal) []string {
	out := make([]string, 0, len(p.Grants))
	for _, g := range p.Grants {
		out = append(out, string(g))
	}
	return out
}

// companyWriters is the managed-document rule as the config surface reads it:
// Tier A's `api.auth.company_writers` ([Sources.CompanyWriters]), read through
// [config.APIAuth] so the viewer and the write path share one reading.
func (s Sources) companyWriters() *config.APIAuth {
	return &config.APIAuth{CompanyWriters: s.CompanyWriters}
}

// seatOf resolves the caller's own seat on one chart reading, or nil.
func seatOf(organization *org.Organization, p iam.Principal) *org.Role {
	if p.Seat == "" || organization == nil {
		return nil
	}
	// EVERY SEAT AND NOT ONLY THE AGENTS: a person holds a HUMAN seat,
	// which is the whole case this resolves.
	return organization.SeatByHandle(p.Seat)
}

// recordHandle is the handle a PERSONAL RECORD question answers for, and the
// authority check when it is somebody else's. THE NAME IS RESOLVED BY
// [iam.OwnerOf], the one function every tool reads and writes a person's record
// through: the caller's own is [iam.RecordOwner]'s, somebody else's LOGIN is
// their holder's record ([Sources.Holders]), anything else is read as named.
// Authority is DECIDED ON THAT RESOLUTION: the lead relation is a fact about a
// seat, not a login.
//
// BUT THE DIRECTORY IS ASKED ONLY ONCE THE AUTHORITY IS DECIDED AS FAR AS IT
// CAN BE WITHOUT THE RECORD ([Sources.mayLook]): its answers differ by login,
// so a caller with no authority would read which logins exist off the
// difference ("not found" before "you may not" is a roster).
func (s Sources) recordHandle(ctx context.Context, action authz.Action,
	asked string) (string, error) {
	// THE SCOPE RULE IS THE AUTHORITY TABLE'S, asked with the question's OWN
	// verb: `work_inbox` [authz.ActionInboxRead], `work_my_work`
	// [authz.ActionMyWork], `work_person` and `decisions`
	// [authz.ActionPersonRead], a seat's trail [authz.ActionSeatTrailRead]
	// (no app: it belongs to none).

	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return "", unresolved(ctx, "viewer")
	}
	owner, _, err := iam.OwnerOf(ctx, principal, asked, s.Holders,
		s.mayLook(principal, action))
	var looked *iam.LookRefused
	switch {
	case errors.As(err, &looked):
		return "", looked.Err
	case errors.Is(err, iam.ErrNoHolder), errors.Is(err, iam.ErrHolderUnseated):
		if refusal := s.mayRead(ctx, principal, action, asked); refusal != nil {
			return "", refusal
		}
		return "", fmt.Errorf("%w: %w", ErrNotFound, err)
	case err != nil:
		// THE DIRECTORY'S OWN WORDS GO TO THE LOG: they name the seat a
		// login is bound to, and this error is what a caller is answered.
		log.WarnContext(ctx, "queries_record_undecidable", "question",
			string(action), "name", asked, "error", err.Error())
		return "", fmt.Errorf("%w: this node cannot say whose record %q is yet",
			ErrUnavailable, asked)
	case owner == "":
		return "", errNoRecord
	case asked == "", iam.NamesSelf(principal, asked):
		return owner, nil
	}
	if err := s.mayRead(ctx, principal, action, owner); err != nil {
		return "", err
	}
	return owner, nil
}

// mayLook is the authority a personal question decides BEFORE the identity
// directory is asked whose record somebody else's login is — see
// [iam.MayLook] and [authz.Object.Unresolved]: the question's own verb, asked
// of a record nobody has resolved yet, answered exactly as [Sources.mayRead]
// answers it. [authz.ErrUnresolved] lets the lookup happen and nothing more:
// it is a caller who could be admitted only as the lead of whoever holds the
// login, and [Sources.recordHandle] decides again on the record it resolves to.
func (s Sources) mayLook(principal iam.Principal, action authz.Action) iam.MayLook {
	return func(ctx context.Context, login string) error {
		d := authz.Decide(ctx, principal, action, authz.Object{
			Kind: authz.KindPerson, ID: login, Owner: login, Unresolved: true,
		}, s.Chart, s.clock())
		switch {
		case d.Allowed, errors.Is(d.Err, authz.ErrUnresolved):
			return nil
		case d.Unknown():
			return fmt.Errorf("%w: this node cannot decide %s on somebody "+
				"else's record yet: %w", ErrUnavailable, action, d.Err)
		}
		return NewRefusal(string(action)+" of somebody else's record", d)
	}
}

// mayRead decides whether principal may take action on the record one seat
// holds, and renders the answer as the error this surface refuses with.
//
// THE REFUSAL IS THE DECISION'S OWN — its reason and the grants that would
// have admitted the caller — and never a sentence written here, which is how
// the one this replaced came to name an admin grant for a rule that did not
// govern the question asking it.
func (s Sources) mayRead(ctx context.Context, principal iam.Principal,
	action authz.Action, handle string) error {

	d := authz.Decide(ctx, principal, action,
		authz.Object{Kind: authz.KindPerson, ID: handle, Owner: handle}, s.Chart,
		s.clock())
	switch {
	case d.Unknown():
		// THIS NODE COULD NOT TELL, which a surface renders as 503 and
		// never as a refusal: a node running no company yet telling a lead
		// they lead nobody sends them to ask for authority they hold.
		return fmt.Errorf("%w: this node cannot decide %s on %s's record "+
			"yet: %w", ErrUnavailable, action, handle, d.Err)
	case !d.Allowed:
		return NewRefusal(string(action)+" of "+handle, d)
	}
	return nil
}
