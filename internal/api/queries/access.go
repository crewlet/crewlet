// Who can reach the company through this engine's own surface, and as whom.

package queries

import (
	"context"
	"slices"
	"strings"
)

// AccessPosture is Tier A's auth posture as the `access` answer reads it.
//
// LABELS, NEVER VALUES, BY CONSTRUCTION: the type has no field a token's value
// could travel in, so no later edit to the answer can put one on the wire by
// reaching for the wrong member. The API builds it from its own guard (see
// `api.New`), which is what decides who authenticates — a disabled guard
// accepts no listed token at all, and a posture read off the document would
// name credentials that authenticate nobody.
type AccessPosture struct {
	// TokenIDs are the `api.auth.tokens[].id` labels the guard accepts.
	TokenIDs []string
	// Disabled is `api.auth.disabled`: every caller is
	// [org.ReservedOperatorID] and nobody is a person.
	Disabled bool
	// AnonymousRead is `api.auth.allow_anonymous_read`.
	AnonymousRead bool
	// AllowedOrigins is `api.auth.allowed_origins`; empty is same-origin only.
	AllowedOrigins []string
}

// TokenScope is what one accepted credential reaches.
type TokenScope string

const (
	// ScopeOperator is a credential no human seat binds: every guarded
	// read, /config, /secrets, /setup, /backup and the operator MCP
	// surface, acting under its own label. Not the act transport, which
	// admits a person and nobody else (ADR-0024).
	ScopeOperator TokenScope = "operator"
	// ScopePerson is a credential a human seat binds with
	// `contact.crewlet_operator_id`: everything [ScopeOperator] reaches,
	// plus `/operator/act` as that seat — the dashboard's writes.
	ScopePerson TokenScope = "person"
)

// TokenScopes is every scope, for the gate holding the dashboard's copy.
var TokenScopes = []TokenScope{ScopeOperator, ScopePerson}

// Valid reports whether s is a scope this build sends.
func (s TokenScope) Valid() bool { return slices.Contains(TokenScopes, s) }

// AccessBinding is how a human seat's `contact.crewlet_operator_id` stands
// against the credentials the guard accepts.
//
// FOUR VALUES, because the three failures have three different remedies: an
// unbound seat is a line of company configuration away from acting, an
// unresolved one is a variable missing from this process's environment, and
// one naming no token is a Tier A entry that does not exist — or a typo on
// either side.
type AccessBinding string

const (
	// BindingBound names a token the guard accepts: the person acts as
	// themself.
	BindingBound AccessBinding = "bound"
	// BindingUnbound names no token. An ordinary state (see
	// [org.HumanContact]): the person is reached on their other surfaces
	// and does not act through this one.
	BindingUnbound AccessBinding = "unbound"
	// BindingUnresolved is a `${VAR}` binding whose variable is unset or
	// empty here, or resolves to the reserved id.
	BindingUnresolved AccessBinding = "unresolved"
	// BindingNoToken resolves to an id no accepted token carries — every
	// binding under a disabled guard, which accepts none.
	BindingNoToken AccessBinding = "no_token"
)

// AccessBindings is every binding state, for the gate holding the dashboard's
// copy.
var AccessBindings = []AccessBinding{BindingBound, BindingUnbound, BindingUnresolved, BindingNoToken}

// Valid reports whether b is a binding state this build sends.
func (b AccessBinding) Valid() bool { return slices.Contains(AccessBindings, b) }

// AccessAnswer is `access`: the credentials, the people, and the posture.
type AccessAnswer struct {
	Auth AccessAuth `json:"auth"`
	// Tokens in label order. ALWAYS A LIST: no token is a real posture.
	Tokens []AccessToken `json:"tokens"`
	// People are the chart's human seats, in handle order. Always a list.
	People []AccessPerson `json:"people"`
}

// AccessAuth is the posture the guard enforces.
type AccessAuth struct {
	Disabled       bool     `json:"disabled"`
	AnonymousRead  bool     `json:"anonymous_read"`
	AllowedOrigins []string `json:"allowed_origins"`
}

// AccessToken is one accepted credential: its label, what it reaches and the
// person it acts as. Never its value.
type AccessToken struct {
	ID    string     `json:"id"`
	Scope TokenScope `json:"scope"`
	// Seat is the human seat binding this token, or null.
	Seat *AccessSeat `json:"seat"`
	// Yours is whether the caller presented this token.
	Yours bool `json:"yours"`
}

// AccessSeat names a seat.
type AccessSeat struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
}

// AccessPerson is one human seat: how agents reach them and whether they act
// here.
type AccessPerson struct {
	Handle       string `json:"handle"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Availability string `json:"availability"`
	// OperatorID is `contact.crewlet_operator_id` VERBATIM — a literal or
	// a `${VAR}` reference, never the variable's value — or "".
	OperatorID string        `json:"operator_id"`
	Binding    AccessBinding `json:"binding"`
	// Contacts are the seat's OTHER identity fields — where agents reach
	// them. The binding is not among them: it is an attribution, never an
	// address ([org.Transport.Reachable]).
	Contacts []AccessContact `json:"contacts"`
}

// AccessContact is one identity field as configured.
type AccessContact struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Reference bool   `json:"reference"`
	Resolves  bool   `json:"resolves"`
}

// operatorIDKey is the contact field the binding is written under.
const operatorIDKey = "crewlet_operator_id"

// access answers who can reach the company through this engine and as whom.
//
// OPERATOR-ONLY, for the reason `/setup` guards its reads: which labels the
// guard accepts, which person each one is and which are nobody's is a map of
// which credential to steal. The value is never here at all — see
// [AccessPosture].
//
// THE TWO LISTS ARE ONE JOIN, walked from both ends: a token names the seat
// binding it, and a person names whether their binding reaches a token. Either
// alone hides the half that goes wrong — a token nobody binds looks like a
// pipeline's credential, and a binding with a typo in it looks bound until
// that person presses a button and is refused.
func (s Sources) access(ctx context.Context, _ Params) (any, error) {
	posture := s.Access
	caller := operatorFrom(ctx)
	organization := s.organization()

	accepted := make(map[string]string, len(posture.TokenIDs))
	for _, id := range posture.TokenIDs {
		// The chart's binding is compared lower-cased (see
		// [org.HumanContact]'s CrewletOperatorID), so the join is too.
		accepted[strings.ToLower(strings.TrimSpace(id))] = id
	}

	out := AccessAnswer{
		Auth: AccessAuth{
			Disabled:       posture.Disabled,
			AnonymousRead:  posture.AnonymousRead,
			AllowedOrigins: append([]string{}, posture.AllowedOrigins...),
		},
		Tokens: make([]AccessToken, 0, len(posture.TokenIDs)),
		People: []AccessPerson{},
	}
	for _, id := range slices.Sorted(slices.Values(posture.TokenIDs)) {
		row := AccessToken{ID: id, Scope: ScopeOperator, Yours: caller != "" && caller == id}
		// THE SAME LOOKUP THE VIEWER AND THE ACT TRANSPORT MAKE, so a
		// token this screen calls a person is one those admit as one.
		if organization != nil {
			if seat := organization.SeatByOperatorID(id, nil); seat != nil && seat.IsHuman() {
				row.Scope = ScopePerson
				row.Seat = &AccessSeat{Handle: seat.Handle(), Name: seat.Name}
			}
		}
		out.Tokens = append(out.Tokens, row)
	}

	if organization == nil {
		return out, nil
	}
	for role := range organization.AllRoles() {
		if !role.IsHuman() {
			continue
		}
		person := AccessPerson{
			Handle:       role.Handle(),
			Name:         role.Name,
			Email:        role.Email,
			Availability: role.Availability,
			Binding:      BindingUnbound,
			Contacts:     []AccessContact{},
		}
		for _, field := range role.Contact.Fields(nil) {
			if field.Key == operatorIDKey {
				person.OperatorID = field.Value
				continue
			}
			person.Contacts = append(person.Contacts, AccessContact{
				Key: field.Key, Value: field.Value,
				Reference: field.Reference, Resolves: field.Resolves,
			})
		}
		if person.OperatorID != "" {
			// THROUGH THE ENGINE'S OWN RESOLUTION, so "unresolved" here is
			// exactly the binding the guard's lookup cannot see.
			resolved := role.ResolvedOperatorID(nil)
			switch {
			case resolved == "":
				person.Binding = BindingUnresolved
			case accepted[resolved] != "":
				person.Binding = BindingBound
			default:
				person.Binding = BindingNoToken
			}
		}
		out.People = append(out.People, person)
	}
	slices.SortFunc(out.People, func(a, b AccessPerson) int { return strings.Compare(a.Handle, b.Handle) })
	return out, nil
}
