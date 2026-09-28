package statelog

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The vocabulary of a partitioned estate: a SPACE of numbered PARTITIONS, a
// LOG per domain in each, and the LAYOUT that says how many of each there
// are.
//
// # Why a partition is a value the framework carries
//
// The unit a data node holds, adopts, snapshots, joins and leaves is a
// partition, and a partition carries one log per domain it holds — the
// tracker's mutations and their vectors, say. Every piece of per-stream
// machinery this package already has (the checkpoint row, the generation,
// the anchors, the trim floor, the register entry) is therefore instantiated
// per LOG, and a domain stays ONE declaration.
//
// The alternative — a Domain value per partition, named `tracker.007` — would
// reuse every per-domain map unchanged, and is wrong three ways. [Domain.Name]
// is durable in three places that know nothing of each other and is never
// renamed, so it would make the domain's identity a function of the layout. A
// hundred-odd Domain values would each carry a copy of the table classes. And
// the one question a domain must answer about partitions — which one a record
// belongs to — would have nowhere to live, because an instance would only
// know itself.
//
// # Layout 0 is today's estate, and a real layout
//
// Layout 0 is one space, [SpaceEstate], with one partition, `estate.000`,
// carrying all three domains under the stream names and the register keys
// they have always had. It is a real layout rather than a special case, so
// everything written against layouts can land and run under it with the
// coordination records a running fleet already holds, byte for byte — which
// is what [LogID.String] and [Layout.Stream] each preserve. Nothing runs any
// other layout yet.
//
// # Validated where one is made
//
// A layout is checked by [Layout.Validate] once, where it is built or decoded,
// and its other methods answer for a valid layout — the idiom
// [Position.Valid] follows for the same reason: a check on every lookup puts
// an error on every caller for a fault that has one source.

// Space is a family of partitions that share one partition function and one
// file schema.
type Space string

const (
	// SpaceEstate is layout 0's only space: today's whole estate, as one
	// partition. No other layout may carry it — see [Layout.Validate].
	SpaceEstate Space = "estate"

	// SpaceTracker holds projects and everything homed in one, and people.
	SpaceTracker Space = "tracker"

	// SpacePages holds the knowledge base's containers and their pages.
	SpacePages Space = "pages"

	// SpaceCompany holds the tracker's company-wide objects: the
	// catalogues every tracker partition must see in order, and the
	// workspace's own views.
	SpaceCompany Space = "company"
)

// Spaces is every space, for the enum's own validation and for a test that
// walks them all.
var Spaces = []Space{SpaceEstate, SpaceTracker, SpacePages, SpaceCompany}

// Valid reports whether a space off the wire or out of a record is one this
// build knows.
func (s Space) Valid() bool { return slices.Contains(Spaces, s) }

// MaxPartitions is the most partitions one space may have: every index is
// written in three digits in every name the grammar produces (topics), and a
// thousand is what three digits hold.
const MaxPartitions = topics.MaxPartitionIndex + 1

// ErrInvalidPartitionID reports a string that is not a partition's name.
var ErrInvalidPartitionID = errors.New("not a partition id")

// ErrInvalidLayout reports a layout that [Layout.Validate] refuses.
var ErrInvalidLayout = errors.New("invalid layout")

// EstatePartition is layout 0's one partition, `estate.000`: today's whole
// estate, whose logs are keyed by their domain alone ([LogID.String]).
var EstatePartition = PartitionID{Space: SpaceEstate}

// PartitionID names one partition: a space and an index within it.
//
// THE ZERO VALUE IS NOT A PARTITION. Its space is empty, which no layout
// carries, so it is refused wherever a partition enters — [ParsePartitionID]
// never yields it, [PartitionID.Valid] reports false and it renders as the
// empty string — rather than meaning `estate.000`. Read as layout 0's whole
// estate, a field somebody forgot to fill would silently address every row
// the node holds.
type PartitionID struct {
	Space Space
	Index uint16
}

// Valid reports whether p is a partition some valid layout could carry: a
// known space, an index the names can hold, and — for [SpaceEstate] — the
// index 0, because the estate space is layout 0's and has one partition.
//
// The last clause is what keeps [LogID.String] injective: a log of
// `estate.000` is keyed by its domain alone, and no other partition may ever
// be read as that one.
func (p PartitionID) Valid() bool {
	if !p.Space.Valid() || int(p.Index) >= MaxPartitions {
		return false
	}
	return p.Space != SpaceEstate || p.Index == 0
}

// String is the partition's name — its space, a dot and the index in three
// digits, `tracker.007` — or the empty string for one the grammar cannot
// name, the zero PartitionID among them.
//
// The spelling is [topics.PartitionName]'s, because the name is two tokens of
// every subject the partition's logs carry: a second spelling here would be a
// second place to change it.
func (p PartitionID) String() string {
	return topics.PartitionName(string(p.Space), int(p.Index))
}

// ParsePartitionID reads a partition's name back, and refuses anything
// [PartitionID.String] could not have produced for a valid partition.
//
// Exact rather than lenient: `tracker.7`, `tracker.0007` and `Tracker.007`
// are refused, because every place that keys on a partition's name — a
// register row, the estate map, a lease — would otherwise hold one partition
// under several keys.
func ParsePartitionID(s string) (PartitionID, error) {
	space, index, ok := topics.ParsePartitionName(s)
	if !ok {
		return PartitionID{}, fmt.Errorf("%w: %q is not a space and a "+
			"three-digit index, like tracker.007", ErrInvalidPartitionID, s)
	}
	p := PartitionID{Space: Space(space), Index: uint16(index)}
	if !p.Space.Valid() {
		return PartitionID{}, fmt.Errorf("%w: %q names the space %q, which this "+
			"build does not know; the spaces are %v", ErrInvalidPartitionID, s, space, Spaces)
	}
	if !p.Valid() {
		return PartitionID{}, fmt.Errorf("%w: %q: the %s space is layout 0's "+
			"and has exactly one partition, %s", ErrInvalidPartitionID, s,
			SpaceEstate, PartitionID{Space: SpaceEstate})
	}
	return p, nil
}

// LogID names one log: one domain's records in one partition, on one stream.
type LogID struct {
	// Domain is the domain's [Domain.Name] — tracker, vectors or pages.
	Domain string

	// Partition is the partition the log belongs to.
	Partition PartitionID
}

// String is the log's KEY — in the positions register, a snapshot's manifest
// and a session's floors. A log of `estate.000` is keyed by its domain alone
// (`tracker`), which is exactly the key each domain has today, so no build
// that runs layout 0 changes a coordination record; any other log is its
// domain, `@` and its partition (`tracker@tracker.007`).
//
// THE KEY DOES NOT CARRY THE LAYOUT NUMBER: layout 1's `tracker@tracker.007`
// and a repartitioned layout 2's are the same string. The STREAM does carry
// it ([Layout.Stream]), so anything that must never compare two layouts keys
// on the stream, as [Cut] does; anything keyed by this string records the
// layout beside it.
func (l LogID) String() string {
	if l.Partition == (PartitionID{Space: SpaceEstate}) {
		return l.Domain
	}
	return l.Domain + "@" + l.Partition.String()
}

// SpaceLayout is one space of a layout: how many partitions it has, and which
// domains have a log in every one of them.
type SpaceLayout struct {
	// Space is the space.
	Space Space `json:"space"`

	// Partitions is how many partitions the space has, 1…MaxPartitions.
	// FIXED FOR THE LIFE OF THE LAYOUT: a partition is a hash of an
	// object's key MODULO this count, so changing it re-homes nearly every
	// object and is a new layout, never an edit to this one.
	Partitions int `json:"partitions"`

	// Domains are the domains with a log in EVERY partition of the space,
	// in apply order — the order a partition's file composes their schemas
	// in.
	Domains []string `json:"domains"`
}

// Layout is how the replicated estate is divided: its spaces, their partition
// counts and the domains with a log in each.
//
// NUMBERED, and every partitioned layout's number is in each of its stream
// names, so a repartition to the next layout creates a disjoint set of
// streams and files rather than reusing a name for a second history. The layout in force is RECORDED rather
// than assumed from a build's defaults, so a node whose defaults differ still
// runs the recorded one.
type Layout struct {
	// Number is 0 for today's single estate and counts repartitions from
	// there: 1 is the first partitioned layout.
	Number int `json:"number"`

	// Spaces are the layout's spaces.
	Spaces []SpaceLayout `json:"spaces"`
}

// Validate refuses a layout that cannot be run, naming what to change.
//
// THE ZERO LAYOUT IS REFUSED, and is deliberately not layout 0: a recorded
// layout that decoded to nothing — a missing field — must not read as the
// whole estate in one file. Layout 0 is exactly one space, [SpaceEstate], with
// one partition, and only layout 0 may carry that space, because a log of
// `estate.000` is keyed by its domain alone and must be the only log that is.
// Every log must also be one the stream grammar can name: a lowercase word
// for each domain, and under layout 0 only the three logs it has always had.
func (l Layout) Validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: layout %d: %s", ErrInvalidLayout, l.Number, fmt.Sprintf(format, args...))
	}
	if l.Number < 0 {
		return fail("the number is negative; a layout number counts repartitions from 0")
	}
	if len(l.Spaces) == 0 {
		return fail("it has no spaces; layout 0 is the %s space with one partition", SpaceEstate)
	}
	seen := map[Space]bool{}
	for _, s := range l.Spaces {
		if !s.Space.Valid() {
			return fail("the space %q is not one this build knows; the spaces are %v", s.Space, Spaces)
		}
		if seen[s.Space] {
			return fail("the space %s is listed twice", s.Space)
		}
		seen[s.Space] = true
		if s.Partitions < 1 || s.Partitions > MaxPartitions {
			return fail("the space %s has %d partitions; a space has 1 to %d",
				s.Space, s.Partitions, MaxPartitions)
		}
		if len(s.Domains) == 0 {
			return fail("the space %s carries no domain, so its partitions would hold no log", s.Space)
		}
		domains := map[string]bool{}
		for _, d := range s.Domains {
			if domains[d] {
				return fail("the space %s lists the domain %q twice", s.Space, d)
			}
			domains[d] = true
		}
	}
	if l.Number == 0 {
		if len(l.Spaces) != 1 || l.Spaces[0].Space != SpaceEstate || l.Spaces[0].Partitions != 1 {
			return fail("layout 0 is today's estate: exactly one space, %s, with one partition",
				SpaceEstate)
		}
	} else if seen[SpaceEstate] {
		return fail("the %s space is layout 0's alone; a partitioned layout's logs are "+
			"keyed by their partition, and %s's are keyed by their domain alone",
			SpaceEstate, PartitionID{Space: SpaceEstate})
	}
	for _, s := range l.Spaces {
		for index := range s.Partitions {
			for _, d := range s.Domains {
				log := LogID{Domain: d, Partition: PartitionID{Space: s.Space, Index: uint16(index)}}
				if name, _ := l.Stream(log); name == "" {
					return fail("the stream grammar cannot name the %s log of %s: a domain is a "+
						"lowercase word (a letter, then letters or digits), and layout 0 has only "+
						"the tracker, vectors and pages logs", d, log.Partition)
				}
			}
		}
	}
	return nil
}

// Partitions is every partition of the layout, sorted by space and then
// index — which is also the order of their names: an index is always three
// digits, and a space is a lowercase word, every character of which sorts
// after the dot that ends it.
func (l Layout) Partitions() []PartitionID {
	var out []PartitionID
	for _, s := range l.Spaces {
		for index := range s.Partitions {
			out = append(out, PartitionID{Space: s.Space, Index: uint16(index)})
		}
	}
	slices.SortFunc(out, func(a, b PartitionID) int {
		return cmp.Or(cmp.Compare(a.Space, b.Space), cmp.Compare(a.Index, b.Index))
	})
	return out
}

// Logs is p's logs in its space's domain order, or nil for a partition the
// layout does not have.
func (l Layout) Logs(p PartitionID) []LogID {
	s, ok := l.spaceOf(p)
	if !ok {
		return nil
	}
	out := make([]LogID, 0, len(s.Domains))
	for _, d := range s.Domains {
		out = append(out, LogID{Domain: d, Partition: p})
	}
	return out
}

// Count is how many partitions the space has in this layout, or 0 for a
// space it does not carry.
func (l Layout) Count(s Space) int {
	for _, sl := range l.Spaces {
		if sl.Space == s {
			return sl.Partitions
		}
	}
	return 0
}

// Stream is the log's stream and the prefix its subjects are appended to,
// from the grammar in [topics.PartitionLogStream] — under layout 0 the names
// the three logs have always had — or two empty strings for a log this layout
// does not carry, which callers treat as "no such stream".
func (l Layout) Stream(log LogID) (name, prefix string) {
	s, ok := l.spaceOf(log.Partition)
	if !ok || !slices.Contains(s.Domains, log.Domain) {
		return "", ""
	}
	space, index := string(log.Partition.Space), int(log.Partition.Index)
	return topics.PartitionLogStream(l.Number, space, index, log.Domain),
		topics.PartitionLogPrefix(l.Number, space, index, log.Domain)
}

// AllLogs is every log of the layout: each partition's, in [Layout.Partitions]
// order, and each partition's in its space's domain order — the order every
// surface that walks a node's logs renders them in, so no screen's rows move
// between two refreshes.
func (l Layout) AllLogs() []LogID {
	var out []LogID
	for _, p := range l.Partitions() {
		out = append(out, l.Logs(p)...)
	}
	return out
}

// LogsOf is every log the named domain has in this layout, in
// [Layout.AllLogs]' order, or nil for a domain no space carries.
func (l Layout) LogsOf(domain string) []LogID {
	var out []LogID
	for _, log := range l.AllLogs() {
		if log.Domain == domain {
			out = append(out, log)
		}
	}
	return out
}

// LogShare is one log's byte ceiling out of its domain's whole budget: the
// budget divided EVENLY across every log the domain has in this layout, rounded
// up — or 0 for a domain the layout does not carry.
//
// # Even, because a ceiling is a reservation
//
// The broker reserves a stream's ceiling in full when it creates it and refuses
// a create it cannot back. A share that was a multiple of the even one "for
// skew" would reserve that multiple of the domain's budget on every broker
// member holding the logs; a partition that runs hot is resized alone, through
// the capacity operation every stream already has.
//
// # Across the domain's logs, not a space's partitions
//
// A domain can have logs in more than one space — the tracker's in every
// tracker partition and in the company partition, the vectors' in the tracker
// and the pages spaces. Dividing by the partitions of each space would give the
// company partition's one log the tracker's WHOLE budget, and the pages space's
// vector logs the vectors' whole budget again, so the logs together would
// reserve twice what the domain was sized for. Dividing by the domain's logs is
// what keeps their sum within the budget plus the rounding, which is under one
// byte a log. Under layout 0 every domain has one log, and its share is its
// budget.
func (l Layout) LogShare(domain string, budget int64) int64 {
	logs := int64(len(l.LogsOf(domain)))
	if logs == 0 || budget <= 0 {
		return 0
	}
	return (budget + logs - 1) / logs
}

// StreamSpec is domain d's shape instantiated on one of its logs: the names the
// grammar gives that log ([Layout.Stream]) and its [Layout.LogShare] of the
// domain's budget — or the zero StreamSpec, which [StreamSpec.Validate]
// refuses, for a log this layout does not carry or one of another domain.
func (l Layout) StreamSpec(d Domain, log LogID) StreamSpec {
	if d == nil || d.Name() != log.Domain {
		return StreamSpec{}
	}
	name, prefix := l.Stream(log)
	if name == "" {
		return StreamSpec{}
	}
	shape := d.StreamShape()
	shape.MaxBytes = l.LogShare(log.Domain, shape.MaxBytes)
	// A COPY, because the spec outlives the call and a domain that
	// returned one slice to every caller would share its backing array
	// with every log's spec.
	shape.ArbitratedKinds = slices.Clone(shape.ArbitratedKinds)
	space, index := string(log.Partition.Space), int(log.Partition.Index)
	return StreamSpec{
		Name:          name,
		Subjects:      []string{topics.PartitionLogWildcard(l.Number, space, index, log.Domain)},
		SubjectPrefix: prefix,
		StreamShape:   shape,
	}
}

// EstateLayout is layout 0 carrying exactly the named domains: one space,
// [SpaceEstate], with one partition whose logs are those domains', in the
// order given. The engine's own layout 0 is this over every domain it
// registers; a domain's suite is this over the one domain it certifies.
func EstateLayout(domains ...string) Layout {
	return Layout{Number: 0, Spaces: []SpaceLayout{
		{Space: SpaceEstate, Partitions: 1, Domains: slices.Clone(domains)},
	}}
}

// EstateStream is domain d's log in layout 0 — the one stream the domain has
// always had, under its whole budget — for the code that addresses layout 0's
// log directly rather than through a running layout: a domain's own reads of
// the rows its one log keyed, and the suites that stand one up.
//
// It names d's log exactly as the engine's layout 0 does: the name is the
// grammar's for that domain in `estate.000`, and every domain has one log
// there, so its share is its whole budget however many domains the layout
// carries beside it.
func EstateStream(d Domain) StreamSpec {
	return EstateLayout(d.Name()).StreamSpec(d, LogID{Domain: d.Name(), Partition: EstatePartition})
}

// OnlyPartition is the one partition that carries the named domain's log, when
// exactly one does, and otherwise the zero PartitionID — a partition no layout
// carries.
//
// It is the whole partition function of a domain whose records are not keyed
// to partitions: under a layout that gives the domain a single log there is
// one place a record of it can belong, and under one that divides the domain
// there is no answer such a domain can give. The zero value is that answer —
// "another partition" for every log there is — so a domain answering it from
// [Domain.PartitionOf] or [Domain.ScopePartition] keeps a record off every
// log rather than being placed on one by a guess.
func (l Layout) OnlyPartition(domain string) PartitionID {
	logs := l.LogsOf(domain)
	if len(logs) != 1 {
		return PartitionID{}
	}
	return logs[0].Partition
}

// spaceOf is p's space in this layout, and whether the layout has p at all.
func (l Layout) spaceOf(p PartitionID) (SpaceLayout, bool) {
	for _, s := range l.Spaces {
		if s.Space == p.Space {
			return s, int(p.Index) < s.Partitions
		}
	}
	return SpaceLayout{}, false
}

// Cut is where a gather was answered: one position per log stream, keyed by
// the STREAM's name — the position's own [Position.Stream].
//
// The stream rather than the [LogID] key, because the stream carries the
// layout number and the key does not: a cut can then never set a position of
// one layout's log beside another's under one key, and a key that disagreed
// with its value's stream would be one value with two names.
type Cut map[string]Position
