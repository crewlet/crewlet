package code_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PACK REFERENCES ARE READ THROUGH THE CODE ESTATE, as the collector, its
// audit and the backup read them — through the declaration and the statements
// it builds, never a query of this package's — from a push's pack to its
// purge.
func TestThePackReferencesAreReadThroughTheCodeEstate(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	push := entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1")))
	n.push(id, push)
	sources, err := collect.Sources([]objstore.ReferenceTable{code.PackReferences},
		code.ObjectEstate{Reader: n.reader})
	if err != nil {
		t.Fatalf("the collector's sources: %v", err)
	}
	// AT THIS NODE'S OWN POSITION: the barrier is a linearizable read, which
	// a read authority with no read index — this harness's — refuses.
	src, at := sources[0], n.waiter.Committed()
	key, unnamed := push.Pack.Object, keyOf("nobody")
	named, complete, err := src.Referenced(t.Context(), []objstore.Key{key, unnamed}, at)
	if err != nil || !complete {
		t.Fatalf("Referenced = %v (complete %v)", err, complete)
	}
	if _, ok := named[key]; !ok {
		t.Fatal("the pack a push stored is not referenced — the collector would delete it")
	}
	if _, ok := named[unnamed]; ok {
		t.Fatal("a key no row names is referenced")
	}

	var refs []objstore.Reference
	for _, w := range src.Required(at) {
		for {
			page, complete, err := w.Next(t.Context())
			if err != nil || !complete {
				t.Fatalf("the audit walk = %v (complete %v)", err, complete)
			}
			if len(page) == 0 {
				break
			}
			refs = append(refs, page...)
		}
	}
	if len(refs) != 1 || refs[0].Object != push.Pack.Content() ||
		refs[0].NamedBy != objstore.JoinOwner(id, push.Pack.PackID, "pack") {
		t.Fatalf("the audit walk found %+v, want the pack named %s", refs,
			objstore.JoinOwner(id, push.Pack.PackID, "pack"))
	}

	// THE PURGE TAKES THE REFERENCE WITH IT, and the collector then deletes
	// the object a day after it was written, as it deletes anything no row
	// names.
	if _, err := n.store.DeleteRepo(t.Context(), n.activate(t2), id); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	if _, err := n.publish(code.RepoSubject(id), code.OpPurge,
		code.Scope{code.NamePath(code.NameToken("app")), code.RepoPath(id)},
		code.Purge{RepoID: id, Name: "app", Reason: "the test"}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	named, _, err = src.Referenced(t.Context(), []objstore.Key{key}, statelog.Position{})
	if err != nil {
		t.Fatalf("Referenced after the purge: %v", err)
	}
	if _, ok := named[key]; ok {
		t.Fatal("a purged repository's pack is still referenced")
	}
}

// A RECORD'S PACK REFERENCES ARE WHAT ITS APPLY WRITES: a push names the row
// the applier writes for its pack, spelled as the walk spells it; every other
// record this build reads names none; and a record this build cannot read
// that may reach a repository refuses — the backup's ADR-0034 contract.
func TestARecordNamesThePackRowsItsApplyWrites(t *testing.T) {
	t.Parallel()
	records := code.PackReferences.Records
	push, err := encodeSuiteRecord(string(code.KindRepo), "repo-a", "op-1", code.RecordVersion)
	if err != nil {
		t.Fatal(err)
	}
	got, err := records(push)
	want := suitePack("repo-a")
	if err != nil || len(got) != 1 || !got[0].Names || got[0].Object != want.Content() ||
		got[0].NamedBy != objstore.JoinOwner("repo-a", want.PackID, "pack") {
		t.Fatalf("a push's references = %+v, %v; want its pack's row", got, err)
	}
	for _, kind := range []code.ObjectKind{code.KindName, code.KindEviction, code.KindGeneration, code.KindCompany} {
		body, err := encodeSuiteRecord(string(kind), "repo-a", "op-1", code.RecordVersion)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := records(body); err != nil || len(got) != 0 {
			t.Errorf("a %s record's references = %+v, %v; want none", kind, got, err)
		}
	}

	// A NEWER BUILD'S PUSH: it may name a pack, and this build cannot say
	// which.
	future, err := encodeSuiteRecord(string(code.KindRepo), "repo-a", "op-2", code.RecordVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := records(future); !errors.Is(err, objstore.ErrRecordUnreadable) {
		t.Fatalf("a newer push = %v, want ErrRecordUnreadable", err)
	}
	// A KIND THIS BUILD HAS NEVER HEARD OF, scoped to the name claims alone:
	// still unreadable, since what a kind it does not know writes is
	// unknowable here.
	unknown := mustEncode(t, code.MutationRecord{RecordEnvelope: code.RecordEnvelope{
		V: code.RecordVersion + 1, OpID: "op-3", Subject: code.Subject{Kind: "mirror", ID: "x"},
		Op: "sync", Gen: 1, Scope: code.Scope{code.NamePath("abc")},
	}})
	if _, err := records(unknown); !errors.Is(err, objstore.ErrRecordUnreadable) {
		t.Fatalf("an unknown kind = %v, want ErrRecordUnreadable", err)
	}
	// A KNOWN KIND AT A NEWER VERSION whose writer scoped it to a name's
	// claim reaches no repository, so it names no pack row.
	claim := mustEncode(t, code.MutationRecord{RecordEnvelope: code.RecordEnvelope{
		V: code.RecordVersion + 1, OpID: "op-4", Subject: code.NameSubject("app"),
		Op: code.OpCreate, Gen: 1, Scope: code.Scope{code.NamePath(code.NameToken("app"))},
	}, Mutation: json.RawMessage(`{"novel":1}`)})
	if got, err := records(claim); err != nil || len(got) != 0 {
		t.Fatalf("a newer record scoped to a name's claim = %+v, %v; want none", got, err)
	}
}

// mustEncode encodes a hand-built record.
func mustEncode(t *testing.T, rec code.MutationRecord) []byte {
	t.Helper()
	body, err := code.Encode(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return body
}
