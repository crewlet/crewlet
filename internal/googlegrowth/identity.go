package googlegrowth

import (
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

	// Link is the product's page a person adds the identity on, Steps their
	// steps there in its exact labels, and Note what it allows that they
	// should know.
	Link  seataccess.GuideLink
	Steps func(g Guided) []seataccess.GuideStep
	Note  string

	// Scopes are what the check mints the seat's token with.
	Scopes []string

	// Reach reads, as the seat's identity, each entry the seat names: one
	// [Reached] per entry, in the seat's order. A product that shows the
	// caller its own role (Search Console, Ads) reads it here too.
	Reach func(ctx context.Context, in Probe) ([]Reached, error)

	// Admin adds and reads the identity with the company's admin sign-in;
	// nil where the product has no API for it (Ads, Search Console).
	Admin *Admin
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
	// Name is the product's name for it, "" where it shows none, and
	// Parent what holds it (an Analytics property's account).
	Name, Parent string
	// OK is whether the identity reaches it, and Said why not in Google's
	// words.
	OK   bool
	Said string
	// Roles are the role IDs the product showed the identity holds there,
	// and Read whether it showed them.
	Roles []string
	Read  bool
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
	// reads it; "" where none do.
	More string
	// Broad are roles more than the level needs, which the check warns of.
	Broad []string
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
func (r Roles) Words(ids []string) string {
	if len(ids) == 0 {
		return ""
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
	return and(names)
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
	// Level is the seat's, Need what it needs and NeedWords that in the
	// product's words.
	Level     growth.Level
	Need      Need
	NeedWords string
	Company   *config.Company
}

// Copy is the step that gives the identity's email, ready to copy.
func (g Guided) Copy(text string) seataccess.GuideStep {
	return seataccess.GuideStep{Text: text, Copy: &seataccess.GuideCopy{Label: "identity's email",
		Value: g.Email, Rule: "Crewlet made this identity for " + g.Agent + "; add it exactly as written."}}
}

// The check's reads, as the sheet lists them.
const (
	readAdd   = "add"
	readToken = "token"
	readReach = "reach"
	readRole  = "role"
)

// levelOf is d's level, read where it names none.
func levelOf(d seataccess.Desired) growth.Level {
	if l := growth.Level(d.Level); l.Valid() {
		return l
	}
	return growth.LevelRead
}

// guide is the sheet for seat on product v's lane.
func (v SeatAccess) guide(c *config.Company, seat seataccess.SeatRef, want seataccess.Desired,
	email string) seataccess.IdentityGuide {
	id := v.Identity()
	level := levelOf(want)
	need := id.Needs(level)
	needs := id.Roles.Words(need.Roles)
	g := Guided{Agent: seat.Label(), Email: email, Entries: want.Scope, Level: level, Need: need,
		NeedWords: needs, Company: c}
	why := v.Name + " lets " + seat.Label() + " in as its own Google identity"
	if id.Records != "" {
		why += " and records " + id.Records
	}
	out := seataccess.IdentityGuide{
		Vendor: v.Name, Noun: "Google identity", Records: id.Records,
		Why: why + ".", Name: email, NameRule: "Crewlet made it; it cannot be renamed.",
		NameWhat: "Google identity", Role: needs, Steps: id.Steps(g), Link: id.Link, Note: id.Note,
		Back: seataccess.GuideBack{Kind: seataccess.BackNothing,
			Text: "Nothing to bring back: Crewlet checks " + v.Name + " as the identity."},
		Checks: v.checks(email, false),
	}
	if need.More != "" {
		out.Note = strings.TrimSpace(out.Note + " " + need.More)
	}
	if id.Admin != nil {
		out.Make = &seataccess.GuideMake{Label: "Add it for me",
			Does: "With " + id.Admin.Who + "'s sign-in, Crewlet adds " + email + " to " +
				and(want.Scope) + " with " + needs + ".",
			Unavailable: id.Admin.Unavailable(c)}
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

// check reads the seat's identity at the product and judges it against its
// level: alive (a token mints), reach (every entry answers), role (where the
// product shows it, as the identity or to the admin). A fault (Google or the
// engine unreachable) is an error and judges nothing.
func (v SeatAccess) check(ctx context.Context, in checkInput) (seataccess.IdentityCheck, error) {
	id := v.Identity()
	level := levelOf(in.want)
	need := id.Needs(level)
	out := seataccess.IdentityCheck{Needs: id.Roles.Words(need.Roles), At: in.now}
	agent := in.seat.Label()
	token, email, err := in.tokens.SeatToken(ctx, v.Product, in.seat.Handle, id.Scopes)
	out.ID, out.Name = email, email
	switch {
	case errors.Is(err, seataccess.ErrNotIssued):
		out.Status = seataccess.IdentityNone
		out.Said = "Crewlet is still making " + agent + "'s Google identity; the next pass checks it."
		out.Reads = []seataccess.IdentityRead{{Label: "Sign in as the identity", Said: "not made yet"}}
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
	reached, err := id.Reach(ctx, Probe{Company: in.company, Email: email, Scope: in.want.Scope, Token: token,
		Send: in.send})
	if err != nil {
		return seataccess.IdentityCheck{}, fmt.Errorf("googlegrowth: read what %s reaches: %w", email, err)
	}
	if id.Admin != nil && slices.ContainsFunc(reached, func(r Reached) bool { return !r.Read }) {
		if reached, err = v.adminRoles(ctx, in, email, need, reached); err != nil {
			return seataccess.IdentityCheck{}, err
		}
	}
	return v.verdict(out, agent, email, need, reached), nil
}

// adminRoles fills each reached entry's roles from the admin's read, and
// marks an entry not reached that the admin shows it added to, where somebody
// signed in as the product's admin; reached as it was where nobody did. An
// entry the admin shows no role on is left unread: it may reach it through
// what the admin's read does not show (a user group).
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
			reached[i].Roles, reached[i].Read = held, true
		default:
			reached[i].Added = true
		}
	}
	return reached, nil
}

// verdict judges what the reads found: every entry reached, at the role the
// level needs where the product showed it.
func (v SeatAccess) verdict(out seataccess.IdentityCheck, agent, email string, need Need,
	reached []Reached) seataccess.IdentityCheck {
	id := v.Identity()
	var missing, applying, wrong, unread, broad []string
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
		case !r.Read:
			unread = append(unread, r.label())
		case len(id.Roles.Missing(r.Roles, need)) > 0:
			wrong = append(wrong, firstNonEmpty(id.Roles.Words(r.Roles), "no role")+" on "+r.label())
			short = append(short, r.Roles...)
			role.OK = false
		default:
			held = append(held, r.Roles...)
			broad = append(broad, broader(r.Roles, need)...)
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
	case len(unread) > 0:
		role.OK, role.Said = true, v.Name+" shows the role only to "+adminWho(id)
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
	out.Status = seataccess.IdentityReady
	out.Said = agent + " reaches every one of its " + id.Entries + " as its own Google identity."
	if len(unread) > 0 {
		out.Said += " " + v.Name + " shows its role only to " + adminWho(id) + ", so a call it refuses names " +
			"the role it lacks."
	}
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
