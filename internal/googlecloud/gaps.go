package googlecloud

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// GOOGLE CHANGES ITS PREDEFINED ROLES, and did: Cloud SQL Editor gained
// cloudsql.sslCerts.create live. So every predefined role a level grants is
// read for its permissions, and a level one of them no longer suits is
// unavailable until the table names another role in its place.

// levelGaps is each level Google's roles cannot be granted at now: a role
// Google no longer has, or one holding a permission the level may not hold
// ([barred]). A read that fails is an error, since no level can be vouched for.
func levelGaps(ctx context.Context, c *Client) ([]seataccess.LevelGap, error) {
	firstAt := predefinedRoles()
	names := slices.Sorted(maps.Keys(firstAt))
	read := make([]Role, len(names))
	found := make([]bool, len(names))
	errs := make([]error, len(names))
	gate := make(chan struct{}, discoverReads)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			gate <- struct{}{}
			defer func() { <-gate }()
			read[i], found[i], errs[i] = c.Role(ctx, name)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("googlecloud: read the role %s a level grants: %w", names[i], err)
		}
	}
	var out []seataccess.LevelGap
	for at, l := range levels {
		var missing []string
		var reasons []words.Said
		for i, name := range names {
			if _, first, _ := levelOf(firstAt[name]); first > at {
				continue
			}
			switch held := barred(l.id, read[i].Permissions); {
			case !found[i] || read[i].Deleted:
				missing = append(missing, name)
				reasons = append(reasons, words.Said("Google has no role "+name))
			case len(held) > 0:
				missing = append(missing, name)
				reasons = append(reasons, words.Said(name+" now holds "+words.And(held)))
			}
		}
		if len(missing) == 0 {
			continue
		}
		reason := fmt.Sprintf("The %s level is unavailable: %s, which %s may not hold", l.name, words.And(reasons), l.name)
		out = append(out, seataccess.LevelGap{
			Level: l.id, Name: l.name, Missing: missing, Reason: reason, ActionURL: RolesURL,
			Remedy: "Crewlet grants no level whose roles reach another identity, a secret or billing, so this " +
				"level waits on a build whose level table names other roles",
			Blocking: integration.Finding{
				Kind: integration.FindingUnknownTier, Subject: "google_cloud level " + l.id,
				Title: "Google changed a level's roles", Detail: reason + ", so no agent can be given that level",
				Remedy:    "Move the agents at this level to another one on the Agents tab",
				ActionURL: RolesURL, Subjects: missing,
			},
		})
	}
	return out, nil
}
