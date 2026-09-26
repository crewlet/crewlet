package tracker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

var stale = statelog.Freshness{Level: statelog.ReadStale}

// manifestOf cuts content into a manifest, uploading nothing: the tracker
// never reads a byte, so a test of it needs only the names.
func manifestOf(t *testing.T, content []byte) objstore.Manifest {
	t.Helper()
	m, err := objstore.Split(t.Context(), bytes.NewReader(content), int64(len(content)),
		func(context.Context, objstore.Chunk, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (r *roundTrip) putFile(op, path string, content []byte) tracker.WriteResult {
	r.t.Helper()
	res, err := r.writer.PutFile(r.t.Context(), op, tracker.FilePut{
		Project: "ENG", Path: path, ContentType: "text/markdown",
		Manifest: manifestOf(r.t, content),
	})
	if err != nil {
		r.t.Fatalf("PutFile %s: %v", path, err)
	}
	r.drain()
	return res
}

// chunkRows is how many chunk rows name h — what the object store reads.
func (r *roundTrip) chunkRows(t *testing.T, h objstore.Hash) int {
	t.Helper()
	// THROUGH THE DECLARED LIST, exactly as the engine builds the passes'
	// sources, so this reads the statement the collector runs.
	sources, err := upkeep.Sources(references.All, tracker.ObjectEstate{Reader: r.reader})
	if err != nil {
		t.Fatal(err)
	}
	set, complete, err := sources[0].Referenced(t.Context(), h.PG(), statelog.Position{})
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Fatal("the references read reported itself incomplete on a node holding every record")
	}
	if _, ok := set[h]; ok {
		return 1
	}
	return 0
}

// A FILE PUT, READ BACK AND REMOVED: the row, its manifest and the chunk rows
// the object store keeps its bytes alive by, at every step.
func TestAFileIsWrittenReadAndRemoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	content := bytes.Repeat([]byte("a quarterly plan\n"), 100_000) // two chunks
	written := r.putFile("op-put", "/reports/q3 plan.md", content)
	if written.Position.Seq == 0 {
		t.Fatal("a put landed at no position")
	}

	detail, err := r.reader.File(t.Context(), "eng", "reports/q3 plan.md", stale)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	f := detail.File
	want := manifestOf(t, content)
	if f.Path != "reports/q3 plan.md" || f.Project != "ENG" || f.Size != int64(len(content)) ||
		f.Hash != want.Hash || len(f.Chunks) != len(want.Chunks) || f.ContentType != "text/markdown" {
		t.Fatalf("read back %+v", f)
	}
	if got := f.Manifest(); got.Validate() != nil || got.Hash != want.Hash {
		t.Fatalf("the stored manifest does not validate: %+v", got)
	}
	for _, c := range want.Chunks {
		if r.chunkRows(t, c.Hash) != 1 {
			t.Fatalf("chunk %s of a live file is not referenced", c.Hash)
		}
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
	for _, c := range want.Chunks {
		if r.chunkRows(t, c.Hash) != 0 {
			t.Fatalf("chunk %s of a removed file is still referenced, so its "+
				"bytes are never collected", c.Hash)
		}
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
	// used again — with its own content's chunks referenced.
	again := []byte("the plan, rewritten")
	r.putFile("op-put-again", "reports/q3 plan.md", again)
	if _, err := r.reader.File(t.Context(), "ENG", "reports/q3 plan.md", stale); err != nil {
		t.Fatalf("File after a put over a removal: %v", err)
	}
	if r.chunkRows(t, objstore.HashOf(again)) != 1 {
		t.Fatal("the new content's chunk is not referenced")
	}
}

// A PATH IS ONE FILE: a second put replaces the first and keeps who made it,
// and the chunks only the first content named stop being referenced.
func TestAPutOverAFileReplacesItsContent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.putFile("op-1", "notes.txt", []byte("first"))
	first, err := r.reader.File(t.Context(), "ENG", "notes.txt", stale)
	if err != nil {
		t.Fatal(err)
	}
	editor := r.writer.As("bo", tracker.AuthorAgent, tracker.Provenance{})
	if _, err := editor.PutFile(t.Context(), "op-2", tracker.FilePut{
		Project: "ENG", Path: "notes.txt", Manifest: manifestOf(t, []byte("second")),
	}); err != nil {
		t.Fatal(err)
	}
	r.drain()
	second, err := r.reader.File(t.Context(), "ENG", "notes.txt", stale)
	if err != nil {
		t.Fatal(err)
	}
	if second.File.Hash != objstore.HashOf([]byte("second")) {
		t.Fatalf("the content was not replaced: %+v", second.File)
	}
	if second.File.CreatedBy != first.File.CreatedBy || !second.File.CreatedAt.Equal(first.File.CreatedAt) {
		t.Fatalf("a put re-attributed the file: created by %q at %s, was %q at %s",
			second.File.CreatedBy, second.File.CreatedAt, first.File.CreatedBy, first.File.CreatedAt)
	}
	if second.File.UpdatedBy != "bo" {
		t.Fatalf("updated by %q, want bo", second.File.UpdatedBy)
	}
	if r.chunkRows(t, objstore.HashOf([]byte("first"))) != 0 {
		t.Fatal("the replaced content's chunk is still referenced")
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
		Project: "ENG", Path: "plan.md", Manifest: manifestOf(t, []byte("v3")),
		IfMatch: read.File.Version,
	})
	if !errors.Is(err, tracker.ErrStaleVersion) {
		t.Fatalf("a put conditioned on a moved version = %v, want ErrStaleVersion", err)
	}
	_, err = r.writer.PutFile(t.Context(), "op-4", tracker.FilePut{
		Project: "ENG", Path: "never.md", Manifest: manifestOf(t, []byte("x")), IfMatch: 7,
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
		Project: "NOPE", Path: "a.txt", Manifest: manifestOf(t, []byte("a")),
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

// A FILE OVER ITS CAPS IS REFUSED BY NAME, before anything is published.
func TestAFileOverItsCapsIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	big := objstore.Manifest{Hash: objstore.HashOf([]byte("x")), Size: tracker.MaxFileBytes + 1}
	for i := int64(0); big.Size > 0 && i*objstore.ChunkSize < big.Size; i++ {
		size := min(int64(objstore.ChunkSize), big.Size-i*objstore.ChunkSize)
		big.Chunks = append(big.Chunks, objstore.Chunk{Hash: objstore.HashOf([]byte{byte(i)}), Size: size})
	}
	if _, err := r.writer.PutFile(t.Context(), "op-big", tracker.FilePut{
		Project: "ENG", Path: "big.bin", Manifest: big,
	}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("a file over the size cap = %v", err)
	}
	if _, err := r.writer.PutFile(t.Context(), "op-type", tracker.FilePut{
		Project: "ENG", Path: "a.txt", ContentType: "text/plain\r\nX-Evil: 1",
		Manifest: manifestOf(t, []byte("a")),
	}); err == nil {
		t.Fatal("a content type spanning lines was accepted")
	}
}

// THE LARGEST MANIFEST A FILE MAY CARRY FITS ONE RECORD, measured rather than
// asserted from arithmetic: [tracker.MaxFileChunks] entries, a path at its cap
// that escapes six-fold, and a content type at its cap.
func TestTheMaximalFileFitsItsRecord(t *testing.T) {
	t.Parallel()
	file := tracker.File{
		V: tracker.DocumentVersion, Project: "ENGINEERING",
		Path:        strings.Repeat("\x01", 0) + strings.Repeat("é", tracker.MaxFilePath/2),
		ContentType: strings.Repeat("t", tracker.MaxContentType),
		Hash:        objstore.HashOf([]byte("whole")), Size: tracker.MaxFileBytes,
		CreatedBy: strings.Repeat("c", 64), UpdatedBy: strings.Repeat("u", 64),
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(), UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	for i := range tracker.MaxFileChunks {
		h := objstore.HashOf([]byte{byte(i), byte(i >> 8)})
		file.Chunks = append(file.Chunks, tracker.FileChunk{Hash: h, Size: objstore.ChunkSize, PG: 255})
	}
	body, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	rec := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "0193f0a0-0000-7000-8000-000000000001",
			Subject: tracker.FileSubject(file.Project, "p"), Op: tracker.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Gen: 1,
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
	if len(encoded) > tracker.MaxCommitBytes/3 {
		t.Fatalf("the maximal file record is %d bytes, over a third of the %d the "+
			"commit ceiling allows", len(encoded), tracker.MaxCommitBytes)
	}
}
