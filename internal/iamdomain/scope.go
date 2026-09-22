package iamdomain

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD'S SCOPE: the complete set of objects its apply may write, stated by
// the WRITER and readable at every version.
//
// It is what a node that cannot decode a record files the deferral under, and
// what a later read or write probes to discover it is behind. The framework
// knows exactly one thing about a scope path — that it is a hierarchy written
// left to right with a separator, from which it computes CONTAINMENT.
//
// # FLAT AND BUCKETED, and neither half is an optimisation
//
// A scope here is a set of BUCKETS: `i/b/07`, two levels below the domain
// letter, whatever the record is about. There is no per-person path and no
// per-claim path, and the two reasons are different.
//
// BUCKETED, because A CLAIM'S SUBJECT DOES NOT NAME ITS PERSON. An address
// claim arbitrates on a keyed blind, a login claim on a login, a seat claim on
// a seat id, a session on a lineage — and the PERSON each of them is about is
// inside a payload the deferring node, by construction, could not decode. A
// per-person path would therefore be computable by every node except the one
// that needs it. A bucket is on the ENVELOPE, in the clear, stated by the
// writer, so the node that cannot read the record still files it where a read
// about that person will look.
//
// Per-person paths would also be a privacy leak into the one place this estate
// most carefully keeps clear of personal data: a deferral row and every
// operator surface that renders it would carry a person's id, for every
// undecodable record, on every node.
//
// FLAT, because there is no hierarchy to reflect. A person is not inside
// another person, and a bucket is a partition rather than a container — so
// `i/b/07` has no descendants and the framework's containment collapses to
// equality plus the root. Any deeper path would be structure invented to give
// containment something to do.
//
// # Sixty-four, and why the cap and the bucket count are one number
//
// [Buckets] is the fan-out this design uses everywhere, and it makes
// [MaxScopeTerms] not a policy but an identity: a record naming more than
// sixty-four buckets has named a bucket twice, and a record naming all
// sixty-four has named the ROOT. So the cap is reached by construction rather
// than by a writer that hits it and has to decide what to do — [BucketScope]
// normalises both cases, and neither is a refusal.
//
// # The domain letter is on EVERY path
//
// `i` is the whole estate, and `i/b/...` sits under it, because the domain term
// is the widest-on-unreadable answer and an answer that covers nothing is not
// one: `i` beside `b/07` would make the root a SIBLING of every bucket, so a
// record whose scope this build could not parse would block exactly nothing.
// The tracker's `t`, the pages log's `p` and the chart's `g` carry the same
// weight for the same reason.
const (
	pathDomain = "i"
	pathBucket = "b"
)

// Buckets is how many partitions the identity estate is divided into.
//
// SIXTY-FOUR, the fan-out this design uses for everything it partitions, and
// it is THIS DOMAIN'S OWN division — unrelated to the corpus shards
// [search.ShardOf] computes, which partition documents rather than people and
// must never be assumed to line up. What a bucket buys here is three things a
// per-person key cannot: a scope term a deferring node can compute, a sweep
// that is sixty-four bounded transactions rather than one unbounded one, and a
// duplicate-claim scan whose cost is measurable per bucket rather than per
// company.
//
// It is not routed on. Every node holds every bucket and reads every bucket.
const Buckets = 64

// Bucket is one partition of the identity estate.
//
// A uint8 rather than an int, because the whole value space is 0..63 and the
// wire form is an array of them: the type is what stops a caller passing a
// person's id where a bucket goes, and [ScopeSet.Validate] is what stops a
// value above the count reaching a path.
type Bucket uint8

// BucketOf is the partition a person falls in.
//
// FNV-1a OVER THE PERSON'S ID, which is the id nothing renames — so a person
// keeps their bucket through every rename, every address change and every seat
// they are bound to or unbound from. A bucket derived from anything mutable
// would move under a deferral row that had already been filed, and the probe
// would then search a bucket the record is not in: exactly the failure the
// chart's flat scope paths were chosen to avoid, in a domain where the moving
// value would be an email address.
//
// The hash is FNV-1a because it is in the standard library, it is fast, and
// nothing here needs it to resist an adversary: a person who could choose
// their own bucket would gain the ability to share a sweep transaction with
// somebody else, which is not a capability.
func BucketOf(personID string) Bucket {
	h := fnv.New64a()
	_, _ = h.Write([]byte(personID))
	return Bucket(h.Sum64() % uint64(Buckets))
}

// BootstrapBucket is the bucket the company's bootstrap state falls in.
//
// The bootstrap has no person, and it still needs a bucket, because the sweep
// that collects an expired bootstrap code is the same per-bucket sweep that
// collects everything else — a second mechanism for one table would be a table
// that stops being swept the day somebody forgets it exists. It is the bucket
// of the literal subject, so it is a fixed number derived the same way every
// other one is rather than a constant somebody chose.
func BootstrapBucket() Bucket { return BucketOf(string(KindBootstrap)) }

// String renders a bucket ZERO-PADDED to two digits.
//
// So a subject listing and a scope path both sort the way the buckets do: `07`
// before `10`, where `7` sorts after it. It is also what makes every path in
// this domain the same width, which is what an operator reading a deferral row
// beside sixty-three others actually needs.
func (b Bucket) String() string {
	if b < 10 {
		return "0" + strconv.Itoa(int(b))
	}
	return strconv.Itoa(int(b))
}

// Path renders the bucket as a scope path the framework can order.
func (b Bucket) Path() string {
	return pathDomain + statelog.ScopeSeparator + pathBucket +
		statelog.ScopeSeparator + b.String()
}

// MaxScopeTerms is the cap on a record's declared scope, and it IS [Buckets].
//
// See the header: a record naming more than sixty-four buckets has named one
// twice, and a record naming all sixty-four has named the root. The constant
// exists so a reader looking for the cap finds it under the name every other
// domain gives it, rather than having to know the two numbers are the same one.
const MaxScopeTerms = Buckets

// ScopeSet is the COMPLETE set of buckets a record's apply may write.
//
// NEVER EMPTY on the wire. An absent, empty or unreadable scope on a record
// this build cannot decode is the ROOT, never "nothing": assuming a record
// that failed to parse touched nothing is the one claim a record may not make.
type ScopeSet struct {
	// Root is the whole estate: what a gate names, what an unreadable
	// scope resolves to, and what a record naming every bucket collapses
	// to.
	Root bool

	// Buckets are the enumeration, sorted and deduplicated by
	// [BucketScope] so two nodes forming the same set form the same
	// scope — which matters because the scope is what a THIRD node files
	// their deferral under.
	Buckets []Bucket
}

// RootScope is the whole estate.
func RootScope() ScopeSet { return ScopeSet{Root: true} }

// BucketScope is the scope a record touching one or more people states.
//
// IT NORMALISES rather than refusing, in both directions. Duplicates are
// removed and the result sorted, so a caller assembling buckets from a list of
// people need not care about order or repetition; and a set covering every
// bucket collapses to [RootScope], because naming all sixty-four IS naming the
// root and a scope that said it the long way would be sixty-four rows in the
// scope index where one would do.
//
// A caller passing no buckets at all gets the root, for the type's own reason:
// an empty scope claims the record makes nothing stale, and that is the one
// claim a record may not make. [ScopeSet.Validate] is what refuses a WRITER
// that reached that state, so the widening only ever covers a decode.
func BucketScope(buckets ...Bucket) ScopeSet {
	if len(buckets) == 0 {
		return RootScope()
	}
	out := slices.Clone(buckets)
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) >= Buckets {
		return RootScope()
	}
	return ScopeSet{Buckets: out}
}

// PeopleScope is the scope covering a set of people, which is what almost
// every writer here actually holds.
func PeopleScope(personIDs ...string) ScopeSet {
	buckets := make([]Bucket, 0, len(personIDs))
	for _, id := range personIDs {
		buckets = append(buckets, BucketOf(id))
	}
	return BucketScope(buckets...)
}

// RootSentinel is the one-byte encoding of "the whole estate".
//
// A BARE STRING beside the array form, for the reason the chart's sentinel is
// one: the two shapes are distinguishable by their first byte, so a decoder
// tells them apart without a discriminator field and the common case stays
// small.
const RootSentinel = "*"

// MarshalJSON encodes the root as a bare string and everything else as an
// array of bucket numbers.
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Root || len(s.Buckets) == 0 {
		return json.Marshal(RootSentinel)
	}
	// []uint16 AND NOT []uint8. encoding/json marshals a []uint8 as a
	// BASE64 STRING, because []uint8 IS []byte to the reflect package — so
	// the obvious element type would encode the scope as the one shape the
	// decoder reads as the root sentinel, and every record would silently
	// claim the whole estate.
	out := make([]uint16, 0, len(s.Buckets))
	for _, b := range s.Buckets {
		out = append(out, uint16(b))
	}
	return json.Marshal(out)
}

// UnmarshalJSON accepts both encodings, and reads anything else as the root.
//
// A SCOPE THAT DOES NOT DECODE IS THE WIDEST TERM, never an error and never an
// empty set. This runs on a node reading a record a newer build wrote: the only
// honest reading of a blast radius it cannot parse is "everything", and
// returning an error here would take the whole two-pass decode down with it.
//
// A BUCKET NUMBER ABOVE THE COUNT IS ALSO THE ROOT, rather than a skipped
// term. A peer that partitions into more buckets than this build does is
// stating a blast radius this build cannot place, and dropping the term would
// narrow a newer peer's scope to whatever this build happened to recognise —
// which is the same silent narrowing the chart's unknown-term rule refuses.
func (s *ScopeSet) UnmarshalJSON(b []byte) error {
	// ANY string is the root, not only [RootSentinel]. A peer that spells
	// its widest term differently is still stating a term this build
	// cannot place, and the rule for one of those is the same rule: the
	// widest reading, never a narrower guess.
	var sentinel string
	if err := json.Unmarshal(b, &sentinel); err == nil {
		*s = RootScope()
		return nil
	}
	var raw []uint16
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) == 0 {
		*s = RootScope()
		//nolint:nilerr // WIDEST-ON-UNREADABLE IS THE CONTRACT, per the
		// paragraph above: this runs on a node decoding a record a newer
		// build wrote, and returning the decode error here would fail the
		// envelope pass that exists precisely so such a record can be
		// filed, gated and reprocessed rather than dropped.
		return nil
	}
	out := make([]Bucket, 0, len(raw))
	for _, v := range raw {
		if v >= Buckets {
			*s = RootScope()
			return nil
		}
		out = append(out, Bucket(v))
	}
	*s = BucketScope(out...)
	return nil
}

// Resolve renders the scope as the framework's own paths.
//
// IT SWITCHES ON THE SUBJECT KIND FIRST, for three kinds whose scope is a
// property of what they ARE rather than of what their writer declared:
//
//   - A BARRIER resolves to the framework's own [statelog.BarrierScope], which
//     intersects nothing. A barrier writes no row, so a scope that intersected
//     anything would make every linearizable read wait behind every other.
//     The framework publishes barriers itself, with that scope already on the
//     envelope, so this is the arm that reads one back.
//
//   - AN EVICTION and a GENERATION resolve to the ROOT, and they are the only
//     two kinds here that ever do. Neither writes a person's row at all; what
//     each does is decide whether records on EVERY subject count. An eviction
//     pays nothing for it because it INSTALLS A GATE, so a version this build
//     cannot read STOPS the applier rather than being filed at a path every
//     read queues behind. A generation does pay it, and correctly: a node that
//     cannot decode a record saying this log was reanchored cannot certify any
//     read over it either.
//
// EVERY OTHER KIND RESOLVES TO BUCKETS, including a kind this build has never
// heard of. That is deliberate and it is what the bucketed scope buys: the
// buckets are DATA, readable at every version, so a newer peer's record on a
// kind this build cannot name still files its deferral exactly where a read
// about those people will probe.
func (s ScopeSet) Resolve(subject Subject) statelog.ScopeSet {
	switch subject.Kind {
	case KindBarrier:
		return statelog.ScopeSet{Paths: []string{statelog.BarrierScope}}
	case KindEviction, KindGeneration:
		return statelog.ScopeSet{Paths: []string{pathDomain}}
	}
	if s.Root || len(s.Buckets) == 0 {
		return statelog.ScopeSet{Paths: []string{pathDomain}}
	}
	paths := make([]string, 0, len(s.Buckets))
	for _, b := range s.Buckets {
		paths = append(paths, b.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised()
}

// Validate refuses a scope a writer could not have meant.
//
// It is the WRITE-SIDE half of the widening rule: [ScopeSet.UnmarshalJSON]
// reads anything it cannot place as the root, because a record off the wire
// has to resolve to something, and this refuses one being WRITTEN that way —
// so the widening only ever covers a peer's record and never this build's own
// laziness.
func (s ScopeSet) Validate(subject Subject) error {
	if s.Root {
		if len(s.Buckets) != 0 {
			return fmt.Errorf("iamdomain: the scope on %s claims the whole "+
				"estate and also enumerates %d bucket(s): the two say different "+
				"things about one record", subject, len(s.Buckets))
		}
		if !subject.Kind.RootScoped() {
			return fmt.Errorf("iamdomain: the record on %s claims the whole "+
				"estate as its scope. A root scope is a deferral every read in "+
				"this domain queues behind, so the first record a node cannot "+
				"decode freezes every suspension, every revocation and every "+
				"login at once — only an eviction and a reanchor may state it, "+
				"and every other record, the bootstrap's included, enumerates "+
				"the buckets its apply writes", subject)
		}
		return nil
	}
	if len(s.Buckets) == 0 {
		return fmt.Errorf("iamdomain: the scope on %s is empty — a record that "+
			"touches nothing writes nothing, and an absent scope is read as the "+
			"whole estate rather than as nothing", subject)
	}
	if len(s.Buckets) > MaxScopeTerms {
		return fmt.Errorf("iamdomain: the scope on %s enumerates %d buckets and "+
			"there are only %d — a set that large has named one twice, which "+
			"BucketScope removes, or has named them all, which it collapses to "+
			"the root", subject, len(s.Buckets), MaxScopeTerms)
	}
	for _, b := range s.Buckets {
		if b >= Buckets {
			return fmt.Errorf("iamdomain: the scope on %s names bucket %d and "+
				"this estate has %d — the path it would resolve to is one no "+
				"probe ever forms, so the record would be filed where nothing "+
				"looks for it", subject, b, Buckets)
		}
	}
	return nil
}

// RootPath is the scope path that covers the whole estate.
//
// EXPORTED so a test can assert which kinds produce it without restating the
// alphabet — a constant compared against a literal spelled somewhere else is a
// guard that passes when the alphabet moves underneath it.
func RootPath() string { return pathDomain }
