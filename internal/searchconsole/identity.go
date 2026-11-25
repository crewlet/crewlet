package searchconsole

import (
	"context"
	"net/http"
	"net/url"
	"sync"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/vendortext"
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
		Steps: steps,
		Note: vendortext.Plain("Only an owner can add users, at most 100 per property. Search Console keeps no " +
			"history of who did what; the agent's own identity lets you remove it alone."),
		Scopes: []string{scopeReadonly},
		Reach:  reach,
		AddURL: func(l googlegrowth.Listed) string { return usersURL + "?" + url.Values{"resource_id": {l.ID}}.Encode() },
	}
})

// needs is the permission each level needs on each property.
func needs(level growth.Level) googlegrowth.Need {
	switch level {
	case growth.LevelOperate:
		return googlegrowth.Need{Roles: []string{roleFull}, More: vendortext.Of(vendortext.W("The Indexing API needs "),
			vendortext.UI("Owner"), vendortext.W("."))}
	case growth.LevelAdminister:
		return googlegrowth.Need{Roles: []string{roleOwner}}
	}
	return googlegrowth.Need{Roles: []string{roleRestricted}, Broad: []string{roleOwner}}
}

// steps are an owner's steps in Search Console.
func steps(g googlegrowth.Guided) []seataccess.GuideStep {
	return []seataccess.GuideStep{
		{Text: vendortext.Of(vendortext.W("Open each property below with its button, or go to "),
			vendortext.UI("Search Console", "Settings", "Users and permissions").At(usersURL),
			vendortext.W(" with the property chosen."))},
		{Text: vendortext.Of(vendortext.W("Press "), vendortext.UI("Add user"), vendortext.W("."))},
		g.Copy("Email address"),
		{Text: vendortext.Of(append(append([]vendortext.Part{vendortext.UI("Permission"), vendortext.W(": ")},
			vendortext.And(g.NeedNames...)...), vendortext.W(". "), vendortext.UI("Save"), vendortext.W("."))...)},
	}
}

// permission is each permission level sites.list answers, as the catalog
// names it.
var permission = map[string]string{
	"siteRestrictedUser": roleRestricted, "siteFullUser": roleFull, "siteOwner": roleOwner,
	"siteUnverifiedUser": roleUnverified,
}

// list is every site the token reaches, each with its permission, which is
// its role there.
func list(ctx context.Context, in googlegrowth.Lister) ([]googlegrowth.Listed, error) {
	var page struct {
		SiteEntry []struct {
			SiteURL         string `json:"siteUrl"`
			PermissionLevel string `json:"permissionLevel"`
		} `json:"siteEntry"`
	}
	if err := in.Send.Do(ctx, http.MethodGet, searchAPI+"/webmasters/v3/sites", in.Token, nil, &page); err != nil {
		return nil, err
	}
	out := make([]googlegrowth.Listed, 0, len(page.SiteEntry))
	for _, e := range page.SiteEntry {
		l := googlegrowth.Listed{ID: e.SiteURL}
		if role := permission[e.PermissionLevel]; role != "" {
			l.Roles, l.Read = []string{role}, true
		}
		out = append(out, l)
	}
	return out, nil
}

// reach is the seat's sites as the identity's own listing finds them: each
// reached where it is listed with a permission, which is its role.
func reach(ctx context.Context, in googlegrowth.Probe) ([]googlegrowth.Reached, error) {
	listed, err := list(ctx, googlegrowth.Lister{Company: in.Company, Token: in.Token, Send: in.Send})
	if err != nil {
		return nil, err
	}
	levels := map[string]string{}
	listedAt := map[string]bool{}
	for _, l := range listed {
		listedAt[l.ID] = true
		if len(l.Roles) > 0 {
			levels[l.ID] = l.Roles[0]
		}
	}
	out := make([]googlegrowth.Reached, 0, len(in.Scope))
	for _, site := range in.Scope {
		role := levels[site]
		r := googlegrowth.Reached{Entry: site}
		switch {
		case !listedAt[site]:
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
