package seataccess

import (
	"cmp"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// A seat's access at a lane is its level plus its live grants: one vendor
// permission each, on a scope, held beside the level for a while or for good.
// The level stays the safe default; a grant is how one agent is given one
// permission no level holds, with no engine change. This file is the shared
// half every vendor reads the same way; a vendor whose lane takes grants
// ([LaneSpec.Grants]) adds its catalogue ([Granter]), reads its grants back
// ([Observed.Grants]) and narrows its credential to level and grants
// ([OpGrants]).

// Grant is one grant as the company document stores it, the document's own
// type, so the document and the wire cannot spell a grant two ways.
type Grant = config.SeatGrant

// MaxGrants is how many grants a seat holds at one lane ([config.MaxSeatGrants]).
const MaxGrants = config.MaxSeatGrants

// GrantHours bound how long an expiring grant may last, and DefaultGrantHours
// is the length an agent's request offers when it names none: long enough
// for one change and its checks, short enough that it lapses the same day.
const (
	MinGrantHours     = 1
	MaxGrantHours     = 168
	DefaultGrantHours = 4
)

// LapseLead is how long before its expiry a grant leaves the credential. The
// integration loop wakes every 15 seconds and a pass takes seconds, so a pass
// armed this far ahead ([NextExpiry]) has narrowed the credential before the
// instant a person was told the grant lapses: a grant never outlives it.
const LapseLead = time.Minute

// GrantKey is a grant as one comparable string, its permission and sorted
// scope: what decides whether two grants give the same.
func GrantKey(g Grant) string {
	return g.Permission + "@" + strings.Join(normalSet(g.Scope), ",")
}

// LiveGrants is the grants a credential holds at now: each permanent or not
// within [LapseLead] of its expiry, each key once (the latest lapse kept),
// sorted by key. Nil for none.
func LiveGrants(grants []Grant, now time.Time) []Grant {
	at := now.Add(LapseLead)
	var out []Grant
	for _, g := range NormalGrants(grants) {
		if !g.Live(at) {
			continue
		}
		i := slices.IndexFunc(out, func(h Grant) bool { return GrantKey(h) == GrantKey(g) })
		switch {
		case i < 0:
			out = append(out, g)
		case outlasts(g, out[i]):
			out[i] = g
		}
	}
	return out
}

// DesiredAt is [DesiredOf] as a plan reads it at now: its grants only the
// live ones.
func DesiredAt(role *config.Role, lane Lane, now time.Time) (Desired, bool) {
	d, ok := DesiredOf(role, lane)
	d.Grants = LiveGrants(d.Grants, now)
	return d, ok
}

// NextExpiry is the instant the earliest live grant of the company leaves its
// credential, zero where none lapses: the integration loop runs the kind's
// next pass then, rather than on its settled interval.
func NextExpiry(c *config.Company, kind integration.Kind, now time.Time) time.Time {
	var next time.Time
	for _, seat := range SeatsOf(c) {
		for _, lane := range LanesOf(kind) {
			d, ok := DesiredOf(seat.Role, lane)
			if !ok || !d.Enabled {
				continue
			}
			for _, g := range LiveGrants(d.Grants, now) {
				if g.ExpiresAt.IsZero() {
					continue
				}
				if lapse := g.ExpiresAt.Add(-LapseLead); next.IsZero() || lapse.Before(next) {
					next = lapse
				}
			}
		}
	}
	return next
}

// NormalGrants is grants with each permission and scope entry trimmed, the
// scope sorted and deduplicated, sorted by key then id: the form a [Desired]
// holds and the document stores. Every stored grant is kept, so a save never
// drops one; nil for none.
func NormalGrants(grants []Grant) []Grant {
	var out []Grant
	for _, g := range grants {
		g.Permission = strings.TrimSpace(g.Permission)
		g.Scope = normalSet(g.Scope)
		if g.Permission != "" {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b Grant) int {
		return cmp.Or(cmp.Compare(GrantKey(a), GrantKey(b)), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// outlasts reports whether a lapses later than b, permanent being latest.
func outlasts(a, b Grant) bool {
	switch {
	case b.ExpiresAt.IsZero():
		return false
	case a.ExpiresAt.IsZero():
		return true
	default:
		return a.ExpiresAt.After(b.ExpiresAt)
	}
}

// SameGrants reports whether two lists give the same permissions on the same
// scopes, whatever their order, ids, authors or expiries.
func SameGrants(a, b []Grant) bool {
	return slices.Equal(grantKeys(a), grantKeys(b))
}

func grantKeys(grants []Grant) []string {
	out := make([]string, 0, len(grants))
	for _, g := range NormalGrants(grants) {
		out = append(out, GrantKey(g))
	}
	return slices.Compact(out)
}

// GrantScopeKind is what a grant's scope entries are: the vendor's own word
// for them, as a picker labels its list ("zone"), or [GrantScopeAccount] for a
// permission on the whole connection, which takes none.
type GrantScopeKind string

// GrantScopeAccount is a permission granted on the company's connection as a
// whole: a Cloudflare account permission, which names no zone.
const GrantScopeAccount GrantScopeKind = "account"

// GrantEntry is one permission a vendor's live catalogue offers a seat beside
// its level on a lane, as the company's connection can grant it now.
type GrantEntry struct {
	// ID is the vendor's own name for the permission, which a [Grant]
	// stores (a Cloudflare permission group's API name, "Origin Write").
	ID string `json:"id"`

	// Name is how a person finds it at the vendor (Cloudflare's token
	// builder: "Origin Rules: Edit"), the ID where Crewlet knows no other.
	Name string `json:"name"`

	// Group is the heading a picker lists it under ("Zone", "Account").
	Group string `json:"group,omitempty"`

	// ScopeKind is what its scope entries are: [Options.GrantScope]'s kind
	// ("zone"), or [GrantScopeAccount] for a permission that takes none.
	ScopeKind GrantScopeKind `json:"scope_kind"`

	// Write is a permission that may change anything at the vendor, judged
	// failing closed: only one the vendor shows read-only is not. Granting
	// it is a raise ([NeedsStepUp]).
	Write bool `json:"write,omitempty"`

	// InLevel is the lowest level that already holds it, so a seat on that
	// level or above gains nothing from a grant of it; empty for none.
	InLevel string `json:"in_level,omitempty"`

	// Unavailable is why the connection cannot grant it yet, empty where it
	// can; Missing is what to add, as a person adds it at the vendor, and
	// ActionURL where. A picker offers it disabled, a stored grant of it is
	// reported and left off the credential.
	Unavailable string   `json:"unavailable,omitempty"`
	Missing     []string `json:"missing,omitempty"`
	ActionURL   string   `json:"action_url,omitempty"`
}

// Grantable is the catalogue entry for a permission, or false where the
// lane's catalogue does not offer it.
func (o Options) Grantable(permission string) (GrantEntry, bool) {
	i := slices.IndexFunc(o.Grants, func(e GrantEntry) bool { return e.ID == permission })
	if i < 0 {
		return GrantEntry{}, false
	}
	return o.Grants[i], true
}

// GrantReaches reports a scope entry a grant may name: one of
// [Options.GrantScope], compared as the lane's vendor matches it.
func (o Options) GrantReaches(entry string) bool {
	key := ScopeKey(o.Lane, entry)
	return key != "" && slices.ContainsFunc(o.GrantScope, func(s string) bool { return ScopeKey(o.Lane, s) == key })
}

// CheckGrant is why one grant cannot be saved on lane as the catalogue offers
// it now, or nil: a permission the catalogue does not list, one it cannot
// grant yet, one the seat's level already holds, or a scope outside what a
// grant may name or one an account permission takes at all.
func CheckGrant(opts Options, level string, g Grant) error {
	entry, ok := opts.Grantable(g.Permission)
	switch {
	case !ok:
		return fmt.Errorf("%s offers no permission %q a seat can be granted", LaneName(opts.Lane), g.Permission)
	case entry.Unavailable != "":
		return fmt.Errorf("%s: %s", entry.Name, entry.Unavailable)
	case entry.InLevel != "" && heldByLevel(opts, entry.InLevel, level):
		return fmt.Errorf("%s is already in this agent's level", entry.Name)
	case entry.ScopeKind == GrantScopeAccount && len(g.Scope) > 0:
		return fmt.Errorf("%s is granted on the whole account and takes no scope", entry.Name)
	}
	for _, s := range g.Scope {
		if !opts.GrantReaches(s) {
			return fmt.Errorf("%s is not one the company's %s connection reaches", s, LaneName(opts.Lane))
		}
	}
	return nil
}

// heldByLevel reports whether a seat on level holds what inLevel holds: the
// offer lists levels least access first, each holding the ones before it.
func heldByLevel(opts Options, inLevel, level string) bool {
	_, need, ok := opts.Level(inLevel)
	_, have, held := opts.Level(level)
	return ok && held && have >= need
}

// CheckGrants is why after's grants cannot be saved over before's on lane:
// more than [MaxGrants] live, or a grant added or changed that [CheckGrant]
// refuses, or one that lapses already or later than [MaxGrantHours] from now.
// A grant kept as it was is not judged again, so a catalogue change never
// stops a seat's other edits; the sync reports it instead.
func CheckGrants(opts Options, before, after Desired, now time.Time) error {
	live := LiveGrants(after.Grants, now)
	if len(live) > MaxGrants {
		return fmt.Errorf("%d grants; a seat holds at most %d at one integration", len(live), MaxGrants)
	}
	for _, g := range after.Grants {
		if slices.ContainsFunc(before.Grants, func(h Grant) bool { return sameStored(h, g) }) {
			continue
		}
		if !g.ExpiresAt.IsZero() {
			switch {
			case !g.Live(now.Add(LapseLead)):
				return fmt.Errorf("%s lapses before it could be applied", g.Permission)
			case g.ExpiresAt.After(now.Add(MaxGrantHours * time.Hour)):
				return fmt.Errorf("%s lasts past %d hours; make it permanent or shorter", g.Permission, MaxGrantHours)
			}
		}
		if err := CheckGrant(opts, after.Level, g); err != nil {
			return err
		}
	}
	return nil
}

// sameStored reports two stored grants as one: same id, key and expiry.
func sameStored(a, b Grant) bool {
	return a.ID == b.ID && GrantKey(a) == GrantKey(b) && a.ExpiresAt.Equal(b.ExpiresAt)
}

// AddedGrants is every grant of after that before does not hold by id, and
// RemovedGrants every one of before that after does not: what a save changed,
// for its audit.
func AddedGrants(before, after []Grant) []Grant {
	return slices.DeleteFunc(slices.Clone(after), func(g Grant) bool {
		return slices.ContainsFunc(before, func(h Grant) bool { return h.ID == g.ID })
	})
}

// RemovedGrants is [AddedGrants] the other way round.
func RemovedGrants(before, after []Grant) []Grant { return AddedGrants(after, before) }

// RaisesGrant reports whether moving a seat's grants from before to after
// hands out a permission that changes things at the vendor: a grant of a
// write permission, or of one the catalogue cannot vouch for, that before did
// not hold with the same key for at least as long. Failing closed, as
// [NeedsStepUp] does for a level; removing or shortening a grant never raises.
func RaisesGrant(opts Options, before, after Desired) bool {
	if !after.Enabled {
		return false
	}
	for _, g := range NormalGrants(after.Grants) {
		if entry, ok := opts.Grantable(g.Permission); ok && !entry.Write {
			continue
		}
		if !before.Enabled || !slices.ContainsFunc(before.Grants, func(h Grant) bool {
			return GrantKey(h) == GrantKey(g) && !outlasts(g, h)
		}) {
			return true
		}
	}
	return false
}

// NewGrantID is a fresh grant id: g and ten lowercase letters or digits.
func NewGrantID() string { return "g" + randomID() }

// NewRequestID is a fresh request id: ten lowercase letters or digits, which
// a request's option ids carry ([GrantOptionTemporary]).
func NewRequestID() string { return randomID() }

// randomID is ten symbols of crypto/rand's base32 text, lowercased: 50 random
// bits, unique among every grant and request a company will ever make.
func randomID() string { return strings.ToLower(rand.Text()[:10]) }

// StampGrants is after with every grant it adds stamped by the engine: a fresh
// id, who granted it and when. A grant after names by id must be the one
// before holds, field for field: a grant is added or removed, never edited,
// so who granted what stays what the engine recorded.
func StampGrants(before, after Desired, by string, now time.Time) (Desired, error) {
	out := after
	out.Grants = make([]Grant, 0, len(after.Grants))
	for _, g := range after.Grants {
		if g.ID == "" {
			g.ID, g.GrantedBy, g.GrantedAt = NewGrantID(), by, now.UTC()
			out.Grants = append(out.Grants, g)
			continue
		}
		i := slices.IndexFunc(before.Grants, func(h Grant) bool { return h.ID == g.ID })
		if i < 0 || !sameGrant(before.Grants[i], g) {
			return Desired{}, fmt.Errorf("grant %s is not one this agent holds as it was saved; "+
				"a grant is added or removed, never edited", g.ID)
		}
		out.Grants = append(out.Grants, before.Grants[i])
	}
	if len(out.Grants) == 0 {
		out.Grants = nil
	}
	return out, nil
}

// sameGrant reports two stored grants equal in every field.
func sameGrant(a, b Grant) bool {
	return a.ID == b.ID && a.Permission == b.Permission && slices.Equal(normalSet(a.Scope), normalSet(b.Scope)) &&
		a.ExpiresAt.Equal(b.ExpiresAt) && a.GrantedBy == b.GrantedBy && a.GrantedAt.Equal(b.GrantedAt) &&
		a.Reason == b.Reason && a.Request == b.Request
}

// SameStoredGrants reports two lists holding the same stored grants, field for
// field, whatever their order: what tells a save that changed a grant's
// expiry or author from one that changed nothing.
func SameStoredGrants(a, b []Grant) bool {
	a, b = NormalGrants(a), NormalGrants(b)
	return slices.EqualFunc(a, b, sameGrant)
}

// GrantChange is what one save did to one seat's grants on a lane.
type GrantChange struct {
	Handle         string
	Lane           Lane
	Added, Removed []Grant
}

// GrantLanes is every lane a registered vendor takes grants on
// ([LaneSpec.Grants]), in reading order: what an agent may ask a grant on.
func GrantLanes() []Lane {
	var out []Lane
	for _, lane := range Lanes() {
		v, ok := Lookup(lane.Kind())
		if !ok {
			continue
		}
		if spec, ok := v.Catalog().Lane(lane); ok && spec.Grants {
			out = append(out, lane)
		}
	}
	return out
}
