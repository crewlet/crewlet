package config

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/org"
)

// AppleAds connects the company's Apple Ads organizations through the Apple
// Ads Platform API: each connection is one API user, whose private key the
// engine signs its client secret with, and the engine makes every call for
// the agents (ADR-0170).
type AppleAds struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Approvers route the integration's held acts; they never decide who may
	// answer (an administrator, ADR-0171).
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Optional. Handles of human seats asked first about a spend, a creative or an account change an agent asks for. Only an administrator may answer; without this list every administrator is asked."`

	// AdAccounts narrows the ad accounts agents may be given; absent is every
	// ad account the connections reach.
	AdAccounts []string `yaml:"ad_accounts,omitempty" json:"ad_accounts,omitempty" desc:"Optional. IDs (digits) of the ad accounts agents may be given. Absent: every ad account the connections' API users reach."`

	// Connections are the API users, by connection id.
	Connections map[string]AppleAdsConnection `yaml:"connections,omitempty" json:"connections,omitempty" desc:"The Apple Ads API users the engine signs in as, each under its connection id (lowercase letters, digits and hyphens, starting with a letter). Required when enabled."`

	// Spend caps each ad account's daily capacity (internal/spendguard).
	Spend *GrowthSpend `yaml:"spend,omitempty" json:"spend,omitempty" desc:"Optional. The spending caps per ad account, in its currency. An ad account without both caps refuses every spend act; reads, operate changes and pausing still work."`
}

// AppleAdsConnection is one Apple Ads API user: its organization, the client
// credentials Apple shows under Account Settings, API, and its private key.
type AppleAdsConnection struct {
	OrgID string `yaml:"org_id,omitempty" json:"org_id,omitempty" desc:"Optional. The organization's ID (digits), shown beside the account name in Apple Ads. When set, the API user must answer it."`

	ClientID string `yaml:"client_id,omitempty" json:"client_id,omitempty" desc:"The API user's client ID, SEARCHADS. followed by an identifier, shown under Account Settings, API once its public key is uploaded."`

	TeamID string `yaml:"team_id,omitempty" json:"team_id,omitempty" desc:"The API user's team ID, SEARCHADS. followed by an identifier, shown beside the client ID."`

	KeyID string `yaml:"key_id,omitempty" json:"key_id,omitempty" desc:"The uploaded public key's ID, shown beside the client ID."`

	PrivateKey string `secret:"true" yaml:"private_key,omitempty" json:"private_key,omitempty" desc:"Reference to the sealed private key whose public half was uploaded, the PEM block from BEGIN EC PRIVATE KEY to END EC PRIVATE KEY that openssl ecparam wrote. Apple keeps no copy."`
}

// ConnectionIDs are the block's connection ids, sorted: the order a call
// tries the connections in.
func (a *AppleAds) ConnectionIDs() []string {
	if a == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(a.Connections))
}

// RoleAppleAds is a seat's access to Apple Ads.
type RoleAppleAds struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may use Apple Ads through the engine's tools. No agent holds a key or a token."`

	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): reads campaigns, ad groups, keywords, ads, creatives and reports. operate: also pauses, lowers budgets and bids, adds negative keywords at once, and asks an administrator for any spend or creative customers see. administer: also asks for ad account changes and deletes."`

	AdAccounts []string `yaml:"ad_accounts,omitempty" json:"ad_accounts,omitempty" desc:"IDs (digits) of the ad accounts this agent may touch. Required while enabled."`
}

// Apple Ads' own shapes: an API user's client and team IDs carry Apple's
// SEARCHADS. prefix. Apple documents no key ID format (its examples are
// UUIDs), so a key ID is held to one word, never to a guess.
var (
	aaDigits   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,20}$`) })
	aaClientID = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^SEARCHADS\.[0-9A-Za-z-]{1,64}$`) })
	aaKeyID    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[^\s]{1,128}$`) })
)

// AppleAdsID reports whether v is an Apple Ads API user's client or team ID.
func AppleAdsID(v string) bool { return aaClientID().MatchString(v) }

// GrowthKind is the block's integration kind.
func (a *AppleAds) GrowthKind() string { return "apple_ads" }

// GrowthEnabled reports whether the block is on.
func (a *AppleAds) GrowthEnabled() bool { return a != nil && a.Enabled }

// GrowthApprovers are the people its held acts are routed to first.
func (a *AppleAds) GrowthApprovers() []string { return a.Approvers }

// GrowthSpend is the block's caps per ad account.
func (a *AppleAds) GrowthSpend() *GrowthSpend { return a.Spend }

func (a *AppleAds) validate(path Path) error {
	var p problems
	for i, acct := range a.AdAccounts {
		if !aaDigits().MatchString(acct) {
			p.add(idx(at(path, "ad_accounts"), i), ErrUnknownValue, "%q is not an ad account ID: its digits", acct)
		}
	}
	if a.Spend != nil {
		for i, c := range a.Spend.Accounts {
			here := at(idx(at(path, "spend.accounts"), i), "account")
			switch acct := strings.TrimSpace(c.Account); {
			case acct == "":
			case !aaDigits().MatchString(acct):
				p.add(here, ErrUnknownValue, "%q is not an ad account ID: its digits", c.Account)
			case len(a.AdAccounts) > 0 && !slices.Contains(a.AdAccounts, acct):
				p.add(here, ErrUnknownValue, "%q is not among integrations.apple_ads.ad_accounts", c.Account)
			}
		}
	}
	if !a.Enabled {
		return p.err()
	}
	if len(a.Connections) == 0 {
		p.add(at(path, "connections"), ErrMissing, "name at least one API user: its client, team and key")
	}
	clients := map[string]string{}
	for _, id := range a.ConnectionIDs() {
		c := a.Connections[id]
		here := entry(at(path, "connections"), id)
		if org := strings.TrimSpace(c.OrgID); org != "" && !envref.Has(org) && !aaDigits().MatchString(org) {
			p.add(at(here, "org_id"), ErrUnknownValue, "%q is not an organization ID: digits", c.OrgID)
		}
		for _, f := range []struct{ name, value string }{{"client_id", c.ClientID}, {"team_id", c.TeamID}} {
			switch v := strings.TrimSpace(f.value); {
			case v == "":
				p.add(at(here, f.name), ErrMissing, "the API user's %s, under Account Settings, API", f.name)
			case !envref.Has(v) && !aaClientID().MatchString(v):
				p.add(at(here, f.name), ErrUnknownValue, "%q is not an Apple Ads %s: SEARCHADS. and an identifier",
					f.value, f.name)
			}
		}
		if client := strings.TrimSpace(c.ClientID); client != "" {
			if other, dup := clients[client]; dup {
				p.add(at(here, "client_id"), ErrConflict, "%s is also connection %s's API user: each connection "+
					"is its own", client, other)
			}
			clients[client] = id
		}
		switch key := strings.TrimSpace(c.KeyID); {
		case key == "":
			p.add(at(here, "key_id"), ErrMissing, "the uploaded public key's ID, beside the client ID")
		case !envref.Has(key) && !aaKeyID().MatchString(key):
			p.add(at(here, "key_id"), ErrUnknownValue, "%q is not a key ID: one word, as Apple shows it", c.KeyID)
		}
		if _, whole := envref.Whole(c.PrivateKey); !whole {
			p.add(at(here, "private_key"), ErrUnknownValue, "a ${VAR} naming the sealed private key, never the key itself")
		}
	}
	checkGrowthConnections(&p, at(path, "connections"), a.ConnectionIDs())
	return p.err()
}

// checkAppleAds holds a seat's level to the three and its ad accounts to the
// company's reach, each once.
func (c *Company) checkAppleAds(p *problems, path Path, b *RoleAppleAds) {
	if b == nil {
		return
	}
	if msg := growthLevelProblem(b.Level); msg != "" {
		p.add(at(path, "level"), ErrUnknownValue, "%s", msg)
	}
	reach := c.Integrations.AppleAds
	seen := map[string]bool{}
	for i, acct := range b.AdAccounts {
		here := idx(at(path, "ad_accounts"), i)
		switch {
		case !aaDigits().MatchString(acct):
			p.add(here, ErrUnknownValue, "%q is not an ad account ID: its digits", acct)
		case seen[acct]:
			p.add(here, ErrConflict, "%q is listed twice", acct)
		case reach != nil && len(reach.AdAccounts) > 0 && !slices.Contains(reach.AdAccounts, acct):
			p.add(here, ErrUnknownValue, "%q is not among integrations.apple_ads.ad_accounts", acct)
		}
		seen[acct] = true
	}
}

// AppleAdsSeat is agent seat handle's Apple Ads block while it and the
// company's block are on, nil otherwise.
func (c *Company) AppleAdsSeat(handle string) *RoleAppleAds {
	if c == nil || !c.Integrations.AppleAds.GrowthEnabled() {
		return nil
	}
	for role := range c.EachRole() {
		if role.Kind == org.KindHuman || role.IdentityKey() != handle {
			continue
		}
		if b := role.Integrations.AppleAds; b != nil && b.Enabled != nil && *b.Enabled {
			return b
		}
		return nil
	}
	return nil
}

// GrowthKind is the seat block's integration kind, the company block's.
func (*RoleAppleAds) GrowthKind() string { return (*AppleAds)(nil).GrowthKind() }

// SeatAccess is the block as seat access reads it.
func (r *RoleAppleAds) SeatAccess() GrowthSeatAccess {
	return GrowthSeatAccess{On: r.Enabled, Level: string(growth.LevelOf(r.Level)), Scope: r.AdAccounts}
}

// SetSeatAccess writes the block from seat access.
func (r *RoleAppleAds) SetSeatAccess(a GrowthSeatAccess) {
	r.Enabled, r.Level, r.AdAccounts = a.On, a.Level, a.Scope
}

func (r *RoleAppleAds) seatScopeKey() string { return "ad_accounts" }
func (r *RoleAppleAds) seatEnabled() **bool  { return &r.Enabled }

// check holds the block to the company's reach.
func (r *RoleAppleAds) check(c *Company, p *problems, here Path) { c.checkAppleAds(p, here, r) }

// GrowthSeatLevel is the seat's level while its block is on, "" while off.
func (r *RoleAppleAds) GrowthSeatLevel() string {
	if r == nil || r.Enabled == nil || !*r.Enabled {
		return ""
	}
	return string(growth.LevelOf(r.Level))
}
