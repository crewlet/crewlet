package api

import (
	"slices"

	"github.com/crewlet/crewlet/internal/config"
)

// The org projection: the company's identity and its seat and unit tree, as
// GET /org, the socket snapshot's `org` key and the `org` push carry it.
//
// # An explicit public type, never a marshal of the config's own structs
//
// These three surfaces are ANONYMOUSLY READABLE under the default posture
// (`api.auth.allow_anonymous_read: true`), and the only surface that shows the
// whole document, /config, is guarded in full. The projection used to marshal
// config.Role and config.Unit verbatim and rely on nothing else, which made it
// deny-by-omission: every field somebody added to a role was public the day it
// landed. That had already happened. Contact identities (a person's Slack
// member id, GitHub login, email), `${VAR}` names in a Mattermost username,
// placement labels and sandbox setup commands (routinely the place a registry
// credential is written) all reached an anonymous reader, and the secret tags
// redaction relies on covered none of them because none of them is a
// credential in the narrow sense.
//
// So the public shape is spelled out here field by field, and a field reaches
// an anonymous reader only by being written into one of these types on
// purpose. Everything else stays behind the guarded `config` query, which is
// where the dashboard reads it. Configured work that has a read surface of its
// own is not repeated here either: schedules are described by /schedules and a
// seat's token cap by /budgets, each under the same read posture as this one.
// TestEveryOrgFieldIsClassified fails the day config.Role or config.Unit gains
// a field nobody has classified, so a new field needs a decision rather than
// defaulting to either side.
//
// # Authored values, not effective ones
//
// Each field carries what the founder WROTE: a seat's declared handle (empty
// when the engine derives it from the name), a unit's own lead (empty when it
// inherits one). The effective hierarchy the engine derives is a different
// fact with a different shape, and mixing the two into one field would leave a
// reader unable to tell a declared lead from an inherited one.

// OrgProjection is the anonymous view of a company.
//
// Every field is omitted when empty, so a node with no active company answers
// `{}`, which is the shape the dashboard already reads as "nothing loaded".
type OrgProjection struct {
	Name     string    `json:"name,omitempty"`
	Mission  string    `json:"mission,omitempty"`
	Vision   string    `json:"vision,omitempty"`
	Policies []string  `json:"policies,omitempty"`
	Roles    []OrgSeat `json:"roles,omitempty"`
	Units    []OrgUnit `json:"units,omitempty"`

	// Derived is the hierarchy the engine derives from the document above:
	// each seat's handle, its effective unit, its primary manager and
	// reports, and each unit's effective type, lead and channel.
	//
	// THE ENGINE SAYS IT RATHER THAN A CLIENT WORKING IT OUT. Every rule
	// here is one a second implementation gets wrong, and the dashboard's
	// TypeScript had already diverged from the engine on three of them and
	// on the handle of a name with a dotted capital I; see [config.Derive].
	// The fields above stay as WRITTEN, so a reader can still tell a
	// declared lead from an inherited one.
	//
	// WITHOUT PATHS, which is the one difference from the guarded answers:
	// an anonymous reader is given no document to point into, and membership
	// is in each unit's seats. Nothing else here is a fact the fields above
	// do not already carry, so it needs no classification of its own beyond
	// this: it is those values, resolved.
	Derived *config.Derived `json:"derived,omitempty"`
}

// OrgSeat is the public half of one seat: who it is and what it is for.
//
// Plain strings rather than the config's enum types, so the wire contract does
// not move when an internal type is renamed or reshaped.
type OrgSeat struct {
	Name                 string   `json:"name"`
	Kind                 string   `json:"kind,omitempty"`
	Handle               string   `json:"handle,omitempty"`
	Goal                 string   `json:"goal,omitempty"`
	Backstory            string   `json:"backstory,omitempty"`
	Responsibilities     []string `json:"responsibilities,omitempty"`
	BehavioralGuidelines []string `json:"behavioral_guidelines,omitempty"`
	Manages              []string `json:"manages,omitempty"`
	Availability         string   `json:"availability,omitempty"`
}

// OrgUnit is the public half of one unit, nesting to any depth.
type OrgUnit struct {
	// ID is the unit's KEY — what a `manages:` entry and a seat's `unit:`
	// resolve — and it carries [config.Unit.IdentityKey] rather than the
	// authored field, so a unit that declares no id answers with the name
	// that is its key instead of with nothing.
	//
	// IT IS PUBLIC BECAUSE NOTHING ELSE HERE RESOLVES A REFERENCE. A seat's
	// `manages` entry on this projection is the authored value, which may
	// name a unit — and with only display names beside it a client had
	// nothing in the same response to resolve that key against. It either
	// matched on the name, which is a different value the moment a unit
	// declares an id, or rendered the raw string and called the reference
	// broken. Giving the client the id-or-name rule instead would be a
	// second derivation of the one thing this engine keeps in one place.
	ID        string    `json:"id,omitempty"`
	Name      string    `json:"name"`
	Type      string    `json:"type,omitempty"`
	Purpose   string    `json:"purpose,omitempty"`
	Lead      string    `json:"lead,omitempty"`
	Goals     []string  `json:"goals,omitempty"`
	Channel   string    `json:"channel,omitempty"`
	Knowledge []string  `json:"knowledge,omitempty"`
	Roles     []OrgSeat `json:"roles,omitempty"`
	Children  []OrgUnit `json:"children,omitempty"`
}

// orgProjection builds the anonymous view of the current company.
//
// Read through the source on every call rather than captured, because an apply
// replaces the company and a projection built at boot would keep showing a
// seat a revision deleted.
//
// Every slice is COPIED, which the marshal round trip this replaced did
// implicitly. The projection is handed to the socket hub and to REST callers,
// while the applied company is shared by every reader of the epoch, so an
// aliased slice would make the projection only as immutable as every one of
// those readers is careful.
func orgProjection(company func() *config.Company) OrgProjection {
	if company == nil {
		return OrgProjection{}
	}
	c := company()
	if c == nil {
		return OrgProjection{}
	}
	derived := config.Derive(c).WithoutPaths()
	return OrgProjection{
		Name:     c.Name,
		Mission:  c.Mission,
		Vision:   c.Vision,
		Policies: slices.Clone(c.Policies),
		Roles:    orgSeats(c.Roles),
		Units:    orgUnits(c.Units),
		Derived:  &derived,
	}
}

func orgSeats(roles []config.Role) []OrgSeat {
	if len(roles) == 0 {
		return nil
	}
	out := make([]OrgSeat, 0, len(roles))
	for i := range roles {
		r := &roles[i]
		out = append(out, OrgSeat{
			Name:                 r.Name,
			Kind:                 string(r.Kind),
			Handle:               r.Handle,
			Goal:                 r.Goal,
			Backstory:            r.Backstory,
			Responsibilities:     slices.Clone(r.Responsibilities),
			BehavioralGuidelines: slices.Clone(r.BehavioralGuidelines),
			Manages:              slices.Clone(r.Manages),
			Availability:         r.Availability,
		})
	}
	return out
}

func orgUnits(units []config.Unit) []OrgUnit {
	if len(units) == 0 {
		return nil
	}
	out := make([]OrgUnit, 0, len(units))
	for i := range units {
		u := &units[i]
		out = append(out, OrgUnit{
			ID:        u.IdentityKey(),
			Name:      u.Name,
			Type:      string(u.Type),
			Purpose:   u.Purpose,
			Lead:      u.Lead,
			Goals:     slices.Clone(u.Goals),
			Channel:   u.Channel,
			Knowledge: slices.Clone(u.Knowledge),
			Roles:     orgSeats(u.Roles),
			Children:  orgUnits(u.Children),
		})
	}
	return out
}
