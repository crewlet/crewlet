package searchconsole

import (
	"context"
	"net/http"
	"sync"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// identity is Search Console's half of a seat's own identity: a permission on
// each of the seat's properties, which only an owner adds, by hand; the
// caller's own permission is in its list of sites. Search Console keeps no
// history of who did what, so the identity's worth here is removing one agent
// alone.
var identity = sync.OnceValue(func() *googlegrowth.Identity {
	return &googlegrowth.Identity{
		Entry: "property", Entries: "properties",
		Roles: googlegrowth.Roles{Ladder: []googlegrowth.RoleName{
			{ID: roleRestricted, Name: "Restricted user"}, {ID: roleFull, Name: "Full user"},
			{ID: roleOwner, Name: "Owner"},
		}},
		Needs: needs,
		Link:  seataccess.GuideLink{Label: "Open Search Console", URL: usersURL},
		Steps: steps,
		Note: "Only an owner can add users, at most 100 per property. Search Console keeps no history of who did " +
			"what; the agent's own identity lets you remove it alone.",
		Scopes: []string{scopeReadonly},
		Reach:  reach,
	}
})

// needs is the permission each level needs on each property.
func needs(level growth.Level) googlegrowth.Need {
	switch level {
	case growth.LevelOperate:
		return googlegrowth.Need{Roles: []string{roleFull}, More: "The Indexing API needs Owner."}
	case growth.LevelAdminister:
		return googlegrowth.Need{Roles: []string{roleOwner}}
	}
	return googlegrowth.Need{Roles: []string{roleRestricted}, Broad: []string{roleOwner}}
}

// steps are an owner's steps in Search Console.
func steps(g googlegrowth.Guided) []seataccess.GuideStep {
	return []seataccess.GuideStep{
		{Text: "Open Search Console and choose " + said(g.Entries) + "."},
		{Text: "Open Settings, then Users and permissions."},
		{Text: "Press Add user."},
		g.Copy("Email address:"),
		{Text: "Permission: " + g.NeedWords + ". Save."},
	}
}

// said is IDs as one list a sentence says.
func said(ids []string) string {
	out := make([]words.Said, len(ids))
	for i, id := range ids {
		out[i] = words.Said(id)
	}
	return words.And(out)
}

// permission is each permission level sites.list answers, as the catalog
// names it.
var permission = map[string]string{
	"siteRestrictedUser": roleRestricted, "siteFullUser": roleFull, "siteOwner": roleOwner,
	"siteUnverifiedUser": roleUnverified,
}

// reach lists the identity's sites: each of the seat's properties is reached
// where it is listed with a permission, which is its role.
func reach(ctx context.Context, in googlegrowth.Probe) ([]googlegrowth.Reached, error) {
	var page struct {
		SiteEntry []struct {
			SiteURL         string `json:"siteUrl"`
			PermissionLevel string `json:"permissionLevel"`
		} `json:"siteEntry"`
	}
	if err := in.Send.Do(ctx, http.MethodGet, searchAPI+"/webmasters/v3/sites", in.Token, nil, &page); err != nil {
		return nil, err
	}
	levels := map[string]string{}
	for _, e := range page.SiteEntry {
		levels[e.SiteURL] = permission[e.PermissionLevel]
	}
	out := make([]googlegrowth.Reached, 0, len(in.Scope))
	for _, site := range in.Scope {
		role, listed := levels[site]
		r := googlegrowth.Reached{Entry: site}
		switch {
		case !listed:
			r.Said = "not among the properties it reaches"
		case role == roleUnverified || role == "":
			r.Said = "listed with no permission"
		default:
			r.OK, r.Roles, r.Read = true, []string{role}, true
		}
		out = append(out, r)
	}
	return out, nil
}
