package config

import (
	"slices"
	"strings"
)

// SeatAccessMigratedFor reports whether kind's agent seats state their access
// per seat in this document: the engine's migration has moved them onto it,
// or the document was written with them. It is the gate the access pass for an
// integration reads before it acts on any seat.
//
// A DOCUMENT CARRIES IT RATHER THAN THE FLEET, so a revert to a revision
// written before the migration closes the gate again: that document's seats
// say nothing per seat, and read as off they would have every account turned
// off.
func (c *Company) SeatAccessMigratedFor(kind string) bool {
	return c != nil && slices.Contains(c.SeatAccessMigrated, kind)
}

// RefuseEngineWritten is the refusal of a write, by anybody but the engine,
// that changes a seat field only the engine writes ([EngineWrittenChanges]): a
// GitHub App's id, slug or installation, which the engine records after GitHub
// answers. Nil where the write changes none.
//
// A person or a model writing one would point an agent at an app that is not
// its own, so the field is the engine's alone (design section 2), and the
// refusal names every path the write changed.
func RefuseEngineWritten(prior, next *Company) error {
	paths := EngineWrittenChanges(prior, next)
	if len(paths) == 0 {
		return nil
	}
	return fault(nil, ErrConflict, "%s %s written by the engine once GitHub answers, and "+
		"no other writer may change them; leave them as they are",
		strings.Join(paths, ", "), pluralAre(len(paths)))
}

func pluralAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// SeatAccessConnected reports whether the company connects kind's integration
// at all, which is the only place its seats' access means anything: Jira or
// Confluence for atlassian, and the integration's own block for every other
// kind.
func (c *Company) SeatAccessConnected(kind string) bool {
	if c == nil {
		return false
	}
	in := c.Integrations
	switch kind {
	case "datadog":
		return in.Datadog != nil
	case "gitlab":
		return in.GitLab != nil
	case "atlassian":
		return in.Jira != nil || in.Confluence != nil
	case "mattermost":
		return in.Mattermost != nil
	case "github":
		return in.GitHub != nil
	case "slack":
		return in.Slack != nil
	}
	return false
}

// SeatAccessPending reports an integration the company connects whose seats
// the migration onto per-seat access has not moved yet: the access pass for it
// waits, and says so. An integration the company does not connect is not
// waiting on anything.
func (c *Company) SeatAccessPending(kind string) bool {
	return c.SeatAccessConnected(kind) && !c.SeatAccessMigratedFor(kind)
}
