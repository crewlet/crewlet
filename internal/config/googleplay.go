package config

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// GooglePlay lets agent seats read and manage Google Play apps as each seat's
// own Google growth identity (internal/googleauth, ADR-0172), which a person
// invites in Play Console with the app permissions the seat may act with. It
// signs in through the Google Cloud connection's identity, so it needs
// integrations.google_cloud on with its provisioning block. It holds no secret.
type GooglePlay struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on. Needs integrations.google_cloud on with its provisioning block, whose identity agents sign in through."`

	// Apps narrows what agents may be given; absent is every app an agent's
	// identity is invited to.
	Apps []string `yaml:"apps,omitempty" json:"apps,omitempty" desc:"Package names of the apps agents may be given, at most 200. Absent: every app an agent's identity is invited to. Optional."`

	// DeveloperAccounts are the Play developer accounts whose users and
	// grants an agent may ask an administrator to change.
	DeveloperAccounts []string `yaml:"developer_accounts,omitempty" json:"developer_accounts,omitempty" desc:"Google Play developer account IDs (the digits after /developers/ in a Play Console address), at most 20, whose users and permissions an administer agent may ask to change. Optional."`

	// ReportsBuckets are the monthly reports buckets the company's reader
	// reads installs, ratings, reviews and financial reports from.
	ReportsBuckets []string `yaml:"reports_buckets,omitempty" json:"reports_buckets,omitempty" desc:"Each developer account's monthly reports bucket, as Play Console's Download reports page copies it (pubsite_prod_rev_ followed by digits), at most 20. The company reader reads them. Optional."`

	// OAuth is the client a Play Console admin signs in as, so Crewlet adds
	// each agent's identity itself.
	OAuth *GoogleOAuthClient `yaml:"oauth,omitempty" json:"oauth,omitempty" desc:"The OAuth client Google Play's admin sign-in alone is made as, in place of integrations.google_cloud.sign_in_client. Optional."`

	// Approvers route an agent's held acts: who is asked first.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Handles of human seats an agent's publish, spend and admin acts are routed to first. Only an administrator answers one, at a fresh step-up; without approvers every administrator is asked. Optional."`
}

// RoleGooglePlay is one seat's Google Play access.
type RoleGooglePlay struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may reach Google Play. Off turns its Google growth identity off once no other Google growth product is on for it."`

	// Level is one level for every app the seat names; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): reviews, listings, releases, products, vitals and reports, nothing changed. operate: also draft releases, halted rollouts, tester groups and draft products made at once, and asks an administrator to approve each reply, listing change, release, price and refund. administer: also asks for user, access, key, deletion and other irreversible changes."`

	Apps []string `yaml:"apps,omitempty" json:"apps,omitempty" desc:"Package names of the apps this agent may reach, within integrations.google_play.apps when that is set. Required when the agent is on."`
}

// GooglePlayKind is the integration's kind, config key and lane.
const GooglePlayKind = "google_play"

// Bounds of a block's lists: a company with more apps is a publisher that
// connects each studio's company, and more developer accounts than this is an
// agency.
const (
	googlePlayMaxApps     = 200
	googlePlayMaxAccounts = 20
)

var (
	// googlePlayPackage is an Android package name: two or more dotted parts,
	// each a letter then letters, digits or underscores (Play's limit is 150).
	googlePlayPackage = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	})
	// googlePlayDeveloper is a developer account's ID: digits.
	googlePlayDeveloper = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,25}$`) })
	// googlePlayBucket is a monthly reports bucket, new or legacy.
	googlePlayBucket = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^pubsite_prod(_rev)?_[0-9]{1,25}$`)
	})
)

// GooglePlayPackage reports whether name is an Android package name.
func GooglePlayPackage(name string) bool {
	return len(name) <= 150 && googlePlayPackage().MatchString(name)
}

// GrowthKind makes the block one of the growth blocks the shared rules find
// (growth.go).
func (g *GooglePlay) GrowthKind() string { return GooglePlayKind }

// GrowthEnabled reports whether the block is on.
func (g *GooglePlay) GrowthEnabled() bool { return g != nil && g.Enabled }

// GrowthApprovers are the people its held acts are routed to first.
func (g *GooglePlay) GrowthApprovers() []string { return g.Approvers }

// GrowthSpend is nil: Google Play's spend is refunds, each one a person's
// decision on the order's own amount, never a budget a cap bounds.
func (g *GooglePlay) GrowthSpend() *GrowthSpend { return nil }

// Reaches reports whether the company lets agents be given app name.
func (g *GooglePlay) Reaches(name string) bool {
	return g != nil && (len(g.Apps) == 0 || slices.Contains(g.Apps, name))
}

// GrowthKind is the seat block's integration kind.
func (r *RoleGooglePlay) GrowthKind() string { return GooglePlayKind }

// GrowthSeatLevel is the seat's level where it is on with an app, which
// internal/authz's Google Play grants admit to its tools; "" where it is off.
func (r *RoleGooglePlay) GrowthSeatLevel() string {
	if r == nil || !SeatAccessOn(r.Enabled) || len(r.Apps) == 0 {
		return ""
	}
	return string(r.LevelOrDefault())
}

// SeatAccess is the block as seat access reads it.
func (r *RoleGooglePlay) SeatAccess() GrowthSeatAccess {
	return GrowthSeatAccess{On: r.Enabled, Level: string(r.LevelOrDefault()), Scope: r.Apps}
}

// SetSeatAccess writes the block from seat access.
func (r *RoleGooglePlay) SetSeatAccess(a GrowthSeatAccess) {
	r.Enabled, r.Level, r.Apps = a.On, a.Level, a.Scope
}

func (r *RoleGooglePlay) seatScopeKey() string { return "apps" }
func (r *RoleGooglePlay) seatEnabled() **bool  { return &r.Enabled }

// LevelOrDefault is the seat's level, read where it names none.
func (r *RoleGooglePlay) LevelOrDefault() growth.Level {
	if r == nil {
		return growth.LevelRead
	}
	return growth.LevelOf(r.Level)
}

// validate holds the block to its shape; in is the document's integrations,
// whose Google Cloud connection an enabled block signs in through.
func (g *GooglePlay) validate(in *Integrations, path Path) error {
	var p problems
	if g.Enabled {
		if gc := in.GoogleCloud; gc == nil || !gc.Enabled || gc.Provisioning == nil {
			p.add(at(path, "enabled"), ErrMissing, "Google Play signs in through the Google Cloud connection's "+
				"identity: turn integrations.google_cloud on with its provisioning block (no project needs "+
				"preparing)")
		}
	}
	g.OAuth.validate(&p, at(path, "oauth"), g.Enabled)
	checkGooglePlayIDs(&p, at(path, "apps"), g.Apps, googlePlayMaxApps, GooglePlayPackage, packageShape)
	checkGooglePlayIDs(&p, at(path, "developer_accounts"), g.DeveloperAccounts, googlePlayMaxAccounts,
		googlePlayDeveloper().MatchString, "a developer account ID: the digits after /developers/ in a Play "+
			"Console address")
	checkGooglePlayIDs(&p, at(path, "reports_buckets"), g.ReportsBuckets, googlePlayMaxAccounts,
		googlePlayBucket().MatchString, "a monthly reports bucket: pubsite_prod_rev_ followed by digits, as Play "+
			"Console's Download reports page copies it without gs://")
	return p.err()
}

// packageShape says what a package name looks like.
const packageShape = "an Android package name such as com.example.app"

// checkGooglePlayIDs holds a list to at most most entries, each valid and once.
func checkGooglePlayIDs(p *problems, here Path, ids []string, most int, valid func(string) bool, shape string) {
	if len(ids) > most {
		p.add(here, ErrOutOfRange, "%d entries is more than %d", len(ids), most)
	}
	seen := map[string]bool{}
	for i, id := range ids {
		at := idx(here, i)
		switch {
		case !valid(id):
			p.add(at, ErrUnknownValue, "%q is not %s", id, shape)
		case seen[id]:
			p.add(at, ErrConflict, "%q is listed twice", id)
		}
		seen[id] = true
	}
}

// check holds a seat's block to its level and apps.
func (r *RoleGooglePlay) check(c *Company, p *problems, path Path) {
	if level := strings.TrimSpace(r.Level); level != "" && !growth.Level(level).Valid() {
		p.add(at(path, "level"), ErrUnknownValue, "%q is not a Google Play level; give %s", r.Level,
			words.Or(growth.LevelNames()))
	}
	checkGooglePlayIDs(p, at(path, "apps"), r.Apps, googlePlayMaxApps, GooglePlayPackage, packageShape)
	company := c.Integrations.GooglePlay
	for i, name := range r.Apps {
		if company != nil && GooglePlayPackage(name) && !company.Reaches(name) {
			p.add(idx(at(path, "apps"), i), ErrUnknownValue, "app %q is not in integrations.google_play.apps; "+
				"add it there, or take it off this agent", name)
		}
	}
}
