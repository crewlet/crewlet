package scaleway

import (
	"context"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// OpenAccess opens the registered access vendor with a transport pointed at a
// fake and a clock, which the production Open cannot be handed.
func OpenAccess(
	_ context.Context, vendor seataccess.Vendor, in seataccess.OpenInput, hc *http.Client, now func() time.Time,
) (seataccess.Session, error) {
	return vendor.(accessVendor).open(in, hc, now)
}

// SetWait replaces how a client waits out a rate limit, so a test can record
// the wait rather than spend it.
func SetWait(c *Client, wait func(context.Context, time.Duration) error) { c.wait = wait }

// LevelOf is the level rules hold, "" where they are none exactly, and the
// projects they name.
func LevelOf(rules []Rule) (string, []string) { return heldBy(rules) }

// RulesFor is a seat's policy at a level on its projects.
func RulesFor(level, org string, projects []string) ([]RuleSpec, error) {
	return rulesFor(level, org, projects)
}

// NeverGranted is what no level may hold.
func NeverGranted() []string { return append([]string(nil), neverGranted...) }

// ProvisionerSets is what the provisioner's own policy must grant.
func ProvisionerSets() []string { return append([]string(nil), provisionerSets...) }

// LevelAdds is each level's ID and the project sets it adds to the levels
// before it, least access first.
func LevelAdds() (ids []string, adds [][]string) {
	for _, l := range levels {
		ids, adds = append(ids, l.id), append(adds, append([]string(nil), l.adds...))
	}
	return ids, adds
}
