package seataccess

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// What a lane view says of a seat's grants beside its level (ADR-0150), its
// agent's open requests for one (ADR-0118) and the lane's catalogue, worded by
// the engine from the access answer and the stored requests, so a reader of
// one agent's access (the assistant's get_seat_access) can say what the agent
// holds, what it asked for and what a person must add at the vendor without
// reading the whole Agents tab.
//
// THE CATALOGUE IS NEVER WORDED WHOLE. A vendor's catalogue runs to hundreds
// of permissions (Cloudflare's permission groups, AWS's actions), past what one
// read hands a model, so the view counts it and words only the entries the
// seat's grants and requests name and those a lookup matches, at most
// [MaxLookup] of them, as a list of their own.

// MaxLookup is the most catalogue entries a lookup answers: a screen of one
// permission's variants (its read and its edit, at the account and at a zone)
// with room to spare, about a fifth of what a model reads of one card, so a
// lookup too broad to answer says that more matched and asks for a longer
// part of the name rather than listing half a catalogue.
const MaxLookup = 12

// MaxLookupChars bounds a lookup ([ViewQuery.Lookup]) in characters: the
// longest name a catalogue gives a permission (Cloudflare's run to about sixty
// characters) with room to spare, so a lookup is part of one name and never a
// paragraph. The seat read refuses a longer one.
const MaxLookupChars = 100

// viewTime is how a lane view writes an instant: to the minute, in UTC, said
// as such, since the sentence is read by people in any zone.
const viewTime = "2006-01-02 15:04 UTC"

// wordGrants fills out's grant sentences from the lane's answer, its offer and
// q's requests and lookup.
func wordGrants(out *LaneView, offer Options, la LaneAnswer, q ViewQuery) {
	now := q.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	want := la.Desired
	var grants []string
	for _, g := range NormalGrants(want.Grants) {
		grants = append(grants, grantWords(offer, g, want.Enabled, la.Observed, now))
	}
	out.Grants = strings.Join(grants, "; ")

	named := []string{}
	for _, g := range NormalGrants(want.Grants) {
		named = append(named, g.Permission)
	}
	var requests []string
	for _, r := range seatRequests(q.Requests, out.Handle, out.Lane) {
		requests = append(requests, requestWords(offer, r))
		named = append(named, r.Permission)
	}
	out.Requests = strings.Join(requests, "; ")

	lookup := strings.TrimSpace(q.Lookup)
	if offer.Grants == nil {
		if lookup != "" {
			out.LookupNote = out.Integration + "'s catalogue of single permissions could not be read just now"
			if GrantTermsOf(out.Lane) == (GrantTerms{}) {
				out.LookupNote = out.Integration + " grants no single permissions beside a level"
			}
		}
		return
	}
	out.CatalogueReadAt = offer.ReadAt
	beside, lacking := 0, 0
	for _, e := range offer.Grants {
		if e.InLevel != "" && heldByLevel(offer, e.InLevel, want.Level) {
			continue
		}
		beside++
		if e.Unavailable != "" {
			lacking++
		}
	}
	out.Grantable = grantableWords(out.Integration, beside, lacking)

	var lines []string
	seen := map[string]bool{}
	for _, id := range named {
		if e, ok := offer.Grantable(id); ok && !seen[e.ID] {
			seen[e.ID] = true
			lines = append(lines, entryWords(offer, e, want.Level))
		}
	}
	out.Permissions = strings.Join(lines, "; ")

	if lookup == "" {
		return
	}
	for _, e := range offer.Grants {
		if !entryMatches(e, lookup) {
			continue
		}
		if len(out.Lookup) == MaxLookup {
			out.LookupMore = true
			break
		}
		out.Lookup = append(out.Lookup, lookupEntry(offer, e, want.Level))
	}
	if len(out.Lookup) == 0 {
		out.LookupNote = "No permission in " + out.Integration + "'s catalogue matches " + strconv.Quote(lookup)
	}
}

// LookupEntry is one catalogue entry a lookup matched, judged for one seat.
type LookupEntry struct {
	// ID is the vendor's own name for the permission, which a grant
	// stores; Name how a person finds it at the vendor; Group the heading
	// the vendor lists it under.
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group,omitempty"`

	// Write is a permission that may change things at the vendor, so
	// granting it asks a fresh step-up.
	Write bool `json:"write"`

	// Level is which level holds it, in words: "Already in this agent's
	// level", "Configure and above", or "No level".
	Level string `json:"level"`

	// Grantable is "Yes" where the company's connection can grant it, and
	// otherwise the vendor's reason it cannot yet; Missing is what a person
	// adds to the connection at the vendor first, and ActionURL where.
	Grantable string `json:"grantable"`
	Missing   string `json:"missing,omitempty"`
	ActionURL string `json:"action_url,omitempty"`
}

// lookupEntry is e as a lookup answers it for a seat on level.
func lookupEntry(offer Options, e GrantEntry, level string) LookupEntry {
	out := LookupEntry{ID: e.ID, Name: e.Name, Group: e.Group, Write: e.Write, Level: "No level",
		Grantable: "Yes", Missing: strings.Join(e.Missing, ", "), ActionURL: e.ActionURL}
	switch {
	case e.InLevel == "":
	case heldByLevel(offer, e.InLevel, level):
		out.Level = "Already in this agent's level"
	default:
		out.Level = inLevelName(offer, e.InLevel) + " and above"
	}
	if e.Unavailable != "" {
		out.Grantable = e.Unavailable
	}
	return out
}

// seatRequests is each request of requests that the seat asked on lane, oldest
// first.
func seatRequests(requests []GrantRequest, handle string, lane Lane) []GrantRequest {
	var out []GrantRequest
	for _, r := range requests {
		if r.Handle == handle && r.Lane == lane {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b GrantRequest) int {
		return cmp.Or(a.AskedAt.Compare(b.AskedAt), strings.Compare(a.ID, b.ID))
	})
	return out
}

// grantWords is one grant as a sentence: "Origin Rules: Edit on example.com
// until 2026-11-15 20:00 UTC, granted by Ada Lovelace on 2026-11-15 for the
// agent's request abcde12345".
func grantWords(offer Options, g Grant, enabled bool, have Observed, now time.Time) string {
	entry, listed := offer.Grantable(g.Permission)
	var b strings.Builder
	b.WriteString(entryName(entry, listed, g.Permission))
	b.WriteString(" " + grantWhere(offer, entry, listed, g.Scope))
	live := g.Live(now)
	switch {
	case g.ExpiresAt.IsZero():
		b.WriteString(" for good")
	case live:
		b.WriteString(" until " + g.ExpiresAt.UTC().Format(viewTime))
	default:
		b.WriteString(", lapsed at " + g.ExpiresAt.UTC().Format(viewTime))
	}
	if g.GrantedBy != "" {
		b.WriteString(", granted by " + g.GrantedBy)
		if !g.GrantedAt.IsZero() {
			b.WriteString(" on " + g.GrantedAt.UTC().Format(time.DateOnly))
		}
	}
	if g.Request != "" {
		b.WriteString(" for the agent's request " + g.Request)
	}
	onCredential := have.Exists && slices.ContainsFunc(have.Grants, func(h Grant) bool {
		return GrantKey(h) == GrantKey(g)
	})
	switch {
	case live && enabled && !onCredential:
		b.WriteString(" (not on its credential yet: the next sync adds it)")
	case !live && onCredential:
		b.WriteString(" (still on its credential until the next sync takes it off)")
	}
	return b.String()
}

// requestWords is one open request as a sentence, with the option ids its
// question grants it by: "request abcde12345 asks for Origin Rules: Edit on
// example.com for 4 hours, on OPS-77 on 2026-11-15; its question grants it
// with grant-rabcde12345-t (for 4 hours) or grant-rabcde12345-p (for good)".
func requestWords(offer Options, r GrantRequest) string {
	entry, listed := offer.Grantable(r.Permission)
	hours := strconv.Itoa(r.Hours) + " hours"
	if r.Hours == 1 {
		hours = "1 hour"
	}
	var b strings.Builder
	b.WriteString("request " + r.ID + " asks for " + entryName(entry, listed, r.Permission))
	b.WriteString(" " + grantWhere(offer, entry, listed, r.Scope) + " for " + hours)
	if r.Item != "" {
		b.WriteString(", on " + r.Item)
	}
	if !r.AskedAt.IsZero() {
		b.WriteString(" on " + r.AskedAt.UTC().Format(time.DateOnly))
	}
	b.WriteString("; its question grants it with " + GrantOptionTemporary(r.ID) + " (for " + hours +
		") or " + GrantOptionPermanent(r.ID) + " (for good)")
	return b.String()
}

// entryWords is one catalogue entry as a sentence, judged for a seat on level:
// "Origin Rules: Edit (Zone, changes things, in no level): The provisioner
// token does not hold Origin Rules: Edit; a person adds Origin Rules: Edit to
// the company's connection at https://...".
func entryWords(offer Options, e GrantEntry, level string) string {
	var facts []string
	if e.Group != "" {
		facts = append(facts, e.Group)
	}
	if e.Write {
		facts = append(facts, "changes things")
	} else {
		facts = append(facts, "read-only")
	}
	switch {
	case e.InLevel == "":
		facts = append(facts, "in no level")
	case heldByLevel(offer, e.InLevel, level):
		facts = append(facts, "already in this agent's level")
	default:
		facts = append(facts, "in the "+inLevelName(offer, e.InLevel)+" level and above")
	}
	words := e.Name + " (" + strings.Join(facts, ", ") + ")"
	if e.Unavailable == "" {
		return words + ": the company's connection can grant it"
	}
	words += ": " + e.Unavailable
	if len(e.Missing) > 0 {
		words += "; a person adds " + strings.Join(e.Missing, ", ") + " to the company's connection"
		if e.ActionURL != "" {
			words += " at " + e.ActionURL
		}
	}
	return words
}

// inLevelName is the one level a catalogue entry names as the lowest holding
// it ([GrantEntry.InLevel]), by the vendor's name for it, its id where the
// offer names none.
func inLevelName(offer Options, id string) string {
	if l, _, ok := offer.Level(id); ok && l.Name != "" {
		return l.Name
	}
	return id
}

// grantableWords counts the catalogue beside a seat's level.
func grantableWords(integration string, beside, lacking int) string {
	if beside == 0 {
		return integration + " offers no permission beside this agent's level"
	}
	noun := "permissions"
	if beside == 1 {
		noun = "permission"
	}
	words := fmt.Sprintf("%s offers %d %s beside this agent's level", integration, beside, noun)
	switch lacking {
	case 0:
		return words + ", every one of which the company's connection can grant"
	case beside:
		return words + ", none of which the company's connection holds yet"
	}
	return words + fmt.Sprintf("; the company's connection lacks %d of them", lacking)
}

// entryName is a permission by the catalogue's name, its identifier where the
// catalogue does not list it now.
func entryName(e GrantEntry, listed bool, permission string) string {
	if listed && e.Name != "" {
		return e.Name
	}
	return permission
}

// grantWhere is where a grant or request applies, in the lane's own words: its
// scope entries, the whole connection for an account permission, or the
// lane's whole reach for a scoped one naming none.
func grantWhere(offer Options, e GrantEntry, listed bool, scope []string) string {
	switch {
	case len(scope) > 0:
		return "on " + strings.Join(normalSet(scope), ", ")
	case listed && e.ScopeKind == GrantScopeAccount && offer.GrantAccount != "":
		return "on " + offer.GrantAccount
	case offer.GrantWhole != "":
		return "on " + offer.GrantWhole
	}
	return "on its whole reach"
}

// entryMatches reports whether lookup is part of e's name or identifier,
// ignoring case and the punctuation between words, so "origin rules edit"
// finds "Origin Rules: Edit".
func entryMatches(e GrantEntry, lookup string) bool {
	want := looseWords(lookup)
	return want != "" && (strings.Contains(looseWords(e.Name), want) || strings.Contains(looseWords(e.ID), want))
}

// looseWords is s lowercased with every run of characters other than letters
// and digits one space.
func looseWords(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}
