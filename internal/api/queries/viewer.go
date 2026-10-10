// Who the caller is, which is the question every personal surface rests on.

package queries

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The refusals a personal question makes, told apart because the remedies are
// different: one is a line of company configuration, the other is asking
// somebody whose line the record is in.
var (
	errNoSeat = fmt.Errorf("%w: this key is not linked to a seat — give a human "+
		"seat contact.crewlet_operator_id matching the api.auth token id, or name a handle",
		ErrBadParams)
	errNotYoursForbidden = fmt.Errorf("%w: this record belongs to a seat outside "+
		"your own line — a person reads their own and their reports'", ErrForbidden)
	errNotYoursUnauthorized = fmt.Errorf("%w: reading a person's record needs a key "+
		"linked to their seat or to a lead in their line", ErrUnauthorized)
)

// errNotYours is the refusal of a personal record outside the caller's line,
// classed by WHO ASKED: [ErrForbidden] for a caller whose key was accepted —
// signing in again would change nothing — and [ErrUnauthorized] for one with
// none, whose remedy is a key.
func errNotYours(caller auth.Principal) error {
	if caller.Authenticated() {
		return errNotYoursForbidden
	}
	return errNotYoursUnauthorized
}

// ViewerAdmin is one person who holds an admin key: the seat linked to it.
type ViewerAdmin struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
}

// viewer answers who this caller is: their key, its role and reach, the
// person it is linked to and that person's line.
//
// THE FRAME HAS NEVER HAD A VIEWER, and everything personal in the dashboard
// is fiction without one: "my work" picked the alphabetically first seat,
// `work_views` was asked without a viewer so no pinned or personal view could
// exist, the five `preset=` values that resolve against the caller's identity
// were never sent, and an inbox could not be anyone's.
//
// The resolution is a chain of two, and NEITHER STEP IS NEW: Tier A's
// `api.auth.tokens` maps a presented key to its id and ROLE, and a seat links
// one of those ids with `contact.crewlet_operator_id`. What was missing is a
// question that walks it.
//
// OPEN, the one question that is: a sign-in page asks it of a caller who has
// not signed in, and "nobody" is an answer — an empty token_id, no role, the
// anonymous reach.
//
// AN UNLINKED KEY IS AN ORDINARY STATE. `org.HumanContact` says so in as many
// words, so this answers the key and its role with no seat rather than an
// error — the screen then says what to link, which is a different thing from a
// screen that looks broken.
func (s Sources) viewer(ctx context.Context, _ Params) (any, error) {
	caller := callerFrom(ctx)
	out := map[string]any{
		// WHICH KEY, by its label — never its value — or "" for a caller
		// who presented none.
		"token_id": caller.ID,
		// WHAT IT IS FOR AND HOW FAR IT REACHES, decided once by the
		// guard and compared against by every surface, so a screen draws
		// a locked row rather than discovering the refusal per question.
		// Both "" / the anonymous posture's reach for nobody.
		"role":  string(caller.Role),
		"reach": string(caller.Reach),
		// WHETHER A PERSON IS LINKED TO THE KEY, which is a different fact
		// from the role: a role says what the key may read and run, the
		// link says who it acts as.
		"linked": false,
		"handle": "",
		"name":   "",
		"kind":   "",
		// THE SEATS IN THIS PERSON'S LINE ([org.Organization.LeadsInLine]),
		// whose personal records they may read beside their own — the same
		// set [Sources.viewerParty] admits, from the same derivation, so a
		// screen offers exactly the reports the engine answers for.
		// ALWAYS AN ARRAY.
		"line": []string{},
		// WHAT THIS CALLER MAY DO on the act transport, which admits a
		// person and nobody else (ADR-0024): empty for an anonymous
		// reader and for a key no seat links, so a screen disables its
		// write controls with the reason rather than offering a press
		// the engine refuses. ALWAYS AN ARRAY, never null, so "may do
		// nothing" is a value a reader can test rather than an absence.
		"acts": []string{},
		// WHERE THIS PERSON'S CREATE LANDS when it names no project — the
		// project `create_work_item` defaults a person's create to, from
		// the same derivation. "" is a real answer: a seat whose team, and
		// every team above it, owns no project, whose create is refused
		// until one is named.
		"project": "",
		// WHETHER THIS CALLER MAY CHANGE THE COMPANY DOCUMENT — an admin
		// key, and one company_writers admits when the document is
		// managed (ADR-0030, ADR-0031) — so a screen draws every editing
		// control disabled with the reason rather than offering a save the
		// config surface refuses.
		"config_writer": false,
		// WHO MAY, when the document is managed: named to an ADMIN only,
		// because which key can rewrite the company is the most valuable
		// line of the `access` answer, which is admin for the same reason.
		// ALWAYS AN ARRAY; empty is not managed.
		"config_managed_by": []string{},
		// WHO TO ASK for what this key does not reach: the people holding
		// an admin key. Named to a caller with a key only — never to a
		// stranger, who is given no name of anybody who can open the
		// engine. ALWAYS AN ARRAY.
		"admins": []ViewerAdmin{},
	}
	if caller.Authenticated() {
		out["config_writer"] = s.Access != nil && s.Access.MayWriteCompany(caller.ID)
		out["admins"] = s.admins()
	}
	if caller.IsAdmin() {
		out["config_managed_by"] = s.companyWriters()
	}
	// ONE CHART for the whole answer: [org.Organization.LeadsInLine] finds
	// a lead by POINTER, and every read of the chart builds a new one, so a
	// seat looked up in one and its line asked of another is a lead with no
	// line at all.
	organization := s.organization()
	seat := s.seatIn(organization, caller.ID)
	if seat == nil {
		return out, nil
	}
	out["linked"] = true
	if s.OperatorActs != nil {
		if acts := s.OperatorActs(); acts != nil {
			out["acts"] = acts
		}
	}
	out["handle"] = seat.Handle()
	out["name"] = seat.Name
	// The zero value is an agent, which is what config.Role's own `kind`
	// defaults to — reporting "" would make the common case look unset.
	kind := seat.Kind
	if kind == "" {
		kind = org.KindAgent
	}
	out["kind"] = string(kind)
	// THE CHART THIS ANSWER WAS READ FROM, through the one derivation the
	// operator surface's create applies (`engine.ProjectOfSeat`), so the
	// project promised here is the project the create files into.
	out["project"] = s.projectOf(seat.Handle())
	if line := organization.LeadsInLine(seat); line != nil {
		out["line"] = line
	}
	return out, nil
}

// admins is every human seat linked to an ADMIN key the guard accepts, in
// handle order — never nil.
//
// THROUGH THE SAME LOOKUP the act transport and the `access` answer make, so a
// person named here as an admin is one those name as one.
func (s Sources) admins() []ViewerAdmin {
	out := []ViewerAdmin{}
	organization := s.organization()
	if s.Access == nil || organization == nil {
		return out
	}
	for _, key := range s.Access.Keys {
		if key.Role != config.RoleAdmin {
			continue
		}
		seat := organization.SeatByOperatorID(key.ID, s.Env)
		if seat == nil || !seat.IsHuman() {
			continue
		}
		out = append(out, ViewerAdmin{Handle: seat.Handle(), Name: seat.Name})
	}
	slices.SortFunc(out, func(a, b ViewerAdmin) int { return strings.Compare(a.Handle, b.Handle) })
	return slices.CompactFunc(out, func(a, b ViewerAdmin) bool { return a.Handle == b.Handle })
}

// companyWriters is the managed document's writers, or empty — never nil.
func (s Sources) companyWriters() []string {
	if s.Access == nil {
		return []string{}
	}
	return append([]string{}, s.Access.CompanyWriters...)
}

// seatIn resolves a key's id to the seat organization links it to, or nil.
//
// THE CHART IS AN ARGUMENT, read once by the caller, because each read builds a
// new one and a seat is compared by pointer within the chart it came from — see
// [Sources.viewerParty].
func (s Sources) seatIn(organization *org.Organization, operatorID string) *org.Role {
	if operatorID == "" || organization == nil {
		return nil
	}
	// THROUGH THE NODE'S OWN CHAIN ([Sources.Env]), which is where every
	// other consumer of `contact` resolves a `${VAR}` binding.
	return organization.SeatByOperatorID(operatorID, s.Env)
}

// viewerParty is who a personal question answers about when the caller named
// nobody, the authority check when they named somebody, and BOTH IDENTITIES
// that person's rows may carry.
//
// # The scope rule, in one place because four questions share it
//
// A caller reads the seat their own key is linked to, and the seats in their
// LINE — every seat whose management chain passes through theirs
// ([org.Organization.LeadsInLine]), which is the authority a lead already
// holds over a report's queue. Anybody else's is refused: [ErrForbidden] for a
// caller whose key was accepted, since a different key would not change whose
// line they are in.
//
// AN ADMIN KEY ADDS NOTHING HERE (ADR-0031). The role says how much of the
// MACHINE a key reaches — transcripts, configuration, nodes — and a person's
// inbox and day are not the machine's: an admin who is nobody's lead reads
// their own and no one else's, exactly as a member does. The founder at the
// root of the chart leads everybody, so the person who most needs every
// report's day already has it, from the chart rather than from a key.
//
// Registering these admin instead — which is what `work_my_work` once was —
// makes the landing screen the most-gated screen in the product and the human
// teammate, who is one of the two readers this dashboard is for, fictional.
//
// # And the party, which is what the reader needs rather than a handle
//
// A write made through somebody's own key is attributed to the KEY, not to
// their seat, and deliberately so: a tracker whose author field is chosen by
// the writer is not an audit trail (see internal/api/operator). The
// consequence is that one person's rows carry two names — `jane-founder` on
// what a colleague assigned them, `founder` on everything their own assistant
// filed — so a personal read asked about one of them answered nothing. A
// founder who was reporter and watcher on eleven items opened My work and
// found seven empty tabs.
//
// # The party belongs to the person ASKED ABOUT, not to the caller
//
// A lead reading a report's day gets that report's own alias, resolved from
// the chart, rather than the key in their own hand: whose two names these are
// is a fact about the seat, and the caller's key has nothing to do with it.
// That it is the caller's own id in the ordinary case falls out of the same
// lookup rather than being a second rule.
//
// Returns the party to read and an error to refuse with.
func (s Sources) viewerParty(ctx context.Context, asked string) (tracker.Party, error) {
	caller := callerFrom(ctx)
	// ONE CHART for the lookup and the line: [org.Organization.LeadsInLine]
	// finds a lead by POINTER, and every read of the chart builds a new one.
	organization := s.organization()
	seat := s.seatIn(organization, caller.ID)
	own := ""
	if seat != nil {
		own = seat.Handle()
	}
	switch {
	case asked == "":
		if own == "" {
			// NOT AN AUTHORIZATION FAILURE. Nobody was refused: there
			// is no person to answer about, and the remedy is a line
			// of company configuration rather than a different key.
			return tracker.Party{}, errNoSeat
		}
		return s.partyOf(own), nil
	case asked == own:
		return s.partyOf(asked), nil
	case seat != nil && slices.Contains(organization.LeadsInLine(seat), asked):
		// A REPORT, at any depth: the lead reads the day of somebody
		// whose work they are answerable for.
		return s.partyOf(asked), nil
	}
	return tracker.Party{}, errNotYours(caller)
}

// partyOf is one seat and the credential bound to it.
//
// THROUGH THE NODE'S OWN CHAIN ([Sources.Env]), as [Sources.seatIn]
// resolves the way in. The two directions go through the same resolution in
// `org` with the same lookup, so a company cannot be bound for one and unbound
// for the other.
//
// A SEAT THAT IS NOT IN THE CHART IS STILL A PARTY. An operator naming a
// handle that no seat holds — a person who has left, a handle on old rows —
// gets that handle alone rather than a refusal: the rows are what the question
// is about, and this lookup only ever ADDS an alias.
func (s Sources) partyOf(handle string) tracker.Party {
	party := tracker.Party{Handle: handle}
	organization := s.organization()
	if organization == nil {
		return party
	}
	party.OperatorID = organization.SeatByHandle(handle).ResolvedOperatorID(s.Env)
	return party
}

// viewerPins is the authority rule over the party a view STRIP is
// personalised by.
//
// The same check as [Sources.viewerParty] over a different ABSENCE, which is
// why the named case delegates rather than restating it. A personal question
// is ABOUT somebody, so naming nobody with no seat to fall back on has no
// answer and [errNoSeat] says so. A view strip is about a CONTAINER and the
// viewer only decides whose pins order it, so naming nobody is the SHARED
// strip — a real answer, the documented meaning of an unnamed
// [tracker.ViewQuery.Viewer], and the one an anonymous or unbound caller must
// keep getting, because the sidebar and the board ask for exactly that.
//
// What is identical is the half that matters. `viewer` selected whose record
// was read and nothing checked it, so a reader could walk the org chart and
// page through every seat's pinned views by handle — the personal record
// `work_person` is scoped for. A scope rule three of the four personal
// questions follow is not a rule.
//
// AND IT CARRIES BOTH NAMES, because a saved view and a pin are both written
// through the person's own key: owned by the key's id, asked for under the
// seat's, so a founder's own strip came back with neither.
func (s Sources) viewerPins(ctx context.Context, asked string) (tracker.Party, error) {
	if asked == "" {
		return tracker.Party{}, nil
	}
	return s.viewerParty(ctx, asked)
}
