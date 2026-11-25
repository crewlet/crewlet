package config

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// GoogleAnalytics lets agent seats read and manage Google Analytics 4
// properties as each seat's own Google growth identity (internal/googleauth,
// ADR-0172), which a person adds to each property at the role the seat may
// act with. It signs in through the Google Cloud connection's identity, so it
// needs integrations.google_cloud on with its provisioning block. It holds no
// secret.
type GoogleAnalytics struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on. Needs integrations.google_cloud on with its provisioning block, whose identity agents sign in through."`

	// Properties narrows what agents may be given; absent is every property
	// an agent's identity reaches.
	Properties []string `yaml:"properties,omitempty" json:"properties,omitempty" desc:"Numeric Google Analytics property IDs agents may be given, at most 200. Absent: every property an agent's identity is added to. Optional."`

	// OAuth is the client an Administrator signs in as, so Crewlet adds each
	// agent's identity itself.
	OAuth *GoogleOAuthClient `yaml:"oauth,omitempty" json:"oauth,omitempty" desc:"The OAuth client Google Analytics' admin sign-in alone is made as, in place of integrations.google_cloud.sign_in_client. Optional."`

	// Approvers route an agent's held acts: who is asked first.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Handles of human seats an agent's admin acts are routed to first. Only an administrator answers one, at a fresh step-up; without approvers every administrator is asked. Optional."`
}

// RoleGoogleAnalytics is one seat's Google Analytics access.
type RoleGoogleAnalytics struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may reach Google Analytics. Off turns its Google growth identity off once no other Google growth product is on for it."`

	// Level is one level for every property the seat names; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): reports and settings, nothing changed. operate: also reversible changes such as key events, custom definitions and links' settings. administer: also asks an administrator to approve account, access, key and other irreversible changes, one each."`

	Properties []string `yaml:"properties,omitempty" json:"properties,omitempty" desc:"Numeric property IDs this agent may reach, within integrations.google_analytics.properties when that is set. Required when the agent is on."`
}

// GoogleAnalyticsKind is the integration's kind, config key and lane.
const GoogleAnalyticsKind = "google_analytics"

// googleAnalyticsMaxProperties bounds a block's properties: a company with
// more is an agency, which connects each client's company.
const googleAnalyticsMaxProperties = 200

// googleAnalyticsProperty is a property's ID as Google writes it: digits.
var googleAnalyticsProperty = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9]{1,20}$`)
})

// GrowthKind makes the block one of the growth blocks the shared rules find
// (growth.go).
func (g *GoogleAnalytics) GrowthKind() string { return GoogleAnalyticsKind }

// GrowthEnabled reports whether the block is on.
func (g *GoogleAnalytics) GrowthEnabled() bool { return g != nil && g.Enabled }

// GrowthApprovers are the people its held acts are routed to first.
func (g *GoogleAnalytics) GrowthApprovers() []string { return g.Approvers }

// GrowthSpend is nil: nothing in Google Analytics is spend. A BigQuery export
// bills the linked Google Cloud project with no amount a cap could hold, so
// turning one on is an admin act, held at a fresh step-up.
func (g *GoogleAnalytics) GrowthSpend() *GrowthSpend { return nil }

// Reaches reports whether the company lets agents be given property id.
func (g *GoogleAnalytics) Reaches(id string) bool {
	return g != nil && (len(g.Properties) == 0 || slices.Contains(g.Properties, id))
}

// GrowthKind is the seat block's integration kind.
func (r *RoleGoogleAnalytics) GrowthKind() string { return GoogleAnalyticsKind }

// GrowthSeatLevel is the seat's level where it is on with a property, which
// internal/authz's Google Analytics grants admit to its tools; "" where it is
// off.
func (r *RoleGoogleAnalytics) GrowthSeatLevel() string {
	if r == nil || !SeatAccessOn(r.Enabled) || len(r.Properties) == 0 {
		return ""
	}
	return string(r.LevelOrDefault())
}

// SeatAccess is the block as seat access reads it.
func (r *RoleGoogleAnalytics) SeatAccess() GrowthSeatAccess {
	return GrowthSeatAccess{On: r.Enabled, Level: string(r.LevelOrDefault()), Scope: r.Properties}
}

// SetSeatAccess writes the block from seat access.
func (r *RoleGoogleAnalytics) SetSeatAccess(a GrowthSeatAccess) {
	r.Enabled, r.Level, r.Properties = a.On, a.Level, a.Scope
}

func (r *RoleGoogleAnalytics) seatScopeKey() string { return "properties" }
func (r *RoleGoogleAnalytics) seatEnabled() **bool  { return &r.Enabled }

// LevelOrDefault is the seat's level, read where it names none.
func (r *RoleGoogleAnalytics) LevelOrDefault() growth.Level {
	if r == nil {
		return growth.LevelRead
	}
	return growth.LevelOf(r.Level)
}

// validate holds the block to its shape; in is the document's integrations,
// whose Google Cloud connection an enabled block signs in through.
func (g *GoogleAnalytics) validate(in *Integrations, path Path) error {
	var p problems
	if g.Enabled {
		if gc := in.GoogleCloud; gc == nil || !gc.Enabled || gc.Provisioning == nil {
			p.add(at(path, "enabled"), ErrMissing, "Google Analytics signs in through the Google Cloud "+
				"connection's identity: turn integrations.google_cloud on with its provisioning block "+
				"(no project needs preparing)")
		}
	}
	g.OAuth.validate(&p, at(path, "oauth"), g.Enabled)
	if n := len(g.Properties); n > googleAnalyticsMaxProperties {
		p.add(at(path, "properties"), ErrOutOfRange, "%d properties is more than %d", n,
			googleAnalyticsMaxProperties)
	}
	checkGoogleAnalyticsProperties(&p, at(path, "properties"), g.Properties)
	return p.err()
}

// checkGoogleAnalyticsProperties holds property IDs to Google's shape, each
// once.
func checkGoogleAnalyticsProperties(p *problems, here Path, ids []string) {
	seen := map[string]bool{}
	for i, id := range ids {
		at := idx(here, i)
		switch {
		case !googleAnalyticsProperty().MatchString(id):
			p.add(at, ErrUnknownValue, "%q is not a Google Analytics property ID: digits, as in "+
				"properties/123456789", id)
		case seen[id]:
			p.add(at, ErrConflict, "%q is listed twice", id)
		}
		seen[id] = true
	}
}

// check holds a seat's block to its level and properties.
func (r *RoleGoogleAnalytics) check(c *Company, p *problems, path Path) {
	if level := strings.TrimSpace(r.Level); level != "" && !growth.Level(level).Valid() {
		p.add(at(path, "level"), ErrUnknownValue, "%q is not a Google Analytics level; give %s", r.Level,
			words.Or(growth.LevelNames()))
	}
	checkGoogleAnalyticsProperties(p, at(path, "properties"), r.Properties)
	company := c.Integrations.GoogleAnalytics
	for i, id := range r.Properties {
		if company != nil && googleAnalyticsProperty().MatchString(id) && !company.Reaches(id) {
			p.add(idx(at(path, "properties"), i), ErrUnknownValue, "property %q is not in "+
				"integrations.google_analytics.properties; add it there, or take it off this agent", id)
		}
	}
}
