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
	"github.com/crewlet/crewlet/internal/words"
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
		Steps: steps,
		Note: vendortext.Of(vendortext.W("Only an "), vendortext.UI("Administrator"),
			vendortext.W(" can add users. Added under "), vendortext.UI("Account"), vendortext.W(" rather than "),
			vendortext.UI("Property"), vendortext.W(", it reaches every property of the account; Crewlet still "+
				"holds the agent to its own.")),
		Scopes: []string{scopeReadonly},
		Reach:  reach,
		Admin: &googlegrowth.Admin{Who: "a Google Analytics Administrator", Roles: adminRoles, Add: add,
			Remove: remove, Unavailable: adminUnavailable},
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
// Editor, which access bindings can grant; the Marketer role Google Analytics
// offers covers only some of them, and its API cannot grant it.
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

// steps are a person's steps in Google Analytics.
func steps(g googlegrowth.Guided) []seataccess.GuideStep {
	return []seataccess.GuideStep{
		{Text: vendortext.Of(vendortext.W("Open "), vendortext.UI("Google Analytics", "Admin").At(accessManagementURL),
			vendortext.W("."))},
		{Text: vendortext.Of(vendortext.W("Under "), vendortext.UI("Property"),
			vendortext.W(", choose "+said(propertyNames(g))+", then press "),
			vendortext.UI("Property access management"), vendortext.W("."))},
		{Text: vendortext.Of(vendortext.W("Press "), vendortext.UI("+", "Add users"), vendortext.W("."))},
		g.Copy("Email address"),
		{Text: vendortext.Of(append(append([]vendortext.Part{vendortext.UI("Role"), vendortext.W(": ")},
			vendortext.And(g.NeedNames...)...), vendortext.W(". Press "), vendortext.UI("Add"), vendortext.W("."))...)},
	}
}

// propertyNames are the seat's properties as the steps name them.
func propertyNames(g googlegrowth.Guided) []string {
	out := make([]string, len(g.Entries))
	for i, id := range g.Entries {
		out[i] = "property " + id
	}
	return out
}

// said is phrases as one list a sentence says.
func said(phrases []string) string {
	out := make([]words.Said, len(phrases))
	for i, p := range phrases {
		out[i] = words.Said(p)
	}
	return words.And(out)
}

// adminUnavailable is why Crewlet cannot add an identity for c.
func adminUnavailable(c *config.Company) string {
	if c == nil || c.Integrations.GoogleAnalytics == nil || c.Integrations.GoogleAnalytics.OAuth == nil {
		return "A Google Analytics Administrator signs in on the Google page first."
	}
	return ""
}

// reach lists the account summaries the identity reaches: each of the seat's
// properties is reached where it is listed, under its account.
func reach(ctx context.Context, in googlegrowth.Probe) ([]googlegrowth.Reached, error) {
	type found struct{ name, account string }
	listed := map[string]found{}
	token := ""
	for {
		q := url.Values{"pageSize": {"200"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			AccountSummaries []struct {
				Account           string `json:"account"`
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
				listed[strings.TrimPrefix(p.Property, "properties/")] = found{name: p.DisplayName, account: a.Account}
			}
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	out := make([]googlegrowth.Reached, 0, len(in.Scope))
	for _, id := range in.Scope {
		f, ok := listed[id]
		r := googlegrowth.Reached{Entry: id, Name: f.name, Parent: f.account, OK: ok}
		if !ok {
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
