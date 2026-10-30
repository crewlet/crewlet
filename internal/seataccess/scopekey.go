package seataccess

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// ScopeKey is a scope entry spelled as the lane's vendor matches it: a Jira
// project key ([config.JiraProjectKey]) or a Confluence space key
// ([config.ConfluenceSpaceKey]) as the vendor stores it, and any other lane's
// entry trimmed and otherwise as written.
//
// ONE SPELLING ON BOTH SIDES of every comparison: a seat's list, the
// connection's reach ([Options.Reaches]) and what the vendor holds, so a key
// typed in lower case is within reach and does not read as drift on every
// pass.
func ScopeKey(lane Lane, entry string) string {
	switch lane {
	case LaneJira:
		return config.JiraProjectKey(entry)
	case LaneConfluence:
		return config.ConfluenceSpaceKey(entry)
	default:
		return strings.TrimSpace(entry)
	}
}

// NormalizedFor is [Desired.Normalized] with each scope entry spelled as the
// lane's vendor matches it ([ScopeKey]), which is the form the document is
// read and written in, the ledger records and the plan compares.
func (d Desired) NormalizedFor(lane Lane) Desired {
	out := d.Normalized()
	if len(out.Scope) == 0 {
		return out
	}
	keys := make([]string, 0, len(out.Scope))
	for _, entry := range out.Scope {
		keys = append(keys, ScopeKey(lane, entry))
	}
	out.Scope = normalSet(keys)
	return out
}

// Reaches reports an entry within what the company's connection reaches: any
// entry where the connection does not list its reach ([Options.ScopeOpen]),
// and otherwise one of [Options.Scope], both compared as the lane's vendor
// matches them ([ScopeKey]).
func (o Options) Reaches(entry string) bool {
	if o.ScopeOpen {
		return true
	}
	key := ScopeKey(o.Lane, entry)
	return key != "" && slices.ContainsFunc(o.Scope, func(s string) bool {
		return ScopeKey(o.Lane, s) == key
	})
}

// MissingRequired is [Options.RequiredLevel] where level is a set that names
// other levels but leaves it out, and false otherwise: an empty level (product
// access only) and a lane without a required level never miss it.
func (o Options) MissingRequired(level string) (Level, bool) {
	ids := strings.Fields(level)
	if !o.Multiple || o.RequiredLevel == "" || len(ids) == 0 || slices.Contains(ids, o.RequiredLevel) {
		return Level{}, false
	}
	required, _, ok := o.Level(o.RequiredLevel)
	if !ok {
		required = Level{ID: o.RequiredLevel, Name: o.RequiredLevel}
	}
	return required, true
}
