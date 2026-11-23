package config

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/org"
)

// MetaAds connects the company's Meta ad accounts through the Marketing API:
// the engine holds one system user's tokens from the company's own Meta app
// and makes every call for the agents (ADR-0170).
type MetaAds struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Approvers route the integration's held acts; they never decide who may
	// answer (an administrator, ADR-0171).
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Optional. Handles of human seats asked first about a spend, something customers see or an account change an agent asks for. Only an administrator may answer; without this list every administrator is asked."`

	AppID string `yaml:"app_id,omitempty" json:"app_id,omitempty" desc:"The company's own Meta app's ID (digits), a Business app with the Marketing API product. Required when enabled."`

	AppSecret string `secret:"true" yaml:"app_secret,omitempty" json:"app_secret,omitempty" desc:"Reference to the sealed app secret, from the app's Basic settings. Every call carries a proof made with it, so turn on Require App Secret in the app's Advanced settings. Required when enabled."`

	ReadToken string `secret:"true" yaml:"read_token,omitempty" json:"read_token,omitempty" desc:"Reference to the sealed token of the app's Employee system user with ads_read and no permission that writes, for every read. Required when enabled."`

	OperateToken string `secret:"true" yaml:"operate_token,omitempty" json:"operate_token,omitempty" desc:"Optional. Reference to the sealed token of the same system user with ads_management, for operate changes and the spend and public acts an administrator allows. Without it agents read only."`

	AdminToken string `secret:"true" yaml:"admin_token,omitempty" json:"admin_token,omitempty" desc:"Optional. Reference to the sealed token of the same system user with business_management, used only for account changes an administrator allows (users, assets, catalogs, deletes), never for a read or a change made at once."`

	BusinessID string `yaml:"business_id,omitempty" json:"business_id,omitempty" desc:"Optional. The business portfolio's ID (digits). When set, the system user's ad accounts are read as this business's."`

	// AdAccounts narrows the ad accounts agents may be given; absent is every
	// ad account the system user is assigned.
	AdAccounts []string `yaml:"ad_accounts,omitempty" json:"ad_accounts,omitempty" desc:"Optional. IDs of the ad accounts agents may be given, act_ and its digits (bare digits are read as act_ and them). Absent: every ad account the system user is assigned."`

	// Spend caps each ad account's daily capacity (internal/spendguard).
	Spend *GrowthSpend `yaml:"spend,omitempty" json:"spend,omitempty" desc:"Optional. The spending caps per ad account (act_ and its digits), in its currency. An ad account without both caps refuses every spend act; reads, operate changes and pausing still work."`
}

// RoleMetaAds is a seat's access to Meta Ads.
type RoleMetaAds struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may use Meta Ads through the engine's tools. No agent holds a token."`

	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): reads ad accounts, campaigns, ad sets, ads, creatives, audiences and insights. operate: also pauses, lowers budgets and bids and makes changes nothing serves yet at once, and asks an administrator for any spend or anything customers see. administer: also asks for account changes and deletes."`

	AdAccounts []string `yaml:"ad_accounts,omitempty" json:"ad_accounts,omitempty" desc:"IDs of the ad accounts this agent may touch, act_ and its digits. Required while enabled."`

	BusinessResources bool `yaml:"business_resources,omitempty" json:"business_resources,omitempty" desc:"Optional. Let this agent reach what no ad account owns (the business portfolio, Pages, Instagram accounts, pixels, catalogs, apps, billing), within its level, and name it in a change."`

	Identity string `yaml:"identity,omitempty" json:"identity,omitempty" js:"enum=own|company" desc:"Optional. own: calls go as this agent's own system user (Crewlet makes it with the company's admin token, or a person makes it in Business settings), which Meta records under it and Crewlet checks before it keeps it and on every pass; until one is kept the agent reads Needs you. company (the default): calls go as the company's system user."`
}

// Meta's own shapes.
var (
	metaDigits    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,20}$`) })
	metaAdAccount = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(act_)?[0-9]{1,20}$`) })
)

// MetaAdAccountID is an ad account ID as Meta's paths write it, act_ and its
// digits, for bare digits too; "" for a value that is no ad account ID.
func MetaAdAccountID(v string) string {
	v = strings.TrimSpace(v)
	if !metaAdAccount().MatchString(v) {
		return ""
	}
	return "act_" + strings.TrimPrefix(v, "act_")
}

// AdAccountIDs are the block's ad accounts as Meta writes them.
func (m *MetaAds) AdAccountIDs() []string {
	if m == nil {
		return nil
	}
	return metaIDs(m.AdAccounts)
}

// AdAccountIDs are the seat's ad accounts as Meta writes them.
func (r *RoleMetaAds) AdAccountIDs() []string {
	if r == nil {
		return nil
	}
	return metaIDs(r.AdAccounts)
}

func metaIDs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if id := MetaAdAccountID(v); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// GrowthKind is the block's integration kind.
func (m *MetaAds) GrowthKind() string { return "meta_ads" }

// GrowthEnabled reports whether the block is on.
func (m *MetaAds) GrowthEnabled() bool { return m != nil && m.Enabled }

// GrowthApprovers are the people its held acts are routed to first.
func (m *MetaAds) GrowthApprovers() []string { return m.Approvers }

// GrowthSpend is the block's caps per ad account.
func (m *MetaAds) GrowthSpend() *GrowthSpend { return m.Spend }

func (m *MetaAds) validate(path Path) error {
	var p problems
	for i, acct := range m.AdAccounts {
		if MetaAdAccountID(acct) == "" {
			p.add(idx(at(path, "ad_accounts"), i), ErrUnknownValue, "%q is not an ad account ID: act_ and its digits", acct)
		}
	}
	if m.Spend != nil {
		for i, c := range m.Spend.Accounts {
			here := at(idx(at(path, "spend.accounts"), i), "account")
			switch acct := strings.TrimSpace(c.Account); {
			case acct == "":
			case !strings.HasPrefix(acct, "act_") || MetaAdAccountID(acct) == "":
				p.add(here, ErrUnknownValue, "%q is not an ad account ID as Meta writes it: act_ and its digits", c.Account)
			case len(m.AdAccounts) > 0 && !slices.Contains(m.AdAccountIDs(), acct):
				p.add(here, ErrUnknownValue, "%q is not among integrations.meta_ads.ad_accounts", c.Account)
			}
		}
	}
	for _, f := range []struct{ name, value string }{{"app_id", m.AppID}, {"business_id", m.BusinessID}} {
		if v := strings.TrimSpace(f.value); v != "" && !envref.Has(v) && !metaDigits().MatchString(v) {
			p.add(at(path, f.name), ErrUnknownValue, "%q is not a Meta %s: digits", f.value, f.name)
		}
	}
	for _, f := range []struct{ name, value string }{{"app_secret", m.AppSecret}, {"read_token", m.ReadToken},
		{"operate_token", m.OperateToken}, {"admin_token", m.AdminToken}} {
		if _, whole := envref.Whole(f.value); f.value != "" && !whole {
			p.add(at(path, f.name), ErrUnknownValue, "a ${VAR} naming the sealed value, never the value itself")
		}
	}
	if !m.Enabled {
		return p.err()
	}
	for _, f := range []struct{ name, value, what string }{
		{"app_id", m.AppID, "the company's Meta app's ID, on its Basic settings"},
		{"app_secret", m.AppSecret, "the app secret, on the app's Basic settings"},
		{"read_token", m.ReadToken, "the system user's token with ads_read"},
	} {
		if strings.TrimSpace(f.value) == "" {
			p.add(at(path, f.name), ErrMissing, "%s", f.what)
		}
	}
	return p.err()
}

// checkMetaAds holds a seat's level to the three and its ad accounts to the
// company's, each once.
func (c *Company) checkMetaAds(p *problems, path Path, b *RoleMetaAds) {
	if b == nil {
		return
	}
	if msg := growthLevelProblem(b.Level); msg != "" {
		p.add(at(path, "level"), ErrUnknownValue, "%s", msg)
	}
	reach := c.Integrations.MetaAds
	seen := map[string]bool{}
	for i, acct := range b.AdAccounts {
		here := idx(at(path, "ad_accounts"), i)
		id := MetaAdAccountID(acct)
		switch {
		case id == "":
			p.add(here, ErrUnknownValue, "%q is not an ad account ID: act_ and its digits", acct)
		case seen[id]:
			p.add(here, ErrConflict, "%q is listed twice", acct)
		case reach != nil && len(reach.AdAccounts) > 0 && !slices.Contains(reach.AdAccountIDs(), id):
			p.add(here, ErrUnknownValue, "%q is not among integrations.meta_ads.ad_accounts", acct)
		}
		seen[id] = true
	}
}

// MetaAdsSeat is agent seat handle's Meta Ads block while it and the
// company's block are on, nil otherwise.
func (c *Company) MetaAdsSeat(handle string) *RoleMetaAds {
	if c == nil || !c.Integrations.MetaAds.GrowthEnabled() {
		return nil
	}
	for role := range c.EachRole() {
		if role.Kind == org.KindHuman || role.IdentityKey() != handle {
			continue
		}
		if b := role.Integrations.MetaAds; b != nil && b.Enabled != nil && *b.Enabled {
			return b
		}
		return nil
	}
	return nil
}

// GrowthKind is the seat block's integration kind, the company block's.
func (*RoleMetaAds) GrowthKind() string { return (*MetaAds)(nil).GrowthKind() }

// SeatAccess is the block as seat access reads it.
func (r *RoleMetaAds) SeatAccess() GrowthSeatAccess {
	return GrowthSeatAccess{On: r.Enabled, Level: string(growth.LevelOf(r.Level)), Scope: r.AdAccounts,
		Identity: r.Identity}
}

// SetSeatAccess writes the block from seat access; business_resources is kept.
func (r *RoleMetaAds) SetSeatAccess(a GrowthSeatAccess) {
	r.Enabled, r.Level, r.AdAccounts, r.Identity = a.On, a.Level, a.Scope, a.Identity
}

func (r *RoleMetaAds) seatIdentity() *string { return &r.Identity }

func (r *RoleMetaAds) seatScopeKey() string { return "ad_accounts" }
func (r *RoleMetaAds) seatEnabled() **bool  { return &r.Enabled }

// check holds the block to the company's reach.
func (r *RoleMetaAds) check(c *Company, p *problems, here Path) { c.checkMetaAds(p, here, r) }

// GrowthSeatLevel is the seat's level while its block is on, "" while off.
func (r *RoleMetaAds) GrowthSeatLevel() string {
	if r == nil || r.Enabled == nil || !*r.Enabled {
		return ""
	}
	return string(growth.LevelOf(r.Level))
}
