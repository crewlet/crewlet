package iamdomain_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// NO KIND THIS BUILD WRITES PRODUCES THE ROOT SCOPE, bar the two that cannot
// be deferred behind a gate.
//
// THE case for this domain. A deferred record blocks every read whose closure
// its scope covers, so a record filed at the root freezes every suspension,
// every revocation and every login in the company at once — on the first
// record a node cannot decode, which is an ordinary event during a rolling
// upgrade. Every other domain can afford a root-scoped kind because the worst
// it costs is a stale answer; here it costs everybody their access.
//
// TWO KINDS ARE EXEMPT AND THE EXEMPTION IS NAMED RATHER THAN COUNTED:
//
//   - an EVICTION resolves to the root and INSTALLS A GATE, so it can never be
//     filed as a deferral at all — a version this build cannot read stops the
//     applier instead;
//   - a GENERATION resolves to the root and pays for it, correctly: a node
//     that cannot decode a record saying this log was reanchored cannot
//     certify any read over it either.
//
// The BARRIER is exempt the other way: it resolves to the framework's own
// scope, which intersects nothing.
func TestOnlyAGateOrAReanchorEverProducesTheRootScope(t *testing.T) {
	t.Parallel()

	// One representative subject per kind, and every one of them stating
	// the scope its ordinary writer would state.
	subjects := map[iamdomain.ObjectKind]struct {
		subject iamdomain.Subject
		scope   iamdomain.ScopeSet
	}{
		iamdomain.KindPerson: {iamdomain.PersonSubject("p1"),
			iamdomain.PeopleScope("p1")},
		iamdomain.KindEmail: {iamdomain.EmailSubject("blind1"),
			iamdomain.PeopleScope("p1")},
		iamdomain.KindLogin: {iamdomain.LoginSubject("jane.doe"),
			iamdomain.PeopleScope("p1")},
		iamdomain.KindSeat: {iamdomain.SeatSubject("seat1"),
			iamdomain.PeopleScope("p1")},
		iamdomain.KindSession: {iamdomain.SessionSubject("lin1"),
			iamdomain.PeopleScope("p1")},
		iamdomain.KindBootstrap: {iamdomain.BootstrapSubject(),
			iamdomain.BucketScope(iamdomain.BootstrapBucket())},
		iamdomain.KindSweep: {iamdomain.SweepSubject(17),
			iamdomain.BucketScope(17)},
		iamdomain.KindBarrier: {iamdomain.BarrierSubject(),
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
		iamdomain.KindEviction, iamdomain.KindGeneration,
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

// THE CONTROL: a kind whose records resolve to the root and is neither a gate
// nor a reanchor is exactly what the case above has to catch.
//
// It is a FIXTURE KIND rather than a mutation of the production switch, so the
// control lives beside the assertion it controls instead of in a comment
// telling a future reader which line to break.
func TestAKindThatResolvesToTheRootIsCaught(t *testing.T) {
	t.Parallel()

	// A kind this build has never heard of, whose scope failed to decode —
	// which is the one path by which an ordinary record legitimately
	// reaches the root, and the reason the widening rule is a hazard worth
	// a test rather than a convenience.
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
	// And the write side refuses it, so the widening only ever covers a
	// peer's record and never this build's own laziness.
	if err := iamdomain.RootScope().Validate(
		iamdomain.PersonSubject("p1")); err == nil {
		t.Error("a person record was allowed to claim the whole estate as its " +
			"scope — every record about people enumerates their buckets")
	}
}

// A BUCKET IS DERIVED FROM THE ID NOTHING RENAMES, AND IS STABLE.
//
// A bucket derived from anything mutable would move under a deferral row that
// had already been filed, and every later probe would search a bucket the
// record is not in — silently, and in exactly the window the deferral was most
// needed. An email address is the value that would have been tempting.
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

// AND THE PARTITION IS ACTUALLY A PARTITION: every bucket is reachable.
//
// A hash that folded every id into one bucket would pass every other case here
// — the paths would be valid, the scopes would resolve, the deferrals would be
// filed — and would make the sweep one transaction over the whole estate and
// the duplicate scan a full directory walk, which is the property the division
// exists for and the one nothing else asserts.
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

// THE SENTINEL AND THE ARRAY ENCODINGS BOTH ROUND-TRIP.
//
// And the case that matters is the ARRAY one: encoding/json marshals a
// []uint8 as a BASE64 STRING, because []uint8 IS []byte to the reflect
// package — so the obvious element type would encode every scope as the one
// shape the decoder reads as the root sentinel, and every record in the domain
// would silently claim the whole estate.
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

// EVERY BUCKET PATH IS UNDER THE DOMAIN'S OWN ROOT.
//
// The root term is the widest-on-unreadable answer, and an answer that covers
// nothing is not one: a bucket path SIBLING to the root would make a record
// whose scope this build could not parse block exactly nothing.
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

// AND THE PATHS SORT THE WAY THE BUCKETS DO, which is what the zero padding
// buys and the only thing it buys.
//
// It is worth stating what it does NOT buy, because the obvious claim is
// wrong: sibling paths under a fixed-depth scheme cannot contain each other
// whatever their spelling, since the framework's containment test appends the
// separator before comparing — `i/b/7` is not a prefix of `i/b/70` because
// `i/b/7/` is not. So there is no correctness hazard here to guard, and a test
// asserting one would be a test that cannot fail.
//
// What padding is for is the OPERATOR: a deferral listing, a stalled-log
// report and a broker subject listing are all read in whatever order the
// strings sort in, and sixty-four buckets spelled `0`..`63` sort `0, 1, 10,
// 11, ... 2, 20` — which reads as a listing somebody has shuffled.
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
	// THE BOOTSTRAP MAY NOT, although it is the other kind with no id and
	// is the natural place to reach for "the whole estate". It writes a row
	// like any other record and is deferrable like any other record, so it
	// takes the bucket of its own subject and a node that cannot decode it
	// blocks a sixty-fourth of the domain rather than all of it.
	if err := iamdomain.RootScope().Validate(
		iamdomain.BootstrapSubject()); err == nil {
		t.Error("the bootstrap was allowed to claim the whole estate — one " +
			"undecodable bootstrap record would then freeze every login in " +
			"the company")
	}
	if err := iamdomain.BucketScope(iamdomain.BootstrapBucket()).Validate(
		iamdomain.BootstrapSubject()); err != nil {
		t.Errorf("the bootstrap was refused its own bucket: %v", err)
	}
	// And the kinds that MAY state the root do.
	for _, subject := range []iamdomain.Subject{
		iamdomain.EvictionSubject("node-a"),
		iamdomain.GenerationSubject(3),
		iamdomain.BarrierSubject(),
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
