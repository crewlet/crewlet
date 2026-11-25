package googleplay

import (
	"cmp"
	"context"
	"errors"
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

// identity is Google Play's half of a seat's own identity: app permissions on
// each of the seat's apps, invited in Play Console or added through the users
// and grants API with a Play Console admin's sign-in.
var identity = sync.OnceValue(func() *googlegrowth.Identity {
	return &googlegrowth.Identity{
		Records: "its changes under its own email in the activity log",
		Entry:   "app", Entries: "apps",
		Roles: googlegrowth.Roles{Permissions: appPermissions(), Every: permAdmin},
		Needs: needs,
		Steps: steps,
		Note: vendortext.Plain("Only the account owner or an admin can invite users. Play can take up to two days " +
			"to apply new permissions."),
		Scopes: []string{scopePublisher},
		Reach:  reach,
		Admin: &googlegrowth.Admin{Who: "a Play Console admin", Roles: adminRoles, Add: add, Remove: remove,
			Unavailable: adminUnavailable},
	}
})

// appPermissions are the permissions a grant gives on one app, by their
// labels in Play Console's App permissions where they differ from the
// catalog's shorter names.
func appPermissions() []googlegrowth.RoleName {
	labels := map[string]string{
		permView:    "View app information (read-only)",
		permTesters: "Manage testing tracks and edit tester lists",
	}
	var out []googlegrowth.RoleName
	for _, r := range roles() {
		if r.ID != permManaged && r.ID != roleThirdPartyStore {
			out = append(out, googlegrowth.RoleName{ID: r.ID, Name: cmp.Or(labels[r.ID], r.Name)})
		}
	}
	return out
}

// needs is what each level needs on each app: the permissions of the calls it
// makes, and of the calls an administrator approves for it, which go as its
// identity too.
func needs(level growth.Level) googlegrowth.Need {
	operate := []string{permView, permReply, permStore, permTesting, permTesters, permProduction}
	switch level {
	case growth.LevelOperate:
		return googlegrowth.Need{Roles: operate, Broad: []string{permAdmin},
			More: vendortext.Of(vendortext.W("Refunds also need "), vendortext.UI("Manage orders and subscriptions"),
				vendortext.W("."))}
	case growth.LevelAdminister:
		return googlegrowth.Need{Roles: append(operate, permOrders),
			More: vendortext.Of(vendortext.W("User and access changes need "),
				vendortext.UI("Admin (all permissions)"), vendortext.W("."))}
	}
	return googlegrowth.Need{Roles: []string{permView}, Broad: []string{permAdmin}}
}

// steps are a person's steps in Play Console.
func steps(g googlegrowth.Guided) []seataccess.GuideStep {
	return []seataccess.GuideStep{
		{Text: vendortext.Of(vendortext.W("Open "), vendortext.UI("Play Console", "Users and permissions").At(usersURL),
			vendortext.W("."))},
		{Text: vendortext.Of(vendortext.W("Press "), vendortext.UI("Invite new users"), vendortext.W("."))},
		g.Copy("Email address"),
		{Text: vendortext.Of(vendortext.W("Under "), vendortext.UI("App permissions"),
			vendortext.W(", add "+said(g.Entries)+"."))},
		{Text: vendortext.Of(append(append([]vendortext.Part{vendortext.W("Tick ")},
			vendortext.And(g.NeedNames...)...), vendortext.W("."))...)},
		{Text: vendortext.Of(vendortext.W("Leave "), vendortext.UI("Account permissions"),
			vendortext.W(" empty, then press "), vendortext.UI("Invite user"), vendortext.W("."))},
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

// adminUnavailable is why Crewlet cannot add an identity for c: no admin
// sign-in to add it with, or no developer account to add it to.
func adminUnavailable(c *config.Company) string {
	var block config.GooglePlay
	if c != nil && c.Integrations.GooglePlay != nil {
		block = *c.Integrations.GooglePlay
	}
	switch {
	case block.OAuth == nil:
		return "A Play Console admin signs in on the Google page first."
	case len(block.DeveloperAccounts) == 0:
		return "Name the Play developer account on the Settings tab first, so Crewlet knows where to add it."
	}
	return ""
}

// reach lists each app's device tier configs as the identity, a read any
// permission allows, holding nobody's words and drawing on no review quota:
// Play answers 403 for an app it is not invited to and 404 for one its account
// does not hold.
func reach(ctx context.Context, in googlegrowth.Probe) ([]googlegrowth.Reached, error) {
	out := make([]googlegrowth.Reached, 0, len(in.Scope))
	for _, app := range in.Scope {
		address := publisherAPI + "/androidpublisher/v3/applications/" + url.PathEscape(app) + "/deviceTierConfigs"
		var ignored struct{}
		err := in.Send.Do(ctx, http.MethodGet, address, in.Token, nil, &ignored)
		switch status := googleauth.Status(err); {
		case err == nil:
			out = append(out, googlegrowth.Reached{Entry: app, OK: true})
		case status == http.StatusForbidden || status == http.StatusNotFound:
			out = append(out, googlegrowth.Reached{Entry: app, Said: googleauth.Sentence(err)})
		default:
			return nil, err
		}
	}
	return out, nil
}

// playUser is a user as users.list answers it.
type playUser struct {
	Name                        string      `json:"name"`
	Email                       string      `json:"email"`
	AccessState                 string      `json:"accessState"`
	DeveloperAccountPermissions []string    `json:"developerAccountPermissions"`
	Grants                      []playGrant `json:"grants"`
}

// playGrant is one app's permissions.
type playGrant struct {
	Name                string   `json:"name,omitempty"`
	PackageName         string   `json:"packageName"`
	AppLevelPermissions []string `json:"appLevelPermissions"`
}

// holds reports a user whose access stands: invited or granted.
func (u playUser) holds() bool {
	return u.AccessState != "INVITATION_EXPIRED" && u.AccessState != "ACCESS_EXPIRED"
}

// on is the permissions u holds on app: its grant's, and its account-wide
// ones where the account holds the app, which global answers where Play's
// list does not say (an app with no grant, of one of several accounts).
func (u playUser) on(app string, global bool) []string {
	var out []string
	for _, g := range u.Grants {
		if g.PackageName == app {
			global = true
			for _, p := range g.AppLevelPermissions {
				out = append(out, appLevel(p))
			}
		}
	}
	if global {
		for _, p := range u.DeveloperAccountPermissions {
			out = append(out, appLevel(strings.TrimSuffix(p, "_GLOBAL")))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// appLevel is a permission as a grant names it, its deprecated spellings read
// as View app information.
func appLevel(p string) string {
	if p == "CAN_ACCESS_APP" || p == "CAN_SEE_ALL_APPS" {
		return permView
	}
	return p
}

// user is the identity's user in developer account dev; found is false
// where it is none, or one whose access expired.
func user(ctx context.Context, in googlegrowth.AdminCall, dev string) (playUser, bool, error) {
	return userOf(ctx, in, dev, playUser.holds)
}

// userOf is the identity's user in dev that keep admits.
func userOf(ctx context.Context, in googlegrowth.AdminCall, dev string, keep func(playUser) bool) (playUser, bool,
	error) {
	token := ""
	for {
		q := url.Values{"pageSize": {"100"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Users         []playUser `json:"users"`
			NextPageToken string     `json:"nextPageToken"`
		}
		address := publisherAPI + "/androidpublisher/v3/developers/" + url.PathEscape(dev) + "/users?" + q.Encode()
		if err := in.Send.Do(ctx, http.MethodGet, address, in.Token, nil, &page); err != nil {
			return playUser{}, false, err
		}
		for _, u := range page.Users {
			if strings.EqualFold(u.Email, in.Email) && keep(u) {
				return u, true, nil
			}
		}
		if token = page.NextPageToken; token == "" {
			return playUser{}, false, nil
		}
	}
}

// accounts are the developer accounts the company names.
func accounts(c *config.Company) []string {
	if c == nil || c.Integrations.GooglePlay == nil {
		return nil
	}
	return c.Integrations.GooglePlay.DeveloperAccounts
}

// adminRoles reads the identity's permissions on each app from every
// developer account the company names.
func adminRoles(ctx context.Context, in googlegrowth.AdminCall) (map[string][]string, error) {
	out := map[string][]string{}
	devs := accounts(in.Company)
	for _, dev := range devs {
		u, found, err := user(ctx, in, dev)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		for _, app := range in.Scope {
			// ONE ACCOUNT HOLDS EVERY APP; of several, an app with no grant's is unknown.
			if held := u.on(app, len(devs) == 1); len(held) > 0 {
				out[app] = append(out[app], held...)
			}
		}
	}
	return out, nil
}

// add invites the identity to each app with the level's permissions, in the
// first developer account holding the app: a user made in an account that
// holds none of them is removed again. A grant it holds keeps its other
// permissions.
func add(ctx context.Context, in googlegrowth.AdminCall) error {
	left := slices.Clone(in.Scope)
	for _, dev := range accounts(in.Company) {
		if len(left) == 0 {
			break
		}
		var err error
		if left, err = addIn(ctx, in, dev, left); err != nil {
			return err
		}
	}
	if len(left) > 0 {
		return &seataccess.Refusal{Said: "No developer account the company names holds " + said(left),
			Remedy: "Name the account that holds them on the Settings tab", Actor: seataccess.ByYou}
	}
	return nil
}

// addIn adds the identity to every app of apps that developer account dev
// holds, answering the apps it does not.
func addIn(ctx context.Context, in googlegrowth.AdminCall, dev string, apps []string) ([]string, error) {
	u, found, err := user(ctx, in, dev)
	if err != nil {
		return nil, err
	}
	base := publisherAPI + "/androidpublisher/v3/developers/" + url.PathEscape(dev) + "/users"
	userPath := base + "/" + url.PathEscape(in.Email)
	made := false
	if !found {
		body := map[string]any{"name": "developers/" + dev + "/users/" + in.Email, "email": in.Email}
		err := in.Send.Do(ctx, http.MethodPost, base, in.Token, body, &u)
		switch {
		case googleauth.Status(err) == http.StatusConflict:
			// ITS ACCESS EXPIRED: the user stands, and grants give it access again.
			if u, _, err = userOf(ctx, in, dev, func(playUser) bool { return true }); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, err
		default:
			made = true
		}
	}
	var left, granted []string
	var failed error
	for _, app := range apps {
		err := grant(ctx, in, dev, userPath, u, app)
		switch {
		case googleauth.Status(err) == http.StatusNotFound:
			left = append(left, app)
		case err != nil:
			failed = err
		default:
			granted = append(granted, app)
		}
		if failed != nil {
			break
		}
	}
	if made && len(granted) == 0 {
		// MADE HERE FOR NOTHING: the account holds none of the apps, or no grant landed.
		if err := in.Send.Do(context.WithoutCancel(ctx), http.MethodDelete, userPath, in.Token, nil, nil); err != nil {
			failed = errors.Join(failed, errors.New("googleplay: remove the user made for no grant"), err)
		}
	}
	return left, failed
}

// grant gives u the level's permissions on app in dev, keeping any it holds.
func grant(ctx context.Context, in googlegrowth.AdminCall, dev, userPath string, u playUser, app string) error {
	for _, g := range u.Grants {
		if g.PackageName != app {
			continue
		}
		want := slices.Clone(g.AppLevelPermissions)
		for _, p := range in.Need.Roles {
			if !slices.Contains(want, p) {
				want = append(want, p)
			}
		}
		if len(want) == len(g.AppLevelPermissions) {
			return nil
		}
		body := playGrant{Name: g.Name, PackageName: app, AppLevelPermissions: want}
		return in.Send.Do(ctx, http.MethodPatch, userPath+"/grants/"+url.PathEscape(app)+
			"?updateMask=appLevelPermissions", in.Token, body, nil)
	}
	body := playGrant{Name: "developers/" + dev + "/users/" + in.Email + "/grants/" + app, PackageName: app,
		AppLevelPermissions: in.Need.Roles}
	return in.Send.Do(ctx, http.MethodPost, userPath+"/grants", in.Token, body, nil)
}

// remove takes the identity out of every developer account the company names.
func remove(ctx context.Context, in googlegrowth.AdminCall) error {
	for _, dev := range accounts(in.Company) {
		_, found, err := user(ctx, in, dev)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		address := publisherAPI + "/androidpublisher/v3/developers/" + url.PathEscape(dev) + "/users/" +
			url.PathEscape(in.Email)
		if err := in.Send.Do(ctx, http.MethodDelete, address, in.Token, nil, nil); err != nil &&
			googleauth.Status(err) != http.StatusNotFound {
			return err
		}
	}
	return nil
}

// adminClient is the OAuth client an admin signs in as (googleauth.SignInClient),
// false where Google Play is off.
func adminClient(c *config.Company) (googleauth.ConsentClient, bool) {
	b := c.Integrations.GooglePlay
	if b == nil || !b.Enabled {
		return googleauth.ConsentClient{}, false
	}
	return googleauth.SignInClient(c, b.OAuth), true
}

// adminScopes are what the admin sign-in asks for: managing users, and
// nothing a seat's call needs.
func adminSignInScopes(*config.Company) []string { return []string{scopePublisher} }
