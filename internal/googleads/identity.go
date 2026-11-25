package googleads

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
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

// identity is Google Ads' half of a seat's own identity: an access level on
// each of the seat's accounts, or once on the manager they are reached
// through, which an Admin adds by hand (the API only sends invitations a
// mailbox accepts). The identity reads its own level as a user of the
// account it was added to, where Google lets it.
var identity = sync.OnceValue(func() *googlegrowth.Identity {
	return &googlegrowth.Identity{
		Records: "its changes under its own email in the change history",
		Entry:   "account", Entries: "accounts",
		Roles: googlegrowth.Roles{Ladder: []googlegrowth.RoleName{
			{ID: roleReadOnly, Name: "Read-only"}, {ID: roleStandard, Name: "Standard"}, {ID: roleAdmin, Name: "Admin"},
		}},
		Needs: needs,
		Steps: steps,
		Note: vendortext.Of(vendortext.W("Only an "), vendortext.UI("Admin"), vendortext.W(" can add users. Added "+
			"once to a manager account, it reaches every account under it; one email reaches at most 20 accounts directly.")),
		Scopes: []string{scopeAdwords},
		Reach:  reach,
	}
})

// needs is the access level each level needs on each account.
func needs(level growth.Level) googlegrowth.Need {
	switch level {
	case growth.LevelOperate:
		return googlegrowth.Need{Roles: []string{roleStandard}, Broad: []string{roleAdmin}}
	case growth.LevelAdminister:
		return googlegrowth.Need{Roles: []string{roleStandard}, More: vendortext.Of(
			vendortext.W("Users, links and billing need "), vendortext.UI("Admin"),
			vendortext.W(", which Google Ads gives a service account only by raising it after it is added."))}
	}
	return googlegrowth.Need{Roles: []string{roleReadOnly}, Broad: []string{roleAdmin}}
}

// steps are an Admin's steps in Google Ads, on the manager where the seat's
// accounts are reached through one.
func steps(g googlegrowth.Guided) []seataccess.GuideStep {
	return []seataccess.GuideStep{
		{Text: vendortext.Of(vendortext.W("Open Google Ads in "+said(holders(g.Company, g.Entries))+", then "),
			vendortext.UI("Admin", "Access and security").At(accessURL), vendortext.W("."))},
		{Text: vendortext.Of(vendortext.W("On the "), vendortext.UI("Users"), vendortext.W(" tab, press "),
			vendortext.UI("+"), vendortext.W("."))},
		g.Copy("Email"),
		{Text: vendortext.Of(append(append([]vendortext.Part{vendortext.UI("Access level"), vendortext.W(": ")},
			vendortext.And(g.NeedNames...)...), vendortext.W(". Press "), vendortext.UI("Add account"),
			vendortext.W("."))...)},
	}
}

// holders are the accounts the identity is added to for entries: each one's
// manager where it is reached through one, else itself, each once.
func holders(c *config.Company, entries []string) []string {
	var out []string
	for _, id := range entries {
		h := "account " + id
		if m := managerOf(c, id); m != "" {
			h = "manager account " + m
		}
		if !strings.Contains(strings.Join(out, "\n")+"\n", h+"\n") {
			out = append(out, h)
		}
	}
	return out
}

// managerOf is the manager the company reaches customer id through, "" where
// it names none.
func managerOf(c *config.Company, id string) string {
	if c == nil || c.Integrations.GoogleAds == nil {
		return ""
	}
	for _, customer := range c.Integrations.GoogleAds.Customers {
		if customer.ID == id {
			return customer.Manager
		}
	}
	return ""
}

// said is phrases as one list a sentence says.
func said(phrases []string) string {
	out := make([]words.Said, len(phrases))
	for i, p := range phrases {
		out[i] = words.Said(p)
	}
	return words.And(out)
}

// search runs one query in customer id, through manager where set.
func search(ctx context.Context, in googlegrowth.Probe, id, manager, query string, out any) error {
	call := googleauth.Call{Method: http.MethodPost, Bearer: in.Token, Reads: true,
		Address: adsAPI + "/" + apiVersion + "/customers/" + url.PathEscape(id) + "/googleAds:search",
		Body:    map[string]string{"query": query}}
	if manager != "" {
		call.Header = http.Header{"Login-Customer-Id": {manager}}
	}
	return in.Send.Send(ctx, call, out)
}

// reach reads each of the seat's accounts as the identity, then its own
// access level as a user of the account it was added to; an account whose
// users it may not list leaves the level unread.
func reach(ctx context.Context, in googlegrowth.Probe) ([]googlegrowth.Reached, error) {
	out := make([]googlegrowth.Reached, 0, len(in.Scope))
	for _, id := range in.Scope {
		manager := managerOf(in.Company, id)
		var customer struct {
			Results []struct {
				Customer struct {
					DescriptiveName string `json:"descriptiveName"`
				} `json:"customer"`
			} `json:"results"`
		}
		err := search(ctx, in, id, manager, "SELECT customer.id, customer.descriptive_name FROM customer LIMIT 1",
			&customer)
		switch status := googleauth.Status(err); {
		case status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusUnauthorized:
			out = append(out, googlegrowth.Reached{Entry: id, Said: googleauth.Sentence(err)})
			continue
		case err != nil:
			return nil, err
		}
		r := googlegrowth.Reached{Entry: id, OK: true}
		if len(customer.Results) > 0 {
			r.Name = customer.Results[0].Customer.DescriptiveName
		}
		r.Roles, r.Read, err = level(ctx, in, id, manager)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// level is the identity's access level as a user of the account it was
// added to: the manager where it reaches id through one. Read is false where
// Google lets it list no users there.
func level(ctx context.Context, in googlegrowth.Probe, id, manager string) ([]string, bool, error) {
	holder := id
	if manager != "" {
		holder = manager
	}
	if strings.ContainsAny(in.Email, `'\`) {
		return nil, false, nil
	}
	var users struct {
		Results []struct {
			CustomerUserAccess struct {
				AccessRole string `json:"accessRole"`
			} `json:"customerUserAccess"`
		} `json:"results"`
	}
	err := search(ctx, in, holder, manager, "SELECT customer_user_access.access_role FROM customer_user_access "+
		"WHERE customer_user_access.email_address = '"+in.Email+"'", &users)
	switch status := googleauth.Status(err); {
	case status == http.StatusForbidden || status == http.StatusNotFound:
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case len(users.Results) == 0:
		return nil, false, nil
	}
	return []string{strings.ToLower(users.Results[0].CustomerUserAccess.AccessRole)}, true, nil
}

// maxListedHolders bounds the accounts a listing reads the clients of: one
// search each, and a company reaching more is an agency, which connects each
// client's company.
const maxListedHolders = 50

// listAccounts is every account the token reaches: each it is added to, and
// each client of a manager among them, under that manager.
func listAccounts(ctx context.Context, in googlegrowth.Lister) ([]googlegrowth.Listed, error) {
	var accessible struct {
		ResourceNames []string `json:"resourceNames"`
	}
	if err := in.Send.Do(ctx, http.MethodGet, adsAPI+"/"+apiVersion+"/customers:listAccessibleCustomers", in.Token,
		nil, &accessible); err != nil {
		return nil, err
	}
	var out []googlegrowth.Listed
	seen := map[string]bool{}
	probe := googlegrowth.Probe{Company: in.Company, Token: in.Token, Send: in.Send}
	for i, name := range accessible.ResourceNames {
		holder := strings.TrimPrefix(name, "customers/")
		if i >= maxListedHolders {
			break
		}
		var clients struct {
			Results []struct {
				CustomerClient struct {
					ID              string `json:"id"`
					DescriptiveName string `json:"descriptiveName"`
					Level           string `json:"level"`
				} `json:"customerClient"`
			} `json:"results"`
		}
		err := search(ctx, probe, holder, holder, "SELECT customer_client.id, customer_client.descriptive_name, "+
			"customer_client.level FROM customer_client WHERE customer_client.level <= 1", &clients)
		if s := googleauth.Status(err); s == http.StatusForbidden || s == http.StatusNotFound {
			// LISTED BUT NOT READABLE (a cancelled account): offered by its ID.
			if !seen[holder] {
				seen[holder] = true
				out = append(out, googlegrowth.Listed{ID: holder})
			}
			continue
		} else if err != nil {
			return nil, err
		}
		var holderName string
		for _, r := range clients.Results {
			if r.CustomerClient.Level == "0" {
				holderName = r.CustomerClient.DescriptiveName
			}
		}
		for _, r := range clients.Results {
			c := r.CustomerClient
			if c.ID == "" || seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			l := googlegrowth.Listed{ID: c.ID, Name: c.DescriptiveName}
			if c.Level != "0" {
				l.Parent, l.ParentName = holder, cmp.Or(holderName, holder)
			}
			out = append(out, l)
		}
	}
	return out, nil
}
