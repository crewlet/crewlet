package cloudflare

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

// LevelOfPolicies is the level a token's policies hold against a catalogue
// built from groups, zones and the configured zone names.
func LevelOfPolicies(accountID string, groups []PermissionGroup, zones []Zone, configured []string, held []Policy) string {
	return newCatalogue(accountID, groups, zones, configured).levelOf(held)
}

// PoliciesFor is the policy a level grants against the same catalogue.
func PoliciesFor(accountID string, groups []PermissionGroup, zones []Zone, configured []string, level string) ([]Policy, error) {
	cat := newCatalogue(accountID, groups, zones, configured)
	g, err := cat.grantFor(level)
	if err != nil {
		return nil, err
	}
	return cat.policies(g), nil
}
