package builtin_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fakeFiles is a project's files held in memory: the tracker's rows for them,
// keyed by address.
type fakeFiles struct {
	mu      sync.Mutex
	files   map[string]tracker.File
	version uint64
	writers []string

	// puts counts the writes that reached the row, and refuse is what
	// every write is refused with when set.
	puts   int
	refuse error
}

func newFakeFiles() *fakeFiles { return &fakeFiles{files: map[string]tracker.File{}} }

func (f *fakeFiles) key(project, path string) string { return project + "/" + path }

func (f *fakeFiles) Files(_ context.Context, q tracker.FileQuery) (tracker.FileListing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q.Project != "ENG" {
		return tracker.FileListing{}, fmt.Errorf("%w: %s", tracker.ErrNoProject, q.Project)
	}
	var out tracker.FileListing
	for _, file := range f.files {
		if !file.Removed() && file.Project == q.Project {
			out.Files = append(out.Files, tracker.FileRow{Project: file.Project, Path: file.Path,
				Size: file.Size, Version: file.Version})
		}
	}
	out.Complete, out.Level = true, q.Freshness.Level
	return out, nil
}

func (f *fakeFiles) File(_ context.Context, project, path string,
	_ statelog.Freshness) (tracker.FileDetail, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[f.key(project, path)]
	if !ok || file.Removed() {
		return tracker.FileDetail{}, fmt.Errorf("%w: %s", tracker.ErrNoFile, path)
	}
	return tracker.FileDetail{File: file}, nil
}

func (f *fakeFiles) as(actor builtin.Actor) builtin.FileWriter {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writers = append(f.writers, actor.Handle)
	return f
}

func (f *fakeFiles) PutFile(_ context.Context, _ string, put tracker.FilePut) (tracker.WriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, held := f.files[f.key(put.Project, put.Path)]
	if put.IfMatch != tracker.NoIfMatch && (!held || current.Version != put.IfMatch) {
		return tracker.WriteResult{}, tracker.ErrStaleVersion
	}
	if f.refuse != nil {
		return tracker.WriteResult{}, f.refuse
	}
	f.version++
	f.puts++
	file := tracker.File{Project: put.Project, Path: put.Path, ContentType: put.ContentType,
		Hash: put.Object.Hash, Size: put.Object.Size, Object: put.Object.Key, Version: f.version,
		UpdatedAt: time.Unix(1_700_000_000, 0).UTC()}
	f.files[f.key(put.Project, put.Path)] = file
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied,
		Version: int64(f.version), Position: statelog.Position{Stream: "S", Generation: 1, Seq: f.version}}}, nil
}

func (f *fakeFiles) RemoveFile(_ context.Context, _, project, path string,
	_ uint64) (tracker.WriteResult, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[f.key(project, path)]
	if !ok || file.Removed() {
		return tracker.WriteResult{}, fmt.Errorf("%w: %s", tracker.ErrNoFile, path)
	}
	now := time.Now()
	file.RemovedAt = &now
	f.files[f.key(project, path)] = file
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

// fakeObjects is an object store in memory, each upload an object under a key
// of its own, as the real store keeps them.
type fakeObjects struct {
	mu      sync.Mutex
	objects map[objstore.Key][]byte

	// corrupt makes every read answer the store's corruption sentinel.
	corrupt bool
}

func newFakeObjects() *fakeObjects { return &fakeObjects{objects: map[objstore.Key][]byte{}} }

func (o *fakeObjects) Put(_ context.Context, r io.Reader, limit int64,
	_ objstore.PutMeta) (objstore.Object, error) {

	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return objstore.Object{}, err
	}
	if int64(len(data)) > limit {
		return objstore.Object{}, objstore.ErrTooLarge
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	k := objstore.KeyAt(time.Now())
	o.objects[k] = data
	return objstore.Object{Key: k, Hash: objstore.HashOf(data), Size: int64(len(data))}, nil
}

func (o *fakeObjects) ReadAt(_ context.Context, obj objstore.Object, off, n int64) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.corrupt {
		return nil, fmt.Errorf("%w: %s", objstore.ErrCorrupt, obj.Key)
	}
	whole, ok := o.objects[obj.Key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", objstore.ErrNotFound, obj.Key)
	}
	end := min(off+n, int64(len(whole)))
	if off >= end {
		return []byte{}, nil
	}
	return whole[off:end], nil
}

func (o *fakeObjects) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.objects)
}

func fileDeps(files *fakeFiles, objects *fakeObjects) builtin.WorkDeps {
	return builtin.WorkDeps{
		Files: files, FileWriter: files.as, Objects: objects,
		DefaultProject: func(string) string { return "ENG" },
	}
}

func fileAnswer(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("the answer is not json: %v\n%s", err, raw)
	}
	return out
}

// A FILE WRITTEN BY A SEAT READS BACK AS TEXT, IS LISTED, AND IS GONE WHEN
// REMOVED — with its bytes stored before its row was written.
func TestASeatWritesReadsListsAndRemovesAFile(t *testing.T) {
	t.Parallel()
	files, objects := newFakeFiles(), newFakeObjects()
	reg := workRegistry(t, fileDeps(files, objects))

	got := callWork(t, reg, tracker.WriteProjectFileTool, map[string]any{
		"path": "reports/q3.md", "content": "# Q3\n\nIt went well.",
	})
	if got.Failed {
		t.Fatalf("write: %s", got.Output)
	}
	written := fileAnswer(t, got.Output)
	if written["project"] != "ENG" || written["content_type"] != "text/markdown; charset=utf-8" {
		t.Fatalf("write answered %v", written)
	}
	if n := objects.count(); n != 1 {
		t.Fatalf("the content is in %d objects, want 1 stored before the row", n)
	}
	if files.writers[0] != "eng" {
		t.Fatalf("the write was attributed to %v, want the turn's seat", files.writers)
	}

	got = callWork(t, reg, tracker.ReadProjectFileTool, map[string]any{"path": "/reports/q3.md"})
	if got.Failed {
		t.Fatalf("read: %s", got.Output)
	}
	if read := fileAnswer(t, got.Output); read["content"] != "# Q3\n\nIt went well." ||
		read["encoding"] != "text" || read["next_offset"] != nil {
		t.Fatalf("read answered %v", read)
	}

	got = callWork(t, reg, tracker.ListProjectFilesTool, map[string]any{})
	if listed := fileAnswer(t, got.Output); got.Failed || listed["count"] != float64(1) {
		t.Fatalf("list answered %s", got.Output)
	}

	got = callWork(t, reg, tracker.RemoveProjectFileTool, map[string]any{"path": "reports/q3.md"})
	if got.Failed {
		t.Fatalf("remove: %s", got.Output)
	}
	got = callWork(t, reg, tracker.ReadProjectFileTool, map[string]any{"path": "reports/q3.md"})
	if !got.Failed || !strings.Contains(got.Output, "there is no file") {
		t.Fatalf("a read of a removed file answered %s", got.Output)
	}
}

// A LARGE FILE IS READ A PAGE AT A TIME, each page ending on a whole character
// and saying where the next begins — so paging through reassembles the text.
func TestALargeFileIsReadInPagesOnWholeCharacters(t *testing.T) {
	t.Parallel()
	files, objects := newFakeFiles(), newFakeObjects()
	reg := workRegistry(t, fileDeps(files, objects))
	text := strings.Repeat("naïve café — ", 10_000) // multi-byte, well past a page
	if got := callWork(t, reg, tracker.WriteProjectFileTool, map[string]any{
		"path": "notes.txt", "content": text,
	}); got.Failed {
		t.Fatal(got.Output)
	}
	var sb strings.Builder
	offset := 0.0
	for pages := 0; ; pages++ {
		got := callWork(t, reg, tracker.ReadProjectFileTool, map[string]any{
			"path": "notes.txt", "offset": offset, "max_bytes": 1001,
		})
		if got.Failed {
			t.Fatalf("page %d: %s", pages, got.Output)
		}
		page := fileAnswer(t, got.Output)
		if page["encoding"] != "text" {
			t.Fatalf("page %d came back as %v — a page cut inside a character "+
				"reads as binary", pages, page["encoding"])
		}
		sb.WriteString(page["content"].(string))
		next, more := page["next_offset"].(float64)
		if !more {
			break
		}
		offset = next
		if pages > 1000 {
			t.Fatal("the pages never ended")
		}
	}
	if sb.String() != text {
		t.Fatalf("the pages reassemble %d bytes, want %d", sb.Len(), len(text))
	}
}

// BYTES THAT ARE NOT TEXT ARE DESCRIBED, NOT DUMPED — and handed back whole
// when the caller asks for base64.
func TestABinaryFileIsDescribedUnlessAskedForBytes(t *testing.T) {
	t.Parallel()
	files, objects := newFakeFiles(), newFakeObjects()
	reg := workRegistry(t, fileDeps(files, objects))
	raw := []byte{0x89, 'P', 'N', 'G', 0, 1, 2, 0xff}
	if got := callWork(t, reg, tracker.WriteProjectFileTool, map[string]any{
		"path": "logo.png", "content": base64.StdEncoding.EncodeToString(raw), "encoding": "base64",
	}); got.Failed {
		t.Fatal(got.Output)
	}
	got := callWork(t, reg, tracker.ReadProjectFileTool, map[string]any{"path": "logo.png"})
	if read := fileAnswer(t, got.Output); read["encoding"] != "binary" || read["content"] != nil ||
		read["content_type"] != "image/png" {
		t.Fatalf("a binary file read as text answered %v", read)
	}
	got = callWork(t, reg, tracker.ReadProjectFileTool, map[string]any{
		"path": "logo.png", "encoding": "base64",
	})
	read := fileAnswer(t, got.Output)
	if decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(read["content"])); err != nil ||
		string(decoded) != string(raw) {
		t.Fatalf("base64 read answered %v", read)
	}
}

// A FILE THAT IS NOT THERE AND A PROJECT THAT IS NOT THERE ARE NAMED AS SUCH,
// never answered as a read that failed — and a read that failed is never
// answered as a file that is not there.
func TestAMissingFileIsNamedAndAFailedReadIsNot(t *testing.T) {
	t.Parallel()
	files, objects := newFakeFiles(), newFakeObjects()
	reg := workRegistry(t, fileDeps(files, objects))
	got := callWork(t, reg, tracker.ReadProjectFileTool, map[string]any{"path": "nope.md"})
	if !got.Failed || !strings.Contains(got.Output, "there is no file nope.md in ENG") {
		t.Fatalf("a missing file answered %s", got.Output)
	}
	got = callWork(t, reg, tracker.ListProjectFilesTool, map[string]any{"project": "ops"})
	if !got.Failed || !strings.Contains(got.Output, "there is no project OPS") {
		t.Fatalf("an unknown project answered %s", got.Output)
	}
	// Content the store cannot supply: the row is there, the bytes are not.
	if got := callWork(t, reg, tracker.WriteProjectFileTool, map[string]any{
		"path": "lost.md", "content": "gone",
	}); got.Failed {
		t.Fatal(got.Output)
	}
	objects.mu.Lock()
	clear(objects.objects)
	objects.mu.Unlock()
	got = callWork(t, reg, tracker.ReadProjectFileTool, map[string]any{"path": "lost.md"})
	if !got.Failed || !strings.Contains(got.Output, "NOT empty or missing") {
		t.Fatalf("a read whose bytes failed answered %s", got.Output)
	}
}

// A LIVE FILE NAMING NO OBJECT IS THE ENGINE'S FAULT, NOT A FILE THAT IS GONE:
// the applier refuses to write one, so a read that meets one says the content
// cannot be returned and a retry will not help — never not_found, which would
// tell the seat the file does not exist while it is listed.
func TestALiveFileNamingNoObjectIsNotReadAsGone(t *testing.T) {
	t.Parallel()
	files, objects := newFakeFiles(), newFakeObjects()
	reg := workRegistry(t, fileDeps(files, objects))
	files.files[files.key("ENG", "odd.md")] = tracker.File{Project: "ENG", Path: "odd.md",
		Hash: objstore.HashOf([]byte("odd")), Size: 3, Version: 1}

	got := callWork(t, reg, tracker.ReadProjectFileTool, map[string]any{"path": "odd.md"})
	if !got.Failed || got.Refusal != tools.RefusalUnavailable ||
		!strings.Contains(got.Output, "will not help") {
		t.Fatalf("a read of a live file naming no object answered %s (refusal %q)",
			got.Output, got.Refusal)
	}
}

// CONTENT THE STORE HOLDS WRONG IS NOT A READ TO RETRY: the store answered,
// and its bytes are not the file's — so the seat is told not to use them and
// to get the file restored, rather than to try again.
func TestCorruptContentIsNotAReadToRetry(t *testing.T) {
	t.Parallel()
	files, objects := newFakeFiles(), newFakeObjects()
	reg := workRegistry(t, fileDeps(files, objects))
	if got := callWork(t, reg, tracker.WriteProjectFileTool, map[string]any{
		"path": "a.md", "content": "a",
	}); got.Failed {
		t.Fatal(got.Output)
	}
	objects.mu.Lock()
	objects.corrupt = true
	objects.mu.Unlock()
	got := callWork(t, reg, tracker.ReadProjectFileTool, map[string]any{"path": "a.md"})
	if !got.Failed || got.Refusal != tools.RefusalUnavailable ||
		!strings.Contains(got.Output, "will not help") {
		t.Fatalf("a read of corrupt content answered %s (refusal %q)", got.Output, got.Refusal)
	}
}

// WRITING WHAT THE FILE ALREADY HOLDS STORES NOTHING: every upload is an
// object of its own, so a seat re-saving an unchanged file would upload all of
// it again, and the store would hold both copies until the collector's first
// pass after the replaced one is a day old. A change of content, or of type,
// is written.
func TestWritingTheSameContentAgainStoresNothing(t *testing.T) {
	t.Parallel()
	files, objects := newFakeFiles(), newFakeObjects()
	reg := workRegistry(t, fileDeps(files, objects))
	write := func(args map[string]any) map[string]any {
		t.Helper()
		got := callWork(t, reg, tracker.WriteProjectFileTool, args)
		if got.Failed {
			t.Fatal(got.Output)
		}
		return fileAnswer(t, got.Output)
	}
	first := write(map[string]any{"path": "r.md", "content": "same"})
	again := write(map[string]any{"path": "r.md", "content": "same"})
	if again["outcome"] != "unchanged" || again["version"] != first["version"] ||
		objects.count() != 1 || files.puts != 1 {
		t.Fatalf("writing the same content again answered %v, storing %d objects "+
			"and %d writes", again, objects.count(), files.puts)
	}
	if changed := write(map[string]any{"path": "r.md", "content": "other"}); changed["outcome"] == "unchanged" {
		t.Fatalf("a change of content was skipped: %v", changed)
	}
	if retyped := write(map[string]any{"path": "r.md", "content": "other",
		"content_type": "text/plain"}); retyped["outcome"] == "unchanged" {
		t.Fatalf("a change of type was skipped: %v", retyped)
	}
	if objects.count() != 3 || files.puts != 3 {
		t.Fatalf("three different writes stored %d objects and %d rows", objects.count(), files.puts)
	}
}

// AN UPLOAD TOO OLD TO RECORD IS ONE THE SAME CALL FIXES: the call uploads the
// content anew, under a fresh key — so the seat is told to make it again,
// rather than that the write failed.
func TestAnUploadTooOldToRecordIsMadeAgain(t *testing.T) {
	t.Parallel()
	files, objects := newFakeFiles(), newFakeObjects()
	files.refuse = fmt.Errorf("%w: r.md", tracker.ErrUploadStale)
	reg := workRegistry(t, fileDeps(files, objects))
	got := callWork(t, reg, tracker.WriteProjectFileTool, map[string]any{
		"path": "r.md", "content": "late",
	})
	if !got.Failed || got.Refusal != tools.RefusalUnavailable ||
		!strings.Contains(got.Output, "Make the same call again") {
		t.Fatalf("a stale upload answered %s (refusal %q)", got.Output, got.Refusal)
	}
	if !errors.Is(files.refuse, tracker.ErrUploadStale) {
		t.Fatal("the premise")
	}
}
