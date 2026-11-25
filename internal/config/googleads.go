package config

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// GoogleAds lets agent seats read and manage Google Ads accounts as each
// seat's own Google growth identity (internal/googleauth, ADR-0172), which a
// person adds to each account at the access level the seat may act with. It
// signs in through the Google Cloud connection's identity, so it needs
// integrations.google_cloud on with its provisioning block. It holds no secret:
// Google retired developer tokens.
type GoogleAds struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on. Needs integrations.google_cloud on with its provisioning block, whose identity agents sign in through."`

	// Customers are the accounts agents may be given, each with the manager
	// it is reached through, if any.
	Customers []GoogleAdsCustomer `yaml:"customers,omitempty" json:"customers,omitempty" desc:"The Google Ads accounts agents may be given, at most 200, each with the manager account it is reached through. Absent: every account an agent's identity is added to directly. Optional."`

	// Approvers route an agent's held acts: who is asked first.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Handles of human seats an agent's spend, publish and admin acts are routed to first. Only an administrator answers one, at a fresh step-up; without approvers every administrator is asked. Optional."`

	// Spend caps each account's daily capacity: without both caps an account
	// refuses every spend call.
	Spend *GrowthSpend `yaml:"spend,omitempty" json:"spend,omitempty" desc:"Each account's spending caps, in its own currency. An account without both caps refuses every spend call; reads, operate calls and pausing still work. Optional."`
}

// GoogleAdsCustomer is one Google Ads account agents may be given.
type GoogleAdsCustomer struct {
	ID string `yaml:"id" json:"id" desc:"The account's 10-digit customer ID, without hyphens."`

	// Manager is the manager account whose access reaches this one, sent as
	// login-customer-id; empty where the identity is added to it directly.
	Manager string `yaml:"manager,omitempty" json:"manager,omitempty" desc:"The 10-digit customer ID of the manager account the identity is added to, where it reaches this account through a manager. Optional."`
}

// RoleGoogleAds is one seat's Google Ads access.
type RoleGoogleAds struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may reach Google Ads. Off turns its Google growth identity off once no other Google growth product is on for it."`

	// Level is one level for every account the seat names; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): reports, planning and settings, nothing changed. operate: also pausing, lowering budgets and bids, negatives and other changes that spend nothing more. administer: also asks an administrator to approve each change that spends, publishes ad text, removes or touches users, links and billing."`

	Customers []string `yaml:"customers,omitempty" json:"customers,omitempty" desc:"10-digit customer IDs this agent may reach, within integrations.google_ads.customers when that is set. Required when the agent is on."`
}

// GoogleAdsKind is the integration's kind, config key and lane.
const GoogleAdsKind = "google_ads"

// googleAdsMaxCustomers bounds a block's accounts: a company with more is an
// agency, which connects each client's company.
const googleAdsMaxCustomers = 200

// googleAdsCustomerID is a customer ID as the API writes it: ten digits.
var googleAdsCustomerID = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(GoogleAdsCustomerPattern) })

// GoogleAdsCustomerPattern is an account's ten-digit customer ID, without
// dashes; the seat picker offers a typed account by it too.
const GoogleAdsCustomerPattern = `^[0-9]{10}$`

// GrowthKind makes the block one of the growth blocks the shared rules find
// (growth.go).
func (g *GoogleAds) GrowthKind() string { return GoogleAdsKind }

// GrowthEnabled reports whether the block is on.
func (g *GoogleAds) GrowthEnabled() bool { return g != nil && g.Enabled }

// GrowthApprovers are the people its held acts are routed to first.
func (g *GoogleAds) GrowthApprovers() []string { return g.Approvers }

// GrowthSpend is the accounts' spending caps.
func (g *GoogleAds) GrowthSpend() *GrowthSpend { return g.Spend }

// Reaches reports whether the company lets agents be given customer id.
func (g *GoogleAds) Reaches(id string) bool {
	return g != nil && (len(g.Customers) == 0 || slices.ContainsFunc(g.Customers,
		func(c GoogleAdsCustomer) bool { return c.ID == id }))
}

// ManagerOf is the manager customer id is reached through, "" for none.
func (g *GoogleAds) ManagerOf(id string) string {
	if g == nil {
		return ""
	}
	if i := slices.IndexFunc(g.Customers, func(c GoogleAdsCustomer) bool { return c.ID == id }); i >= 0 {
		return g.Customers[i].Manager
	}
	return ""
}

// GrowthKind is the seat block's integration kind.
func (r *RoleGoogleAds) GrowthKind() string { return GoogleAdsKind }

// GrowthSeatLevel is the seat's level where it is on with an account, which
// internal/authz's Google Ads grants admit to its tools; "" where it is off.
func (r *RoleGoogleAds) GrowthSeatLevel() string {
	if r == nil || !SeatAccessOn(r.Enabled) || len(r.Customers) == 0 {
		return ""
	}
	return string(r.LevelOrDefault())
}

// SeatAccess is the block as seat access reads it.
func (r *RoleGoogleAds) SeatAccess() GrowthSeatAccess {
	return GrowthSeatAccess{On: r.Enabled, Level: string(r.LevelOrDefault()), Scope: r.Customers}
}

// SetSeatAccess writes the block from seat access.
func (r *RoleGoogleAds) SetSeatAccess(a GrowthSeatAccess) {
	r.Enabled, r.Level, r.Customers = a.On, a.Level, a.Scope
}

func (r *RoleGoogleAds) seatScopeKey() string { return "customers" }
func (r *RoleGoogleAds) seatEnabled() **bool  { return &r.Enabled }

// LevelOrDefault is the seat's level, read where it names none.
func (r *RoleGoogleAds) LevelOrDefault() growth.Level {
	if r == nil {
		return growth.LevelRead
	}
	return growth.LevelOf(r.Level)
}

// validate holds the block to its shape; in is the document's integrations,
// whose Google Cloud connection an enabled block signs in through.
func (g *GoogleAds) validate(in *Integrations, path Path) error {
	var p problems
	if g.Enabled {
		if gc := in.GoogleCloud; gc == nil || !gc.Enabled || gc.Provisioning == nil {
			p.add(at(path, "enabled"), ErrMissing, "Google Ads signs in through the Google Cloud connection's "+
				"identity: turn integrations.google_cloud on with its provisioning block (no project needs preparing)")
		}
	}
	if n := len(g.Customers); n > googleAdsMaxCustomers {
		p.add(at(path, "customers"), ErrOutOfRange, "%d accounts is more than %d", n, googleAdsMaxCustomers)
	}
	seen := map[string]bool{}
	for i, c := range g.Customers {
		here := idx(at(path, "customers"), i)
		switch {
		case !googleAdsCustomerID().MatchString(c.ID):
			p.add(at(here, "id"), ErrUnknownValue, "%q is not a Google Ads customer ID: ten digits, without "+
				"hyphens", c.ID)
		case seen[c.ID]:
			p.add(at(here, "id"), ErrConflict, "%q is listed twice", c.ID)
		}
		seen[c.ID] = true
		if c.Manager != "" && (!googleAdsCustomerID().MatchString(c.Manager) || c.Manager == c.ID) {
			p.add(at(here, "manager"), ErrUnknownValue, "%q is not another account's customer ID: ten digits, "+
				"without hyphens", c.Manager)
		}
	}
	if g.Spend != nil {
		for i, cap := range g.Spend.Accounts {
			if !g.Reaches(cap.Account) || !googleAdsCustomerID().MatchString(cap.Account) {
				p.add(at(idx(at(at(path, "spend"), "accounts"), i), "account"), ErrUnknownValue, "%q is not one of "+
					"integrations.google_ads.customers: cap an account agents may be given", cap.Account)
			}
		}
	}
	return p.err()
}

// check holds a seat's block to its level and accounts.
func (r *RoleGoogleAds) check(c *Company, p *problems, path Path) {
	if level := strings.TrimSpace(r.Level); level != "" && !growth.Level(level).Valid() {
		p.add(at(path, "level"), ErrUnknownValue, "%q is not a Google Ads level; give %s", r.Level,
			words.Or(growth.LevelNames()))
	}
	seen := map[string]bool{}
	company := c.Integrations.GoogleAds
	for i, id := range r.Customers {
		here := idx(at(path, "customers"), i)
		switch {
		case !googleAdsCustomerID().MatchString(id):
			p.add(here, ErrUnknownValue, "%q is not a Google Ads customer ID: ten digits, without hyphens", id)
		case seen[id]:
			p.add(here, ErrConflict, "%q is listed twice", id)
		case company != nil && !company.Reaches(id):
			p.add(here, ErrUnknownValue, "account %q is not in integrations.google_ads.customers; add it there, "+
				"or take it off this agent", id)
		}
		seen[id] = true
	}
}
