package config

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// SearchConsole lets agent seats read and manage Google Search Console
// properties, their site verification and Indexing API notifications, as each
// seat's own Google growth identity (internal/googleauth, ADR-0172), which a
// person adds to each property at the permission the seat may act with. It
// signs in through the Google Cloud connection's identity, so it needs
// integrations.google_cloud on with its provisioning block. It holds no
// secret.
type SearchConsole struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on. Needs integrations.google_cloud on with its provisioning block, whose identity agents sign in through."`

	// Sites narrows what agents may be given; absent is every property an
	// agent's identity reaches.
	Sites []string `yaml:"sites,omitempty" json:"sites,omitempty" desc:"Search Console properties agents may be given, at most 1000, each as Search Console writes it: a URL prefix with its scheme and trailing slash (https://www.example.com/) or a domain (sc-domain:example.com). Absent: every property an agent's identity is added to. Optional."`

	// Approvers route an agent's held acts: who is asked first.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Handles of human seats an agent's admin acts are routed to first. Only an administrator answers one, at a fresh step-up; without approvers every administrator is asked. Optional."`
}

// RoleSearchConsole is one seat's Search Console access.
type RoleSearchConsole struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may reach Search Console. Off turns its Google growth identity off once no other Google growth product is on for it."`

	// Level is one level for every site the seat names; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): search performance, sitemaps, URL inspection and verification reads, nothing changed. operate: also submitting and removing sitemaps and Indexing API notifications. administer: also asks an administrator to approve adding or removing a property, verifying a site and changing its owners, one each."`

	Sites []string `yaml:"sites,omitempty" json:"sites,omitempty" desc:"Search Console properties this agent may reach, within integrations.search_console.sites when that is set, each as Search Console writes it (https://www.example.com/ or sc-domain:example.com). Required when the agent is on."`
}

// SearchConsoleKind is the integration's kind, config key and lane.
const SearchConsoleKind = "search_console"

// searchConsoleMaxSites bounds a block's sites at Search Console's own limit
// on the properties one account may hold.
const searchConsoleMaxSites = 1000

// searchConsoleDomain is a Domain property's host: a lowercase DNS name with
// a dot, punycode for an international one, as Search Console writes it.
var searchConsoleDomain = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^` + searchConsoleDomainPattern + `$`)
})

const searchConsoleDomainPattern = `([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`

// SearchConsoleSitePattern is the shape [SearchConsoleSite] holds a site to,
// which the seat picker checks a typed site against before offering it; the
// document's own check then refuses what the shape alone lets by.
const SearchConsoleSitePattern = `^(sc-domain:` + searchConsoleDomainPattern + `|https?://` +
	searchConsoleDomainPattern + `(:[0-9]{1,5})?/[^?#\s]*)$`

// SearchConsoleSite reports whether s is a Search Console property as Google
// writes it: `sc-domain:` and a domain, or an http or https URL prefix with
// no query or fragment, ending in a slash.
func SearchConsoleSite(s string) bool {
	if domain, ok := strings.CutPrefix(s, "sc-domain:"); ok {
		return len(domain) <= 253 && searchConsoleDomain().MatchString(domain)
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || !strings.HasSuffix(u.Path, "/") {
		return false
	}
	host := u.Hostname()
	return host == strings.ToLower(host) && searchConsoleDomain().MatchString(host) && u.String() == s
}

// GrowthKind is the block's kind: with GrowthEnabled, GrowthApprovers and
// GrowthSpend, it makes the block one the shared growth rules find (growth.go).
func (g *SearchConsole) GrowthKind() string { return SearchConsoleKind }

// GrowthEnabled reports whether the block is on.
func (g *SearchConsole) GrowthEnabled() bool { return g != nil && g.Enabled }

// GrowthApprovers are the people an agent's held acts are routed to first.
func (g *SearchConsole) GrowthApprovers() []string { return g.Approvers }

// GrowthSpend is nil: nothing in Search Console spends money.
func (g *SearchConsole) GrowthSpend() *GrowthSpend { return nil }

// Reaches reports whether the company lets agents be given site.
func (g *SearchConsole) Reaches(site string) bool {
	return g != nil && (len(g.Sites) == 0 || slices.Contains(g.Sites, site))
}

// GrowthKind is the kind of the product the seat block reaches.
func (r *RoleSearchConsole) GrowthKind() string { return SearchConsoleKind }

// GrowthSeatLevel is the seat's level where it is on with a site, which
// internal/authz's growth grants admit to its tools; "" where it is off.
func (r *RoleSearchConsole) GrowthSeatLevel() string {
	if r == nil || !SeatAccessOn(r.Enabled) || len(r.Sites) == 0 {
		return ""
	}
	return string(r.LevelOrDefault())
}

// SeatAccess is the block as seat access reads it.
func (r *RoleSearchConsole) SeatAccess() GrowthSeatAccess {
	return GrowthSeatAccess{On: r.Enabled, Level: string(r.LevelOrDefault()), Scope: r.Sites}
}

// SetSeatAccess writes the block from seat access.
func (r *RoleSearchConsole) SetSeatAccess(a GrowthSeatAccess) {
	r.Enabled, r.Level, r.Sites = a.On, a.Level, a.Scope
}

func (r *RoleSearchConsole) seatScopeKey() string { return "sites" }
func (r *RoleSearchConsole) seatEnabled() **bool  { return &r.Enabled }

// LevelOrDefault is the seat's level, read where it names none.
func (r *RoleSearchConsole) LevelOrDefault() growth.Level {
	if r == nil {
		return growth.LevelRead
	}
	return growth.LevelOf(r.Level)
}

// validate holds the block to its shape; in is the document's integrations,
// whose Google Cloud connection an enabled block signs in through.
func (g *SearchConsole) validate(in *Integrations, path Path) error {
	var p problems
	if g.Enabled {
		if gc := in.GoogleCloud; gc == nil || !gc.Enabled || gc.Provisioning == nil {
			p.add(at(path, "enabled"), ErrMissing, "Search Console signs in through the Google Cloud "+
				"connection's identity: turn integrations.google_cloud on with its provisioning block "+
				"(no project needs preparing)")
		}
	}
	if n := len(g.Sites); n > searchConsoleMaxSites {
		p.add(at(path, "sites"), ErrOutOfRange, "%d sites is more than %d", n, searchConsoleMaxSites)
	}
	checkSearchConsoleSites(&p, at(path, "sites"), g.Sites)
	return p.err()
}

// checkSearchConsoleSites holds sites to Search Console's shape, each once.
func checkSearchConsoleSites(p *problems, here Path, sites []string) {
	seen := map[string]bool{}
	for i, s := range sites {
		at := idx(here, i)
		switch {
		case !SearchConsoleSite(s):
			p.add(at, ErrUnknownValue, "%q is not a Search Console property: write a URL prefix with its "+
				"scheme and trailing slash (https://www.example.com/) or a domain (sc-domain:example.com)", s)
		case seen[s]:
			p.add(at, ErrConflict, "%q is listed twice", s)
		}
		seen[s] = true
	}
}

// check holds a seat's block to its level and sites.
func (r *RoleSearchConsole) check(c *Company, p *problems, path Path) {
	if level := strings.TrimSpace(r.Level); level != "" && !growth.Level(level).Valid() {
		p.add(at(path, "level"), ErrUnknownValue, "%q is not a Search Console level; give %s", r.Level,
			words.Or(growth.LevelNames()))
	}
	checkSearchConsoleSites(p, at(path, "sites"), r.Sites)
	company := c.Integrations.SearchConsole
	for i, s := range r.Sites {
		if company != nil && SearchConsoleSite(s) && !company.Reaches(s) {
			p.add(idx(at(path, "sites"), i), ErrUnknownValue, "site %q is not in "+
				"integrations.search_console.sites; add it there, or take it off this agent", s)
		}
	}
}
