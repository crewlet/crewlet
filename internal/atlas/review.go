package atlas

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/words"
)

// ReviewKind is why an entry waits for a person.
type ReviewKind string

// The reasons an entry waits, in the order a reader decides them.
const (
	// ReviewDisagreement is one fact or relation its sources disagree on:
	// both values are shown and nothing is settled until a person decides.
	ReviewDisagreement ReviewKind = "disagreement"
	// ReviewUnconfirmed is an entry only an agent reported, with nothing the
	// engine could check: hidden from the map until a person confirms it.
	ReviewUnconfirmed ReviewKind = "unconfirmed"
	// ReviewLikely is an entry an agent backed with a call or a file, that
	// no person, integration or setting stands behind yet. An entry that is
	// likely only because a repository's files declare it is not one: a file
	// suggests, and a suggestion asks nothing of anybody (ADR-0133).
	ReviewLikely ReviewKind = "likely"
)

// ReviewKinds is every reason, in that order.
var ReviewKinds = []ReviewKind{ReviewDisagreement, ReviewUnconfirmed, ReviewLikely}

// Valid reports whether k is a member of the closed set.
func (k ReviewKind) Valid() bool { return slices.Contains(ReviewKinds, k) }

// ReviewItem is one thing that waits for a person. NOTHING STORES IT: it is
// read off the folded view on every read, so it never outlives its cause and
// two nodes never raise it twice. A person's decision is an ordinary claim.
type ReviewItem struct {
	// ID names the item among one answer's: its entry and what on it waits.
	ID    string     `json:"id"`
	Kind  ReviewKind `json:"kind"`
	Entry Summary    `json:"entry"`
	// Fact or Relation is what the sources disagree on, with every value a
	// source holds beside the one shown. Both absent for an entry that waits
	// as a whole.
	Fact     *FactView `json:"fact,omitempty"`
	Relation *Edge     `json:"relation,omitempty"`
	// Target is what a disputed relation points at, by name.
	Target *LineTarget `json:"target,omitempty"`
	// Reports is, for an entry that waits as a whole, each agent's newest
	// word on it, newest first and at most [MaxReports]: the seat, the turn,
	// the call it cited and what that call is evidence of.
	Reports []Report `json:"reports,omitempty"`
	// Since is the newest claim the item rests on: when it began to wait as
	// it stands now.
	Since time.Time `json:"since"`
}

// Review is one page of what waits for a person.
type Review struct {
	Items []ReviewItem
	Next  string
	// Total and Counts are over every item of the asked kind and of each kind,
	// whatever the page holds.
	Total  int
	Counts map[ReviewKind]int
}

// reviewCursor is where a page of the review ended: its last item's place in
// the one order every read gives.
type reviewCursor struct {
	Kind  ReviewKind `json:"k"`
	Entry Kind       `json:"e"`
	Key   string     `json:"key"`
	Facet string     `json:"f,omitempty"`
}

// reviewPlace is an item's place in the review's order: by reason, then the
// map's order of entries, then the fact or relation on it.
type reviewPlace struct {
	item  ReviewItem
	facet string
}

func (p reviewPlace) compare(kind ReviewKind, entry Kind, key, facet string) int {
	return cmp.Or(cmp.Compare(slices.Index(ReviewKinds, p.item.Kind), slices.Index(ReviewKinds, kind)),
		cmp.Compare(p.item.Entry.Kind.rank(), entry.rank()), cmp.Compare(p.item.Entry.Key, key),
		cmp.Compare(p.facet, facet))
}

// Review is what waits for a person in v, one page of at most limit items of
// kind ("" for every kind) after the cursor a previous page answered. Retired
// entries wait for nobody. An unconfirmed entry is listed here though a map
// leaves it out, since here is where it is confirmed.
func (v View) Review(kind ReviewKind, after string, limit int) (Review, error) {
	return v.ReviewOf(ReviewQuery{Kind: kind, After: after, Limit: limit})
}

// ReviewQuery is what one page of the review is asked for.
type ReviewQuery struct {
	// Kind is the reason to list, "" for every reason; After the cursor a
	// previous page answered; Limit the page, 1 to [PageMax].
	Kind  ReviewKind
	After string
	Limit int

	// ContradictedFor narrows the review to what contradicts this person's
	// own word: a fact or a relation they set or confirmed that a newer
	// report differs from ([StandingContested]). Empty lists everybody's.
	// Total and Counts are then of that person's alone.
	ContradictedFor string
}

// contradicts reports whether a contested fact or relation is the word of
// the person handle, which a newer report differs from.
func contradicts(handle string, standing Standing, src Source) bool {
	return standing == StandingContested && src.Kind == SourcePerson && src.Name == handle
}

// ReviewOf is [View.Review] for q.
func (v View) ReviewOf(q ReviewQuery) (Review, error) {
	kind, after, limit, mine := q.Kind, q.After, q.Limit, q.ContradictedFor
	if kind != "" && !kind.Valid() {
		return Review{}, fmt.Errorf("%w: `kind` is %s", ErrQuery, words.Or(ReviewKinds))
	}
	if limit < 1 || limit > PageMax {
		return Review{}, fmt.Errorf("%w: `limit` is 1 to %d", ErrQuery, PageMax)
	}
	var from *reviewCursor
	if after != "" {
		raw, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil || json.Unmarshal(raw, &from) != nil || from == nil || !from.Kind.Valid() {
			return Review{}, fmt.Errorf("%w: `cursor` is the `next_cursor` a page of the review answered", ErrQuery)
		}
	}
	summaries := v.Summaries(true)
	index := v.Index()
	var places []reviewPlace
	add := func(e *Entry, k ReviewKind, facet string, since time.Time, fill func(*ReviewItem)) {
		item := ReviewItem{ID: e.ID.String() + "/" + string(k), Kind: k, Entry: summaries.Of(*e), Since: since}
		if facet != "" {
			item.ID += "/" + facet
		}
		if fill != nil {
			fill(&item)
		}
		places = append(places, reviewPlace{item: item, facet: facet})
	}
	for i := range v.Entries {
		e := &v.Entries[i]
		if e.State == StateRetired {
			continue
		}
		for j := range e.Facts {
			f := e.Facts[j]
			if f.Standing == StandingSettled {
				continue
			}
			if mine != "" && !contradicts(mine, f.Standing, f.Source) {
				continue
			}
			facet := "fact/" + string(f.Fact) + "/" + f.Of.Key + "/" + f.Value.Text
			add(e, ReviewDisagreement, facet, newestOf(f.ObservedAt, f.Others), func(it *ReviewItem) { it.Fact = &f })
		}
		for j := range e.Relations {
			ed := e.Relations[j]
			if ed.Standing == StandingSettled {
				continue
			}
			if mine != "" && !contradicts(mine, ed.Standing, ed.Source) {
				continue
			}
			target := &LineTarget{External: ed.External, Name: ed.Name}
			facet := "relation/" + string(ed.Relation) + "/" + string(ed.External) + "/" + ed.Name
			if ed.External == "" {
				target.ID, target.Name = ed.To, ed.To.String()
				if at, ok := index[ed.To]; ok {
					target.Name = v.Entries[at].Name
				}
				facet = "relation/" + string(ed.Relation) + "/" + ed.To.String()
			}
			add(e, ReviewDisagreement, facet, newestOf(ed.ObservedAt, ed.Others), func(it *ReviewItem) {
				it.Relation, it.Target = &ed, target
			})
		}
		if mine != "" {
			// AN ENTRY THAT WAITS AS A WHOLE contradicts nobody's word.
			continue
		}
		reports := func(it *ReviewItem) { it.Reports = e.reports[:min(len(e.reports), MaxReports)] }
		switch e.Word {
		case WordUnconfirmed:
			add(e, ReviewUnconfirmed, "", e.LastSeen, reports)
		case WordLikely:
			// ONLY WHAT AN AGENT REPORTED WAITS. A repository's files make an
			// entry likely too, and a scan declares many at once: listing
			// them would bury the reports a person is asked to decide, and
			// its entry page confirms one all the same.
			if slices.ContainsFunc(e.Sources, func(s Source) bool { return s.Kind == SourceAgent }) {
				add(e, ReviewLikely, "", e.LastSeen, reports)
			}
		}
	}
	slices.SortFunc(places, func(a, b reviewPlace) int {
		return a.compare(b.item.Kind, b.item.Entry.Kind, b.item.Entry.Key, b.facet)
	})
	out := Review{Items: []ReviewItem{}, Counts: map[ReviewKind]int{}}
	for _, p := range places {
		out.Counts[p.item.Kind]++
		if kind != "" && p.item.Kind != kind {
			continue
		}
		out.Total++
		if from != nil && p.compare(from.Kind, from.Entry, from.Key, from.Facet) <= 0 {
			continue
		}
		if len(out.Items) == limit {
			if out.Next == "" {
				last := places[slices.IndexFunc(places, func(q reviewPlace) bool {
					return q.item.ID == out.Items[limit-1].ID
				})]
				raw, err := json.Marshal(reviewCursor{Kind: last.item.Kind, Entry: last.item.Entry.Kind,
					Key: last.item.Entry.Key, Facet: last.facet})
				if err != nil {
					return Review{}, fmt.Errorf("atlas: review cursor: %w", err)
				}
				out.Next = base64.RawURLEncoding.EncodeToString(raw)
			}
			continue
		}
		out.Items = append(out.Items, p.item)
	}
	return out, nil
}

// newestOf is the newest instant among a shown claim's and the claims beside it.
func newestOf(shown time.Time, others []Claimed) time.Time {
	for _, o := range others {
		if o.ObservedAt.After(shown) {
			shown = o.ObservedAt
		}
	}
	return shown
}
