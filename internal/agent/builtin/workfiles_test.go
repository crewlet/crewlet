package builtin_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fakeFiles is a project's files held in memory: the tracker's rows for them,
// keyed by address.
type fakeFiles struct {
	mu      sync.Mutex
	files   map[string]tracker.File
	version uint64
	writers []string
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
	f.version++
	file := tracker.File{Project: put.Project, Path: put.Path, ContentType: put.ContentType,
		Hash: put.Manifest.Hash, Size: put.Manifest.Size, Version: f.version,
		UpdatedAt: time.Unix(1_700_000_000, 0).UTC()}
	for _, c := range put.Manifest.Chunks {
		file.Chunks = append(file.Chunks, tracker.FileChunk{Hash: c.Hash, Size: c.Size, PG: c.Hash.PG()})
	}
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

// fakeObjects is an object store in memory, which reads a range as the real
// client does: by the manifest's chunks.
type fakeObjects struct {
	mu     sync.Mutex
	chunks map[objstore.Hash][]byte
}

func newFakeObjects() *fakeObjects { return &fakeObjects{chunks: map[objstore.Hash][]byte{}} }

func (o *fakeObjects) Put(_ context.Context, h objstore.Hash, data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.chunks[h] = append([]byte(nil), data...)
	return 1, nil
}

func (o *fakeObjects) ReadAt(_ context.Context, m objstore.Manifest, off, n int64) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var whole []byte
	for _, c := range m.Chunks {
		data, ok := o.chunks[c.Hash]
		if !ok {
			return nil, fmt.Errorf("chunk %s is nowhere", c.Hash)
		}
		whole = append(whole, data...)
	}
	end := min(off+n, int64(len(whole)))
	if off >= end {
		return []byte{}, nil
	}
	return whole[off:end], nil
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
	if len(objects.chunks) != 1 {
		t.Fatalf("the content is in %d chunks, want 1 stored before the row", len(objects.chunks))
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
	clear(objects.chunks)
	objects.mu.Unlock()
	got = callWork(t, reg, tracker.ReadProjectFileTool, map[string]any{"path": "lost.md"})
	if !got.Failed || !strings.Contains(got.Output, "NOT empty or missing") {
		t.Fatalf("a read whose bytes failed answered %s", got.Output)
	}
}
