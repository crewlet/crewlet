package config

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// AdSense lets agent seats read and change AdSense through the company's
// consent grant: Google refuses service accounts at AdSense, so a person who
// manages the account signs in with Google once, as the company's own OAuth
// client, and the engine seals and renews the grant. Every agent's call goes
// as that grant, held to the seat's level and accounts by the engine.
type AdSense struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on. Needs the company's own OAuth client (integrations.google_cloud.sign_in_client, or oauth here) and a person's Google sign-in, made on the Google page."`

	OAuth *GoogleOAuthClient `yaml:"oauth,omitempty" json:"oauth,omitempty" desc:"The OAuth client AdSense alone signs in as, in place of integrations.google_cloud.sign_in_client. Optional."`

	// Accounts narrows what agents may be given; absent is every account the
	// signing person reaches.
	Accounts []string `yaml:"accounts,omitempty" json:"accounts,omitempty" desc:"Publisher IDs agents may be given (pub- and 16 digits), at most 50. Absent: every AdSense account the person who signs in reaches. Optional."`

	// Approvers route an agent's held acts: who is asked first.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Handles of human seats an agent's public and admin acts are routed to first. Only an administrator answers one, at a fresh step-up; without approvers every administrator is asked. Optional."`
}

// RoleAdSense is one seat's AdSense access.
type RoleAdSense struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may reach AdSense."`

	// Level is one level for every account the seat names; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): accounts, sites, ad clients, ad units, channels, alerts, policy issues, payments, ad code and reports. operate: also creates ad units and custom channels and renames, archives or turns them on at once, and asks an administrator to approve a live ad unit's size or type, or a site's review. administer: also asks an administrator to approve what cannot be undone: deleting a custom channel, a platform's sub-accounts, their sites and events, and moving a site to another revenue group."`

	Accounts []string `yaml:"accounts,omitempty" json:"accounts,omitempty" desc:"Publisher IDs this agent may reach, within integrations.adsense.accounts when that is set. Required when the agent is on."`
}

// AdSenseKind is the integration's kind, config key and lane.
const AdSenseKind = "adsense"

// adSenseMaxAccounts bounds a block's accounts, as AdMob's: one sign-in
// reaching more is an agency, which connects each client's company.
const adSenseMaxAccounts = adMobMaxAccounts

// adSenseAccountPage is where AdSense shows a publisher ID.
const adSenseAccountPage = "AdSense's Account, Settings, Account information"

// GrowthKind makes the block one of the growth blocks the shared rules find
// (growth.go).
func (a *AdSense) GrowthKind() string { return AdSenseKind }

// GrowthEnabled reports whether the block is on.
func (a *AdSense) GrowthEnabled() bool { return a != nil && a.Enabled }

// GrowthApprovers are the people its held acts are routed to first.
func (a *AdSense) GrowthApprovers() []string { return a.Approvers }

// GrowthSpend is nil: AdSense pays the publisher, and nothing in it spends.
func (a *AdSense) GrowthSpend() *GrowthSpend { return nil }

// Reaches reports whether the company lets agents be given account id.
func (a *AdSense) Reaches(id string) bool {
	return a != nil && (len(a.Accounts) == 0 || slices.Contains(a.Accounts, id))
}

// GrowthKind is the seat block's integration kind.
func (r *RoleAdSense) GrowthKind() string { return AdSenseKind }

// GrowthSeatLevel is the seat's level where it is on with an account, which
// internal/authz's AdSense grants admit to its tools; "" where it is off.
func (r *RoleAdSense) GrowthSeatLevel() string {
	if r == nil || !SeatAccessOn(r.Enabled) || len(r.Accounts) == 0 {
		return ""
	}
	return string(r.LevelOrDefault())
}

// SeatAccess is the block as seat access reads it.
func (r *RoleAdSense) SeatAccess() GrowthSeatAccess {
	return GrowthSeatAccess{On: r.Enabled, Level: string(r.LevelOrDefault()), Scope: r.Accounts}
}

// SetSeatAccess writes the block from seat access.
func (r *RoleAdSense) SetSeatAccess(a GrowthSeatAccess) {
	r.Enabled, r.Level, r.Accounts = a.On, a.Level, a.Scope
}

func (r *RoleAdSense) seatScopeKey() string { return "accounts" }
func (r *RoleAdSense) seatEnabled() **bool  { return &r.Enabled }

// LevelOrDefault is the seat's level, read where it names none.
func (r *RoleAdSense) LevelOrDefault() growth.Level {
	if r == nil {
		return growth.LevelRead
	}
	return growth.LevelOf(r.Level)
}

// validate holds the block to its shape: an enabled block names its OAuth
// client, which the sign-in is made as.
func (a *AdSense) validate(i *Integrations, path Path) error {
	var p problems
	if a.Enabled && i.GoogleSignInClient(a.OAuth) == nil {
		p.add(at(path, "oauth"), ErrMissing, "AdSense signs in as the company's own OAuth client: set "+
			"integrations.google_cloud.sign_in_client, or oauth here")
	}
	a.OAuth.validate(&p, at(path, "oauth"), a.Enabled)
	if n := len(a.Accounts); n > adSenseMaxAccounts {
		p.add(at(path, "accounts"), ErrOutOfRange, "%d accounts is more than %d", n, adSenseMaxAccounts)
	}
	checkPublisherIDs(&p, at(path, "accounts"), a.Accounts, adSenseAccountPage)
	return p.err()
}

// check holds a seat's block to its level and accounts.
func (r *RoleAdSense) check(c *Company, p *problems, path Path) {
	if level := strings.TrimSpace(r.Level); level != "" && !growth.Level(level).Valid() {
		p.add(at(path, "level"), ErrUnknownValue, "%q is not an AdSense level; give %s", r.Level,
			words.Or(growth.LevelNames()))
	}
	checkPublisherIDs(p, at(path, "accounts"), r.Accounts, adSenseAccountPage)
	company := c.Integrations.AdSense
	for i, id := range r.Accounts {
		if company != nil && googlePublisher().MatchString(id) && !company.Reaches(id) {
			p.add(idx(at(path, "accounts"), i), ErrUnknownValue, "account %q is not in "+
				"integrations.adsense.accounts; add it there, or take it off this agent", id)
		}
	}
}
