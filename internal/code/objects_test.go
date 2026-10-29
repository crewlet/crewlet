package code_test

import (
	"encoding/json"
	"errors"
	"slices"
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

// A RECORD'S PACK REFERENCES ARE WHAT ITS APPLY WRITES: a push record names the
// row the applier writes for each of its pushes' packs — a person's, a seat's
// and its coding box's, in the record's order — spelled as the walk spells it;
// a compaction names each output's row and tells each input's removed; every
// other record this build reads — a sweep of retired packs among them — names
// none; and a record this build cannot read that may reach a repository
// refuses — the backup's ADR-0052 contract.
func TestARecordNamesThePackRowsItsApplyWrites(t *testing.T) {
	t.Parallel()
	records := code.PackReferences.Records
	push, err := encodeSuiteRecord(string(code.KindRepo), "repo-a", "op-1", code.RecordVersion)
	if err != nil {
		t.Fatal(err)
	}
	got, err := records(push)
	entries := suitePush("repo-a").Pushes
	if err != nil || len(got) != len(entries) {
		t.Fatalf("a push record's references = %+v, %v; want one for each of its %d pushes' packs", got, err,
			len(entries))
	}
	for i, e := range entries {
		if r := got[i]; !r.Names || r.Object != e.Pack.Content() ||
			r.NamedBy != objstore.JoinOwner("repo-a", e.Pack.PackID, "pack") {
			t.Errorf("a push record's reference %d = %+v; want push %s's pack row", i, r, e.PushID)
		}
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
	// A SWEEP removes rows of code_packs_retired and writes none of
	// code_packs, so it names no pack row either.
	through, err := json.Marshal(code.RetireSweep{RepoID: "repo-a",
		Through: statelog.Position{Stream: code.Domain{}.Stream().Name, Generation: 1, Seq: 4}})
	if err != nil {
		t.Fatal(err)
	}
	sweep := mustEncode(t, code.MutationRecord{RecordEnvelope: code.RecordEnvelope{
		OpID: "op-5", Subject: code.RepoSubject("repo-a"), Op: code.OpRetireSweep, Gen: 1,
		Scope: code.Scope{code.RepoPath("repo-a")},
	}, Mutation: through})
	if got, err := records(sweep); err != nil || len(got) != 0 {
		t.Errorf("a sweep's references = %+v, %v; want none", got, err)
	}

	// A COMPACTION writes a row for each output, which names its bundle, and
	// removes its inputs' rows, which then name nothing: a backup's walk
	// carries every output a compaction made live and reads the inputs as
	// gone from code_packs.
	cid := statelog.NewOpID(harnessAt, "")
	outputs := []code.PackRef{compactOutput(cid, 0, 2, harnessAt), compactOutput(cid, 1, 1, harnessAt)}
	compaction, err := json.Marshal(code.Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a",
		Seen:   statelog.Position{Stream: code.Domain{}.Stream().Name, Generation: 1, Seq: 4},
		Retire: []string{"push-a", "push-b", "push-c"}, Add: outputs})
	if err != nil {
		t.Fatal(err)
	}
	got, err = records(mustEncode(t, code.MutationRecord{RecordEnvelope: code.RecordEnvelope{
		OpID: cid, Subject: code.RepoSubject("repo-a"), Op: code.OpCompact, Gen: 1,
		Scope: code.Scope{code.RepoPath("repo-a")},
	}, Mutation: compaction}))
	if err != nil {
		t.Fatalf("a compaction's references: %v", err)
	}
	compacted := []objstore.RecordReference{
		{NamedBy: objstore.JoinOwner("repo-a", outputs[0].PackID, "pack"), Object: outputs[0].Content(), Names: true},
		{NamedBy: objstore.JoinOwner("repo-a", outputs[1].PackID, "pack"), Object: outputs[1].Content(), Names: true},
		{NamedBy: objstore.JoinOwner("repo-a", "push-a", "pack")},
		{NamedBy: objstore.JoinOwner("repo-a", "push-b", "pack")},
		{NamedBy: objstore.JoinOwner("repo-a", "push-c", "pack")},
	}
	if !slices.Equal(got, compacted) {
		t.Errorf("a compaction's references = %+v, want its outputs named and its inputs removed: %+v", got,
			compacted)
	}

	// A PUBLICATION writes code_packs exactly as a compaction does — its
	// repack's outputs named, its inputs removed — and a layer-only one none:
	// the layer names its objects in code_layer_parts, a Derived table no
	// backup reads.
	layer := &code.LayerRef{LayerID: oidOf("layer"), Packs: []string{outputs[0].GitChecksum},
		Parts:     []code.LayerPart{{Object: keyOf("part"), Hash: objstore.HashOf([]byte("part")), Size: 3}},
		MidxBytes: 2, BitmapBytes: 1, Objects: 2, Git: "2.56.0"}
	for name, tc := range map[string]struct {
		c    code.Compaction
		want []objstore.RecordReference
	}{
		"a publication with a repack": {code.Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a",
			Seen:   statelog.Position{Stream: code.Domain{}.Stream().Name, Generation: 1, Seq: 4},
			Retire: []string{"push-a", "push-b", "push-c"}, Add: outputs, Layer: layer}, compacted},
		"a layer-only publication": {code.Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a",
			Seen:  statelog.Position{Stream: code.Domain{}.Stream().Name, Generation: 1, Seq: 4},
			Layer: layer}, nil},
	} {
		body, err := json.Marshal(tc.c)
		if err != nil {
			t.Fatal(err)
		}
		got, err := records(mustEncode(t, code.MutationRecord{RecordEnvelope: code.RecordEnvelope{
			OpID: tc.c.CompactionID, Subject: code.RepoSubject("repo-a"), Op: code.OpCompact, Gen: 1,
			Scope: code.Scope{code.RepoPath("repo-a")},
		}, Mutation: body}))
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s's references = %+v, %v; want %+v", name, got, err, tc.want)
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

// AN EXPUNGE RECORD NAMES THE PACKS ITS APPLY WRITES: a begin names a row for
// each pack it keeps and each it leaves dropping — a dropping pack is a row of
// code_packs that names its object as a live one does, until its commit — and
// tells each input's row removed; a commit names its rescue's rows and tells
// its dropping packs' removed; and a request writes no pack row at all. So a
// backup taken while an expunge is open carries the dropping packs a restore's
// rows name, and one taken after its commit reads them as gone.
//
// Mutation: answer a begin's kept packs alone, and a backup taken mid-expunge
// misses the dropping packs its restore's rows name.
func TestAnExpungeRecordNamesThePacksItsApplyWrites(t *testing.T) {
	t.Parallel()
	records := code.PackReferences.Records
	seen := statelog.Position{Stream: code.Domain{}.Stream().Name, Generation: 1, Seq: 4}
	xid := statelog.NewOpID(harnessAt, "")
	kept, dropping := compactOutput(xid, 0, 2, harnessAt), compactOutput(xid, 1, 1, harnessAt)
	cid := statelog.NewOpID(harnessAt, "")
	rescue := compactOutput(cid, 0, 1, harnessAt)
	named := func(p code.PackRef) objstore.RecordReference {
		return objstore.RecordReference{NamedBy: objstore.JoinOwner("repo-a", p.PackID, "pack"),
			Object: p.Content(), Names: true}
	}
	removed := func(id string) objstore.RecordReference {
		return objstore.RecordReference{NamedBy: objstore.JoinOwner("repo-a", id, "pack")}
	}
	for name, tc := range map[string]struct {
		op      code.OpKind
		opID    string
		payload any
		want    []objstore.RecordReference
	}{
		"a begin": {code.OpExpungeBegin, xid, code.ExpungeBegin{RepoID: "repo-a", ExpungeID: xid, Node: "node-a",
			Seen: seen, DropEpoch: 1, Grace: code.PruneGrace, Retire: []string{"push-a", "push-b"},
			Add: []code.PackRef{kept}, Dropping: []code.PackRef{dropping}},
			[]objstore.RecordReference{named(kept), named(dropping), removed("push-a"), removed("push-b")}},
		"a commit": {code.OpExpungeCommit, cid, code.ExpungeCommit{RepoID: "repo-a", ExpungeID: xid,
			CommitID: cid, Node: "node-a", Seen: seen, DropEpoch: 1, Retire: []string{dropping.PackID},
			Add: []code.PackRef{rescue}},
			[]objstore.RecordReference{named(rescue), removed(dropping.PackID)}},
		"a request": {code.OpExpungeRequest, "op-request", code.ExpungeRequest{RepoID: "repo-a",
			RequestID: "op-request", By: asker}, nil},
	} {
		body, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		got, err := records(mustEncode(t, code.MutationRecord{RecordEnvelope: code.RecordEnvelope{
			OpID: tc.opID, Subject: code.RepoSubject("repo-a"), Op: tc.op, Gen: 1,
			Scope: code.Scope{code.RepoPath("repo-a")},
		}, Mutation: body}))
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s's references = %+v, %v; want %+v", name, got, err, tc.want)
		}
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
