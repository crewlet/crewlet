package googlegrowth

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/vendortext"
	"github.com/crewlet/crewlet/internal/words"
)

// Identity is a product's half of a seat's own identity there (package doc):
// what each level needs, the steps a person takes to add the identity, the
// reads that check it and, where the product has an API for it, adding it
// with the company's admin sign-in.
type Identity struct {
	// Records is what the product's own history then names the seat for;
	// "" where it keeps none (Search Console).
	Records string

	// Entry and Entries name one scope entry and several ("app", "apps").
	Entry, Entries string

	// Roles is the product's roles as the check compares them, and Needs
	// what each level needs of them.
	Roles Roles
	Needs func(level growth.Level) Need
	// RoleNeeds is what a level run at one of the product's roles needs
	// (Analytics' operate as Marketer), by that role; nil where it offers none.
	RoleNeeds func(role string) Need

	// Steps are a person's steps at the product in its exact labels, the
	// page linked on the first that names it, and Note what it allows that
	// they should know.
	Steps func(g Guided) []seataccess.GuideStep
	Note  vendortext.Text

	// Scopes are what the check mints the seat's token with.
	Scopes []string

	// Reach reads, as the seat's identity, each entry the seat names: one
	// [Reached] per entry, in the seat's order. A product that shows the
	// caller its own role (Search Console, Ads) reads it here too.
	Reach func(ctx context.Context, in Probe) ([]Reached, error)

	// Admin adds and reads the identity with the company's admin sign-in;
	// nil where the product has no API for it (Ads, Search Console).
	Admin *Admin

	// AddURL is the product's page that adds a user on an entry, its
	// holder known or not; nil where the product has none per entry.
	AddURL func(e Listed) string

	// Wider is adding the identity once to what holds the entries instead;
	// nil where the product has no such place.
	Wider *Wider
}

// Wider is a product's one add on what holds a seat's entries (an Analytics
// account), which reaches every entry there, now and later.
type Wider struct {
	Label string
	Does  vendortext.Text
	// URL is the page that adds a user on what holds e, "" where e's holder
	// is not known.
	URL func(e Listed) string
}

// Admin is a product's calls with the company's admin sign-in (a consent
// grant, ADR-0178), which signs no seat's call: it only adds, reads and
// removes each seat's identity at the product.
type Admin struct {
	// Who is the person who signs in, as the product names them ("a Play
	// Console admin").
	Who string

	// Roles reads the roles in.Email holds on each entry, by entry; an
	// entry it holds none on is absent.
	Roles func(ctx context.Context, in AdminCall) (map[string][]string, error)

	// Add gives in.Email in.Need on each entry, keeping any role it holds
	// beyond it.
	Add func(ctx context.Context, in AdminCall) error

	// Remove takes in.Email off each entry.
	Remove func(ctx context.Context, in AdminCall) error

	// Unavailable is why the admin cannot act for c (a Play block naming
	// no developer account), "" where it can.
	Unavailable func(c *config.Company) string
}

// Probe is one check's reads as the seat's identity.
type Probe struct {
	Company *config.Company
	// Email is the identity's, Scope the entries the seat names.
	Email string
	Scope []string
	// Token is the identity's, minted with [Identity.Scopes].
	Token string
	Send  *googleauth.Transport
}

// AdminCall is one call with the company's admin sign-in.
type AdminCall struct {
	Company *config.Company
	Email   string
	Scope   []string
	Need    Need
	// Reached is what the identity's own reads found of each entry, which
	// a product's admin reads name the entry's parent from (an Analytics
	// property's account).
	Reached []Reached
	Token   string
	Send    *googleauth.Transport
}

// Reached is one scope entry as the identity's reads found it.
type Reached struct {
	Entry string
	// Name is the product's name for it, "" where it shows none, Parent
	// what holds it (an Analytics property's account) and ParentName that
	// holder's name.
	Name, Parent, ParentName string
	// OK is whether the identity reaches it, and Said why not in Google's
	// words.
	OK   bool
	Said string
	// Roles are the role IDs the product showed the identity holds there,
	// and Read whether it showed them. AtLeast marks Roles as a floor a read
	// proves (any listed Analytics property is held at Viewer or above)
	// rather than the role itself.
	Roles   []string
	Read    bool
	AtLeast bool
	// Added is an entry the admin shows the identity added to that it does
	// not reach yet: the product is still applying it.
	Added bool
}

// label is the entry as a person reads it.
func (r Reached) label() string {
	if r.Name != "" && r.Name != r.Entry {
		return r.Name + " (" + r.Entry + ")"
	}
	return r.Entry
}

// Roles is a product's roles as a check compares them: a ladder, where each
// role holds every one below it (Analytics, Search Console, Ads), or
// permissions, each its own (a Play app's).
type Roles struct {
	// Ladder is the roles least first; nil for permissions.
	Ladder []RoleName
	// Permissions are the permissions; nil for a ladder. Every is the one
	// that holds all of them (a Play app's Admin), "" where none does.
	Permissions []RoleName
	Every       string
}

// RoleName is one role's ID and its name in the product.
type RoleName struct{ ID, Name string }

// Need is what one level needs at a product.
type Need struct {
	// Roles are the role IDs the level needs: on a ladder the least that
	// does, of permissions every one.
	Roles []string
	// More is what some of the level's acts need beyond it, as a person
	// reads it; nil where none do.
	More vendortext.Text
	// Broad are roles more than the level needs, which the check warns of.
	Broad []string
	// Manual is why only a person can give these roles (an API that cannot
	// grant them), "" where the admin sign-in can.
	Manual string
}

// all is every role of r, ladder or permissions.
func (r Roles) all() []RoleName { return append(slices.Clone(r.Ladder), r.Permissions...) }

// Name is role id's name in the product, the ID where it names none.
func (r Roles) Name(id string) string {
	for _, role := range r.all() {
		if role.ID == id {
			return role.Name
		}
	}
	return id
}

// Words is roles as a person reads them, in the product's order; "" for none.
func (r Roles) Words(ids []string) string { return and(r.Names(ids)) }

// Names are role ids' names in the product, in its order, then any it does
// not name.
func (r Roles) Names(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	var names []string
	for _, role := range r.all() {
		if slices.Contains(ids, role.ID) {
			names = append(names, role.Name)
		}
	}
	for _, id := range ids {
		if !slices.ContainsFunc(r.all(), func(role RoleName) bool { return role.ID == id }) {
			names = append(names, id)
		}
	}
	return names
}

// rank is id's place on the ladder, -1 off it.
func (r Roles) rank(id string) int {
	return slices.IndexFunc(r.Ladder, func(role RoleName) bool { return role.ID == id })
}

// Missing is what held lacks of need: on a ladder, need's role where held
// reaches none at or above it; of permissions, each one held lacks.
func (r Roles) Missing(held []string, need Need) []string {
	if r.Ladder != nil {
		want := -1
		for _, id := range need.Roles {
			want = max(want, r.rank(id))
		}
		for _, id := range held {
			if r.rank(id) >= want {
				return nil
			}
		}
		return slices.Clone(need.Roles)
	}
	if r.Every != "" && slices.Contains(held, r.Every) {
		return nil
	}
	var out []string
	for _, id := range need.Roles {
		if !slices.Contains(held, id) {
			out = append(out, id)
		}
	}
	return out
}

// broader is the roles of held need warns of.
func broader(held []string, need Need) []string {
	var out []string
	for _, id := range held {
		if slices.Contains(need.Broad, id) {
			out = append(out, id)
		}
	}
	return out
}

// Guided is what a product's steps are drawn from.
type Guided struct {
	Agent string
	// Email is the identity's, to copy, and Entries the seat's entries as
	// the steps name them.
	Email   string
	Entries []string
	// Level is the seat's, Need what it needs, and NeedNames and NeedWords
	// its roles' names in the product.
	Level     growth.Level
	Need      Need
	NeedNames []string
	NeedWords string
	Company   *config.Company
}

// Copy is the step that gives the identity's email, ready to copy.
func (g Guided) Copy(field string) seataccess.GuideStep {
	return seataccess.GuideStep{Text: vendortext.Of(vendortext.UI(field), vendortext.W(":")), Copy: &seataccess.GuideCopy{Label: "identity's email",
		Value: g.Email, Rule: "Crewlet made this identity for " + g.Agent + "; add it exactly as written."}}
}

// The check's reads, as the sheet lists them.
const (
	readAdd   = "add"
	readToken = "token"
	readReach = "reach"
	readRole  = "role"
)

// levelOf is d's level, read where it names none, without the product role
// it may run at (seataccess.GrowthLevelID).
func levelOf(d seataccess.Desired) growth.Level {
	level, _ := seataccess.CutGrowthLevel(d.Level)
	if l := growth.Level(level); l.Valid() {
		return l
	}
	return growth.LevelRead
}

// need is what d's level needs at the product, at the product role it runs
// at where it names one.
func (id *Identity) need(d seataccess.Desired) Need {
	if _, role := seataccess.CutGrowthLevel(d.Level); role != "" && id.RoleNeeds != nil {
		return id.RoleNeeds(role)
	}
	return id.Needs(levelOf(d))
}

// guide is the sheet for seat on product v's lane.
func (v SeatAccess) guide(c *config.Company, seat seataccess.SeatRef, want seataccess.Desired,
	email string) seataccess.IdentityGuide {
	id := v.Identity()
	level := levelOf(want)
	need := id.need(want)
	needs := id.Roles.Words(need.Roles)
	g := Guided{Agent: seat.Label(), Email: email, Entries: want.Scope, Level: level, Need: need,
		NeedNames: id.Roles.Names(need.Roles), NeedWords: needs, Company: c}
	why := v.Name + " lets " + seat.Label() + " in as its own Google identity"
	if id.Records != "" {
		why += " and records " + id.Records
	}
	out := seataccess.IdentityGuide{
		Vendor: v.Name, Noun: "Google identity", Records: id.Records,
		Why: why + ".", Name: email, NameRule: "Crewlet made it; it cannot be renamed.",
		NameWhat: "Google identity", Role: needs, Steps: id.Steps(g), Note: slices.Clone(id.Note),
		Back: seataccess.GuideBack{Kind: seataccess.BackNothing,
			Text: vendortext.Plain("Nothing to bring back: Crewlet checks " + v.Name + " as the identity.")},
		Checks: v.checks(email, false),
	}
	for _, entry := range want.Scope {
		out.Entries = append(out.Entries, v.entry(Listed{ID: entry}))
	}
	if id.Wider != nil {
		out.Wider = &seataccess.GuideWider{Label: id.Wider.Label, Does: slices.Clone(id.Wider.Does)}
	}
	if len(need.More) > 0 {
		if len(out.Note) > 0 {
			out.Note = append(out.Note, vendortext.W(" "))
		}
		out.Note = append(out.Note, need.More...)
	}
	if id.Admin != nil {
		out.Make = &seataccess.GuideMake{Label: "Add it for me",
			Does: vendortext.Of(append(append([]vendortext.Part{vendortext.W("With " + id.Admin.Who +
				"'s sign-in, Crewlet adds " + email + " to " + and(want.Scope) + " with ")}, vendortext.And(g.NeedNames...)...),
				vendortext.W("."))...),
			Unavailable: cmp.Or(need.Manual, id.Admin.Unavailable(c))}
	}
	return out
}

// checks are the check's reads as the sheet lists them; made adds the add.
func (v SeatAccess) checks(email string, made bool) []seataccess.GuideCheck {
	id := v.Identity()
	var out []seataccess.GuideCheck
	if made {
		out = append(out, seataccess.GuideCheck{ID: readAdd, Label: "Add it to the agent's " + id.Entries})
	}
	return append(out,
		seataccess.GuideCheck{ID: readToken, Label: "Sign in as " + email},
		seataccess.GuideCheck{ID: readReach, Label: "Find each of the agent's " + id.Entries},
		seataccess.GuideCheck{ID: readRole, Label: "Read its role on each " + id.Entry})
}

// ErrNoAdmin is an admin act with no admin sign-in to make it with.
var ErrNoAdmin = errors.New("googlegrowth: nobody has signed in as the product's admin")

// checkInput is one check of a seat's identity at the product.
type checkInput struct {
	company *config.Company
	seat    seataccess.SeatRef
	want    seataccess.Desired
	tokens  seataccess.IssuedTokens
	send    *googleauth.Transport
	now     time.Time
}

// IdentitySettles is how long after Crewlet makes a seat's identity a person
// can add it: Google's new accounts settle in up to about two minutes
// (googlecloud's await.go), and Analytics refuses an add before then.
const IdentitySettles = 2 * time.Minute

// check reads the seat's identity at the product and judges it against its
// level: alive, every entry reached, the role where anybody can read it. One
// too new to find is getting ready; a fault is an error and judges nothing.
func (v SeatAccess) check(ctx context.Context, in checkInput) (seataccess.IdentityCheck, error) {
	id := v.Identity()
	need := id.need(in.want)
	out := seataccess.IdentityCheck{Needs: id.Roles.Words(need.Roles), At: in.now}
	agent := in.seat.Label()
	seat, err := in.tokens.SeatToken(ctx, v.Product, in.seat.Handle, id.Scopes)
	email := seat.Account
	out.ID, out.Name = email, email
	switch {
	case errors.Is(err, seataccess.ErrNotIssued):
		out.Status = seataccess.IdentityPreparing
		out.ReadyAt = readyAt(seat.Made, in.now)
		out.Said = "Crewlet is still making " + agent + "'s Google identity; " + v.Name +
			" can find it in about a minute."
		out.Reads = []seataccess.IdentityRead{{Label: "Sign in as the identity", Said: "not made yet"}}
		out.Entries = v.entries(in.want.Scope, nil, v.namer(ctx, in, nil), need)
		return out, nil
	case googleauth.Status(err) == http.StatusForbidden || googleauth.Status(err) == http.StatusNotFound:
		out.Status = seataccess.IdentityRefused
		out.Said = "Google refused to sign in as " + firstNonEmpty(email, agent+"'s Google identity") +
			": it is disabled or deleted in the identity project."
		out.Remedy = "Turn it back on in the identity project's service accounts; Crewlet checks again on its " +
			"next pass."
		out.Reads = []seataccess.IdentityRead{{Label: "Sign in as " + email, Said: googleauth.Sentence(err)}}
		return out, nil
	case err != nil:
		return seataccess.IdentityCheck{}, fmt.Errorf("googlegrowth: sign in as %s's identity: %w", in.seat.Handle, err)
	}
	out.Reads = append(out.Reads, seataccess.IdentityRead{Label: "Sign in as " + email, OK: true})
	reached, err := id.Reach(ctx, Probe{Company: in.company, Email: email, Scope: in.want.Scope, Token: seat.Token,
		Send: in.send})
	if err != nil {
		return seataccess.IdentityCheck{}, fmt.Errorf("googlegrowth: read what %s reaches: %w", email, err)
	}
	// AN ENTRY NOT REACHED MAY BE APPLYING, which only the admin's read shows.
	if id.Admin != nil && slices.ContainsFunc(reached, func(r Reached) bool {
		return !r.OK || unread(id.Roles, r, need)
	}) {
		if reached, err = v.adminRoles(ctx, in, email, need, reached); err != nil {
			return seataccess.IdentityCheck{}, err
		}
	}
	out = v.verdict(out, agent, email, need, reached)
	out.Entries = v.entries(in.want.Scope, reached, v.namer(ctx, in, reached), need)
	if r := readyAt(seat.Made, in.now); out.Status == seataccess.IdentityNone && !r.IsZero() &&
		!slices.ContainsFunc(reached, func(r Reached) bool { return r.OK || r.Added }) {
		// TOO NEW TO FIND: an add now fails at the product, so it is not the person's yet.
		out.Status, out.ReadyAt, out.Remedy = seataccess.IdentityPreparing, r, ""
		out.Said = "Crewlet made " + agent + "'s Google identity a moment ago; " + v.Name +
			" can find it in about a minute, then add it there."
	}
	return out, nil
}

// readyAt is when an identity made at made can be added at a product, zero
// where that is already so or its making is unknown.
func readyAt(made, now time.Time) time.Time {
	if made.IsZero() || !now.Before(made.Add(IdentitySettles)) {
		return time.Time{}
	}
	return made.Add(IdentitySettles).UTC()
}

// unread reports an entry reached whose role the reads have not shown to be
// what need asks: none read, or only a floor short of it.
func unread(roles Roles, r Reached, need Need) bool {
	return r.OK && (!r.Read || (r.AtLeast && len(roles.Missing(r.Roles, need)) > 0))
}

// namer is the company reader's listing, read only where the identity's own
// reads left one of the seat's entries unnamed or unplaced; nil otherwise.
func (v SeatAccess) namer(ctx context.Context, in checkInput, reached []Reached) Listing {
	named := map[string]bool{}
	for _, r := range reached {
		named[r.Entry] = r.Name != "" && (r.Parent != "" || v.Identity().Wider == nil)
	}
	if !slices.ContainsFunc(in.want.Scope, func(e string) bool { return !named[e] }) {
		return nil
	}
	listed, _ := v.readerListing(ctx, in.company, in.tokens, in.send)
	return listingOf(listed)
}

// entries are the seat's entries as the check found its identity on each,
// named from its reads or else the reader's, with where to add it.
func (v SeatAccess) entries(scope []string, reached []Reached, names Listing, need Need) []seataccess.IdentityEntry {
	id := v.Identity()
	out := make([]seataccess.IdentityEntry, 0, len(scope))
	for _, entry := range scope {
		r := Reached{Entry: entry}
		if i := slices.IndexFunc(reached, func(r Reached) bool { return r.Entry == entry }); i >= 0 {
			r = reached[i]
		}
		l := names[entry]
		l.ID = entry
		l.Name, l.Parent = firstNonEmpty(r.Name, l.Name), firstNonEmpty(r.Parent, l.Parent)
		l.ParentName = firstNonEmpty(r.ParentName, l.ParentName)
		e := v.entry(l)
		switch {
		case !r.OK && r.Added:
			e.State = seataccess.EntryApplying
		case !r.OK:
			e.State = seataccess.EntryMissing
		case unread(id.Roles, r, need) || !knownRoles(id.Roles, r.Roles):
			e.State = seataccess.EntryUnchecked
		case len(id.Roles.Missing(r.Roles, need)) > 0:
			e.State, e.Role = seataccess.EntryWrongRole, id.Roles.Words(r.Roles)
		default:
			e.State = seataccess.EntryAdded
			if !r.AtLeast {
				e.Role = id.Roles.Words(r.Roles)
			}
		}
		out = append(out, e)
	}
	return out
}

// entry is l as the sheet lists it, with the product's pages that add a user
// on it and on what holds it.
func (v SeatAccess) entry(l Listed) seataccess.IdentityEntry {
	id := v.Identity()
	e := seataccess.IdentityEntry{ID: l.ID, Name: l.Name, Group: l.ParentName}
	if id.AddURL != nil {
		e.AddURL = id.AddURL(l)
	}
	if id.Wider != nil && id.Wider.URL != nil {
		e.GroupURL = id.Wider.URL(l)
	}
	return e
}

// knownRoles reports roles the product's catalogue names: one Crewlet does
// not know is unread, never judged short.
func knownRoles(roles Roles, held []string) bool {
	for _, h := range held {
		if !slices.ContainsFunc(roles.all(), func(r RoleName) bool { return r.ID == h }) {
			return false
		}
	}
	return true
}

// adminRoles fills each reached entry's roles from the admin's read and marks
// one not reached that the admin shows added; an entry the admin shows no
// role on keeps its own reads (it may reach it through a user group).
func (v SeatAccess) adminRoles(ctx context.Context, in checkInput, email string, need Need,
	reached []Reached) ([]Reached, error) {
	token, err := in.tokens.AdminToken(ctx, v.Product)
	switch {
	case errors.Is(err, seataccess.ErrNotIssued):
		return reached, nil
	case err != nil:
		return nil, fmt.Errorf("googlegrowth: %s's admin sign-in: %w", v.Name, err)
	}
	roles, err := v.Identity().Admin.Roles(ctx, AdminCall{Company: in.company, Email: email, Scope: in.want.Scope,
		Need: need, Reached: reached, Token: token, Send: in.send})
	if s := googleauth.Status(err); s == http.StatusUnauthorized || s == http.StatusForbidden ||
		s == http.StatusNotFound {
		// THE ADMIN MAY SEE LESS THAN THE AGENT'S ENTRIES: their roles go unread.
		return reached, nil
	}
	if err != nil {
		return nil, fmt.Errorf("googlegrowth: read %s's roles at %s: %w", email, v.Name, err)
	}
	for i := range reached {
		held := roles[reached[i].Entry]
		switch {
		case len(held) == 0:
		case reached[i].OK:
			reached[i].Roles, reached[i].Read, reached[i].AtLeast = held, true, false
		default:
			reached[i].Added = true
		}
	}
	return reached, nil
}

// verdict judges what the reads found: every entry reached, at the role the
// level needs; a role nobody could read leaves it working, never ready.
func (v SeatAccess) verdict(out seataccess.IdentityCheck, agent, email string, need Need,
	reached []Reached) seataccess.IdentityCheck {
	id := v.Identity()
	var missing, applying, wrong, unchecked, broad []string
	var held, short []string
	reach := seataccess.IdentityRead{Label: "Find each of the agent's " + id.Entries, OK: true}
	role := seataccess.IdentityRead{Label: "Read its role on each " + id.Entry, OK: true}
	for _, r := range reached {
		switch {
		case !r.OK && r.Added:
			applying = append(applying, r.label())
			reach.OK, reach.Said = false, firstNonEmpty(reach.Said, r.Said)
		case !r.OK:
			missing = append(missing, r.label())
			reach.OK, reach.Said = false, firstNonEmpty(reach.Said, r.Said)
		case unread(id.Roles, r, need) || !knownRoles(id.Roles, r.Roles):
			unchecked = append(unchecked, r.label())
		case len(id.Roles.Missing(r.Roles, need)) > 0:
			wrong = append(wrong, firstNonEmpty(id.Roles.Words(r.Roles), "no role")+" on "+r.label())
			short = append(short, r.Roles...)
			role.OK = false
		default:
			if !r.AtLeast {
				held = append(held, r.Roles...)
				broad = append(broad, broader(r.Roles, need)...)
			}
		}
	}
	slices.Sort(held)
	held = slices.Compact(held)
	out.Reads = append(out.Reads, reach)
	if len(missing)+len(applying) > 0 {
		out.Status = seataccess.IdentityNone
		var said []string
		if len(missing) > 0 {
			said = append(said, email+" is not added to "+and(missing)+" in "+v.Name+" yet.")
			out.Remedy = "Add it with " + out.Needs + "; Crewlet checks again on its next pass."
		}
		if len(applying) > 0 {
			said = append(said, v.Name+" shows it added to "+and(applying)+" but does not let it in yet, "+
				"while it applies the change; Crewlet checks again on its next pass.")
		}
		out.Said = strings.Join(said, " ")
		return out
	}
	switch {
	case len(wrong) > 0:
		role.Said = and(wrong)
	case len(unchecked) > 0:
		role.OK, role.Said = false, v.Name+" shows the role only to "+adminWho(id)
	}
	out.Reads = append(out.Reads, role)
	if len(wrong) > 0 {
		slices.Sort(short)
		if short = slices.Compact(short); len(short) > 0 {
			out.Role = id.Roles.Words(short)
		}
		out.Status = seataccess.IdentityWrongRole
		out.Said = email + " holds " + and(wrong) + "; " + agent + "'s level needs " + out.Needs + "."
		out.Remedy = "Give it " + out.Needs + " in " + v.Name + "; Crewlet checks again on its next pass."
		return out
	}
	if len(held) > 0 {
		out.Role = id.Roles.Words(held)
	}
	if len(unchecked) > 0 {
		out.Status = seataccess.IdentityUnchecked
		out.Said = agent + " reaches every one of its " + id.Entries + ", but its role on " + and(unchecked) +
			" is not checked: " + v.Name + " shows it only to " + adminWho(id) + "."
		out.Remedy = "Make sure it holds " + out.Needs + " there in " + v.Name + "."
		if id.Admin != nil {
			out.Remedy += " With " + id.Admin.Who + "'s sign-in on the Settings tab, Crewlet checks it on every pass."
		}
		return out
	}
	out.Status = seataccess.IdentityReady
	out.Said = agent + " reaches every one of its " + id.Entries + " as its own Google identity"
	if out.Role != "" {
		out.Said += ", at " + out.Role
	}
	out.Said += "."
	if len(broad) > 0 {
		slices.Sort(broad)
		out.Broader = true
		out.Said += " It holds " + id.Roles.Words(slices.Compact(broad)) + ", more than its level needs; " +
			"Crewlet still holds it to its level."
	}
	return out
}

// adminWho is who the product shows a user's role to.
func adminWho(id *Identity) string {
	if id.Admin != nil {
		return id.Admin.Who
	}
	return "an admin"
}

// and is phrases already worded, said as one list.
func and(phrases []string) string {
	said := make([]words.Said, len(phrases))
	for i, p := range phrases {
		said[i] = words.Said(p)
	}
	return words.And(said)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
