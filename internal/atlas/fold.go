package atlas

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// StaleAfter is how long an agent's claim nobody else supports stays
	// current: an infrastructure fact unconfirmed for a month deserves a
	// second look.
	StaleAfter = 30 * 24 * time.Hour
	// LapseAfter is when an agent's claim nobody re-confirmed stops counting
	// at all: half a year without anybody seeing it again, it no longer
	// describes the company.
	LapseAfter = 180 * 24 * time.Hour
)

// Fold is what the atlas says, given every claim and the instant it is read
// at. It is PURE and DETERMINISTIC: the same claims in any order give the same
// view, so every node answers alike and nothing has to be written for the
// answer to be right.
//
// Two entries of one kind sharing an exact key (a public address, a
// repository's forge address, an account's own id) are ONE entry, unless the
// key is shared inside one source, which makes it a shared address rather than
// an identity, or a person said the two are not the same, which always wins.
//
// For each fact and relation, a source's newest claim stands for it; across
// sources the most trusted wins, then the newest. A person outranks every
// inference, and a disagreement is shown beside the value, never settled.
func Fold(claims []Claim, now time.Time) View {
	view := View{Entries: []Entry{}}
	live := make([]Claim, 0, len(claims))
	for _, c := range claims {
		if err := c.validate(); err != nil {
			view.Skipped = append(view.Skipped, Skip{Subject: named(c.Subject), Reason: err.Error()})
			continue
		}
		if c.Source.Kind == SourceAgent && now.Sub(c.ObservedAt) > LapseAfter {
			continue
		}
		live = append(live, c)
	}
	slices.SortFunc(view.Skipped, func(a, b Skip) int {
		return cmp.Or(compareRef(a.Subject, b.Subject), cmp.Compare(a.Reason, b.Reason))
	})
	canonical := merge(live)
	byEntry := map[Ref][]Claim{}
	for _, c := range live {
		if c.Facet.Kind == FacetDistinct {
			continue
		}
		c.Subject = canonical.of(c.Subject)
		if c.Facet.Kind == FacetRelation && c.Facet.Target.External == "" {
			c.Facet.Target.Entry = canonical.of(c.Facet.Target.Entry)
			if c.Facet.Target.Entry == c.Subject {
				continue
			}
		}
		byEntry[c.Subject] = append(byEntry[c.Subject], c)
	}
	for ref, cs := range byEntry {
		if e, ok := entryOf(ref, cs, canonical.members[ref], now); ok {
			view.Entries = append(view.Entries, e)
		}
	}
	present := map[Ref]bool{}
	for _, e := range view.Entries {
		present[e.Ref] = true
	}
	for i := range view.Entries {
		e := &view.Entries[i]
		e.Relations = slices.DeleteFunc(e.Relations, func(ed Edge) bool {
			return ed.External == "" && !present[canonical.byID[ed.To]]
		})
	}
	slices.SortFunc(view.Entries, func(a, b Entry) int { return compareRef(a.Ref, b.Ref) })
	return view
}

// named is the entry a refused claim was about, where its key is one; a key
// its grammar refuses is not repeated, since it may be what was refused.
func named(r Ref) Ref {
	if canonical, err := NewRef(r.Kind, r.Key); err == nil && canonical == r {
		return r
	}
	if r.Kind.Valid() {
		return Ref{Kind: r.Kind}
	}
	return Ref{}
}

func compareRef(a, b Ref) int {
	return cmp.Or(cmp.Compare(a.Kind.rank(), b.Kind.rank()), cmp.Compare(a.Key, b.Key))
}

// facetKey is the one thing a facet claims, after merging: a fact that holds
// many values is one facet per value.
func facetKey(c Claim) string {
	f := c.Facet
	switch f.Kind {
	case FacetFact:
		if specs()[f.Fact].Many {
			return "f\x00" + string(f.Fact) + "\x00" + c.Value.Text
		}
		return "f\x00" + string(f.Fact)
	case FacetRelation:
		if f.Target.External != "" {
			return "r\x00" + string(f.Relation) + "\x00" + string(f.Target.External) + "\x00" + f.Target.Name
		}
		return "r\x00" + string(f.Relation) + "\x00" + f.Target.Entry.String()
	}
	return string(f.Kind)
}

// newer orders two claims of one source on one facet: the later instant, then
// the later log position, stands.
func newer(a, b Claim) int {
	return cmp.Or(b.ObservedAt.Compare(a.ObservedAt), cmp.Compare(b.Position, a.Position))
}

// order is the fold's total order over the claims on one facet: the most
// trusted first, then the newest, then a recorded claim before one computed
// at read, then by source and value so the order never depends on the input's.
func order(a, b Claim) int {
	return cmp.Or(
		cmp.Compare(a.Confidence().Rank(), b.Confidence().Rank()),
		b.ObservedAt.Compare(a.ObservedAt),
		cmp.Compare(b.Position, a.Position),
		cmp.Compare(a.Source.identity(), b.Source.identity()),
		cmp.Compare(a.Value.Text, b.Value.Text),
		compareBool(a.Denied, b.Denied),
	)
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// resolution is one facet as the fold reads it.
type resolution struct {
	winner    Claim
	suggested bool
	standing  Standing
	others    []Claim
	voices    []Claim
}

// meaningful reports whether a facet is one only a person decides: a fact of
// meaning, or an owner. The settings and the engine's records are people's
// decisions written down, so they decide it too.
func meaningful(c Claim) bool {
	return (c.Facet.Kind == FacetFact && specs()[c.Facet.Fact].Meaning) ||
		(c.Facet.Kind == FacetRelation && c.Facet.Relation == RelationOwnedBy)
}

func decides(c Claim) bool { return c.Source.Kind == SourcePerson || c.Source.Kind.Computed() }

// resolve folds the live claims on one facet. Each source's newest claim
// stands for it; the most trusted then newest wins; a fact of meaning is won
// only by a person, the settings or the engine's records, and is otherwise a
// suggestion.
func resolve(claims []Claim) resolution {
	newest := map[string]Claim{}
	for _, c := range claims {
		id := c.Source.identity()
		if prior, ok := newest[id]; !ok || newer(c, prior) < 0 {
			newest[id] = c
		}
	}
	voices := make([]Claim, 0, len(newest))
	for _, c := range newest {
		voices = append(voices, c)
	}
	slices.SortFunc(voices, order)
	r := resolution{winner: voices[0], standing: StandingSettled, voices: voices}
	if meaningful(voices[0]) {
		if i := slices.IndexFunc(voices, decides); i >= 0 {
			r.winner = voices[i]
		} else {
			r.suggested = true
		}
	}
	for _, c := range voices {
		if c.Source.identity() == r.winner.Source.identity() || agrees(c, r.winner) {
			continue
		}
		r.others = append(r.others, c)
		if r.suggested || c.Confidence().Rank() > ConfidenceObserved.Rank() || (meaningful(c) && !decides(c)) {
			continue
		}
		if r.winner.Source.Kind == SourcePerson {
			if c.Source.Kind != SourcePerson && c.ObservedAt.After(r.winner.ObservedAt) {
				r.standing = StandingContested
			}
		} else if r.standing == StandingSettled {
			r.standing = StandingDisputed
		}
	}
	return r
}

func agrees(a, b Claim) bool { return a.Denied == b.Denied && a.Value.Equal(b.Value) }

// entryOf folds one entry's claims. An entry nothing live asserts is no entry.
func entryOf(ref Ref, claims []Claim, members []Ref, now time.Time) (Entry, bool) {
	facets := map[string][]Claim{}
	for _, c := range claims {
		k := facetKey(c)
		facets[k] = append(facets[k], c)
	}
	e := Entry{ID: ref.ID(), Ref: ref, Lane: ref.Kind.Lane(), Name: ref.Key, Members: members,
		Facts: []FactView{}, Relations: []Edge{}}
	var exists *resolution
	asserted, sources := false, map[string]Source{}
	var support, winners []Claim
	for k, cs := range facets {
		r := resolve(cs)
		for _, c := range r.voices {
			if !c.Denied {
				sources[c.Source.identity()] = c.Source
				if c.ObservedAt.After(e.LastSeen) {
					e.LastSeen = c.ObservedAt
				}
			}
		}
		if r.standing != StandingSettled {
			e.Review = true
		}
		if k == string(FacetExists) {
			exists = &r
		}
		if r.winner.Denied {
			continue
		}
		asserted = true
		winners = append(winners, r.winner)
		if !r.suggested {
			support = append(support, r.winner)
		}
		switch r.winner.Facet.Kind {
		case FacetFact:
			e.Facts = append(e.Facts, factView(r))
		case FacetRelation:
			e.Relations = append(e.Relations, edge(r))
		}
	}
	if !asserted && (exists == nil || !exists.winner.Denied) {
		return Entry{}, false
	}
	for _, s := range sources {
		e.Sources = append(e.Sources, s)
	}
	slices.SortFunc(e.Sources, func(a, b Source) int { return cmp.Compare(a.identity(), b.identity()) })
	e.State, e.Confidence = stateOf(exists, support, winners, now)
	e.Word = e.Confidence.Display()
	for _, f := range e.Facts {
		if f.Fact == FactName {
			e.Name = f.Value.Text
		}
	}
	slices.SortFunc(e.Facts, func(a, b FactView) int {
		return cmp.Or(cmp.Compare(slices.Index(Facts, a.Fact), slices.Index(Facts, b.Fact)),
			cmp.Compare(a.Value.Text, b.Value.Text))
	})
	slices.SortFunc(e.Relations, func(a, b Edge) int {
		return cmp.Or(cmp.Compare(slices.Index(Relations, a.Relation), slices.Index(Relations, b.Relation)),
			cmp.Compare(a.To.String(), b.To.String()), cmp.Compare(a.External, b.External), cmp.Compare(a.Name, b.Name))
	})
	return e, true
}

// stateOf reads an entry's state and confidence off the claims supporting it.
// support is the live claims an entry's facts and relations rest on, winners
// those and its suggestions too.
func stateOf(exists *resolution, support, winners []Claim, now time.Time) (State, Confidence) {
	if exists != nil && exists.winner.Denied {
		return StateRetired, exists.winner.Confidence()
	}
	var best Claim
	switch {
	case exists != nil:
		best = exists.winner
	case len(support) > 0:
		best = slices.MinFunc(support, order)
	default:
		best = slices.MinFunc(winners, order)
	}
	switch {
	case slices.ContainsFunc(support, func(c Claim) bool { return decides(c) }):
		return StateConfirmed, best.Confidence()
	case !slices.ContainsFunc(support, func(c Claim) bool {
		return c.Source.Kind != SourceAgent || now.Sub(c.ObservedAt) <= StaleAfter
	}):
		return StateStale, best.Confidence()
	}
	return StateDiscovered, best.Confidence()
}

func claimed(cs []Claim) []Claimed {
	out := make([]Claimed, 0, len(cs))
	for _, c := range cs {
		out = append(out, Claimed{Value: c.Value, Denied: c.Denied, Source: c.Source,
			Confidence: c.Confidence(), ObservedAt: c.ObservedAt})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func factView(r resolution) FactView {
	w := r.winner
	return FactView{Fact: w.Facet.Fact, Value: w.Value, Source: w.Source, Confidence: w.Confidence(),
		Word: w.Confidence().Display(), ObservedAt: w.ObservedAt, Standing: r.standing,
		Suggested: r.suggested, Others: claimed(r.others)}
}

func edge(r resolution) Edge {
	w := r.winner
	ed := Edge{Relation: w.Facet.Relation, External: w.Facet.Target.External, Name: w.Facet.Target.Name,
		Source: w.Source, Confidence: w.Confidence(), Word: w.Confidence().Display(), ObservedAt: w.ObservedAt,
		Standing: r.standing, Suggested: r.suggested, Others: claimed(r.others)}
	if ed.External == "" {
		ed.To = w.Facet.Target.Entry.ID()
	}
	return ed
}

// joinKeysOf is the exact keys beyond its own a live fact gives an entry: a
// public address for a server, a forge address for a repository, an account's
// own id. Only these ever merge two entries, and only from a source the engine
// trusts as confirmed: an agent's claim never merges anything.
func joinKeysOf(c Claim) []string {
	if c.Facet.Kind != FacetFact || c.Denied {
		return nil
	}
	switch c.Facet.Fact {
	case FactAddress:
		if !c.Value.Private {
			return []string{"address\x00" + c.Value.Text}
		}
	case FactPullMirrorOf, FactPushMirrorTo, FactImportedFrom:
		if key, err := RepositoryKeyOf(c.Value.Text); err == nil {
			return []string{"repository\x00" + key}
		}
	case FactAccountRef:
		vendor, _, _ := strings.Cut(c.Subject.Key, "/")
		return []string{"account\x00" + vendor + "/" + c.Value.Text}
	}
	return nil
}

// ownJoinKey is the exact key an entry's own key gives it, where another
// entry's fact can name the same thing.
func ownJoinKey(r Ref) string {
	switch r.Kind {
	case KindRepository:
		if !strings.HasPrefix(r.Key, GitletPrefix+"/") {
			return "repository\x00" + r.Key
		}
	case KindAccount:
		if strings.Contains(r.Key, "/") {
			return "account\x00" + r.Key
		}
	}
	return ""
}

// merges is which entry each entry folds into, and the members of each.
type merges struct {
	parent  map[Ref]Ref
	members map[Ref][]Ref
	byID    map[uuid.UUID]Ref
}

func (m merges) of(r Ref) Ref {
	if p, ok := m.parent[r]; ok {
		return p
	}
	return r
}

// merge decides which entries are one. An address or a forge address carried
// by two entries of ONE source is shared (a load balancer, a NAT, two copies
// of one mirror) and merges nothing; an account's own id is the account
// wherever it is named. Keys are taken in a fixed order, so a person's
// "not the same" that blocks one of two merges blocks the same one whatever
// order the claims came in.
func merge(claims []Claim) merges {
	nodes := map[Ref]bool{}
	holders := map[string][]holder{}
	keyed := map[string]map[Ref]bool{}
	note := func(key string, r Ref, source string) {
		holders[key] = append(holders[key], holder{r, source})
		if keyed[key] == nil {
			keyed[key] = map[Ref]bool{}
		}
		keyed[key][r] = true
	}
	joinFacets := map[string][]Claim{}
	distinct := map[[2]Ref][]Claim{}
	for _, c := range claims {
		nodes[c.Subject] = true
		switch {
		case c.Facet.Kind == FacetDistinct:
			pair := [2]Ref{c.Subject, c.Facet.Target.Entry}
			if compareRef(pair[1], pair[0]) < 0 {
				pair[0], pair[1] = pair[1], pair[0]
			}
			c.Subject, c.Facet.Target.Entry = pair[0], pair[1]
			distinct[pair] = append(distinct[pair], c)
		case c.Facet.Kind == FacetFact && slices.Contains([]Fact{FactAddress, FactPullMirrorOf, FactPushMirrorTo,
			FactImportedFrom, FactAccountRef}, c.Facet.Fact):
			k := c.Subject.String() + "\x00" + facetKey(c)
			joinFacets[k] = append(joinFacets[k], c)
		}
	}
	for r := range nodes {
		if key := ownJoinKey(r); key != "" {
			note(string(r.Kind)+"\x00"+key, r, "\x00own")
		}
	}
	for _, cs := range joinFacets {
		w := resolve(cs).winner
		if w.Confidence().Rank() > ConfidenceVerified.Rank() {
			continue
		}
		for _, key := range joinKeysOf(w) {
			note(string(w.Subject.Kind)+"\x00"+key, w.Subject, w.Source.identity())
		}
	}
	apart := map[[2]Ref]bool{}
	for pair, cs := range distinct {
		if w := resolve(cs).winner; !w.Denied {
			apart[pair] = true
		}
	}
	m := merges{parent: map[Ref]Ref{}, members: map[Ref][]Ref{}, byID: map[uuid.UUID]Ref{}}
	groups := map[Ref][]Ref{}
	for r := range nodes {
		groups[r] = []Ref{r}
	}
	root := func(r Ref) Ref { return m.of(r) }
	keys := make([]string, 0, len(keyed))
	for k := range keyed {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if !strings.Contains(key, "\x00account\x00") && shared(holders[key]) {
			continue
		}
		refs := make([]Ref, 0, len(keyed[key]))
		for r := range keyed[key] {
			refs = append(refs, r)
		}
		slices.SortFunc(refs, compareRef)
		for _, r := range refs[1:] {
			a, b := root(refs[0]), root(r)
			if a == b || blocked(groups[a], groups[b], apart) {
				continue
			}
			if compareRef(b, a) < 0 {
				a, b = b, a
			}
			for _, member := range groups[b] {
				m.parent[member] = a
			}
			m.parent[b] = a
			groups[a] = append(groups[a], groups[b]...)
			delete(groups, b)
		}
	}
	for r := range nodes {
		m.byID[r.ID()] = r
	}
	for top, group := range groups {
		slices.SortFunc(group, compareRef)
		m.members[top] = group
		for _, member := range group {
			if member != top {
				m.parent[member] = top
			}
		}
		m.byID[top.ID()] = top
	}
	return m
}

// holder is one entry carrying a join key, and the source that gave it.
type holder struct {
	ref    Ref
	source string
}

// shared reports whether one source carries the key on two entries, which
// makes it a shared address rather than an identity.
func shared(hs []holder) bool {
	seen := map[string]Ref{}
	for _, h := range hs {
		if prior, ok := seen[h.source]; ok && prior != h.ref {
			return true
		}
		seen[h.source] = h.ref
	}
	return false
}

func blocked(a, b []Ref, apart map[[2]Ref]bool) bool {
	for _, x := range a {
		for _, y := range b {
			pair := [2]Ref{x, y}
			if compareRef(y, x) < 0 {
				pair = [2]Ref{y, x}
			}
			if apart[pair] {
				return true
			}
		}
	}
	return false
}
