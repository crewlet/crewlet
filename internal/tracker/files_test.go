package tracker_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

var stale = statelog.Freshness{Level: statelog.ReadStale}

// objectOf is the object an upload of content would answer, under a key
// minted at the harness's clock: the tracker never reads a byte, so a test of
// it needs only the names.
func (r *roundTrip) objectOf(content []byte) objstore.Object {
	r.t.Helper()
	return objstore.Object{Key: objstore.KeyAt(r.at), Hash: objstore.HashOf(content),
		Size: int64(len(content))}
}

func (r *roundTrip) putFile(op, path string, content []byte) objstore.Object {
	r.t.Helper()
	o := r.objectOf(content)
	if _, err := r.writer.PutFile(r.t.Context(), op, tracker.FilePut{
		Project: "ENG", Path: path, ContentType: "text/markdown", Object: o,
	}); err != nil {
		r.t.Fatalf("PutFile %s: %v", path, err)
	}
	r.drain()
	return o
}

// named is 1 when the collector's references read finds k named, and 0 when
// it does not — what the collector deletes by.
func (r *roundTrip) named(t *testing.T, k objstore.Key) int {
	t.Helper()
	if r.referenced(t, k)[k] {
		return 1
	}
	return 0
}

// referenced is which of among the collector's references read finds named.
func (r *roundTrip) referenced(t *testing.T, among ...objstore.Key) map[objstore.Key]bool {
	t.Helper()
	// THROUGH THE DECLARED LIST, exactly as the engine builds the
	// collector's sources, so this reads the statement the collector runs.
	sources, err := collect.Sources(references.All, tracker.ObjectEstate{Reader: r.reader})
	if err != nil {
		t.Fatal(err)
	}
	set, complete, err := sources[0].Referenced(t.Context(), among, statelog.Position{})
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Fatal("the references read reported itself incomplete on a node holding every record")
	}
	out := make(map[objstore.Key]bool, len(set))
	for k := range set {
		out[k] = true
	}
	return out
}

// every is every object the collector's audit walk finds named, with how
// often it was visited.
func (r *roundTrip) every(t *testing.T) map[objstore.Key]int {
	t.Helper()
	sources, err := collect.Sources(references.All, tracker.ObjectEstate{Reader: r.reader})
	if err != nil {
		t.Fatal(err)
	}
	out := map[objstore.Key]int{}
	complete, err := sources[0].Each(t.Context(), statelog.Position{}, func(k objstore.Key) error {
		out[k]++
		return nil
	})
	if err != nil || !complete {
		t.Fatalf("the audit walk = %v (complete %v)", err, complete)
	}
	return out
}

// A FILE PUT, READ BACK AND REMOVED: the row, the object it names and the
// reference the object store keeps the bytes alive by, at every step.
func TestAFileIsWrittenReadAndRemoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	content := bytes.Repeat([]byte("a quarterly plan\n"), 100_000)
	o := r.objectOf(content)
	written, err := r.writer.PutFile(t.Context(), "op-put", tracker.FilePut{
		Project: "ENG", Path: "/reports/q3 plan.md", ContentType: "text/markdown", Object: o,
	})
	if err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	r.drain()
	if written.Position.Seq == 0 {
		t.Fatal("a put landed at no position")
	}

	detail, err := r.reader.File(t.Context(), "eng", "reports/q3 plan.md", stale)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	f := detail.File
	if f.Path != "reports/q3 plan.md" || f.Project != "ENG" || f.Size != int64(len(content)) ||
		f.Hash != o.Hash || f.Object != o.Key || f.ContentType != "text/markdown" {
		t.Fatalf("read back %+v", f)
	}
	if got, named := f.Content(); !named || got != o {
		t.Fatalf("the file's content is %+v (%v), want the object put %+v", got, named, o)
	}
	if r.named(t, o.Key) != 1 {
		t.Fatalf("the object of a live file is not referenced")
	}

	listing, err := r.reader.Files(t.Context(), tracker.FileQuery{Project: "ENG", Freshness: stale})
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(listing.Files) != 1 || listing.Files[0].Path != "reports/q3 plan.md" ||
		listing.Files[0].Version != f.Version || !listing.Complete {
		t.Fatalf("listing = %+v", listing)
	}

	if _, err := r.writer.RemoveFile(t.Context(), "op-rm", "ENG", "reports/q3 plan.md",
		tracker.NoIfMatch); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	r.drain()
	if _, err := r.reader.File(t.Context(), "ENG", "reports/q3 plan.md", stale); !errors.Is(err, tracker.ErrNoFile) {
		t.Fatalf("File after removal = %v, want ErrNoFile", err)
	}
	if r.named(t, o.Key) != 0 {
		t.Fatalf("the object of a removed file is still referenced, so its bytes are never collected")
	}
	listing, err = r.reader.Files(t.Context(), tracker.FileQuery{Project: "ENG", Freshness: stale})
	if err != nil || len(listing.Files) != 0 {
		t.Fatalf("listing after removal = %+v, %v", listing, err)
	}
	listing, err = r.reader.Files(t.Context(), tracker.FileQuery{Project: "ENG",
		Removed: true, Freshness: stale})
	if err != nil || len(listing.Files) != 1 || listing.Files[0].RemovedAt == nil ||
		listing.Files[0].RemovedBy == "" {
		t.Fatalf("listing of removed files = %+v, %v", listing, err)
	}

	// A PUT AT THE SAME PATH BRINGS IT BACK — the address is meant to be
	// used again — naming the new upload's own object.
	again := r.putFile("op-put-again", "reports/q3 plan.md", []byte("the plan, rewritten"))
	if _, err := r.reader.File(t.Context(), "ENG", "reports/q3 plan.md", stale); err != nil {
		t.Fatalf("File after a put over a removal: %v", err)
	}
	if r.named(t, again.Key) != 1 {
		t.Fatal("the new content's object is not referenced")
	}
}

// THE COLLECTOR'S TWO READS AGREE WITH WHAT WAS WRITTEN: a batch question
// answers exactly the objects of it some file names, and the audit's walk
// answers every object named, each once, across more than one of its pages. An
// object the batch read missed would be deleted; one the walk missed would
// never be counted lost.
func TestTheCollectorsReadsFindEveryNamedObject(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	var keys []objstore.Key
	const files = 520 // past one of the walk's 500-row pages
	for i := range files {
		o := r.objectOf([]byte(fmt.Sprintf("object %d of the reference fixture", i)))
		if _, err := r.writer.PutFile(t.Context(), fmt.Sprintf("op-%d", i), tracker.FilePut{
			Project: "ENG", Path: fmt.Sprintf("refs/%d.md", i), Object: o,
		}); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, o.Key)
	}
	r.drain()
	// AND A REMOVED FILE, whose NULL no statement may answer.
	if _, err := r.writer.RemoveFile(t.Context(), "op-rm", "ENG", "refs/0.md", tracker.NoIfMatch); err != nil {
		t.Fatal(err)
	}
	r.drain()
	stranger := objstore.KeyAt(r.at)
	got := r.referenced(t, append([]objstore.Key{stranger}, keys...)...)
	if got[stranger] {
		t.Error("an object no file names was answered named")
	}
	if got[keys[0]] {
		t.Error("the object of a removed file was answered named")
	}
	for _, k := range keys[1:] {
		if !got[k] {
			t.Errorf("object %s is named and the batch read missed it", k)
		}
	}
	all := r.every(t)
	if len(all) != files-1 {
		t.Fatalf("the audit walk answers %d objects, want the %d live", len(all), files-1)
	}
	for k, n := range all {
		if n != 1 {
			t.Fatalf("the audit walk visited %s %d times", k, n)
		}
	}
}

// A PATH IS ONE FILE: a second put replaces the first and keeps who made it,
// and the object the first content was in stops being referenced.
func TestAPutOverAFileReplacesItsContent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	firstObject := r.putFile("op-1", "notes.txt", []byte("first"))
	first, err := r.reader.File(t.Context(), "ENG", "notes.txt", stale)
	if err != nil {
		t.Fatal(err)
	}
	editor := r.writer.As("bo", tracker.AuthorAgent, tracker.Provenance{})
	second := r.objectOf([]byte("second"))
	if _, err := editor.PutFile(t.Context(), "op-2", tracker.FilePut{
		Project: "ENG", Path: "notes.txt", Object: second,
	}); err != nil {
		t.Fatal(err)
	}
	r.drain()
	read, err := r.reader.File(t.Context(), "ENG", "notes.txt", stale)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := read.File.Content(); got != second {
		t.Fatalf("the content was not replaced: %+v", read.File)
	}
	if read.File.CreatedBy != first.File.CreatedBy || !read.File.CreatedAt.Equal(first.File.CreatedAt) {
		t.Fatalf("a put re-attributed the file: created by %q at %s, was %q at %s",
			read.File.CreatedBy, read.File.CreatedAt, first.File.CreatedBy, first.File.CreatedAt)
	}
	if read.File.UpdatedBy != "bo" {
		t.Fatalf("updated by %q, want bo", read.File.UpdatedBy)
	}
	if r.named(t, firstObject.Key) != 0 {
		t.Fatal("the replaced content's object is still referenced")
	}
	listing, err := r.reader.Files(t.Context(), tracker.FileQuery{Project: "ENG", Freshness: stale})
	if err != nil || len(listing.Files) != 1 {
		t.Fatalf("one path is %d files: %+v, %v", len(listing.Files), listing, err)
	}
}

// A PUT CONDITIONED ON A VERSION THAT MOVED IS REFUSED, so two editors of one
// file cannot silently overwrite each other; and one conditioned on a file
// that is not there is refused too, because the caller read something that is
// gone.
func TestAConditionedPutIsRefusedWhenTheFileMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.putFile("op-1", "plan.md", []byte("v1"))
	read, err := r.reader.File(t.Context(), "ENG", "plan.md", stale)
	if err != nil {
		t.Fatal(err)
	}
	r.putFile("op-2", "plan.md", []byte("v2"))
	_, err = r.writer.PutFile(t.Context(), "op-3", tracker.FilePut{
		Project: "ENG", Path: "plan.md", Object: r.objectOf([]byte("v3")),
		IfMatch: read.File.Version,
	})
	if !errors.Is(err, tracker.ErrStaleVersion) {
		t.Fatalf("a put conditioned on a moved version = %v, want ErrStaleVersion", err)
	}
	_, err = r.writer.PutFile(t.Context(), "op-4", tracker.FilePut{
		Project: "ENG", Path: "never.md", Object: r.objectOf([]byte("x")), IfMatch: 7,
	})
	if !errors.Is(err, tracker.ErrStaleVersion) {
		t.Fatalf("a conditioned put onto nothing = %v, want ErrStaleVersion", err)
	}
}

// A FILE NEEDS A PROJECT THAT IS THERE AND TAKES WORK, and removing one that
// is not there says so rather than succeeding quietly.
func TestAFileIsRefusedOutsideALiveProject(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	_, err := r.writer.PutFile(t.Context(), "op-1", tracker.FilePut{
		Project: "NOPE", Path: "a.txt", Object: r.objectOf([]byte("a")),
	})
	if !errors.Is(err, tracker.ErrNoProject) {
		t.Fatalf("a put into an unknown project = %v, want ErrNoProject", err)
	}
	if _, err := r.writer.RemoveFile(t.Context(), "op-2", "ENG", "absent.txt",
		tracker.NoIfMatch); !errors.Is(err, tracker.ErrNoFile) {
		t.Fatalf("removing an absent file = %v, want ErrNoFile", err)
	}
	if _, err := r.reader.Files(t.Context(), tracker.FileQuery{Project: "NOPE",
		Freshness: stale}); !errors.Is(err, tracker.ErrNoProject) {
		t.Fatalf("listing an unknown project = %v, want ErrNoProject", err)
	}
}

// A LISTING PAGES IN PATH ORDER AND A FOLDER IS A PREFIX OF WHOLE SEGMENTS —
// `reports` holds `reports/a.md` and not `reportsheet.md`.
func TestAListingPagesAndFolders(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, p := range []string{"reports/b.md", "reports/a.md", "reportsheet.md",
		"reports/2026/q1.md", "zeta.txt"} {
		r.putFile("op-"+p, p, []byte(p))
	}
	listing, err := r.reader.Files(t.Context(), tracker.FileQuery{Project: "ENG",
		Folder: "reports/", Freshness: stale})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range listing.Files {
		got = append(got, f.Path)
	}
	if strings.Join(got, ",") != "reports/2026/q1.md,reports/a.md,reports/b.md" {
		t.Fatalf("folder listing = %v", got)
	}

	var all []string
	after := ""
	for pages := 0; ; pages++ {
		page, err := r.reader.Files(t.Context(), tracker.FileQuery{Project: "ENG",
			After: after, Limit: 2, Freshness: stale})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range page.Files {
			all = append(all, f.Path)
		}
		if page.Next == "" {
			break
		}
		after = page.Next
		if pages > 5 {
			t.Fatal("the pages never ended")
		}
	}
	if strings.Join(all, ",") != "reports/2026/q1.md,reports/a.md,reports/b.md,reportsheet.md,zeta.txt" {
		t.Fatalf("paged listing = %v", all)
	}
}

// ONE PATH, ONE SPELLING: what a caller means by a leading slash and spaces is
// repaired, and what it could mean two things by is refused.
func TestAPathHasOneSpelling(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"a.txt":          "a.txt",
		"/a/b.txt":       "a/b.txt",
		"  docs/x y.md ": "docs/x y.md",
	} {
		got, err := tracker.NormalizeFilePath(raw)
		if err != nil || got != want {
			t.Errorf("NormalizeFilePath(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"", "/", "a//b", "a/./b", "../etc/passwd", "docs/",
		"a\\b", "tab\there", strings.Repeat("x", tracker.MaxFilePath+1), "bad\xff"} {
		if _, err := tracker.NormalizeFilePath(bad); err == nil {
			t.Errorf("NormalizeFilePath(%q) was accepted", bad)
		}
	}
	if tracker.FileSubject("eng", "a.txt") != tracker.FileSubject("ENG", "a.txt") {
		t.Error("one file has two subjects under two spellings of its project")
	}
}

// A FILE OVER ITS CAPS IS REFUSED BY NAME, before anything is published — and
// so is a put naming no object, or one no reader could check.
func TestAFileOverItsCapsIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	big := r.objectOf([]byte("x"))
	big.Size = tracker.MaxFileBytes + 1
	if _, err := r.writer.PutFile(t.Context(), "op-big", tracker.FilePut{
		Project: "ENG", Path: "big.bin", Object: big,
	}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("a file over the size cap = %v", err)
	}
	if _, err := r.writer.PutFile(t.Context(), "op-type", tracker.FilePut{
		Project: "ENG", Path: "a.txt", ContentType: "text/plain\r\nX-Evil: 1",
		Object: r.objectOf([]byte("a")),
	}); err == nil {
		t.Fatal("a content type spanning lines was accepted")
	}
	good := r.objectOf([]byte("a"))
	for name, o := range map[string]objstore.Object{
		"no object":     {},
		"no key":        {Hash: good.Hash, Size: good.Size},
		"a bad digest":  {Key: good.Key, Hash: "zz", Size: good.Size},
		"negative size": {Key: good.Key, Hash: good.Hash, Size: -1},
	} {
		if _, err := r.writer.PutFile(t.Context(), "op-"+name, tracker.FilePut{
			Project: "ENG", Path: "a.txt", Object: o,
		}); err == nil {
			t.Errorf("a put with %s was accepted", name)
		}
	}
}

// A PUT NAMING AN OLD KEY IS REFUSED — ADR-0027's bound, judged at the
// write's own decide. A write naming a key minted more than RecordWithin ago
// could land after the collector read that key as nobody's and deleted it; one
// minted inside the bound is recorded. A key minted AHEAD of the deciding
// node's clock is recorded too: the collector judges a key by its own minting
// instant, so a key from a fast clock protects itself, and refusing it would
// only fail uploads from that node.
func TestAPutNamingAnOldKeyIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	at := func(minted time.Time) objstore.Object {
		o := r.objectOf([]byte("aged"))
		o.Key = objstore.KeyAt(minted)
		return o
	}
	_, err := r.writer.PutFile(t.Context(), "op-old", tracker.FilePut{
		Project: "ENG", Path: "old.md", Object: at(r.at.Add(-objstore.RecordWithin - time.Minute)),
	})
	if !errors.Is(err, tracker.ErrUploadStale) {
		t.Fatalf("a put naming a key minted past the bound = %v, want ErrUploadStale", err)
	}
	for name, minted := range map[string]time.Time{
		"just inside the bound": r.at.Add(-objstore.RecordWithin + time.Minute),
		"from a clock ahead":    r.at.Add(6 * time.Hour),
	} {
		if _, err := r.writer.PutFile(t.Context(), "op-"+name, tracker.FilePut{
			Project: "ENG", Path: "fresh.md", Object: at(minted),
		}); err != nil {
			t.Errorf("a put naming a key minted %s = %v, want it recorded", name, err)
		}
		r.drain()
	}
}

// THE LARGEST FILE RECORD FITS ITS COMMIT, measured rather than asserted from
// arithmetic: a path at its cap, a content type at its cap that escapes
// six-fold, the digest, the size and the object's key. A file record no longer
// grows with its content — one key names a gibibyte as it names a byte — but
// it is still a record, and the caps on what it carries are what bound it.
func TestTheMaximalFileFitsItsRecord(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0).UTC()
	file := tracker.File{
		V: tracker.DocumentVersion, Project: "ENGINEERING",
		Path:        strings.Repeat("é", tracker.MaxFilePath/2),
		ContentType: strings.Repeat("\x01", tracker.MaxContentType),
		Hash:        objstore.HashOf([]byte("whole")), Size: tracker.MaxFileBytes,
		Object:    objstore.KeyAt(at),
		CreatedBy: strings.Repeat("c", 64), UpdatedBy: strings.Repeat("u", 64),
		CreatedAt: at, UpdatedAt: at,
	}
	body, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	rec := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "0193f0a0-0000-7000-8000-000000000001",
			Subject: tracker.FileSubject(file.Project, file.Path), Op: tracker.OpPatch,
			CreatedAt: at, Gen: 1,
			Writer: "node-with-a-long-name",
			Scope:  tracker.ScopeSet{Subject: true, Container: file.Project},
		},
		Kind: tracker.ChangeFileWritten, Mutation: body,
		Actor: "an-agent-handle", ActorKind: tracker.AuthorAgent,
	}
	encoded, err := rec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the maximal file record is %d bytes of a %d-byte ceiling", len(encoded), tracker.MaxCommitBytes)
	if len(encoded) > tracker.MaxCommitBytes/100 {
		t.Fatalf("the maximal file record is %d bytes, over a hundredth of the %d the "+
			"commit ceiling allows — a file record carries names, never its content",
			len(encoded), tracker.MaxCommitBytes)
	}
}
