package iamdomain

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A record's scope is the complete set of buckets its apply may write, stated
// by the writer on the envelope and readable at every version: a node that
// cannot decode the record files its deferral there, and a read probes it.
// Buckets, not per-person paths: the person is inside the payload the deferring
// node could not decode, and a person's id would leak into every deferral row
// and the operator surfaces that render it. Flat (`i/b/07`): a bucket is a
// partition, not a container, so containment is equality plus the root. The
// domain letter `i` heads every path (as `t` and `p` do the tracker's and pages
// log's), so the root an unreadable scope widens to covers every bucket rather
// than sitting beside them. [MaxScopeTerms] is [Buckets] by identity.
const (
	pathDomain = "i"
	pathBucket = "b"
)

// Buckets is how many partitions the identity estate is divided into: the
// design's usual fan-out of sixty-four, so a sweep is sixty-four bounded
// transactions. It is this domain's own division, never assumed to line up
// with [search.ShardOf]'s corpus shards, and not routed on: every node holding
// the estate holds and reads every bucket.
const Buckets = 64

// Bucket is one partition of the identity estate. The type stops a person's id
// being passed where a bucket goes; [ScopeSet.Validate] stops a value past
// [Buckets] reaching a path.
type Bucket uint8

// BucketOf is the partition a person falls in: FNV-1a over the person's id,
// which nothing renames. A bucket derived from a mutable value (an address)
// would move under an already-filed deferral row, and the probe would search a
// bucket the record is not in. FNV-1a need not resist an adversary: choosing
// one's bucket buys only sharing a sweep transaction.
func BucketOf(personID string) Bucket {
	h := fnv.New64a()
	_, _ = h.Write([]byte(personID))
	return Bucket(h.Sum64() % uint64(Buckets))
}

// String renders a bucket zero-padded to two digits, so listings and scope
// paths sort in bucket order (`07` before `10`) and share one width.
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

// MaxScopeTerms is the cap on a record's declared scope, and it IS [Buckets]:
// more names a bucket twice, and all of them names the root ([BucketScope]
// normalises both). It carries the name every other domain gives its cap.
const MaxScopeTerms = Buckets

// ScopeSet is the complete set of buckets a record's apply may write. Never
// empty on the wire: an absent, empty or unreadable scope is the root, since
// a record that failed to parse may never be assumed to touch nothing.
type ScopeSet struct {
	// Root is the whole estate: what a gate names, what an unreadable
	// scope resolves to, and what naming every bucket collapses to.
	Root bool

	// Buckets is the enumeration, sorted and deduplicated by [BucketScope]
	// so two nodes forming one set form one scope, which a third node
	// files their deferral under.
	Buckets []Bucket
}

// RootScope is the whole estate.
func RootScope() ScopeSet { return ScopeSet{Root: true} }

// BucketScope is the scope a record touching one or more people states. It
// normalises rather than refusing: buckets are sorted and deduplicated, and a
// set covering every bucket collapses to [RootScope] (one scope-index row, not
// sixty-four). No buckets gives the root too, for [ScopeSet]'s reason;
// [ScopeSet.Validate] refuses a writer that got there, so the widening only
// ever covers a decode.
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

// PeopleScope is the scope covering a set of people.
func PeopleScope(personIDs ...string) ScopeSet {
	buckets := make([]Bucket, 0, len(personIDs))
	for _, id := range personIDs {
		buckets = append(buckets, BucketOf(id))
	}
	return BucketScope(buckets...)
}

// RootSentinel is the one-byte encoding of "the whole estate": a bare string
// beside the array form, so the first byte tells the shapes apart with no
// discriminator field.
const RootSentinel = "*"

// MarshalJSON encodes the root as a bare string and everything else as an
// array of bucket numbers.
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Root || len(s.Buckets) == 0 {
		return json.Marshal(RootSentinel)
	}
	// []uint16, not []uint8: encoding/json marshals a []uint8 ([]byte) as a
	// base64 string, which the decoder reads as the root sentinel, so every
	// record would silently claim the whole estate.
	out := make([]uint16, 0, len(s.Buckets))
	for _, b := range s.Buckets {
		out = append(out, uint16(b))
	}
	return json.Marshal(out)
}

// UnmarshalJSON accepts both encodings, and reads anything else as the root,
// never an error or an empty set: it runs on records a newer build wrote, the
// only honest reading of a blast radius it cannot parse is everything, and an
// error would fail the whole two-pass decode. A bucket past the count is the
// root too, since dropping it would narrow a newer peer's scope to what this
// build recognises.
func (s *ScopeSet) UnmarshalJSON(b []byte) error {
	// Any string is the root, not only [RootSentinel]: a widest term spelled
	// differently is still one this build cannot place.
	var sentinel string
	if err := json.Unmarshal(b, &sentinel); err == nil {
		*s = RootScope()
		return nil
	}
	var raw []uint16
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) == 0 {
		*s = RootScope()
		//nolint:nilerr // WIDEST-ON-UNREADABLE IS THE CONTRACT, per the
		// doc above: the error would fail the envelope pass that exists so
		// such a record is filed, gated and reprocessed, not dropped.
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

// Resolve renders the scope as the framework's own paths. Four kinds resolve
// by what they are, whatever was declared ([ObjectKind.RootScoped] has why):
//
//   - a barrier or a trimmed record to [statelog.FrameworkScope], which
//     intersects nothing: neither writes a row here, so a wider scope would
//     hold reads behind it (a barrier, every linearizable read behind another);
//   - an eviction or a generation to the root, as each decides whether records
//     on every subject count.
//
// Every other kind, an unknown one included, resolves to its declared buckets:
// they are data readable at every version, so a newer peer's deferral lands
// where a read about those people probes.
func (s ScopeSet) Resolve(subject Subject) statelog.ScopeSet {
	switch subject.Kind {
	case KindBarrier, KindTrimmed:
		return statelog.ScopeSet{Paths: []string{statelog.FrameworkScope}}
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

// Covers reports whether every bucket other names is one this scope names too:
// always for the root, never for another root unless this is one. A writer
// that read its scope ahead of its snapshot asks it inside: a snapshot naming
// more would apply where the published blast radius does not reach.
func (s ScopeSet) Covers(other ScopeSet) bool {
	if s.Root {
		return true
	}
	if other.Root {
		return false
	}
	for _, b := range other.Buckets {
		if !slices.Contains(s.Buckets, b) {
			return false
		}
	}
	return true
}

// Validate refuses a scope a writer could not have meant: the write-side half
// of [ScopeSet.UnmarshalJSON]'s widening, so it only ever covers a peer's
// record, never this build's own laziness.
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
				"and every other record enumerates the buckets its apply "+
				"writes", subject)
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

// RootPath is the scope path that covers the whole estate, exported so a test
// need not restate it in a literal that would pass once the alphabet moved.
func RootPath() string { return pathDomain }
