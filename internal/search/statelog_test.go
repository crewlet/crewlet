package search_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
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
			Domain:     search.Domain{},
			Generation: search.GenerationRecord{},
			Applier:    search.NewApplier(),
			// Migrate is nil: these tables ship in the replicated
			// estate's own migration, so a fresh store already has
			// them. A domain that created its tables from test code
			// would be a schema the suite proved and the migration
			// did not.
			Encode: encodeSuiteRecord,
			Kinds:  suiteKinds(),
			Rows:   search.NewRows,
			Write:  suiteWrite,
		}
	})
}

// suiteWrite is one tick of the domain's own [search.Embedder] — the only
// writer this domain has — over one document that needs a vector.
//
// THE TICK LOGS A FAILED BATCH RATHER THAN RETURNING IT, by design, so the
// count is what says whether anything was published: a record the publisher
// refused is a batch that published nothing, and a tick that published
// nothing certifies nothing.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.PartitionReader) error {
	duty, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: pub, Estate: db, Log: statelog.EstateStream(search.Domain{}).Name,
		// THE SUITE'S ONE NODE, applied through its own end: the corpus is
		// one document, below the index's minimum, so the step decides
		// nothing whatever it reads.
		Standing: func(context.Context) (search.LogStanding, error) {
			return search.LogStanding{Current: true,
				Readers: map[string]int{"suite-node": search.RecordVersion}}, nil
		},
		Embedder: embeddings.NewFake(8), Model: "suite-embed",
		Corpora: []search.Corpus{oneStaleDocument{}},
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Budget:  unbounded{},
	})
	if err != nil {
		return err
	}
	published, err := duty.Tick(ctx)
	if err != nil {
		return err
	}
	if published == 0 {
		return fmt.Errorf("the embed duty published nothing for a document that " +
			"had no vector — its log says why")
	}
	return nil
}

// oneStaleDocument is a corpus of one task with no vector yet.
type oneStaleDocument struct{}

func (oneStaleDocument) Source() search.Source { return search.SourceTask }

func (oneStaleDocument) Stale(context.Context, string, int, int) ([]search.Document, []string, error) {
	return []search.Document{{
		ID: "suite-task", Container: "SUITE", Version: 1,
		Title: "The suite's task", Body: "Something to embed.",
	}}, nil, nil
}

func (oneStaleDocument) Coverage(context.Context, string, int) (int, int, error) {
	return 0, 1, nil
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
	for name, mutate := range map[string]func(*search.VectorRecord){
		"no vector": func(r *search.VectorRecord) { r.Embedding = nil },
		"no model":  func(r *search.VectorRecord) { r.Model = "" },
		"no width":  func(r *search.VectorRecord) { r.Dim = 0 },
		"a width the vector is not": func(r *search.VectorRecord) {
			r.Dim = 8
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := base()
			mutate(&rec)
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

// A KIND A NEWER BUILD ADDED IS AN ENVELOPE THIS BUILD FILES, NEVER A STOP.
//
// The envelope is the half every build reads, and a failure to read it stops
// the applier outright — the framework has no position, kind or scope to
// retain the record under. So the envelope asks only whether the subject is a
// subject; whether its KIND is one this build writes is the version-gated
// second pass's question. Asked of the envelope, every kind a later build adds
// — the index records of ADR-0028 were the first — stops every older node of
// a rolling upgrade instead of being deferred, which is what [search.Source]
// promises.
func TestAKindANewerBuildAddedIsDeferredNotStopped(t *testing.T) {
	t.Parallel()
	record := func(version int) []byte {
		return []byte(fmt.Sprintf(`{"v":%d,"op_id":"later","subject":`+
			`{"source":"file","id":"f-1"},"op":"embed","gen":1,`+
			`"scope":{"Paths":["v/ENG/file.f-1"]}}`, version))
	}

	domain := search.Domain{}
	later := record(search.RecordVersion + 1)
	env, err := domain.Envelope(later)
	if err != nil {
		t.Fatalf("a record of a kind a newer build writes, at that build's "+
			"version, yielded no envelope — so the applier stops on it rather "+
			"than retaining it: %v", err)
	}
	if env.Kind != "file" || env.Subject.ID != "f-1" || len(env.Scope.Paths) != 1 {
		t.Fatalf("the envelope reads kind %q, subject %q and %d scope path(s)",
			env.Kind, env.Subject.ID, len(env.Scope.Paths))
	}
	var future *search.ErrFutureVersion
	if _, err := search.Decode(later); !errors.As(err, &future) {
		t.Fatalf("the payload of a newer build's record decoded as %v, not as "+
			"a version this build retains", err)
	}

	// AT A VERSION THIS BUILD READS, the same kind is a writer fault: a build
	// adding a kind states it above every build that cannot read it.
	current := record(search.RecordVersion)
	if _, err := domain.Envelope(current); err != nil {
		t.Fatalf("the envelope of a record at this build's own version refused "+
			"its kind — that is the payload's question: %v", err)
	}
	if _, err := search.Decode(current); err == nil || errors.As(err, &future) {
		t.Fatalf("a kind this build does not write, at a version it reads, "+
			"decoded as %v", err)
	}

	// AND A SUBJECT THAT IS NOT A SUBJECT STILL HAS NO ENVELOPE.
	malformed := map[string]string{
		"an empty kind":     `{"source":"","id":"f-1"}`,
		"a wildcard kind":   `{"source":"fi>le","id":"f-1"}`,
		"a wildcard id":     `{"source":"file","id":"f.*"}`,
		"whitespace in one": `{"source":"file","id":"f 1"}`,
	}
	for name, subject := range malformed {
		payload := []byte(`{"v":2,"subject":` + subject + `,"op":"embed",` +
			`"scope":{"Paths":["v/ENG/x"]}}`)
		if _, err := domain.Envelope(payload); err == nil {
			t.Errorf("%s: an envelope decoded from a subject no wire can carry", name)
		}
	}
}
