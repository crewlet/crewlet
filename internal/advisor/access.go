package advisor

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// AccessKey is one grant an agent may ask a person for, from a closed list
// (ADR-0118). It is also the id of the option that grants it, so a decision
// names the grant and nothing else: the change is built here, from the
// company document, for the agent that asked, never from words it wrote.
type AccessKey string

// AccessPrefix begins every [AccessKey], so a reader tells a grant option
// from any other option by its id: an id no other decision may use.
const AccessPrefix = decision.ReservedOptionPrefix

// AccessHealth is the grant of the engine's health tools. Every other grant is
// a GitHub permission at one level ([GitHubAccessKey]), derived from what a
// seat can hold ([github.SeatPermissions]) rather than listed by hand.
const AccessHealth AccessKey = "grant-health"

// GitHubAccessKey is the grant of permission at level on the seat's own GitHub
// App: `grant-github-<permission>-<level>`, the key's underscores as hyphens.
func GitHubAccessKey(permission string, level config.GitHubAccess) AccessKey {
	return AccessKey(AccessPrefix + "github-" + strings.ReplaceAll(permission, "_", "-") +
		"-" + string(level))
}

// accessGrant is one key's meaning: what it lets an agent do, whether a seat
// holds it, and the fields of the seat's entity that grant it.
type accessGrant struct {
	key AccessKey
	// does finishes "Allow <agent> to ..." and "<agent> can ...".
	does string
	// lane is the access lane whose grant holds it, "" for none.
	lane seataccess.Lane
	held func(seat *org.Role) bool
	// set is the top-level fields of the role entity that grant it, and
	// merge the nested ones; either may be nil.
	set, merge func() map[string]any
	// takes reports whether a seat can be granted it at all.
	takes func(seat *org.Role) bool
}

// accessGrants is the catalogue: the health tools, then every level of every
// permission a seat's GitHub App may hold, in the Agents tab's order.
var accessGrants = func() []accessGrant {
	out := []accessGrant{{
		key: AccessHealth, does: "watch the engine's health and wake on its alarms",
		held:  func(s *org.Role) bool { return s.HealthTools },
		takes: func(*org.Role) bool { return true },
		set:   func() map[string]any { return map[string]any{"health_tools": true} },
	}}
	for _, p := range github.SeatPermissions {
		for _, level := range p.Levels {
			out = append(out, gitHubGrant(p, level))
		}
	}
	return out
}()

// gitHubGrant is the grant of permission p at level on the seat's own GitHub
// App, which it must already have: that field and whatever the level needs
// ([github.Needs]), merged into the seat's entity as the person's preview reads it.
func gitHubGrant(p github.Permission, level config.GitHubAccess) accessGrant {
	does := "hold GitHub's " + p.Name + " permission at " + level.Label()
	fields := map[string]any{p.Key: string(level)}
	for _, need := range github.Needs(p.Key, level) {
		with, _ := github.LookupPermission(need.Key)
		does += ", with " + with.Name + " at " + need.Level.Label() + ", which it needs"
		fields[need.Key] = string(need.Level)
	}
	return accessGrant{
		key: GitHubAccessKey(p.Key, level), does: does, lane: seataccess.LaneGitHub,
		held: func(s *org.Role) bool {
			return s.GitHub != nil && config.GitHubAccess(s.GitHub.Permissions[p.Key]).Rank() >= level.Rank()
		},
		takes: func(s *org.Role) bool { return s.GitHub != nil && s.GitHub.Enabled },
		merge: func() map[string]any {
			return map[string]any{"integrations": map[string]any{"github": map[string]any{
				"permissions": maps.Clone(fields)}}}
		},
	}
}

// ErrAccessHeld is a grant the seat holds already; ErrAccessNotTakable one it
// cannot be given (a GitHub permission for a seat with no GitHub App);
// ErrAccessUnknown a key the catalogue does not name.
var (
	ErrAccessHeld       = errors.New("advisor: the agent already has this access")
	ErrAccessNotTakable = errors.New("advisor: the agent has nothing this access could be added to")
	ErrAccessUnknown    = errors.New("advisor: no such access")
)

// AccessKeys is every grant an agent may ask for, in the catalogue's order.
func AccessKeys() []AccessKey {
	out := make([]AccessKey, len(accessGrants))
	for i, g := range accessGrants {
		out[i] = g.key
	}
	return out
}

func grantOf(key AccessKey) (accessGrant, bool) {
	i := slices.IndexFunc(accessGrants, func(g accessGrant) bool { return g.key == key })
	if i < 0 {
		return accessGrant{}, false
	}
	return accessGrants[i], true
}

// Valid reports whether k is in the catalogue.
func (k AccessKey) Valid() bool {
	_, ok := grantOf(k)
	return ok
}

// Does is what the grant lets an agent do, to finish "Allow <agent> to ...".
func (k AccessKey) Does() string {
	g, _ := grantOf(k)
	return g.does
}

// Place is where in Crewlet an administrator sets the integration grant k is
// part of ([seataccess.GrantPlace]), "" for a grant no integration holds.
func (k AccessKey) Place() string {
	if g, ok := grantOf(k); ok && g.lane != "" {
		return seataccess.GrantPlace(g.lane)
	}
	return ""
}

// CanAsk reports whether seat may ask for k: a key it does not hold, on
// something it has, or the error saying why not.
func CanAsk(seat *org.Role, k AccessKey) error {
	g, ok := grantOf(k)
	switch {
	case !ok:
		return ErrAccessUnknown
	case !g.takes(seat):
		return ErrAccessNotTakable
	case g.held(seat):
		return ErrAccessHeld
	}
	return nil
}

// AccessFix is the fix that grants k to the agent seat with handle, built from
// c as the config surface reads it: one entity change, previewed and sent by
// a person (ADR-0115), never applied here.
func AccessFix(c *config.Company, handle string, k AccessKey) (Fix, error) {
	g, ok := grantOf(k)
	if !ok {
		return Fix{}, ErrAccessUnknown
	}
	for role := range c.EachRole() {
		seat := role.Seat()
		if seat.Handle() != handle || role.Kind == org.KindHuman {
			continue
		}
		if err := CanAsk(seat, k); err != nil {
			return Fix{}, err
		}
		change := Change{Entity: EntityRoles, ID: handle, Set: map[string]any{},
			Summary: fmt.Sprintf("Allow %s to %s", handle, g.does)}
		if g.set != nil {
			change.Set = g.set()
		}
		if g.merge != nil {
			change.Merge = g.merge()
		}
		allow := fmt.Sprintf("Allow %s to %s", role.Name, g.does)
		return Fix{
			Kind: FixConfigChange, Sentence: allow + ".",
			Unchanged: fmt.Sprintf("%s gets no other new access.", role.Name),
			Options:   []Option{{Label: allow, Change: change}},
		}, nil
	}
	return Fix{}, fmt.Errorf("advisor: no agent seat is %q", handle)
}

// IsAccessOption reports whether an option id names a grant.
func IsAccessOption(id string) bool {
	return strings.HasPrefix(id, AccessPrefix) && AccessKey(id).Valid()
}
