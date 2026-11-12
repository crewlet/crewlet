package atlas

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// Private reports whether r's own key is an address on a network the public
// internet does not route: a host name, an endpoint's host or a server named
// by its address. Such a key is itself what an agent outside its reach is not
// shown.
func (r Ref) Private() bool {
	switch r.Kind {
	case KindHostname:
		return config.MCPPrivateHost(strings.TrimPrefix(r.Key, "*."))
	case KindEndpoint:
		v, err := parseURL(r.Key)
		return err == nil && v.Private
	case KindHost:
		if address, ok := strings.CutPrefix(r.Key, "addr/"); ok {
			return config.MCPPrivateHost(address)
		}
	}
	return false
}

// Within is v as an agent reads it (ADR-0109): an entry reach says is not
// the agent's own loses every private address, its own key included, so the
// agent learns no internal address its own access does not already reach. An
// entry whose key is private is left out whole, with every relation to it.
// It answers how many entries and values it left out.
func Within(v View, reach func(Entry) bool) (View, int) {
	hidden := 0
	gone := map[uuid.UUID]bool{}
	out := make([]Entry, 0, len(v.Entries))
	for _, e := range v.Entries {
		if reach(e) {
			out = append(out, e)
			continue
		}
		if e.Ref.Private() {
			hidden++
			for _, m := range e.Members {
				gone[m.ID()] = true
			}
			continue
		}
		facts := make([]FactView, 0, len(e.Facts))
		for _, f := range e.Facts {
			if f.Value.Private {
				hidden++
				continue
			}
			f.Others = slices.DeleteFunc(slices.Clone(f.Others), func(c Claimed) bool {
				if c.Value.Private {
					hidden++
					return true
				}
				return false
			})
			if len(f.Others) == 0 {
				f.Others = nil
			}
			facts = append(facts, f)
		}
		e.Facts = facts
		out = append(out, e)
	}
	for i := range out {
		out[i].Relations = slices.DeleteFunc(slices.Clone(out[i].Relations), func(ed Edge) bool {
			return ed.External == "" && gone[ed.To]
		})
	}
	v.Entries = out
	return v, hidden
}

// MinNamed is the shortest name or key an entry is found by in prose: a
// shorter one ("db", "api") is a word too common to say the work names it.
const MinNamed = 4

// Named is the current entries of v that text names exactly, the most
// specific first: an entry's name or its key appearing in text as a whole
// word, compared case folded. A name is matched before a key, and a longer
// match before a shorter one, so "api.acme.example" is found before "acme".
func (v View) Named(text string) []Entry {
	folded := strings.ToLower(text)
	type found struct {
		entry  Entry
		byName bool
		length int
		at     int
	}
	var hits []found
	for _, e := range v.Entries {
		if e.State == StateRetired {
			continue
		}
		best := found{at: -1}
		for i, term := range []string{e.Name, e.Ref.Key} {
			term = strings.ToLower(strings.TrimSpace(term))
			if utf8.RuneCountInString(term) < MinNamed {
				continue
			}
			if at := wordAt(folded, term); at >= 0 && (best.at < 0 || len(term) > best.length) {
				best = found{entry: e, byName: i == 0, length: len(term), at: at}
			}
		}
		if best.at >= 0 {
			hits = append(hits, best)
		}
	}
	slices.SortFunc(hits, func(a, b found) int {
		return cmp.Or(compareBool(!a.byName, !b.byName), cmp.Compare(b.length, a.length), cmp.Compare(a.at, b.at),
			cmp.Compare(a.entry.Ref.Key, b.entry.Ref.Key))
	})
	out := make([]Entry, len(hits))
	for i, h := range hits {
		out[i] = h.entry
	}
	return out
}

// wordAt is where term first stands in text as a whole word, or -1: the
// character before it and after it neither a letter nor a digit.
func wordAt(text, term string) int {
	for from := 0; ; {
		i := strings.Index(text[from:], term)
		if i < 0 {
			return -1
		}
		at := from + i
		before, _ := utf8.DecodeLastRuneInString(text[:at])
		after, _ := utf8.DecodeRuneInString(text[at+len(term):])
		if !wordRune(before) && !wordRune(after) {
			return at
		}
		from = at + 1
	}
}

func wordRune(r rune) bool {
	return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// Writers is who wrote what entries carry, once each: a person and an agent
// by their seats, the settings as the company's operators and the engine's
// records as the engine. Listed reports whether any of it is a vendor's
// listing, which no writer a reader can name wrote.
func Writers(entries []Entry) (writers []provenance.Author, listed bool) {
	seen := map[provenance.Author]bool{}
	for _, e := range entries {
		for _, src := range e.Sources {
			var w provenance.Author
			switch src.Kind {
			case SourcePerson:
				w = provenance.Author{Kind: iam.ActorHuman, Seat: src.Name}
			case SourceAgent:
				w = provenance.Author{Kind: iam.ActorAgent, Seat: src.Name}
			case SourceSettings:
				w = provenance.Author{Kind: iam.ActorOperator}
			case SourceRecords:
				w = provenance.Author{Kind: iam.ActorSystem}
			case SourceIntegration:
				listed = true
				continue
			default:
				continue
			}
			if !seen[w] {
				seen[w] = true
				writers = append(writers, w)
			}
		}
	}
	return writers, listed
}
