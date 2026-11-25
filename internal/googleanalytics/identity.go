package googleanalytics

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// predefined is how the Admin API names a role ("predefinedRoles/viewer").
const predefined = "predefinedRoles/"

// identity is Google Analytics' half of a seat's own identity: a role on each
// of the seat's properties, added in Access Management or through access
// bindings with an Administrator's sign-in.
var identity = sync.OnceValue(func() *googlegrowth.Identity {
	return &googlegrowth.Identity{
		Records: "its changes under its own email in the change history",
		Entry:   "property", Entries: "properties",
		Roles: ladder(),
		Needs: needs,
		RoleNeeds: func(string) googlegrowth.Need {
			return googlegrowth.Need{Roles: []string{roleMarketer}, Broad: []string{roleEditor, roleAdministrator},
				Manual: "Google Analytics' API cannot grant Marketer, so a person adds it in Google Analytics."}
		},
		Steps: steps,
		Note: vendortext.Of(vendortext.W("Only an "), vendortext.UI("Administrator"),
			vendortext.W(" can add users.")),
		Scopes: []string{scopeReadonly},
		Reach:  reach,
		Admin: &googlegrowth.Admin{Who: "a Google Analytics Administrator", Roles: adminRoles, Add: add,
			Remove: remove, Unavailable: adminUnavailable},
		AddURL: func(l googlegrowth.Listed) string { return adminPage(l, "property") },
		Wider: &googlegrowth.Wider{Label: "Add it once to the account instead",
			Does: vendortext.Of(vendortext.W("Under "), vendortext.UI("Account access management"),
				vendortext.W(", one add covers every property of the account, now and later. That is broader "+
					"access: the identity reaches every property there, though Crewlet still holds the agent to "+
					"its own.")),
			URL: func(l googlegrowth.Listed) string {
				if accountOf(l) == "" {
					return ""
				}
				return adminPage(l, "account")
			}},
	}
})

// ladder is the property roles, least first, each holding every one below.
func ladder() googlegrowth.Roles {
	return googlegrowth.Roles{Ladder: []googlegrowth.RoleName{
		{ID: roleViewer, Name: "Viewer"}, {ID: roleAnalyst, Name: "Analyst"}, {ID: roleMarketer, Name: "Marketer"},
		{ID: roleEditor, Name: "Editor"}, {ID: roleAdministrator, Name: "Administrator"},
	}}
}

// needs is the role each level needs on each property. Operate's changes need
// Editor, which access bindings can grant; operate as Marketer (RoleNeeds)
// narrows its changes to Marketer's, which only a person can grant.
func needs(level growth.Level) googlegrowth.Need {
	switch level {
	case growth.LevelOperate:
		return googlegrowth.Need{Roles: []string{roleEditor}, Broad: []string{roleAdministrator}}
	case growth.LevelAdminister:
		return googlegrowth.Need{Roles: []string{roleEditor}, More: vendortext.Of(vendortext.W("Access changes need "),
			vendortext.UI("Administrator"), vendortext.W("."))}
	}
	return googlegrowth.Need{Roles: []string{roleViewer}, Broad: []string{roleAdministrator}}
}

// steps are a person's steps in Google Analytics, once for every property
// the sheet lists, each of which links its own access page.
func steps(g googlegrowth.Guided) []seataccess.GuideStep {
	return []seataccess.GuideStep{
		{Text: vendortext.Of(vendortext.W("Open each property below with its button, or go to "),
			vendortext.UI("Google Analytics", "Admin", "Property access management").At(adminHome),
			vendortext.W(" with the property chosen."))},
		{Text: vendortext.Of(vendortext.W("Press "), vendortext.UI("+", "Add users"), vendortext.W("."))},
		g.Copy("Email address"),
		{Text: vendortext.Of(vendortext.W("Untick "), vendortext.UI("Notify new users by email"),
			vendortext.W(": nobody reads the identity's mail."))},
		{Text: vendortext.Of(append(append([]vendortext.Part{vendortext.UI("Role"), vendortext.W(": ")},
			vendortext.And(g.NeedNames...)...), vendortext.W(". Press "), vendortext.UI("Add"), vendortext.W("."))...)},
	}
}

// adminPage is Google Analytics' access management for l's property, or for
// its account where at is "account": the property's account is in the address
// where known, which Google Analytics fills in where it is not.
func adminPage(l googlegrowth.Listed, at string) string {
	place := "p" + l.ID
	if account := accountOf(l); account != "" {
		place = "a" + account + place
	}
	return adminHome + "#/" + place + "/admin/suiteusermanagement/" + at
}

// accountOf is l's account's number, "" where the listing named none.
func accountOf(l googlegrowth.Listed) string {
	account, ok := strings.CutPrefix(l.Parent, "accounts/")
	if !ok || !digits(account) {
		return ""
	}
	return account
}

// digits reports a non-empty run of ASCII digits.
func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// adminUnavailable is why Crewlet cannot add an identity for c.
func adminUnavailable(c *config.Company) string {
	if c == nil || c.Integrations.GoogleAnalytics == nil || c.Integrations.GoogleAnalytics.OAuth == nil {
		return "A Google Analytics Administrator signs in on the Google page first."
	}
	return ""
}

// list is every property the token reaches, under its account, as the
// account summaries list them.
func list(ctx context.Context, in googlegrowth.Lister) ([]googlegrowth.Listed, error) {
	var out []googlegrowth.Listed
	token := ""
	for {
		q := url.Values{"pageSize": {"200"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			AccountSummaries []struct {
				Account           string `json:"account"`
				DisplayName       string `json:"displayName"`
				PropertySummaries []struct {
					Property    string `json:"property"`
					DisplayName string `json:"displayName"`
				} `json:"propertySummaries"`
			} `json:"accountSummaries"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := in.Send.Do(ctx, http.MethodGet, adminAPI+"/v1beta/accountSummaries?"+q.Encode(), in.Token,
			nil, &page); err != nil {
			return nil, err
		}
		for _, a := range page.AccountSummaries {
			for _, p := range a.PropertySummaries {
				out = append(out, googlegrowth.Listed{ID: strings.TrimPrefix(p.Property, "properties/"),
					Name: p.DisplayName, Parent: a.Account, ParentName: a.DisplayName})
			}
		}
		if token = page.NextPageToken; token == "" {
			return out, nil
		}
	}
}

// reach is the seat's properties as the identity's own listing finds them:
// a listed property is reached under its account, at Viewer at least, the
// least role there is.
func reach(ctx context.Context, in googlegrowth.Probe) ([]googlegrowth.Reached, error) {
	listed, err := list(ctx, googlegrowth.Lister{Company: in.Company, Token: in.Token, Send: in.Send})
	if err != nil {
		return nil, err
	}
	found := map[string]googlegrowth.Listed{}
	for _, l := range listed {
		found[l.ID] = l
	}
	out := make([]googlegrowth.Reached, 0, len(in.Scope))
	for _, id := range in.Scope {
		l, ok := found[id]
		r := googlegrowth.Reached{Entry: id, Name: l.Name, Parent: l.Parent, ParentName: l.ParentName, OK: ok}
		if ok {
			r.Roles, r.Read, r.AtLeast = []string{roleViewer}, true, true
		} else {
			r.Said = "not among the properties it reaches"
		}
		out = append(out, r)
	}
	return out, nil
}

// binding is one access binding.
type binding struct {
	Name  string   `json:"name,omitempty"`
	User  string   `json:"user"`
	Roles []string `json:"roles"`
}

// bindingOf is the identity's binding on parent (a property or an account),
// found false where it holds none.
func bindingOf(ctx context.Context, in googlegrowth.AdminCall, parent string) (binding, bool, error) {
	token := ""
	for {
		q := url.Values{"pageSize": {"500"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			AccessBindings []binding `json:"accessBindings"`
			NextPageToken  string    `json:"nextPageToken"`
		}
		if err := in.Send.Do(ctx, http.MethodGet, adminAPI+"/v1alpha/"+parent+"/accessBindings?"+q.Encode(),
			in.Token, nil, &page); err != nil {
			return binding{}, false, err
		}
		for _, b := range page.AccessBindings {
			if strings.EqualFold(b.User, in.Email) {
				return b, true, nil
			}
		}
		if token = page.NextPageToken; token == "" {
			return binding{}, false, nil
		}
	}
}

// roleIDs are a binding's roles as the catalog names them.
func roleIDs(b binding) []string {
	out := make([]string, 0, len(b.Roles))
	for _, r := range b.Roles {
		out = append(out, strings.TrimPrefix(r, predefined))
	}
	return out
}

// adminRoles reads the identity's roles on each property: its binding there
// and on the property's account, whose roles every property inherits.
func adminRoles(ctx context.Context, in googlegrowth.AdminCall) (map[string][]string, error) {
	out := map[string][]string{}
	accounts := map[string][]string{}
	for _, r := range in.Reached {
		held, err := heldOn(ctx, in, r, accounts)
		if err != nil {
			return nil, err
		}
		if len(held) > 0 {
			out[r.Entry] = held
		}
	}
	return out, nil
}

// heldOn is the roles the identity holds on r's property, its account's
// included, read once per account into accounts.
func heldOn(ctx context.Context, in googlegrowth.AdminCall, r googlegrowth.Reached,
	accounts map[string][]string) ([]string, error) {
	b, found, err := bindingOf(ctx, in, "properties/"+r.Entry)
	if unseen(err) {
		// AN ADMINISTRATOR OF SOME PROPERTIES ONLY sees none of this one's.
		found, err = false, nil
	}
	if err != nil {
		return nil, err
	}
	var held []string
	if found {
		held = roleIDs(b)
	}
	if r.Parent != "" {
		inherited, read := accounts[r.Parent]
		if !read {
			ab, found, err := bindingOf(ctx, in, r.Parent)
			if unseen(err) {
				found, err = false, nil
			}
			if err != nil {
				return nil, err
			}
			if found {
				inherited = roleIDs(ab)
			}
			accounts[r.Parent] = inherited
		}
		held = append(held, inherited...)
	}
	slices.Sort(held)
	return slices.Compact(held), nil
}

// add gives the identity the level's role on each property where it holds no
// role at or above it, keeping any data restriction its binding carries.
func add(ctx context.Context, in googlegrowth.AdminCall) error {
	roles := ladder()
	accounts := map[string][]string{}
	for _, id := range in.Scope {
		r := googlegrowth.Reached{Entry: id}
		if i := slices.IndexFunc(in.Reached, func(r googlegrowth.Reached) bool { return r.Entry == id }); i >= 0 {
			r = in.Reached[i]
		}
		held, err := heldOn(ctx, in, r, accounts)
		if err != nil {
			return err
		}
		if len(roles.Missing(held, in.Need)) == 0 {
			continue
		}
		want := []string{}
		for _, role := range in.Need.Roles {
			want = append(want, predefined+role)
		}
		b, found, err := bindingOf(ctx, in, "properties/"+id)
		if err != nil {
			return err
		}
		if !found {
			err = in.Send.Do(ctx, http.MethodPost, adminAPI+"/v1alpha/properties/"+url.PathEscape(id)+
				"/accessBindings", in.Token, binding{User: in.Email, Roles: want}, nil)
		} else {
			for _, role := range b.Roles {
				if restriction(role) {
					want = append(want, role)
				}
			}
			err = in.Send.Do(ctx, http.MethodPatch, adminAPI+"/v1alpha/"+b.Name, in.Token,
				binding{Name: b.Name, User: b.User, Roles: want}, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// restriction reports a data restriction, which stays beside any role.
func restriction(role string) bool {
	id := strings.TrimPrefix(role, predefined)
	return id == roleNoCost || id == roleNoRevenue
}

// remove deletes the identity's binding on each of the seat's properties.
func remove(ctx context.Context, in googlegrowth.AdminCall) error {
	for _, id := range in.Scope {
		b, found, err := bindingOf(ctx, in, "properties/"+id)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if err := in.Send.Do(ctx, http.MethodDelete, adminAPI+"/v1alpha/"+b.Name, in.Token, nil, nil); err != nil &&
			googleauth.Status(err) != http.StatusNotFound {
			return err
		}
	}
	return nil
}

// adminClient is the OAuth client an admin signs in as (googleauth.SignInClient),
// false where Google Analytics is off.
func adminClient(c *config.Company) (googleauth.ConsentClient, bool) {
	b := c.Integrations.GoogleAnalytics
	if b == nil || !b.Enabled {
		return googleauth.ConsentClient{}, false
	}
	return googleauth.SignInClient(c, b.OAuth), true
}

// adminScopes are what the admin sign-in asks for: managing users, and
// nothing a seat's call needs.
func adminSignInScopes(*config.Company) []string { return []string{scopeUsers} }

// unseen reports a binding read the admin may not make or that names nothing:
// its roles are unread, never a fault of the whole check.
func unseen(err error) bool {
	s := googleauth.Status(err)
	return s == http.StatusForbidden || s == http.StatusNotFound
}
