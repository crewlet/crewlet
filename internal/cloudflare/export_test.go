package cloudflare

import (
	"context"
	"net/http"
	"strings"
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
func SetWait(c *Client, wait func(context.Context, time.Duration) error) { c.limit.Wait = wait }

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

// LevelAdds is each level's ID and the groups it adds to the levels before
// it, least access first.
func LevelAdds() (ids []string, adds [][]string) {
	for _, l := range levels {
		ids, adds = append(ids, l.id), append(adds, names(l.adds))
	}
	return ids, adds
}

// ProvisionerOwnGroups is the groups the provisioner holds for itself.
func ProvisionerOwnGroups() []string { return names(provisionerOwnPermissions) }

// BuilderPath is a group's place in the token builder as the docs page writes
// it ("Zone, Cache Rules, Read"), or "" for one this integration does not name.
func BuilderPath(name string) string {
	for _, p := range provisionerPermissions() {
		if p.name != name {
			continue
		}
		scope := "Account"
		if p.zone {
			scope = "Zone"
		}
		resource, access, _ := strings.Cut(p.builder, ": ")
		return scope + ", " + resource + ", " + access
	}
	return ""
}
