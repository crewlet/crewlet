package config

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// SeatGrant is one permission an agent seat holds at an integration beside
// its level: a Cloudflare permission group on some zones or the account, for
// a while or for good. Levels stay the defaults; a grant gives one agent one
// permission no level holds. internal/seataccess reads it as the plan's
// Grant, and only the live ones reach the agent's credential.
//
// WRITTEN BY THE ENGINE, never typed: the Agents tab's save fills the id, who
// granted it and when, and holds the permission and scope to the vendor's
// live catalogue. The document's own rules below hold the shape alone.
type SeatGrant struct {
	// ID names the grant in the audit trail and the ask that requested it.
	ID string `yaml:"id" json:"id,omitempty" js:"pattern=^g[a-z0-9]{10}$" desc:"The grant's identifier, which the engine writes: g and ten lowercase letters or digits."`

	// Permission is the vendor's own name for one permission, as its live
	// catalogue lists it.
	Permission string `yaml:"permission" json:"permission" js:"maxlen=200" desc:"The permission granted, as the integration's catalogue names it (a Cloudflare permission group, such as Origin Write)."`

	// Scope is what the permission applies to; empty is its whole reach on
	// the company's connection.
	Scope []string `yaml:"scope,omitempty" json:"scope,omitempty" desc:"What the permission applies to, such as Cloudflare zone names. Empty is its whole reach: every zone the company's agents reach for a zone permission, the account for an account one."`

	// ExpiresAt is when the grant lapses; zero is permanent.
	ExpiresAt time.Time `yaml:"expires_at,omitempty" json:"expires_at,omitzero" desc:"When the grant lapses, in UTC. Absent is permanent."`

	// GrantedBy is who saved it, as the engine names a person.
	GrantedBy string `yaml:"granted_by" json:"granted_by,omitempty" js:"maxlen=200" desc:"Who granted it, as the engine recorded them."`

	// GrantedAt is when it was saved.
	GrantedAt time.Time `yaml:"granted_at" json:"granted_at,omitzero" desc:"When it was granted, in UTC."`

	// Reason is why: the agent's request or the person's note.
	Reason string `yaml:"reason" json:"reason" js:"maxlen=1500" desc:"Why it was granted: the agent's request, or the note of the person who added it."`

	// Request is the agent's request the grant answered, where it answered
	// one.
	Request string `yaml:"request,omitempty" json:"request,omitempty" js:"pattern=^[a-z0-9]{10}$" desc:"The agent's request the grant answered, which the engine writes; absent for one a person added."`

	// Target is what the one change a grant was asked for changes, as the
	// vendor names it (a bucket), copied from the agent's request.
	Target string `yaml:"target,omitempty" json:"target,omitempty" js:"pattern=^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,254}$" desc:"What the change the agent asked the grant for changes, as the integration names it (a Scaleway bucket), which the engine copies from the request; absent for one a person added."`
}

// MaxSeatGrants is how many grants one seat may hold at one integration: one
// screen of the Agents tab, and well inside a Cloudflare token's policy size.
const MaxSeatGrants = 20

// MaxSeatGrantReason bounds a grant's reason: a decision's rationale, which
// an agent's request is posted as.
const MaxSeatGrantReason = 1500

// Live reports whether the grant holds at now: permanent, or not yet lapsed.
func (g SeatGrant) Live(now time.Time) bool { return g.ExpiresAt.IsZero() || now.Before(g.ExpiresAt) }

// seatGrantID and seatGrantRequest are the shapes the engine gives a grant's
// id and a request's. Compiled at first use, as every pattern in this package.
var (
	seatGrantID      = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^g[a-z0-9]{10}$`) })
	seatGrantRequest = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z0-9]{10}$`) })
	seatGrantTarget  = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,254}$`)
	})
)

// SeatGrantTarget reports a grant's target of the shape a vendor names one
// resource by: no space, quote or line break, so it reads safely in a prompt.
func SeatGrantTarget(s string) bool { return seatGrantTarget().MatchString(s) }

// checkSeatGrants holds one seat block's grants to their shape: ids written
// by the engine and unique, a permission named, a scope listed once, the
// times set and in order, and no more than [MaxSeatGrants]. What a grant may
// name is the vendor's live catalogue, checked where the grant is saved.
func checkSeatGrants(p *problems, path Path, grants []SeatGrant) {
	if len(grants) > MaxSeatGrants {
		p.add(path, ErrOutOfRange, "%d grants; a seat holds at most %d at one integration",
			len(grants), MaxSeatGrants)
	}
	ids := map[string]bool{}
	for i, g := range grants {
		here := idx(path, i)
		switch {
		case !seatGrantID().MatchString(g.ID):
			p.add(at(here, "id"), ErrShape, "%q is not a grant id the engine writes "+
				"(g and ten lowercase letters or digits); add grants on the integration's Agents tab", g.ID)
		case ids[g.ID]:
			p.add(at(here, "id"), ErrConflict, "%q names two grants", g.ID)
		}
		ids[g.ID] = true
		if strings.TrimSpace(g.Permission) == "" {
			p.add(at(here, "permission"), ErrMissing, "name the permission the grant gives")
		}
		seen := map[string]bool{}
		for j, entry := range g.Scope {
			key := strings.TrimSpace(entry)
			switch {
			case key == "":
				p.add(idx(at(here, "scope"), j), ErrMissing, "a scope entry is empty; remove it or name what it reaches")
			case seen[key]:
				p.add(idx(at(here, "scope"), j), ErrConflict, "%q is listed twice", entry)
			}
			seen[key] = true
		}
		if strings.TrimSpace(g.GrantedBy) == "" {
			p.add(at(here, "granted_by"), ErrMissing, "name who granted it")
		}
		if g.GrantedAt.IsZero() {
			p.add(at(here, "granted_at"), ErrMissing, "say when it was granted")
		}
		if !g.ExpiresAt.IsZero() && !g.GrantedAt.IsZero() && !g.ExpiresAt.After(g.GrantedAt) {
			p.add(at(here, "expires_at"), ErrOutOfRange, "the grant lapses before it was granted")
		}
		if strings.TrimSpace(g.Reason) == "" {
			p.add(at(here, "reason"), ErrMissing, "say why it was granted")
		} else if n := utf8.RuneCountInString(g.Reason); n > MaxSeatGrantReason {
			p.add(at(here, "reason"), ErrOutOfRange, "%d characters; a reason is at most %d", n, MaxSeatGrantReason)
		}
		if g.Request != "" && !seatGrantRequest().MatchString(g.Request) {
			p.add(at(here, "request"), ErrShape, "%q is not a request id the engine writes", g.Request)
		}
		if g.Target != "" && !SeatGrantTarget(g.Target) {
			p.add(at(here, "target"), ErrShape, "%q is not one resource's name; the engine copies it from "+
				"the agent's request", g.Target)
		}
	}
}

// LiveSeatGrants is the grants that hold at now, in the order written.
func LiveSeatGrants(grants []SeatGrant, now time.Time) []SeatGrant {
	return slices.DeleteFunc(slices.Clone(grants), func(g SeatGrant) bool { return !g.Live(now) })
}
