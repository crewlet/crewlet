package search_test

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// THE VECTOR DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, and it is the
// second domain to run it — which is the point of running it at all.
//
// A framework with one consumer is a framework shaped like its consumer. Every
// contract the suite checks was written against a strict, arbitrated,
// identity-claiming log; this domain is compacted, arbitrates nothing, keeps no
// operation ledger and claims no identity, so each of those is a branch the
// framework declared and nothing had yet taken. Where the suite's own doc warns
// that a case may be encoding the first domain's shape as a universal rule,
// this is the domain that would find it.
func TestTheVectorDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  search.Domain{},
			Applier: search.NewApplier(),
			// Migrate is nil: these tables ship in the replicated
			// estate's own migration, so a fresh store already has
			// them. A domain that created its tables from test code
			// would be a schema the suite proved and the migration
			// did not.
			Encode: encodeSuiteRecord,
			Kinds:  suiteKinds(),
		}
	})
}

// suiteKinds is every source, derived from the enum rather than typed again.
func suiteKinds() []string {
	out := make([]string, 0, len(search.Sources))
	for _, s := range search.Sources {
		out = append(out, string(s))
	}
	return out
}

// encodeSuiteRecord builds one valid record at an arbitrary version.
//
// IT ENCODES AT ANY VERSION, including one above this build's, because that is
// exactly what a newer peer publishes and what this build has to retain.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	subject := search.Subject{Source: search.Source(kind), ID: id}
	const dim = 8
	rec := search.VectorRecord{
		RecordEnvelope: search.RecordEnvelope{
			V:         version,
			OpID:      opID,
			Subject:   subject,
			Op:        search.OpEmbed,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Gen:       1,
			Writer:    "suite-node",
			Scope: statelog.ScopeSet{
				Paths: []string{search.ScopePath("SUITE", subject)},
			},
		},
		Container: "SUITE",
		Model:     "suite-embed",
		Dim:       dim,
		SourceRev: 3,
		TextSHA:   "sha-" + id,
		Embedding: packed(vectorFor(id, dim)),
	}
	return rec.Encode()
}

// vectorFor is a deterministic vector for one id, so two encodes of the same
// record are byte-identical — which the suite's own idempotency case needs.
func vectorFor(id string, dim int) []float32 {
	out := make([]float32, dim)
	for i := range out {
		out[i] = float32((int(id[len(id)-1])+i)%7) - 3
	}
	return out
}

// packed is the little-endian float32 layout the column holds, written here
// rather than through store.DB.EncodeVector because a record is built before
// any database is opened.
func packed(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out
}

// AN EMBED RECORD REFUSES TO BE WRITTEN WITHOUT THE THREE VALUES THE CANDIDATE
// POOL FILTERS ON.
//
// `model`, `dim` and the vector itself are the stage-1 scan's whole predicate.
// A row missing any of them can never be scanned and can never be excluded
// either — it is not a document that ranks badly, it is a document that has
// silently left the corpus — and the write is the only place that can still
// name the source it belongs to.
func TestAnEmbedRecordWithoutItsPredicateIsRefused(t *testing.T) {
	t.Parallel()
	subject := search.Subject{Source: search.SourcePage, ID: "p1"}
	base := func() search.VectorRecord {
		return search.VectorRecord{
			RecordEnvelope: search.RecordEnvelope{
				Subject: subject, Op: search.OpEmbed, Gen: 1,
			},
			Container: "ENG",
			Model:     "text-embedding-3-large",
			Dim:       4,
			Embedding: packed([]float32{1, -1, 1, -1}),
		}
	}
	if _, err := base().Encode(); err != nil {
		t.Fatalf("the control record is refused: %v", err)
	}
	for name, break_ := range map[string]func(*search.VectorRecord){
		"no vector": func(r *search.VectorRecord) { r.Embedding = nil },
		"no model":  func(r *search.VectorRecord) { r.Model = "" },
		"no width":  func(r *search.VectorRecord) { r.Dim = 0 },
		"a width the vector is not": func(r *search.VectorRecord) {
			r.Dim = 8
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := base()
			break_(&rec)
			if _, err := rec.Encode(); err == nil {
				t.Fatal("encoded")
			}
		})
	}
}

// A SCOPE IS CARRIED, NEVER DERIVED BY A READER.
//
// The path is computable from the container and the subject, which makes it
// tempting to recompute rather than store — and that is exactly wrong: a build
// that CANNOT decode the payload still has to know what the record makes
// stale, and it has no container to derive from. A newer build that widened a
// record's scope would then have that scope silently narrowed by every older
// node that filed it.
func TestTheScopeSurvivesABuildThatCannotReadThePayload(t *testing.T) {
	t.Parallel()
	subject := search.Subject{Source: search.SourceTask, ID: "t1"}
	wider := []string{
		search.ScopePath("ENG", subject),
		// A path only a later build would know to write.
		search.ScopeRoot + statelog.ScopeSeparator + "ENG",
	}
	rec := search.VectorRecord{
		RecordEnvelope: search.RecordEnvelope{
			V: search.RecordVersion + 4, Subject: subject, Op: search.OpEmbed,
			Gen: 1, Scope: statelog.ScopeSet{Paths: wider},
		},
		Container: "ENG", Model: "m", Dim: 4,
		Embedding: packed([]float32{1, 1, 1, 1}),
	}
	payload, err := rec.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	env, err := search.Domain{}.Envelope(payload)
	if err != nil {
		t.Fatalf("a record four versions ahead did not yield an envelope: %v", err)
	}
	if got := len(env.Scope.Paths); got != len(wider) {
		t.Fatalf("the envelope kept %d scope path(s) of %d — a reader deriving "+
			"the scope from the container it cannot decode would keep one",
			got, len(wider))
	}
	if _, err := search.Decode(payload); err == nil {
		t.Fatal("a record above this build's version decoded in full")
	}
}

// A CONTAINERLESS SOURCE IS FILED UNDER A CONCRETE LEVEL.
//
// An empty container would collapse the path to one whose parent is the domain
// root, so a deferral on a document filed nowhere would cover — and block — a
// read scoped to any container in the company.
func TestAnUnfiledSourceDoesNotScopeToTheWholeCompany(t *testing.T) {
	t.Parallel()
	unfiled := search.ScopePath("", search.Subject{Source: search.SourcePage, ID: "p1"})
	filed := search.ScopePath("ENG", search.Subject{Source: search.SourcePage, ID: "p2"})
	if statelog.Covers(unfiled, filed) {
		t.Fatalf("a document with no container scopes to %q, which covers %q",
			unfiled, filed)
	}
	depth := func(p string) int { return len(statelog.Ancestors(p)) }
	if depth(unfiled) != depth(filed) {
		t.Fatalf("the unfiled path %q is %d levels deep and a filed one %q is "+
			"%d — one level of hierarchy is the difference between a scope "+
			"over one container and a scope over the company",
			unfiled, depth(unfiled), filed, depth(filed))
	}
	if fmt.Sprint(statelog.Ancestors(unfiled)[1]) ==
		fmt.Sprint(statelog.Ancestors(filed)[1]) {
		t.Fatal("an unfiled document shares its container level with a filed one")
	}
}
