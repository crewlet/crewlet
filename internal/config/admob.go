package config

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// AdMob lets agent seats read and change AdMob app monetization through the
// company's consent grant: Google refuses service accounts at AdMob, so a
// person who manages the account signs in with Google once, as the company's
// own OAuth client, and the engine seals and renews the grant. Every agent's
// call goes as that grant, held to the seat's level and accounts by the engine.
type AdMob struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on. Needs oauth, the company's own OAuth client, and a person's Google sign-in on the integration's page."`

	OAuth *GoogleOAuthClient `yaml:"oauth,omitempty" json:"oauth,omitempty" desc:"The company's own OAuth client (a Web application, in a Google Cloud project it controls), which the sign-in is made as. Required when enabled."`

	// Accounts narrows what agents may be given; absent is every account the
	// signing person reaches.
	Accounts []string `yaml:"accounts,omitempty" json:"accounts,omitempty" desc:"Publisher IDs agents may be given (pub- and 16 digits), at most 50. Absent: every AdMob account the person who signs in reaches. Optional."`

	// Approvers route an agent's held acts: who is asked first.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Handles of human seats an agent's admin acts are routed to first. Only an administrator answers one, at a fresh step-up; without approvers every administrator is asked. Optional."`
}

// GoogleOAuthClient is a company's own Google OAuth client, from a Google
// Cloud project it controls, that a consent product signs in as. Several
// consent products may share one client, so its secret's variable may be
// named at each of their blocks (growthshare).
type GoogleOAuthClient struct {
	ClientID     string `yaml:"client_id" json:"client_id" desc:"The OAuth client's ID: <project number>-<id>.apps.googleusercontent.com."`
	ClientSecret string `secret:"true" growthshare:"google-oauth-client" yaml:"client_secret" json:"client_secret" desc:"The OAuth client's secret, usually ${GOOGLE_OAUTH_CLIENT_SECRET}; shown back as __redacted__. Every consent product signing in as this client may name the same variable."`
}

// RoleAdMob is one seat's AdMob access.
type RoleAdMob struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may reach AdMob."`

	// Level is one level for every account the seat names; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): accounts, apps, ad units, mediation and reports. operate: also creates apps, ad units and mappings, and changes mediation groups and A/B experiments, at once. administer: also asks an administrator to approve what cannot be undone: linking an app to its store listing, removing a mediation line."`

	Accounts []string `yaml:"accounts,omitempty" json:"accounts,omitempty" desc:"Publisher IDs this agent may reach, within integrations.admob.accounts when that is set. Required when the agent is on."`
}

// AdMobKind is the integration's kind, config key and lane.
const AdMobKind = "admob"

// adMobMaxAccounts bounds a block's accounts: one sign-in reaching more is an
// agency, which connects each client's company.
const adMobMaxAccounts = 50

// googlePublisher is a publisher ID as AdMob and AdSense write it.
var googlePublisher = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^pub-[0-9]{16}$`) })

// googleOAuthClientID is an OAuth client ID: the project number, a hyphen and
// the client's own part.
var googleOAuthClientID = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com$`)
})

// GrowthKind makes the block one of the growth blocks the shared rules find
// (growth.go).
func (a *AdMob) GrowthKind() string { return AdMobKind }

// GrowthEnabled reports whether the block is on.
func (a *AdMob) GrowthEnabled() bool { return a != nil && a.Enabled }

// GrowthApprovers are the people its held acts are routed to first.
func (a *AdMob) GrowthApprovers() []string { return a.Approvers }

// GrowthSpend is nil: AdMob pays the publisher, and nothing in it spends.
func (a *AdMob) GrowthSpend() *GrowthSpend { return nil }

// Reaches reports whether the company lets agents be given account id.
func (a *AdMob) Reaches(id string) bool {
	return a != nil && (len(a.Accounts) == 0 || slices.Contains(a.Accounts, id))
}

// GrowthKind is the seat block's integration kind.
func (r *RoleAdMob) GrowthKind() string { return AdMobKind }

// GrowthSeatLevel is the seat's level where it is on with an account, which
// internal/authz's AdMob grants admit to its tools; "" where it is off.
func (r *RoleAdMob) GrowthSeatLevel() string {
	if r == nil || !SeatAccessOn(r.Enabled) || len(r.Accounts) == 0 {
		return ""
	}
	return string(r.LevelOrDefault())
}

// SeatAccess is the block as seat access reads it.
func (r *RoleAdMob) SeatAccess() GrowthSeatAccess {
	return GrowthSeatAccess{On: r.Enabled, Level: string(r.LevelOrDefault()), Scope: r.Accounts}
}

// SetSeatAccess writes the block from seat access.
func (r *RoleAdMob) SetSeatAccess(a GrowthSeatAccess) {
	r.Enabled, r.Level, r.Accounts = a.On, a.Level, a.Scope
}

func (r *RoleAdMob) seatScopeKey() string { return "accounts" }
func (r *RoleAdMob) seatEnabled() **bool  { return &r.Enabled }

// LevelOrDefault is the seat's level, read where it names none.
func (r *RoleAdMob) LevelOrDefault() growth.Level {
	if r == nil {
		return growth.LevelRead
	}
	return growth.LevelOf(r.Level)
}

// validate holds the block to its shape: an enabled block names its OAuth
// client, which the sign-in is made as.
func (a *AdMob) validate(path Path) error {
	var p problems
	if a.Enabled && a.OAuth == nil {
		p.add(at(path, "oauth"), ErrMissing, "AdMob signs in as the company's own OAuth client: give "+
			"oauth.client_id and oauth.client_secret")
	}
	a.OAuth.validate(&p, at(path, "oauth"), a.Enabled)
	if n := len(a.Accounts); n > adMobMaxAccounts {
		p.add(at(path, "accounts"), ErrOutOfRange, "%d accounts is more than %d", n, adMobMaxAccounts)
	}
	checkPublisherIDs(&p, at(path, "accounts"), a.Accounts, adMobAccountPage)
	return p.err()
}

// validate holds an OAuth client to Google's shapes; required is whether its
// product is on, which then needs both values.
func (o *GoogleOAuthClient) validate(p *problems, here Path, required bool) {
	if o == nil {
		return
	}
	id := strings.TrimSpace(o.ClientID)
	switch {
	case id == "" && required:
		p.add(at(here, "client_id"), ErrMissing, "give the OAuth client's ID")
	case len(o.ClientID) > coord.MaxGrantAddressBytes:
		p.add(at(here, "client_id"), ErrOutOfRange, "is %d bytes, and the grant a sign-in keeps names a "+
			"client id of at most %d", len(o.ClientID), coord.MaxGrantAddressBytes)
	case id != "" && !googleOAuthClientID().MatchString(id):
		p.add(at(here, "client_id"), ErrUnknownValue, "%q is not an OAuth client ID: "+
			"<project number>-<id>.apps.googleusercontent.com", o.ClientID)
	}
	if strings.TrimSpace(o.ClientSecret) == "" && required {
		p.add(at(here, "client_secret"), ErrMissing, "give the OAuth client's secret, usually "+
			"${GOOGLE_OAUTH_CLIENT_SECRET}")
	}
}

// adMobAccountPage is where AdMob shows a publisher ID.
const adMobAccountPage = "AdMob's Settings, Account information"

// checkPublisherIDs holds publisher IDs to Google's shape, each once; page is
// where the product shows one.
func checkPublisherIDs(p *problems, here Path, ids []string, page string) {
	seen := map[string]bool{}
	for i, id := range ids {
		at := idx(here, i)
		switch {
		case !googlePublisher().MatchString(id):
			p.add(at, ErrUnknownValue, "%q is not a publisher ID: pub- and 16 digits, as %s shows it", id, page)
		case seen[id]:
			p.add(at, ErrConflict, "%q is listed twice", id)
		}
		seen[id] = true
	}
}

// check holds a seat's block to its level and accounts.
func (r *RoleAdMob) check(c *Company, p *problems, path Path) {
	if level := strings.TrimSpace(r.Level); level != "" && !growth.Level(level).Valid() {
		p.add(at(path, "level"), ErrUnknownValue, "%q is not an AdMob level; give %s", r.Level,
			words.Or(growth.LevelNames()))
	}
	checkPublisherIDs(p, at(path, "accounts"), r.Accounts, adMobAccountPage)
	company := c.Integrations.AdMob
	for i, id := range r.Accounts {
		if company != nil && googlePublisher().MatchString(id) && !company.Reaches(id) {
			p.add(idx(at(path, "accounts"), i), ErrUnknownValue, "account %q is not in "+
				"integrations.admob.accounts; add it there, or take it off this agent", id)
		}
	}
}
