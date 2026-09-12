package pages_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// TestATitleIsPublISHABLEWhateverItsAuthorTyped.
//
// A title is PROSE and a subject is a broker path: the first routinely carries
// a space, a dot, a `*` and a `>`, and the second may carry none of them. This
// is the guard that says the address survives every one of them — a title that
// produced an unpublishable subject would make the page uncreatable, and one
// that produced a subject with a wildcard in it would make a create match
// every other page's subject.
func TestATitleIsPublishableWhateverItsAuthorTyped(t *testing.T) {
	t.Parallel()
	for name, title := range map[string]string{
		"ordinary prose":       "Deploy Runbook",
		"a version number":     "v1.2 rollout",
		"a broker wildcard":    "everything > here",
		"a subject star":       "the * page",
		"tabs and newlines":    "one\ttwo\nthree",
		"non-ASCII":            "Déploiement · résumé",
		"nothing but spacing":  "   ",
		"a leading separator":  ".hidden",
		"a very long sentence": strings.Repeat("long ", 60),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			subject := pages.TitleSubject("ENG", title)
			if err := subject.Validate(); err != nil {
				t.Fatalf("the address for %q is not a subject: %v", title, err)
			}
			wire := subject.Wire()
			if wire == "" {
				t.Fatalf("the address for %q is not publishable", title)
			}
			kind, id, ok := topics.PagesLogPath(wire)
			if !ok || kind != string(pages.KindTitle) || id != subject.ID {
				t.Fatalf("%q does not round-trip: (%q, %q, %v)", wire, kind, id, ok)
			}
		})
	}
}

// TestOneAddressIsOneSUBJECTHoweverItIsCapitalised.
//
// "Deploy Runbook" and "deploy runbook" are one address, and two subjects
// would make them two: both creates would be accepted by the broker, both
// would apply, and the second would take the title row from the first while
// leaving two pages that a link cannot tell apart.
func TestOneAddressIsOneSubjectHoweverItIsCapitalised(t *testing.T) {
	t.Parallel()
	a := pages.TitleSubject("ENG", "Deploy Runbook")
	b := pages.TitleSubject("eng", "deploy   runbook")
	if a.ID != b.ID {
		t.Fatalf("%q and %q arbitrate on different subjects (%s vs %s) — both "+
			"creates would win", "Deploy Runbook", "deploy   runbook", a.ID, b.ID)
	}
	// AND TWO DIFFERENT ADDRESSES ARE TWO SUBJECTS, which is the other
	// half: a token that collapsed them would make one page uncreatable
	// for ever with no way to say why.
	c := pages.TitleSubject("ENG", "Deploy Runbooks")
	if c.ID == a.ID {
		t.Fatal("two different titles arbitrate on one subject")
	}
	d := pages.TitleSubject("PROD", "Deploy Runbook")
	if d.ID == a.ID {
		t.Fatal("one title in two containers arbitrates on one subject — a " +
			"space's addresses are its own")
	}
}

// TestAScopePathNestsSoADeferralCoversWhatItContains.
//
// The framework knows exactly ONE thing about a scope path: that it is a
// hierarchy written left to right, from which it computes containment. So a
// record deferred on a container must block a write to a page inside it, and a
// record deferred on one page must NOT block a write to its neighbour. Written
// flat, neither holds — and neither failure raises anything.
func TestAScopePathNestsSoADeferralCoversWhatItContains(t *testing.T) {
	t.Parallel()
	space := pages.ScopeTerm{Kind: pages.TermContainer, ID: "ENG"}.Path()
	page := pages.ScopeTerm{
		Kind: pages.TermObject, Container: "ENG", ID: "page-a",
	}.Path()
	neighbour := pages.ScopeTerm{
		Kind: pages.TermObject, Container: "ENG", ID: "page-b",
	}.Path()
	elsewhere := pages.ScopeTerm{
		Kind: pages.TermObject, Container: "PROD", ID: "page-a",
	}.Path()

	sep := statelog.ScopeSeparator
	if !strings.HasPrefix(page, space+sep) {
		t.Fatalf("a page's path %q does not sit under its container's %q — a "+
			"container-wide deferral would cover none of its pages", page, space)
	}
	if strings.HasPrefix(page, neighbour+sep) || page == neighbour {
		t.Fatalf("two pages in one container share a path (%q, %q) — one "+
			"deferred record would block every write in the space",
			page, neighbour)
	}
	if strings.HasPrefix(elsewhere, space+sep) {
		t.Fatalf("a page in PROD (%q) sits under ENG's path %q", elsewhere, space)
	}
	// AND THE DOMAIN TERM COVERS EVERYTHING, which is what an unreadable
	// scope resolves to: the only honest reading of a blast radius this
	// build cannot parse is "all of it".
	domain := pages.ScopeTerm{Kind: pages.TermDomain}.Path()
	for _, path := range []string{space, page, neighbour, elsewhere} {
		if !strings.HasPrefix(path, domain+sep) && path != domain {
			t.Fatalf("%q is outside the domain term %q, so a record whose scope "+
				"could not be read would not cover it", path, domain)
		}
	}
}

// TestAnUnreadableScopeIsTheWholeDomainRatherThanNothing.
//
// This runs on a node reading a record a NEWER build wrote. An empty set would
// say "this record makes nothing stale", which is the one claim a record this
// build cannot read may not make.
func TestAnUnreadableScopeIsTheWholeDomainRatherThanNothing(t *testing.T) {
	t.Parallel()
	for name, encoded := range map[string]string{
		"a shape from a later version":         `{"kind":"something-new"}`,
		"a bare number":                        `7`,
		"an empty array":                       `[]`,
		"a sentinel this build does not write": `"z/ENG"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var set pages.ScopeSet
			if err := set.UnmarshalJSON([]byte(encoded)); err != nil {
				t.Fatalf("decoding a scope must never fail — it would take the "+
					"whole two-pass decode down with it: %v", err)
			}
			paths := set.Resolve(pages.PageSubject("page-a")).Paths
			domain := pages.ScopeTerm{Kind: pages.TermDomain}.Path()
			if len(paths) != 1 || paths[0] != domain {
				t.Fatalf("an unreadable scope resolved to %v, and the only "+
					"honest reading is the whole domain %q", paths, domain)
			}
		})
	}
}

// TestTheSentinelCarriesItsContainerSoAPagesDeferralIsFiledInItsSpace.
//
// A page's subject is its uuid and its container is a fact about the ROW — one
// that changes, because a page can be moved. So the path a record's scope
// resolves to cannot be computed from the subject alone, and the only party
// that knows it when the record is written is the writer. Leaving it out is
// SILENT: every page record would file under the containerless space while
// every space-scoped read probed its own, and the containment probe would
// never match.
func TestTheSentinelCarriesItsContainerSoAPagesDeferralIsFiledInItsSpace(t *testing.T) {
	t.Parallel()
	withSpace := pages.ScopeSet{Subject: true, Container: "ENG"}.
		Resolve(pages.PageSubject("page-a")).Paths
	without := pages.ScopeSet{Subject: true}.
		Resolve(pages.PageSubject("page-a")).Paths
	if len(withSpace) != 1 || len(without) != 1 {
		t.Fatalf("a sentinel resolved to %v / %v", withSpace, without)
	}
	if withSpace[0] == without[0] {
		t.Fatal("a sentinel resolves to the same path with and without its " +
			"container, so a page's deferral is filed where no space-scoped " +
			"read will ever probe")
	}
	space := pages.ScopeTerm{Kind: pages.TermContainer, ID: "ENG"}.Path()
	if !strings.HasPrefix(withSpace[0], space+statelog.ScopeSeparator) {
		t.Fatalf("%q is not under ENG's own path %q", withSpace[0], space)
	}
}

// TestABarrierIntersectsNothing.
//
// A barrier writes no row on any node, so a scope that intersected anything
// would make every linearizable read wait behind every other one.
func TestABarrierIntersectsNothing(t *testing.T) {
	t.Parallel()
	paths := pages.ScopeSet{Subject: true}.Resolve(pages.BarrierSubject()).Paths
	if len(paths) != 1 || paths[0] != statelog.BarrierScope {
		t.Fatalf("a barrier's scope is %v, and it must be the framework's own "+
			"%q — anything else serialises every read behind every other",
			paths, statelog.BarrierScope)
	}
}

// TestAGateIsAboutTheWholeDomain: an eviction and a generation license or drop
// records on every subject, so anything narrower would be a claim the record
// does not make.
func TestAGateIsAboutTheWholeDomain(t *testing.T) {
	t.Parallel()
	domain := pages.ScopeTerm{Kind: pages.TermDomain}.Path()
	for name, subject := range map[string]pages.Subject{
		"an eviction":  pages.EvictionSubject("node-4"),
		"a generation": pages.GenerationSubject(2),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := pages.ScopeSet{Subject: true}.Resolve(subject).Paths
			if len(paths) != 1 || paths[0] != domain {
				t.Fatalf("%s resolves to %v rather than the whole domain %q",
					name, paths, domain)
			}
		})
	}
}

// TestEveryKindIsClassifiedAndEveryArbitratedKindIsDeclared.
//
// Four readers compare against the kind enum and none can see the others: the
// publisher builds the subject, the wake feed filters on it, the applier
// dispatches on it, and the domain's Tables must classify every table it
// writes. A kind added to three of the four is a record that publishes, is
// delivered, wakes nobody and writes nothing.
func TestEveryKindIsClassifiedAndEveryArbitratedKindIsDeclared(t *testing.T) {
	t.Parallel()
	declared := map[string]bool{}
	for _, k := range (pages.Domain{}).Stream().ArbitratedKinds {
		declared[k] = true
	}
	for _, kind := range pages.ObjectKinds {
		if !kind.Valid() {
			t.Errorf("%s is in the enum and Valid says it is not", kind)
		}
		if kind.Arbitrated() != declared[string(kind)] {
			t.Errorf("%s reports Arbitrated=%v and the stream declares %v — the "+
				"two are read by different halves of the framework and a "+
				"disagreement wedges that subject the first time a gate drops "+
				"a record", kind, kind.Arbitrated(), declared[string(kind)])
		}
	}
	if len(declared) != len(pages.ObjectKinds)-1 {
		t.Errorf("the stream declares %d arbitrated kinds out of %d — only the "+
			"barrier is unarbitrated", len(declared), len(pages.ObjectKinds))
	}
}

// TestOnlyTheGateKindsStopTheApplier.
//
// A deferred gate does not postpone one record's effect on one node: it
// silently licenses every record above it, with no inverse that repairs it. So
// the answer has to come from the ENVELOPE — all a node has when it cannot
// decode the payload — and it has TWO conditions, because a purge is a gate by
// its OP on an ordinary page subject.
func TestOnlyTheGateKindsStopTheApplier(t *testing.T) {
	t.Parallel()
	d := pages.Domain{}
	for name, tc := range map[string]struct {
		env  statelog.Envelope
		gate bool
	}{
		"an eviction, by its kind": {
			statelog.Envelope{Kind: string(pages.KindEviction),
				Op: string(pages.OpEviction)}, true},
		"a purge, by its op on an ordinary page": {
			statelog.Envelope{Kind: string(pages.KindPage),
				Op: string(pages.OpPurge)}, true},
		"an ordinary save": {
			statelog.Envelope{Kind: string(pages.KindPage),
				Op: string(pages.OpPatch)}, false},
		"a create": {
			statelog.Envelope{Kind: string(pages.KindTitle),
				Op: string(pages.OpCreate)}, false},
		"a trash, which is reversible": {
			statelog.Envelope{Kind: string(pages.KindPage),
				Op: string(pages.OpTombstone)}, false},
		"a kind this build has never heard of": {
			statelog.Envelope{Kind: "something-new", Op: "something-new"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := d.InstallsGate(tc.env); got != tc.gate {
				t.Fatalf("InstallsGate = %v, want %v", got, tc.gate)
			}
		})
	}
}

// TestEveryTableTheApplierWritesIsClassified.
//
// The scrub list, the identity claim and the local sweep are all derived from
// one map, which is why a table missing from it is three lists that are
// silently short rather than one error.
func TestEveryTableTheApplierWritesIsClassified(t *testing.T) {
	t.Parallel()
	tables := (pages.Domain{}).Tables()
	for _, name := range pages.ReproducibleTables {
		if class, held := tables[name]; !held || class != statelog.Replicated {
			t.Errorf("%s is a reproducible table and is classed %q", name, class)
		}
	}
	for _, name := range pages.MachineryTables {
		if class, held := tables[name]; !held || class != statelog.Local {
			t.Errorf("%s is the log's own machinery and is classed %q", name, class)
		}
	}
	// THE DIVERGENT ONE, and it must be exactly one: the skill flag is
	// this build's parser answering about this build's rules, so two
	// builds mid-upgrade legitimately differ — and every OTHER table has
	// to be inside the identity claim or the claim means nothing.
	var divergent []string
	for name, class := range tables {
		if class == statelog.Divergent {
			divergent = append(divergent, name)
		}
	}
	if len(divergent) != 1 || divergent[0] != "pages_skills" {
		t.Fatalf("the divergent tables are %v, and the only value a fleet may "+
			"legitimately disagree about here is the tool-skill flag", divergent)
	}
	if _, claimed := tables["pages_skills"]; !claimed {
		t.Fatal("pages_skills is not declared at all, so a donated snapshot " +
			"neither carries it nor scrubs it")
	}
	// AND THE DEFERRAL MACHINERY IS NAMED, because the framework writes
	// those statements and the domain owns the schema.
	for _, name := range []string{
		(pages.Domain{}).DeferredTable(), (pages.Domain{}).ScopeIndex(),
		(pages.Domain{}).OpsTable(),
	} {
		if _, held := tables[name]; !held {
			t.Errorf("%s is named by the domain and is not in its own table map",
				name)
		}
	}
}
