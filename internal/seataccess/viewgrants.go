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

// A lane view's words for a seat's grants (ADR-0150), its open requests
// (ADR-0118) and the lane's catalogue, which is counted and never worded whole.

// MaxLookup is the most entries a lookup answers: one permission's variants with
// room to spare.
const MaxLookup = 12

// MaxLookupChars bounds a lookup: longer than any permission's name.
const MaxLookupChars = 100

// viewTime writes an instant to the minute, in UTC.
const viewTime = "2006-01-02 15:04 UTC"

// wordGrants fills out's grant, request and catalogue words.
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
	var worded []GrantEntry
	seen := map[string]bool{}
	for _, id := range named {
		if e, ok := offer.Grantable(id); ok && !seen[e.ID] {
			seen[e.ID] = true
			lines = append(lines, entryWords(offer, e, want.Level))
			worded = append(worded, e)
		}
	}
	out.Permissions = strings.Join(lines, "; ")

	if lookup != "" {
		for _, e := range offer.Grants {
			if !entryMatches(e, lookup) {
				continue
			}
			if len(out.Lookup) == MaxLookup {
				out.LookupMore = true
				break
			}
			out.Lookup = append(out.Lookup, lookupEntry(offer, e, want.Level))
			worded = append(worded, e)
		}
		if len(out.Lookup) == 0 {
			out.LookupNote = "No permission in " + out.Integration + "'s catalogue matches " +
				strconv.Quote(lookup)
		}
	}
	out.AtVendor = vendorStep(out.Integration, worded)
}

// VendorStep is what a person adds to the company's own connection at a vendor
// (Cloudflare's provisioner token, say): Crewlet grants only what it holds.
type VendorStep struct {
	// Label says the step, naming the vendor.
	Label string `json:"label"`

	// Add is each permission to add, as the vendor shows it.
	Add []string `json:"add"`

	// URL is where, from the catalogue's engine-built [GrantEntry.ActionURL].
	URL string `json:"url,omitempty" link:"engine"`
}

// vendorStep is the step the unavailable entries ask of a person, or nil.
func vendorStep(integration string, entries []GrantEntry) *VendorStep {
	var step VendorStep
	for _, e := range entries {
		if e.Unavailable == "" {
			continue
		}
		add := e.Missing
		if len(add) == 0 {
			add = []string{e.Name}
		}
		for _, a := range add {
			if !slices.Contains(step.Add, a) {
				step.Add = append(step.Add, a)
			}
		}
		if step.URL == "" {
			step.URL = e.ActionURL
		}
	}
	if len(step.Add) == 0 {
		return nil
	}
	step.Label = "Add to the company's connection at " + integration
	return &step
}

// LookupEntry is one catalogue entry a lookup matched, judged for one seat.
type LookupEntry struct {
	// ID is the vendor's name a grant stores; Name how a person finds it; Group its
	// heading.
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group,omitempty"`

	// Write is a permission that changes things, so granting it asks a step-up.
	Write bool `json:"write"`

	// Level is which level holds it, in words.
	Level string `json:"level"`

	// Grantable is "Yes", or why the connection cannot grant it yet; Missing is what
	// to add at the vendor, and ActionURL where.
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

// seatRequests is the seat's requests on lane, oldest first.
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

// grantWords is one grant as a sentence.
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

// requestWords is one open request as a sentence, with its two grant options.
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

// entryWords is one catalogue entry as a sentence, judged for a seat on level.
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

// inLevelName is the vendor's name for an entry's lowest level, else its id.
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

// entryName is a permission by the catalogue's name, else its id.
func entryName(e GrantEntry, listed bool, permission string) string {
	if listed && e.Name != "" {
		return e.Name
	}
	return permission
}

// grantWhere is where a grant or request applies, in the lane's words.
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

// entryMatches reports whether lookup is part of e's name or id, ignoring case
// and punctuation.
func entryMatches(e GrantEntry, lookup string) bool {
	want := looseWords(lookup)
	return want != "" && (strings.Contains(looseWords(e.Name), want) || strings.Contains(looseWords(e.ID), want))
}

// looseWords is s lowercased, each run of non-letters one space.
func looseWords(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}
