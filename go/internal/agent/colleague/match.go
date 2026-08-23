// Package colleague resolves a free-text query to a seat in the org.
//
// The lookup an agent does before it addresses anyone: before an A2A ask,
// before a Slack mention, before a Confluence @mention. What makes it more
// than a map lookup is that a model types what it remembers — "ceo", "the
// eng lead", "Yazılım" — and the answer has to be either one seat or an
// honest list of who it might be. NEVER a guess: an agent that silently
// addressed the wrong colleague is worse than one that asked which.
//
// Four tiers, earlier ones short-circuiting later ones, so a query that is
// exactly somebody's handle never gets fuzzy-matched against everybody else.
package colleague

import (
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// The tuning constants, each with the failure it prevents.
const (
	// FuzzyCutoff is the similarity a tier-4 match must clear.
	//
	// 0.6 on a Ratcliff/Obershelp ratio: below it, unrelated short names
	// start matching on a shared prefix, and the disambiguation list stops
	// being a list of plausible colleagues.
	FuzzyCutoff = 0.6

	// MinSubstringQuery is the shortest query tier 3 will run.
	//
	// Two characters substring-match half an org. Three is the shortest
	// query that names anything ("ceo", "cto", "ops").
	MinSubstringQuery = 3

	// MinFuzzyQuery is the shortest query tier 4 will run, and it is
	// deliberately higher than the substring floor: a three-character
	// ratio crosses 0.6 on a single shared character, so fuzzy matching a
	// query that short returns noise ranked by accident.
	MinFuzzyQuery = 4
)

// Method names how a candidate was matched, so the caller can say why.
type Method string

const (
	MethodExactHandle     Method = "exact_handle"
	MethodExternalID      Method = "external_id"
	MethodExactRole       Method = "exact_role"
	MethodCaseInsensitive Method = "case_insensitive"
	MethodSubstring       Method = "substring"
	MethodFuzzy           Method = "fuzzy"
)

// Label is the phrase shown to a model beside an ambiguous candidate.
func (m Method) Label() string {
	switch m {
	case MethodCaseInsensitive:
		return "case / format match"
	case MethodSubstring:
		return "partial-name match"
	case MethodFuzzy:
		return "fuzzy match"
	}
	return ""
}

// Seat is one addressable party in the corpus: an agent or a human.
type Seat struct {
	Handle string
	Name   string
	// Kind is "agent" or "human". A human is addressable and never
	// spawned, and the difference changes how an agent may reach them.
	Kind string
	// External maps a transport to this seat's id there (slack, jira,
	// confluence), for the exact-id tier.
	External map[string]string
}

// Candidate is one match, with how it was found.
type Candidate struct {
	Seat   Seat
	Method Method
	// Score is the fuzzy ratio for a tier-4 match, 1 for every other tier.
	Score float64
}

// entry is a seat with its match keys computed once.
type entry struct {
	seat   Seat
	handle string // normalised handle
	name   string // normalised role name
}

// Resolve returns every candidate for a query, best tier first.
//
// The list is deduplicated by handle, so a length above one means DISTINCT
// seats matched and the caller must not pick between them. Empty means
// nothing matched, which is a different answer from ambiguity and reads
// differently to a model: one says "try another spelling", the other says
// "say which of these".
func Resolve(query string, seats []Seat) []Candidate {
	corpus := make([]entry, 0, len(seats))
	for _, s := range seats {
		if s.Handle == "" {
			continue
		}
		corpus = append(corpus, entry{
			seat: s, handle: Normalize(s.Handle), name: Normalize(s.Name),
		})
	}

	var out []Candidate
	seen := map[string]bool{}
	add := func(e entry, m Method, score float64) {
		if e.seat.Handle == "" || seen[e.seat.Handle] {
			return
		}
		seen[e.seat.Handle] = true
		out = append(out, Candidate{Seat: e.seat, Method: m, Score: score})
	}

	// TIER 1 — exact, case-sensitive, on the identifiers a seat actually
	// has: its handle, its ids on each transport, its role name. A hit
	// here is not a guess at all and must never be diluted by a later
	// tier's near-misses.
	//
	// The underscore fold is for chat: Slack renders handles with
	// underscores and Crewlet's are hyphenated, so an agent copying a
	// name out of a message would otherwise miss its own colleague.
	slackStyle := strings.ReplaceAll(query, "_", "-")
	for _, e := range corpus {
		switch {
		case e.seat.Handle == query, slackStyle != query && e.seat.Handle == slackStyle:
			add(e, MethodExactHandle, 1)
		}
	}
	for _, e := range corpus {
		for _, id := range e.seat.External {
			if id != "" && id == query {
				add(e, MethodExternalID, 1)
				break
			}
		}
	}
	for _, e := range corpus {
		if e.seat.Name == query {
			add(e, MethodExactRole, 1)
		}
	}
	if len(out) > 0 {
		return sortByHandle(out)
	}

	q := Normalize(query)
	if q == "" {
		return nil
	}

	// TIER 2 — equality once case and separators are folded away. Strong
	// enough to short-circuit the rest: it means the query IS the handle
	// or the role name, modulo how it was typed.
	for _, e := range corpus {
		if e.handle == q || e.name == q {
			add(e, MethodCaseInsensitive, 1)
		}
	}
	if len(out) > 0 {
		return sortByHandle(out)
	}

	// TIER 3 — partial names, in both directions.
	if len([]rune(q)) >= MinSubstringQuery {
		tokens := strings.Fields(q)
		var hits []entry
		for _, e := range corpus {
			if partialMatch(q, tokens, e.handle) || partialMatch(q, tokens, e.name) {
				hits = append(hits, e)
			}
		}
		// Sorted before adding, so the disambiguation list is stable
		// across whatever order the org happened to enumerate in.
		sortEntries(hits)
		for _, e := range hits {
			add(e, MethodSubstring, 1)
		}
		if len(out) > 0 {
			return out
		}
	}

	// TIER 4 — fuzzy, and only when everything above found nothing.
	if len([]rune(q)) < MinFuzzyQuery {
		return nil
	}
	type scored struct {
		e     entry
		score float64
	}
	var ranked []scored
	for _, e := range corpus {
		score := max(ratio(q, e.handle), ratio(q, e.name))
		if score >= FuzzyCutoff {
			ranked = append(ranked, scored{e, score})
		}
	}
	// Best first, handle as the deterministic tiebreaker.
	slices.SortFunc(ranked, func(a, b scored) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.e.seat.Handle, b.e.seat.Handle)
	})
	for _, s := range ranked {
		add(s.e, MethodFuzzy, s.score)
	}
	return out
}

// Normalize folds an identifier for cross-style matching.
//
// NFKD-decompose, casefold, then strip combining marks — so Turkish "İK"
// folds to "ik" rather than to "i" plus a combining dot, German "ß" folds to
// "ss", and full-width Latin folds to ASCII. Turkish dotless "ı" is NOT
// decomposed by NFKD and does not casefold to ASCII "i", so it is mapped
// explicitly; without that an ASCII query "yazilim" cannot reach a role named
// "Yazılım", which is exactly the kind of seat a model types from memory.
//
// Then runs of whitespace, underscores and dashes — ASCII and the Unicode
// ones a name pasted out of Confluence carries — collapse to one space, so
// "Agent CEO", "agent-ceo", "agent_ceo" and "agent—ceo" are one string.
func Normalize(value string) string {
	if value == "" {
		return ""
	}
	folded := cases.Fold().String(norm.NFKD.String(value))
	var b strings.Builder
	b.Grow(len(folded))
	space := false
	for _, r := range folded {
		switch {
		case unicode.Is(unicode.Mn, r):
			// A combining mark: dropped, not replaced, so "é" and "e"
			// are one key rather than two separated by a space.
			continue
		case r == 'ı':
			b.WriteRune('i')
			space = false
		case isSeparator(r):
			if !space && b.Len() > 0 {
				b.WriteRune(' ')
				space = true
			}
		default:
			b.WriteRune(r)
			space = false
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// isSeparator reports the characters that collapse to a space: whitespace,
// underscore, and every dash a name might carry — ASCII hyphen-minus, the
// Unicode hyphens and dashes (U+2010..U+2015), and the true minus sign.
func isSeparator(r rune) bool {
	switch {
	case unicode.IsSpace(r), r == '_', r == '-', r == '−':
		return true
	case r >= '‐' && r <= '―':
		return true
	}
	return false
}

// partialMatch is the tier-3 test against one normalised name.
//
// FORWARD: the query appears in the name at a word boundary — "ceo" finds
// "agent ceo". The boundary guard is what stops "log" finding "blog editor",
// which is a mid-word fragment and not a name anyone typed.
//
// REVERSE: the name appears in the query as a contiguous run of whole tokens
// — "senior engineer" finds the role "engineer". Whole tokens, so "engine"
// does not find "engineer" backwards.
func partialMatch(q string, tokens []string, name string) bool {
	if name == "" {
		return false
	}
	if wordAligned(name, q) {
		return true
	}
	nameTokens := strings.Fields(name)
	if len(nameTokens) == 0 || len(nameTokens) > len(tokens) {
		return false
	}
	for i := 0; i+len(nameTokens) <= len(tokens); i++ {
		if slices.Equal(tokens[i:i+len(nameTokens)], nameTokens) {
			return true
		}
	}
	return false
}

// wordAligned reports whether needle occurs in haystack starting at the
// string's start or just after a space.
func wordAligned(haystack, needle string) bool {
	for i := 0; ; {
		pos := strings.Index(haystack[i:], needle)
		if pos < 0 {
			return false
		}
		at := i + pos
		if at == 0 || haystack[at-1] == ' ' {
			return true
		}
		i = at + 1
	}
}
