package worklet_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// payloadAt is the body of the record the log holds at seq: the signed frame
// the broker stores, opened and verified as a backup opens it before handing
// it to the declaration's reader (ADR-0031, ADR-0075).
func (r *roundTrip) payloadAt(t *testing.T, seq uint64) []byte {
	t.Helper()
	_, payload, _, ok, err := statelog.VerifiedLog{Log: r.log, Verifier: r.verifier}.At(t.Context(), seq)
	if err != nil || !ok {
		t.Fatalf("read the log at %d: ok %v, %v", seq, ok, err)
	}
	return payload
}

// lastRecord is the newest record on the log.
func (r *roundTrip) lastRecord(t *testing.T) []byte {
	t.Helper()
	last, err := r.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return r.payloadAt(t, last)
}

// rewritten is payload with fn applied to its decoded top level.
func rewritten(t *testing.T, payload []byte, fn func(map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	fn(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A FILE RECORD NAMES THE OBJECT ITS ROW NAMES (ADR-0075). A backup reads the
// tracker's log past its store copy and carries every object a record names,
// because a restore replays those records into the rows — so what the
// declaration's reader says one record writes has to be what the applier
// wrote: the same row, spelled as the table's walk spells it, and the same
// object, or none for a removal. Read here off the bytes the real writer
// published, applied by the real applier, and compared with the real walk.
//
// AND EVERY OTHER RECORD ON THE LOG IS ANSWERED: none for one that writes no
// file, a refusal the backup recognises for a record this build cannot read
// that may write one — of any kind ([unreadableRecords]) — and the applier's
// own refusal for one it would not apply.
func TestAFileRecordNamesTheObjectItsRowNames(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	records := worklet.FileObjectReferences.Records
	// walked is the row the table's walk finds for a path, if any.
	walked := func(path string) (objstore.Reference, bool) {
		t.Helper()
		for _, refs := range r.every(t) {
			for _, ref := range refs {
				if ref.NamedBy == objstore.JoinOwner("ENG", path) {
					return ref, true
				}
			}
		}
		return objstore.Reference{}, false
	}
	// names checks that the newest record says it leaves path naming o,
	// and that the walk of the applied rows agrees.
	names := func(step, path string, o objstore.Object) []byte {
		t.Helper()
		payload := r.lastRecord(t)
		got, err := records(payload)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: Records = %+v, %v; want the one row it writes", step, got, err)
		}
		want := objstore.RecordReference{NamedBy: objstore.JoinOwner("ENG", path), Object: o, Names: true}
		if got[0] != want {
			t.Fatalf("%s: the record says it writes %+v, want %+v", step, got[0], want)
		}
		ref, ok := walked(path)
		if !ok || ref.Object != o || ref.NamedBy != want.NamedBy {
			t.Fatalf("%s: the applied row reads as %+v (found %v); the record said %+v", step, ref, ok, want)
		}
		return payload
	}

	put := names("a put", "notes/a.md", r.putFile("op-put", "notes/a.md", []byte("one")))
	names("a put replacing the content", "notes/a.md", r.putFile("op-replace", "notes/a.md", []byte("two")))

	if _, err := r.writer.RemoveFile(t.Context(), "op-remove", "ENG", "notes/a.md", worklet.NoIfMatch); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	r.drain()
	got, err := records(r.lastRecord(t))
	if err != nil || len(got) != 1 || got[0].Names ||
		got[0].NamedBy != objstore.JoinOwner("ENG", "notes/a.md") {
		t.Fatalf("a removal: Records = %+v, %v; want the row, naming nothing", got, err)
	}
	if ref, ok := walked("notes/a.md"); ok {
		t.Fatalf("the removed row is still walked as naming %s", ref.Object.Key)
	}

	// THE PROJECT'S CREATE is the log's first record, and writes no file.
	project := r.payloadAt(t, 1)
	if got, err := records(project); err != nil || got != nil {
		t.Errorf("a project record: Records = %+v, %v; want none", got, err)
	}
	barrier, err := worklet.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := records(barrier); err != nil || got != nil {
		t.Errorf("a barrier: Records = %+v, %v; want none", got, err)
	}

	newer := func(payload []byte) []byte {
		return rewritten(t, payload, func(doc map[string]any) { doc["v"] = worklet.RecordVersion + 1 })
	}
	if _, err := records(newer(put)); !errors.Is(err, objstore.ErrRecordUnreadable) {
		t.Errorf("a file record a newer build wrote: %v; want ErrRecordUnreadable", err)
	}
	if got, err := records(newer(project)); err != nil || got != nil {
		t.Errorf("a project record a newer build wrote: Records = %+v, %v; want none — its "+
			"scope is the project object alone, and a file is not that object", got, err)
	}
	unreadableRecords(t, records, put, project)
	elsewhere := rewritten(t, put, func(doc map[string]any) {
		doc["mutation"].(map[string]any)["path"] = "notes/b.md"
	})
	if _, err := records(elsewhere); err == nil {
		t.Error("a file record whose payload claims another path than its subject was read; " +
			"the applier refuses it")
	}
	if _, err := records([]byte("not a record")); err == nil {
		t.Error("bytes no build reads were answered as naming nothing")
	}
}

// unreadableRecords holds what a record this build cannot read is answered as:
// WHETHER IT MAY WRITE A FILE is read from what every build reads of it — its
// kind and the scope its writer stated, the complete set of objects its apply
// may write — and never from what this build's applier does with the kind,
// which says nothing about what a newer build's does.
func unreadableRecords(t *testing.T, records func([]byte) ([]objstore.RecordReference, error),
	put, project []byte) {
	t.Helper()
	at := func(payload []byte, v int, fn func(map[string]any)) []byte {
		return rewritten(t, payload, func(doc map[string]any) {
			doc["v"] = v
			if fn != nil {
				fn(doc)
			}
		})
	}
	kind := func(k string) func(map[string]any) {
		return func(doc map[string]any) { doc["subject"].(map[string]any)["kind"] = k }
	}
	scope := func(terms ...map[string]any) func(map[string]any) {
		return func(doc map[string]any) { doc["scope"] = terms }
	}
	file := worklet.FileSubject("ENG", "notes/a.md")
	_, token, _ := worklet.SplitFileID(file.ID)
	newer := worklet.RecordVersion + 1
	for _, c := range []struct {
		name    string
		payload []byte
		refused bool
	}{
		// A KIND THIS BUILD HAS NEVER HEARD OF: it cannot say what a
		// newer build's applier writes for it, nor even which object the
		// exactly-the-subject sentinel names.
		{"a kind this build does not know, a newer build's", at(put, newer, kind("filecopy")), true},
		{"a kind this build does not know, at a version it reads", at(put, worklet.RecordVersion, kind("filecopy")), true},
		{"a kind this build does not know, on a project's subject", at(project, newer, kind("archive")), true},
		// A KNOWN KIND IS ANSWERED BY ITS SCOPE, which a newer build's
		// writer states: the sentinel is its own object, so only a file
		// subject reaches a file; an enumeration reaches one through a
		// container, the domain, a term this build cannot interpret or a
		// file's own object.
		{"a project record reaching its container", at(project, newer, scope(
			map[string]any{"k": "container", "i": "ENG"})), true},
		{"a record reaching the domain", at(project, newer, scope(
			map[string]any{"k": "domain"})), true},
		{"a record reaching a term kind this build does not know", at(project, newer, scope(
			map[string]any{"k": "shelf", "i": "ENG"})), true},
		{"a record reaching a file's own object", at(project, newer, scope(
			map[string]any{"k": "object", "c": "ENG", "i": token})), true},
		{"a record reaching tasks and keys alone", at(project, newer, scope(
			map[string]any{"k": "object", "c": "ENG", "i": "0199e3c2-5b8a-7c4d-9e1f-2a3b4c5d6e7f"},
			map[string]any{"k": "key", "c": "ENG", "i": "ENG-142"},
			map[string]any{"k": "family", "i": "person"})), false},
	} {
		got, err := records(c.payload)
		switch {
		case c.refused && !errors.Is(err, objstore.ErrRecordUnreadable):
			t.Errorf("%s: Records = %+v, %v; want ErrRecordUnreadable", c.name, got, err)
		case !c.refused && (err != nil || got != nil):
			t.Errorf("%s: Records = %+v, %v; want none — nothing it may write is a file",
				c.name, got, err)
		}
	}
}
