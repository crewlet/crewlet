package atlas

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/words"
)

// The bounds of a read, each a judgement tuned to a few hundred entries and
// stated where it is defined.
const (
	// PageDefault is a list's page: a screen of entries, as Worklet's and
	// Doclet's lists page.
	PageDefault = 50
	// PageMax is the most one page holds: what a person reads through before
	// narrowing by kind.
	PageMax = 500
	// MapMax is the most entries the map draws open. Past it, groups close
	// on the engine's side, largest first, so the answer stays bounded and
	// the client never draws a partial map as if it were whole.
	MapMax = 2000
	// TraceDepthMax is the deepest a trace walks: entry, service, runtime,
	// data and platform are four hops, and six leaves room for chains of
	// services without walking the whole company.
	TraceDepthMax = 6
	// TraceMax is the most entries a trace answers a person. Past it the
	// trace is most of the company, which the map draws better.
	TraceMax = 200
	// TraceToolMax is the most an agent's trace answers: fifty summaries
	// stay well inside one tool answer beside the edges between them.
	TraceToolMax = 50
)

// ErrQuery is a read whose parameters the atlas cannot answer. Its message
// names the parameter and what it takes.
var ErrQuery = errors.New("atlas: bad read")

// Reader is who reads the atlas, as far as what it shows them depends on it:
// the atlas is never a way round another screen, so what only administrators
// read elsewhere (an integration's standing, a connection test) only they
// read here.
type Reader struct {
	Admin bool
}

// For is v as r may read it: every signal r is not in the audience of left
// out. The entries themselves are every holder's.
func For(v View, r Reader) View {
	v.Entries = slices.Clone(v.Entries)
	for i := range v.Entries {
		e := &v.Entries[i]
		e.Signals = slices.DeleteFunc(slices.Clone(e.Signals), func(s Signal) bool {
			return s.Audience == AudienceAdmins && !r.Admin
		})
	}
	return v
}

// Health is the one word an entry's open signals add up to.
type Health string

// The healths, the most urgent first.
const (
	// HealthAlerting is an entry with a signal that something is broken now.
	HealthAlerting Health = "alerting"
	// HealthNeedsLook is an entry that needs a person: a warning, or a fact
	// the sources disagree on.
	HealthNeedsLook Health = "needs_look"
	// HealthQuiet is an entry with nothing open.
	HealthQuiet Health = "quiet"
)

// Healths is every health.
var Healths = []Health{HealthAlerting, HealthNeedsLook, HealthQuiet}

// Valid reports whether h is a member of the closed set.
func (h Health) Valid() bool { return slices.Contains(Healths, h) }

// Health is what e's open signals and its review add up to.
func (e Entry) Health() Health {
	switch {
	case slices.ContainsFunc(e.Signals, func(s Signal) bool { return s.Severity == SeverityDanger }):
		return HealthAlerting
	case len(e.Signals) > 0 || e.Review:
		return HealthNeedsLook
	}
	return HealthQuiet
}

// Show is which entries by their state a read shows.
type Show string

// The scopes a read shows.
const (
	// ShowCurrent is every entry but the retired: the map as it is.
	ShowCurrent Show = "current"
	// ShowRetired is the retired entries alone.
	ShowRetired Show = "retired"
	// ShowAll is every entry.
	ShowAll Show = "all"
)

// Shows is every scope.
var Shows = []Show{ShowCurrent, ShowRetired, ShowAll}

// Sort is the order a list reads in.
type Sort string

// The orders a list reads in.
const (
	// SortKey is the map's order: by lane, then kind, then key.
	SortKey Sort = "key"
	// SortName is by name, then the map's order.
	SortName Sort = "name"
	// SortLastSeen is the most recently seen first.
	SortLastSeen Sort = "last_seen"
)

// Sorts is every order.
var Sorts = []Sort{SortKey, SortName, SortLastSeen}

// Query narrows a read. Its zero value is every current entry anybody can
// vouch for: an unconfirmed one stays out until a person confirms it.
type Query struct {
	// Text matches a name or a key holding it, case folded, or an id.
	Text   string
	Kind   Kind
	Lane   Lane
	Vendor Vendor
	Word   Word
	Health Health
	Show   Show
	// Unconfirmed takes in the entries whose word is unconfirmed: an agent's
	// report nothing the engine could check backs. Asking for that word by
	// Word takes them in too.
	Unconfirmed bool
}

// Validate refuses a query naming a value outside its closed set, naming the
// parameter and the set.
func (q Query) Validate() error {
	bad := func(param string, set string) error {
		return fmt.Errorf("%w: `%s` is %s", ErrQuery, param, set)
	}
	switch {
	case q.Kind != "" && !q.Kind.Valid():
		return bad("kind", words.Or(Kinds))
	case q.Lane != "" && !q.Lane.Valid():
		return bad("lane", words.Or(Lanes))
	case q.Vendor != "" && !q.Vendor.Valid():
		return bad("vendor", words.Or(Vendors))
	case q.Word != "" && !slices.Contains(Words, q.Word):
		return bad("confidence", words.Or(Words))
	case q.Health != "" && !q.Health.Valid():
		return bad("health", words.Or(Healths))
	case q.Show != "" && !slices.Contains(Shows, q.Show):
		return bad("show", words.Or(Shows))
	}
	return nil
}

// facet names one dimension of a query, so facets count each over every
// other dimension's filter.
type facet int

const (
	facetNone facet = iota
	facetKind
	facetLane
	facetVendor
	facetWord
	facetHealth
)

// ShowsUnconfirmed reports whether q takes in the entries whose word is
// unconfirmed.
func (q Query) ShowsUnconfirmed() bool { return q.Unconfirmed || q.Word == WordUnconfirmed }

// matches reports whether e answers q, every dimension but skip.
func (q Query) matches(e *Entry, vendor Vendor, skip facet) bool {
	switch q.Show {
	case ShowRetired:
		if e.State != StateRetired {
			return false
		}
	case ShowAll:
	default:
		if e.State == StateRetired {
			return false
		}
	}
	// An unconfirmed entry is left out on the word's own dimension, so the
	// word's facet still counts what showing them would add.
	hidden := !q.ShowsUnconfirmed() && e.Word == WordUnconfirmed
	switch {
	case skip != facetKind && q.Kind != "" && e.Ref.Kind != q.Kind,
		skip != facetLane && q.Lane != "" && e.Lane != q.Lane,
		skip != facetVendor && q.Vendor != "" && vendor != q.Vendor,
		skip != facetWord && (hidden || q.Word != "" && e.Word != q.Word),
		skip != facetHealth && q.Health != "" && e.Health() != q.Health:
		return false
	}
	text := strings.ToLower(strings.TrimSpace(q.Text))
	return text == "" || e.ID.String() == text || strings.Contains(strings.ToLower(e.Name), text) ||
		strings.Contains(strings.ToLower(e.Ref.Key), text)
}

// Vendors is the vendor of every entry of v by its id: its own vendor fact,
// or the vendor of the account it belongs to, or of the account it is; ""
// where none is known.
func (v View) Vendors() map[uuid.UUID]Vendor {
	out := make(map[uuid.UUID]Vendor, len(v.Entries))
	own := func(e *Entry) Vendor {
		for _, f := range e.Facts {
			if f.Fact == FactVendor {
				return Vendor(f.Value.Text)
			}
		}
		if e.Ref.Kind == KindAccount {
			vendor, _, _ := strings.Cut(e.Ref.Key, "/")
			return Vendor(vendor)
		}
		return ""
	}
	for i := range v.Entries {
		out[v.Entries[i].ID] = own(&v.Entries[i])
	}
	for i := range v.Entries {
		e := &v.Entries[i]
		if out[e.ID] != "" {
			continue
		}
		for _, ed := range e.Relations {
			if ed.Relation == RelationBelongsTo && out[ed.To] != "" {
				out[e.ID] = out[ed.To]
				break
			}
		}
	}
	return out
}

// Select is the entries of v that answer q, in the view's order.
func (v View) Select(q Query) []Entry {
	vendors := v.Vendors()
	out := make([]Entry, 0, len(v.Entries))
	for i := range v.Entries {
		if e := &v.Entries[i]; q.matches(e, vendors[e.ID], facetNone) {
			out = append(out, *e)
		}
	}
	return out
}

// Owner is who answers for an entry: a seat by its handle or a unit by its
// key, the chart's own identities and never free text.
type Owner struct {
	Kind External `json:"kind"`
	Name string   `json:"name"`
}

// RelationCount is how many current entries an entry relies on and how many
// rely on it, through the relations the map draws at rest.
type RelationCount struct {
	DependsOn int `json:"depends_on"`
	UsedBy    int `json:"used_by"`
}

// Summary is one entry as a list row or a map card shows it: no facts, no
// relations, what it is, who answers for it and how it stands.
type Summary struct {
	ID     uuid.UUID `json:"id"`
	Kind   Kind      `json:"kind"`
	Lane   Lane      `json:"lane"`
	Name   string    `json:"name" untrusted:"name"`
	Key    string    `json:"key"`
	Vendor Vendor    `json:"vendor,omitempty"`
	// Owner is the seat or the unit a person or the engine's records name,
	// never one only suggested; Purpose the one line a person gave it.
	Owner     *Owner        `json:"owner,omitempty"`
	Purpose   string        `json:"purpose,omitempty" untrusted:"name"`
	Relations RelationCount `json:"relations"`
	State     State         `json:"state"`
	Word      Word          `json:"word"`
	Health    Health        `json:"health"`
	// Reason is why the entry is not quiet, in one sentence of the engine's
	// own words ([Entry.Reason]); empty for a quiet one.
	Reason          string    `json:"reason,omitempty"`
	Signals         int       `json:"signals"`
	Review          bool      `json:"review"`
	NotUsedRecently bool      `json:"not_used_recently"`
	LastSeen        time.Time `json:"last_seen"`
}

// Summaries is what summarising the entries of one view shares: each entry's
// vendor, and how many entries rely on it.
type Summaries struct {
	vendors map[uuid.UUID]Vendor
	usedBy  map[uuid.UUID]int
	counted map[uuid.UUID]bool
}

// Summaries prepares the summaries of v's entries. Relations are counted
// between current entries, an unconfirmed one only where unconfirmed says the
// read shows them, so a count never names an entry the read leaves out.
func (v View) Summaries(unconfirmed bool) Summaries {
	s := Summaries{vendors: v.Vendors(), usedBy: map[uuid.UUID]int{}, counted: make(map[uuid.UUID]bool, len(v.Entries))}
	for i := range v.Entries {
		e := &v.Entries[i]
		s.counted[e.ID] = e.State != StateRetired && (unconfirmed || e.Word != WordUnconfirmed)
	}
	for i := range v.Entries {
		e := &v.Entries[i]
		if !s.counted[e.ID] {
			continue
		}
		for _, ed := range e.Relations {
			if ed.Relation.Structural() && s.counted[ed.To] {
				s.usedBy[ed.To]++
			}
		}
	}
	return s
}

// Vendor is the vendor of the entry id, "" where none is known.
func (s Summaries) Vendor(id uuid.UUID) Vendor { return s.vendors[id] }

// Of is e as a row shows it.
func (s Summaries) Of(e Entry) Summary {
	out := Summary{ID: e.ID, Kind: e.Ref.Kind, Lane: e.Lane, Name: e.Name, Key: e.Ref.Key, Vendor: s.vendors[e.ID],
		State: e.State, Word: e.Word, Health: e.Health(), Reason: e.Reason(), Signals: len(e.Signals),
		Review: e.Review, NotUsedRecently: e.NotUsedRecently, LastSeen: e.LastSeen}
	out.Relations.UsedBy = s.usedBy[e.ID]
	for _, ed := range e.Relations {
		switch {
		case ed.Relation.Structural() && s.counted[ed.To]:
			out.Relations.DependsOn++
		case ed.Relation == RelationOwnedBy && out.Owner == nil && !ed.Suggested && ed.External != "":
			out.Owner = &Owner{Kind: ed.External, Name: ed.Name}
		}
	}
	for _, f := range e.Facts {
		if f.Fact == FactPurpose && !f.Suggested {
			out.Purpose = f.Value.Text
			break
		}
	}
	return out
}

// Page is one page of a list.
type Page struct {
	Rows  []Summary
	Next  string
	Total int
}

// cursor is where a page ended: the order it was read in, and its last row's
// sort value and identity, so the next page starts after it even where rows
// came or went between the two reads.
type cursor struct {
	Sort Sort   `json:"s"`
	Name string `json:"n,omitempty"`
	Seen int64  `json:"t,omitempty"`
	Kind Kind   `json:"k"`
	Key  string `json:"key"`
}

// rowOrder is how sort compares two rows; every order ends on the map's.
func rowOrder(sort Sort) func(a, b Summary) int {
	byKey := func(a, b Summary) int {
		return cmp.Or(cmp.Compare(a.Kind.rank(), b.Kind.rank()), cmp.Compare(a.Key, b.Key))
	}
	switch sort {
	case SortName:
		return func(a, b Summary) int {
			return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), byKey(a, b))
		}
	case SortLastSeen:
		return func(a, b Summary) int { return cmp.Or(b.LastSeen.Compare(a.LastSeen), byKey(a, b)) }
	}
	return byKey
}

// List is entries in sort's order, one page of at most limit rows after
// the cursor a previous page answered ("" for the first).
func List(entries []Entry, summaries Summaries, sort Sort, after string, limit int) (Page, error) {
	if sort == "" {
		sort = SortKey
	}
	if !slices.Contains(Sorts, sort) {
		return Page{}, fmt.Errorf("%w: `sort` is %s", ErrQuery, words.Or(Sorts))
	}
	if limit < 1 || limit > PageMax {
		return Page{}, fmt.Errorf("%w: `limit` is 1 to %d", ErrQuery, PageMax)
	}
	rows := make([]Summary, len(entries))
	for i, e := range entries {
		rows[i] = summaries.Of(e)
	}
	compare := rowOrder(sort)
	slices.SortFunc(rows, compare)
	start := 0
	if after != "" {
		last, err := decodeCursor(after, sort)
		if err != nil {
			return Page{}, err
		}
		pivot := Summary{Name: last.Name, LastSeen: time.Unix(0, last.Seen).UTC(), Kind: last.Kind, Key: last.Key}
		start, _ = slices.BinarySearchFunc(rows, pivot, compare)
		if start < len(rows) && compare(rows[start], pivot) == 0 {
			start++
		}
	}
	end := min(len(rows), start+limit)
	page := Page{Rows: rows[start:end], Total: len(rows)}
	if end < len(rows) {
		last := rows[end-1]
		page.Next = encodeCursor(cursor{Sort: sort, Name: last.Name, Seen: last.LastSeen.UnixNano(),
			Kind: last.Kind, Key: last.Key})
	}
	return page, nil
}

func encodeCursor(c cursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string, sort Sort) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	var c cursor
	if err == nil {
		err = json.Unmarshal(raw, &c)
	}
	if err != nil || c.Sort != sort || !c.Kind.Valid() {
		return cursor{}, fmt.Errorf("%w: `cursor` is the `next_cursor` a page of this order answered", ErrQuery)
	}
	return c, nil
}

// Facets is how many entries each value of each dimension holds, each counted
// over every other dimension's filter, so a menu says what picking it leaves.
type Facets struct {
	Kinds   map[Kind]int   `json:"kinds"`
	Lanes   map[Lane]int   `json:"lanes"`
	Vendors map[Vendor]int `json:"vendors"`
	Words   map[Word]int   `json:"words"`
	Health  map[Health]int `json:"health"`
}

// Facets counts v's entries for q.
func (v View) Facets(q Query) Facets {
	vendors := v.Vendors()
	out := Facets{Kinds: map[Kind]int{}, Lanes: map[Lane]int{}, Vendors: map[Vendor]int{}, Words: map[Word]int{},
		Health: map[Health]int{}}
	for i := range v.Entries {
		e := &v.Entries[i]
		vendor := vendors[e.ID]
		if q.matches(e, vendor, facetKind) {
			out.Kinds[e.Ref.Kind]++
		}
		if q.matches(e, vendor, facetLane) {
			out.Lanes[e.Lane]++
		}
		if vendor != "" && q.matches(e, vendor, facetVendor) {
			out.Vendors[vendor]++
		}
		if q.matches(e, vendor, facetWord) {
			out.Words[e.Word]++
		}
		if q.matches(e, vendor, facetHealth) {
			out.Health[e.Health()]++
		}
	}
	return out
}

// MapLane is one lane of the map, drawn whether or not it holds anything.
type MapLane struct {
	Lane  Lane `json:"lane"`
	Band  bool `json:"band"`
	Count int  `json:"count"`
}

// MapGroup is the entries of one vendor in one lane: room the map keeps,
// open or closed.
type MapGroup struct {
	Key    string `json:"key"`
	Lane   Lane   `json:"lane"`
	Vendor Vendor `json:"vendor,omitempty"`
	Count  int    `json:"count"`
	Closed bool   `json:"closed"`
}

// MapEdge is one relation between two entries a map or a trace draws, and
// the family it is drawn in.
type MapEdge struct {
	From     uuid.UUID `json:"from"`
	To       uuid.UUID `json:"to"`
	Relation Relation  `json:"relation"`
	Family   Family    `json:"family"`
}

// edgeOf is the relation r from one entry to another as a map draws it.
func edgeOf(from, to uuid.UUID, r Relation) MapEdge {
	return MapEdge{From: from, To: to, Relation: r, Family: r.Family()}
}

// Map is the atlas drawn as its lanes: every lane, its groups, the entries
// of the open groups and the relations between them.
type Map struct {
	Lanes  []MapLane  `json:"lanes"`
	Groups []MapGroup `json:"groups"`
	Nodes  []Summary  `json:"nodes"`
	Edges  []MapEdge  `json:"edges"`
	// Collapsed is the groups the engine closed past [MapMax], beyond those
	// the reader closed.
	Collapsed []string `json:"collapsed"`
}

// GroupKey is the key of the group an entry of vendor stands in, in lane.
func GroupKey(lane Lane, vendor Vendor) string {
	if vendor == "" {
		return string(lane)
	}
	return string(lane) + "/" + string(vendor)
}

// Map draws the entries of v that answer q, leaving the groups closed names
// closed, and closing more, largest first, until at most [MapMax] are open.
// Its edges are the relations the map draws at rest or, with all, every
// relation between two drawn entries.
func (v View) Map(q Query, closed []string, all bool) Map {
	summaries := v.Summaries(q.ShowsUnconfirmed())
	vendors := summaries.vendors
	entries := v.Select(q)
	out := Map{Lanes: make([]MapLane, len(Lanes)), Groups: []MapGroup{}, Nodes: []Summary{}, Edges: []MapEdge{},
		Collapsed: []string{}}
	for i, l := range Lanes {
		out.Lanes[i] = MapLane{Lane: l, Band: l.Band()}
	}
	groups := map[string]*MapGroup{}
	for _, e := range entries {
		out.Lanes[slices.Index(Lanes, e.Lane)].Count++
		key := GroupKey(e.Lane, vendors[e.ID])
		g, ok := groups[key]
		if !ok {
			g = &MapGroup{Key: key, Lane: e.Lane, Vendor: vendors[e.ID], Closed: slices.Contains(closed, key)}
			groups[key] = g
		}
		g.Count++
	}
	open := 0
	for _, g := range groups {
		if !g.Closed {
			open += g.Count
		}
	}
	bySize := make([]*MapGroup, 0, len(groups))
	for _, g := range groups {
		bySize = append(bySize, g)
	}
	slices.SortFunc(bySize, func(a, b *MapGroup) int { return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Key, b.Key)) })
	for _, g := range bySize {
		if open <= MapMax {
			break
		}
		if !g.Closed {
			g.Closed, open = true, open-g.Count
			out.Collapsed = append(out.Collapsed, g.Key)
		}
	}
	slices.Sort(out.Collapsed)
	drawn := map[uuid.UUID]bool{}
	for _, e := range entries {
		if groups[GroupKey(e.Lane, vendors[e.ID])].Closed {
			continue
		}
		drawn[e.ID] = true
		out.Nodes = append(out.Nodes, summaries.Of(e))
	}
	for _, e := range entries {
		if !drawn[e.ID] {
			continue
		}
		for _, ed := range e.Relations {
			if (all || ed.Relation.Structural()) && ed.External == "" && drawn[ed.To] {
				out.Edges = append(out.Edges, edgeOf(e.ID, ed.To, ed.Relation))
			}
		}
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, *g)
	}
	slices.SortFunc(out.Groups, func(a, b MapGroup) int {
		return cmp.Or(cmp.Compare(slices.Index(Lanes, a.Lane), slices.Index(Lanes, b.Lane)), cmp.Compare(a.Key, b.Key))
	})
	return out
}

// Link is one relation into or out of an entry, with the entry at its other
// end as a row shows it.
type Link struct {
	Relation Relation `json:"relation"`
	Entry    Summary  `json:"entry"`
}

// UsedBy is every current entry of v with a relation to the entry id, the
// relation named: what relies on it.
func (v View) UsedBy(id uuid.UUID) []Link {
	summaries := v.Summaries(true)
	out := []Link{}
	for _, e := range v.Entries {
		if e.State == StateRetired {
			continue
		}
		for _, ed := range e.Relations {
			if ed.To == id {
				out = append(out, Link{Relation: ed.Relation, Entry: summaries.Of(e)})
			}
		}
	}
	return out
}

// Direction is which way a trace walks.
type Direction string

// The directions a trace walks.
const (
	// DirectionUp is what depends on the entry: against the arrows.
	DirectionUp Direction = "up"
	// DirectionDown is what the entry depends on: along the arrows.
	DirectionDown Direction = "down"
)

// Directions is every direction.
var Directions = []Direction{DirectionUp, DirectionDown}

// TraceHop is the entries a trace reached at one distance.
type TraceHop struct {
	Distance int       `json:"distance"`
	Entries  []Summary `json:"entries"`
}

// Trace is what depends on an entry, or what it depends on, hop by hop.
type Trace struct {
	From      Summary    `json:"from"`
	Direction Direction  `json:"direction"`
	Hops      []TraceHop `json:"hops"`
	Edges     []MapEdge  `json:"edges"`
	// Truncated is a trace that reached its bound of entries before it
	// walked every hop it was asked for.
	Truncated bool `json:"truncated"`
}

// Trace walks from the entry id in dir, at most depth hops and limit
// entries, through the structural relations or, with all, every relation
// between entries. It reports false for an id that names no entry of v.
func (v View) Trace(id uuid.UUID, dir Direction, depth int, all bool, limit int) (Trace, bool, error) {
	if !slices.Contains(Directions, dir) {
		return Trace{}, false, fmt.Errorf("%w: `direction` is %s", ErrQuery, words.Or(Directions))
	}
	if depth < 1 || depth > TraceDepthMax {
		return Trace{}, false, fmt.Errorf("%w: `depth` is 1 to %d", ErrQuery, TraceDepthMax)
	}
	index := v.Index()
	at, ok := index[id]
	if !ok {
		return Trace{}, false, nil
	}
	summaries := v.Summaries(true)
	from := v.Entries[at]
	next := map[uuid.UUID][]MapEdge{}
	for _, e := range v.Entries {
		if e.State == StateRetired {
			continue
		}
		for _, ed := range e.Relations {
			if ed.External != "" || (!all && !ed.Relation.Structural()) {
				continue
			}
			edge := edgeOf(e.ID, ed.To, ed.Relation)
			if dir == DirectionDown {
				next[e.ID] = append(next[e.ID], edge)
			} else {
				next[ed.To] = append(next[ed.To], edge)
			}
		}
	}
	out := Trace{From: summaries.Of(from), Direction: dir, Hops: []TraceHop{}, Edges: []MapEdge{}}
	seen := map[uuid.UUID]bool{from.ID: true}
	frontier, reached := []uuid.UUID{from.ID}, 0
	for distance := 1; distance <= depth && len(frontier) > 0 && !out.Truncated; distance++ {
		hop := TraceHop{Distance: distance}
		var following []uuid.UUID
		for _, id := range frontier {
			for _, edge := range next[id] {
				other := edge.To
				if dir == DirectionUp {
					other = edge.From
				}
				i, ok := index[other]
				if !ok || v.Entries[i].State == StateRetired {
					continue
				}
				if !seen[other] {
					if reached == limit {
						out.Truncated = true
						break
					}
					seen[other], reached = true, reached+1
					following = append(following, other)
					hop.Entries = append(hop.Entries, summaries.Of(v.Entries[i]))
				}
				out.Edges = append(out.Edges, edge)
			}
		}
		if len(hop.Entries) > 0 {
			slices.SortFunc(hop.Entries, rowOrder(SortKey))
			out.Hops = append(out.Hops, hop)
		}
		frontier = following
	}
	return out, true, nil
}
