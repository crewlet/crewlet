package iamdomain_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// No kind but a gate or a reanchor resolves to the root scope. A deferred
// record blocks every read its scope covers, so a root-filed one freezes every
// suspension, revocation and login in the company at once, on the first record
// a node cannot decode: an ordinary event in a rolling upgrade. The exemptions
// are named, not counted (ObjectKind.RootScoped has why): an invalidation and
// an eviction install a gate, so are never deferred, and a generation pays
// correctly. A barrier and a trimmed record resolve to the framework's scope,
// which intersects nothing.
func TestOnlyAGateOrAReanchorEverProducesTheRootScope(t *testing.T) {
	t.Parallel()

	// One subject per kind, stating the scope its ordinary writer states.
	subjects := map[iamdomain.ObjectKind]struct {
		subject iamdomain.Subject
		scope   iamdomain.ScopeSet
	}{
		iamdomain.KindDirectory: {iamdomain.DirectorySubject(),
			iamdomain.PeopleScope("p1")},
		iamdomain.KindPerson: {iamdomain.PersonSubject("p1"),
			iamdomain.PeopleScope("p1")},
		iamdomain.KindSession: {iamdomain.SessionSubject("lin1"),
			iamdomain.PeopleScope("p1")},
		iamdomain.KindInvalidation: {iamdomain.InvalidationSubject(),
			iamdomain.RootScope()},
		iamdomain.KindSweep: {iamdomain.SweepSubject(17),
			iamdomain.BucketScope(17)},
		iamdomain.KindBarrier: {iamdomain.BarrierSubject(),
			iamdomain.RootScope()},
		iamdomain.KindTrimmed: {iamdomain.TrimmedSubject(statelog.Position{
			Generation: 1, Seq: 9}),
			iamdomain.RootScope()},
		iamdomain.KindEviction: {iamdomain.EvictionSubject("node-a"),
			iamdomain.RootScope()},
		iamdomain.KindGeneration: {iamdomain.GenerationSubject(3),
			iamdomain.RootScope()},
	}
	if len(subjects) != len(iamdomain.ObjectKinds) {
		t.Fatalf("this case covers %d kinds and the domain declares %d — a kind "+
			"added without a row here is one whose scope nothing checks",
			len(subjects), len(iamdomain.ObjectKinds))
	}

	mayBeRoot := []iamdomain.ObjectKind{
		iamdomain.KindInvalidation, iamdomain.KindEviction,
		iamdomain.KindGeneration,
	}
	for _, kind := range iamdomain.ObjectKinds {
		tc := subjects[kind]
		paths := tc.scope.Resolve(tc.subject).Paths
		root := slices.Contains(paths, iamdomain.RootPath())
		switch {
		case root && !slices.Contains(mayBeRoot, kind):
			t.Errorf("a %s record resolves to %v, which covers the whole "+
				"estate — one such record this node cannot decode would freeze "+
				"every suspension, every revocation and every login at once",
				kind, paths)
		case !root && slices.Contains(mayBeRoot, kind):
			t.Errorf("a %s record resolves to %v and no longer covers the whole "+
				"estate — the exemption list says it does, and a gate or a "+
				"reanchor that blocked only some reads would license the rest",
				kind, paths)
		}
	}
}

// The control: a kind resolving to the root without being a gate or a
// reanchor is what the case above must catch. A fixture kind, not a mutation
// of the production switch, keeps the control beside its assertion.
func TestAKindThatResolvesToTheRootIsCaught(t *testing.T) {
	t.Parallel()

	// An unknown kind whose scope failed to decode: the one path by which an
	// ordinary record legitimately reaches the root.
	var scope iamdomain.ScopeSet
	if err := json.Unmarshal([]byte(`{"not":"a scope"}`), &scope); err != nil {
		t.Fatalf("an unreadable scope must not fail the decode: %v", err)
	}
	subject := iamdomain.Subject{Kind: "somethingnewer", ID: "x"}
	paths := scope.Resolve(subject).Paths
	if !slices.Contains(paths, iamdomain.RootPath()) {
		t.Fatalf("an unreadable scope resolved to %v — the contract is the "+
			"WIDEST reading, because narrowing a newer peer's blast radius to "+
			"whatever this build recognised is how a record stops blocking the "+
			"reads it makes stale", paths)
	}
	// The write side refuses it, so the widening covers only a peer's record.
	if err := iamdomain.RootScope().Validate(
		iamdomain.PersonSubject("p1")); err == nil {
		t.Error("a person record was allowed to claim the whole estate as its " +
			"scope — every record about people enumerates their buckets")
	}
}

// A bucket is a stable function of the id nothing renames: one derived from a
// mutable value (an address) would move under an already-filed deferral row,
// and every later probe would silently search the wrong bucket.
func TestABucketIsAPureFunctionOfThePersonId(t *testing.T) {
	t.Parallel()
	const id = "018f3a9c-0000-7000-8000-000000000001"
	first := iamdomain.BucketOf(id)
	for range 100 {
		if got := iamdomain.BucketOf(id); got != first {
			t.Fatalf("BucketOf(%q) answered %d and then %d", id, first, got)
		}
	}
	if first >= iamdomain.Buckets {
		t.Fatalf("BucketOf answered %d and the estate has %d buckets",
			first, iamdomain.Buckets)
	}
}

// Every bucket is reachable. A hash folding every id into one bucket passes
// every other case, yet makes the sweep one transaction over the whole estate
// and the duplicate scan a full directory walk.
func TestEveryBucketIsReachable(t *testing.T) {
	t.Parallel()
	seen := map[iamdomain.Bucket]bool{}
	for i := range 100_000 {
		seen[iamdomain.BucketOf("018f3a9c-0000-7000-8000-"+
			strings.Repeat("0", 6)+string(rune('a'+i%26))+
			itoa(i))] = true
		if len(seen) == iamdomain.Buckets {
			return
		}
	}
	t.Fatalf("only %d of %d buckets were reached over 100 000 ids — the "+
		"division is what makes the sweep sixty-four bounded transactions "+
		"rather than one unbounded one", len(seen), iamdomain.Buckets)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// Both encodings round-trip. The array case matters: encoding/json marshals a
// []uint8 as a base64 string, which decodes as the root sentinel, so every
// record would silently claim the whole estate.
func TestAScopeRoundTripsThroughItsOwnEncoding(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]iamdomain.ScopeSet{
		"the root":     iamdomain.RootScope(),
		"one bucket":   iamdomain.BucketScope(17),
		"three":        iamdomain.BucketScope(0, 17, 63),
		"out of order": iamdomain.BucketScope(63, 0, 17),
		"with repeats": iamdomain.BucketScope(17, 17, 17),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !want.Root && strings.HasPrefix(string(data), `"`) {
				t.Fatalf("a bucket enumeration encoded as the string %s — a "+
					"[]uint8 marshals as base64 and decodes as the root "+
					"sentinel, so every record would claim the whole estate",
					data)
			}
			var got iamdomain.ScopeSet
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal %s: %v", data, err)
			}
			if got.Root != want.Root || !slices.Equal(got.Buckets, want.Buckets) {
				t.Errorf("%s decoded as %+v, want %+v", data, got, want)
			}
		})
	}
}

// AN UNREADABLE OR OUT-OF-RANGE SCOPE IS THE WIDEST TERM, never an error and
// never an empty set.
func TestAnUnreadableScopeIsTheWholeEstate(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"an object":                         `{"buckets":[1]}`,
		"a number":                          `7`,
		"an empty array":                    `[]`,
		"null":                              `null`,
		"a bucket past the count":           `[1,200]`,
		"a string that is not the sentinel": `"everything, obviously"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got iamdomain.ScopeSet
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("%s failed the decode: %v — the envelope pass exists "+
					"precisely so a record this build cannot read can be filed, "+
					"gated and reprocessed rather than dropped", raw, err)
			}
			if !got.Root {
				t.Errorf("%s decoded as %+v, want the whole estate", raw, got)
			}
		})
	}
}

// A SCOPE NAMING EVERY BUCKET IS THE ROOT, said the short way.
func TestAScopeCoveringEveryBucketCollapsesToTheRoot(t *testing.T) {
	t.Parallel()
	all := make([]iamdomain.Bucket, 0, iamdomain.Buckets)
	for b := range iamdomain.Buckets {
		all = append(all, iamdomain.Bucket(b))
	}
	got := iamdomain.BucketScope(all...)
	if !got.Root || len(got.Buckets) != 0 {
		t.Errorf("a scope over all %d buckets is %+v — naming them all IS "+
			"naming the root, and saying it the long way is sixty-four rows in "+
			"the scope index where one would do", iamdomain.Buckets, got)
	}
}

// Every bucket path is under the domain's root: were it a sibling, a record
// whose scope this build could not parse would block nothing.
func TestEveryBucketPathIsUnderTheRoot(t *testing.T) {
	t.Parallel()
	root := iamdomain.RootPath()
	for b := range iamdomain.Buckets {
		path := iamdomain.Bucket(b).Path()
		if !strings.HasPrefix(path, root+statelog.ScopeSeparator) {
			t.Fatalf("bucket %d resolves to %q, which is not inside %q — a "+
				"record whose scope could not be parsed would block nothing",
				b, path, root)
		}
	}
}

// The paths sort in bucket order, the only thing zero padding buys: sibling
// paths cannot contain each other whatever their spelling (containment appends
// the separator, so `i/b/7` is no prefix of `i/b/70`), leaving no correctness
// hazard to assert. Padding is for the operator, whose listings sort as
// strings: `0`..`63` would read `0, 1, 10, 11, ... 2, 20`.
func TestTheBucketPathsSortInBucketOrder(t *testing.T) {
	t.Parallel()
	paths := make([]string, 0, iamdomain.Buckets)
	for b := range iamdomain.Buckets {
		paths = append(paths, iamdomain.Bucket(b).Path())
	}
	if !slices.IsSorted(paths) {
		t.Errorf("the bucket paths are not in sorted order — every listing "+
			"that renders them reads as shuffled: %v", paths[:8])
	}
}

// THE WRITE SIDE REFUSES WHAT THE READ SIDE WIDENS.
func TestValidateRefusesAScopeAWriterCouldNotHaveMeant(t *testing.T) {
	t.Parallel()
	person := iamdomain.PersonSubject("p1")
	for name, scope := range map[string]iamdomain.ScopeSet{
		"empty":                {},
		"the root on a person": iamdomain.RootScope(),
		"the root and an enumeration at once": {
			Root: true, Buckets: []iamdomain.Bucket{1}},
		"a bucket past the count": {Buckets: []iamdomain.Bucket{200}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := scope.Validate(person); err == nil {
				t.Errorf("a writer was allowed to state %+v", scope)
			}
		})
	}
	// And the kinds that MAY state the root do.
	for _, subject := range []iamdomain.Subject{
		iamdomain.EvictionSubject("node-a"),
		iamdomain.GenerationSubject(3),
		iamdomain.BarrierSubject(),
		iamdomain.TrimmedSubject(statelog.Position{
			Generation: 1, Seq: 9}),
	} {
		if err := iamdomain.RootScope().Validate(subject); err != nil {
			t.Errorf("%s was refused the root scope, which is the only honest "+
				"blast radius it has: %v", subject, err)
		}
	}
	if err := iamdomain.PeopleScope("p1").Validate(person); err != nil {
		t.Errorf("an ordinary person record was refused: %v", err)
	}
}

// A deferred bucket makes its own people unavailable and nobody else's, the
// property the bucketed scope buys: a deferral is filed under the writer's
// buckets and a read probes its person's, and were they misaligned one
// undecodable record would block nothing, or every login in the company. It
// is asserted over the paths, whose containment (a prefix test with the
// separator appended) decides it.
func TestADeferredBucketCoversItsOwnPeopleAndNobodyElses(t *testing.T) {
	t.Parallel()

	// Two people in different buckets, found rather than assumed: pinning
	// one would copy the hash.
	var here, elsewhere string
	for i := range 1000 {
		id := "018f3a9c-0000-7000-8000-" + pad(i)
		switch {
		case here == "":
			here = id
		case iamdomain.BucketOf(id) != iamdomain.BucketOf(here):
			elsewhere = id
		}
		if elsewhere != "" {
			break
		}
	}
	if elsewhere == "" {
		t.Fatal("no two of a thousand ids fell in different buckets — the " +
			"partition is not partitioning, so every deferral would block " +
			"every read")
	}

	deferred := iamdomain.PeopleScope(here).
		Resolve(iamdomain.PersonSubject(here)).Paths
	if len(deferred) != 1 {
		t.Fatalf("a record about one person declares %v", deferred)
	}

	// A READ ABOUT THAT PERSON IS COVERED.
	mine := iamdomain.ReadScope(here).Paths
	if !covers(deferred, mine) {
		t.Errorf("a record deferred at %v does not cover a read at %v — the "+
			"read would be served from a node holding a record it cannot "+
			"decode about exactly that person", deferred, mine)
	}
	// A READ ABOUT SOMEBODY IN ANOTHER BUCKET IS NOT.
	theirs := iamdomain.ReadScope(elsewhere).Paths
	if covers(deferred, theirs) {
		t.Errorf("a record deferred at %v covers a read at %v — one "+
			"undecodable record would make every login in the company wait",
			deferred, theirs)
	}
	// A READ THAT NAMES NOBODY IS COVERED BY EVERYTHING: every deferred
	// record concerns it.
	everyone := iamdomain.ReadScope().Paths
	if !covers(deferred, everyone) {
		t.Errorf("a record deferred at %v does not cover the directory read "+
			"at %v — 'is this complete' would answer yes from a node holding "+
			"a record it cannot read", deferred, everyone)
	}
}

// covers is the framework's own containment over two path sets, in either
// direction as the deferral probe's two clauses are: a stored path inside the
// query's closure is one the query is about, and one containing the query
// covers it.
func covers(stored, query []string) bool {
	for _, s := range stored {
		for _, q := range query {
			if statelog.Covers(s, q) || statelog.Covers(q, s) {
				return true
			}
		}
	}
	return false
}

// pad renders a counter as the tail of a uuid-shaped id.
func pad(i int) string {
	out := itoa(i)
	for len(out) < 12 {
		out = "0" + out
	}
	return out
}
