// Who can reach the company through this engine's own surface, and as whom.

package queries

import (
	"context"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
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
	// Keys are the `api.auth.tokens` entries the guard accepts — each one's
	// label and role, never its value — in label order.
	Keys []AccessKey
	// Disabled is `api.auth.disabled`: every caller is an admin named
	// [org.ReservedOperatorID], and nobody is a person.
	Disabled bool
	// Anonymous is `api.auth.anonymous`: what a caller with no key reaches.
	Anonymous config.AnonymousAccess
	// AllowedOrigins is `api.auth.allowed_origins`; empty is same-origin only.
	AllowedOrigins []string
	// CompanyWriters is `api.auth.company_writers`: the admin keys that alone
	// may change the company document, empty when every admin key may
	// (ADR-0030). Read through [AccessPosture.MayWriteCompany].
	CompanyWriters []string
}

// AccessKey is one accepted key as the posture carries it: a label and a role.
type AccessKey struct {
	ID   string
	Role config.TokenRole
}

// RoleOf is the role of the accepted key with this id, or "" for none.
func (p *AccessPosture) RoleOf(id string) config.TokenRole {
	for _, k := range p.Keys {
		if k.ID == id {
			return k.Role
		}
	}
	return ""
}

// MayWriteCompany reports whether the key with this id may change the company
// document: an ADMIN key (ADR-0031), and one company_writers admits when the
// document is managed — Tier A's own reading of the list
// ([config.APIAuth.MayWriteCompany]), never a second one, so the viewer offers
// exactly the edits the config surface admits.
func (p *AccessPosture) MayWriteCompany(id string) bool {
	if p.Disabled {
		auth := config.APIAuth{CompanyWriters: p.CompanyWriters}
		return auth.MayWriteCompany(id)
	}
	if p.RoleOf(id) != config.RoleAdmin {
		return false
	}
	auth := config.APIAuth{CompanyWriters: p.CompanyWriters}
	return auth.MayWriteCompany(id)
}

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
	Disabled bool `json:"disabled"`
	// Anonymous is what a caller with no key reaches: `public` (the
	// company's name, mission and chart) or `none`.
	Anonymous      config.AnonymousAccess `json:"anonymous"`
	AllowedOrigins []string               `json:"allowed_origins"`
	// CompanyWriters is `api.auth.company_writers` as Tier A orders it: the
	// admin keys that alone may change the company document (ADR-0030).
	// ALWAYS A LIST, and empty is a real posture — every admin key may — so
	// the screen that reads the deployment's auth says whether the document
	// is managed, and by which credential, where the operator looks for it.
	CompanyWriters []string `json:"company_writers"`
}

// AccessToken is one accepted key: its label, its role and the person it is
// linked to. Never its value.
type AccessToken struct {
	ID string `json:"id"`
	// Role is what the key reaches, as Tier A names it and the guard
	// enforces it (ADR-0031). Whether a person is linked to it is a
	// different fact, on Seat: a role says what the key may read and run,
	// and the link says who it acts as.
	Role config.TokenRole `json:"role"`
	// Seat is the human seat linking this key, or null.
	Seat *AccessSeat `json:"seat"`
	// Yours is whether the caller presented this key.
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

// access answers who can reach the company through this engine, how far, and
// as whom.
//
// ADMIN, for the reason `/setup` is admin in full: which labels the guard
// accepts, what each reaches, which person each one is and which are nobody's
// is a map of which credential to steal. The value is never here at all — see
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

	accepted := make(map[string]string, len(posture.Keys))
	for _, key := range posture.Keys {
		id := key.ID
		// The chart's binding is compared lower-cased (see
		// [org.HumanContact]'s CrewletOperatorID), so the join is too.
		accepted[strings.ToLower(strings.TrimSpace(id))] = id
	}

	out := AccessAnswer{
		Auth: AccessAuth{
			Disabled:       posture.Disabled,
			Anonymous:      posture.Anonymous,
			AllowedOrigins: append([]string{}, posture.AllowedOrigins...),
			CompanyWriters: append([]string{}, posture.CompanyWriters...),
		},
		Tokens: make([]AccessToken, 0, len(posture.Keys)),
		People: []AccessPerson{},
	}
	keys := slices.SortedFunc(slices.Values(posture.Keys), func(a, b AccessKey) int {
		return strings.Compare(a.ID, b.ID)
	})
	for _, key := range keys {
		row := AccessToken{ID: key.ID, Role: key.Role, Yours: caller != "" && caller == key.ID}
		// THE SAME LOOKUP THE VIEWER AND THE ACT TRANSPORT MAKE, so a
		// key this screen calls a person is one those admit as one.
		if organization != nil {
			if seat := organization.SeatByOperatorID(key.ID, s.Env); seat != nil && seat.IsHuman() {
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
		for _, field := range role.Contact.Fields(s.Env) {
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
			resolved := role.ResolvedOperatorID(s.Env)
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
