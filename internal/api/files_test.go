package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The byte routes' pacing: each chunk of a file has its own window to cross
// the connection, and the server's own work — storing a chunk, fetching one —
// is never counted against the client.
//
// Asserted by CAPTURING the deadlines rather than waiting for one to fire, for
// the reason internal/api/httpjson's own deadline test gives: net/http already
// guarantees a deadline fires, and what is this package's own is that one is
// set at all, how far out, and when.

// paceLog is the order things happened in, shared by the fake store and the
// capturing writer: a deadline set after a chunk was stored is a different
// claim from one set before.
type paceLog struct {
	mu     sync.Mutex
	events []string
	// out is how far past the moment it was set each deadline was.
	out []time.Duration
}

func (l *paceLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *paceLog) deadline(kind string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, kind)
	l.out = append(l.out, time.Until(at))
}

func (l *paceLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// pacedWriter is a recorder that can carry a read and a write deadline, which
// is what http.NewResponseController looks for, and logs each one it is set.
type pacedWriter struct {
	*httptest.ResponseRecorder
	log *paceLog
}

func (w *pacedWriter) SetReadDeadline(t time.Time) error {
	w.log.deadline("read-deadline", t)
	return nil
}

func (w *pacedWriter) SetWriteDeadline(t time.Time) error {
	w.log.deadline("write-deadline", t)
	return nil
}

func (w *pacedWriter) Write(p []byte) (int, error) {
	w.log.add(fmt.Sprintf("write %d", len(p)))
	return w.ResponseRecorder.Write(p)
}

// fakeFiles is a project's files over a map of chunks, logging what it does.
type fakeFiles struct {
	log *paceLog

	mu     sync.Mutex
	chunks map[objstore.Hash][]byte
	file   *tracker.File
	puts   int
}

func newFakeFiles(log *paceLog) *fakeFiles {
	return &fakeFiles{log: log, chunks: map[objstore.Hash][]byte{}}
}

func (f *fakeFiles) File(context.Context, string, string, statelog.Freshness) (tracker.FileDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return tracker.FileDetail{}, tracker.ErrNoFile
	}
	return tracker.FileDetail{File: *f.file}, nil
}

func (f *fakeFiles) Open(_ context.Context, m objstore.Manifest) (io.ReadCloser, error) {
	return io.NopCloser(&fetchingReader{f: f, chunks: m.Chunks}), nil
}

func (f *fakeFiles) PutChunk(_ context.Context, h objstore.Hash, data []byte) (int, error) {
	f.mu.Lock()
	f.chunks[h] = append([]byte(nil), data...)
	f.mu.Unlock()
	f.log.add(fmt.Sprintf("stored %d", len(data)))
	return 3, nil
}

func (f *fakeFiles) PutFileAs(_ context.Context, _, _ string, put tracker.FilePut) (tracker.WriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (f *fakeFiles) RemoveFileAs(context.Context, string, string, string, string, uint64) (tracker.WriteResult, error) {
	return tracker.WriteResult{}, errors.New("not in this suite")
}

// fetchingReader reads a manifest's chunks out of the fake, logging each
// fetch, the way the object client walks a ranking for each.
type fetchingReader struct {
	f      *fakeFiles
	chunks []objstore.Chunk
	cur    *bytes.Reader
}

func (r *fetchingReader) Read(p []byte) (int, error) {
	for r.cur == nil || r.cur.Len() == 0 {
		if len(r.chunks) == 0 {
			return 0, io.EOF
		}
		c := r.chunks[0]
		r.chunks = r.chunks[1:]
		r.f.mu.Lock()
		data := r.f.chunks[c.Hash]
		r.f.mu.Unlock()
		r.f.log.add(fmt.Sprintf("fetched %d", len(data)))
		r.cur = bytes.NewReader(data)
	}
	return r.cur.Read(p)
}

// filesApp is an authenticated node serving fake's files.
func filesApp(t *testing.T, fake *fakeFiles) *api.App {
	t.Helper()
	b := closedPosture()
	return newApp(t, api.Options{Bootstrap: &b, Files: fake})
}

// content is n bytes no two chunks of which are alike.
func content(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i / 7)
	}
	return out
}

// withinPace fails unless every captured deadline was the route's whole
// window: "now" cuts off every real client, an hour bounds no trickle.
func withinPace(t *testing.T, log *paceLog) {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	for i, d := range log.out {
		if d < httpjson.BodyReadTimeout-time.Second || d > httpjson.BodyReadTimeout+time.Second {
			t.Errorf("deadline %d was set %v out, want %v", i, d, httpjson.BodyReadTimeout)
		}
	}
}

// AN UPLOAD'S EVERY CHUNK HAS ITS OWN WINDOW, and the window opens once the
// chunk before it is stored. The route read up to a gibibyte with no deadline
// at all, so a client trickling it a byte at a time held the handler and its
// connection for as long as it liked; and a deadline set before a chunk was
// stored would have spent the client's window on a member's attempt budget.
func TestAnUploadReadsEachChunkInAWindowOfItsOwn(t *testing.T) {
	t.Parallel()
	log := &paceLog{}
	fake := newFakeFiles(log)
	a := filesApp(t, fake)

	body := content(2*objstore.ChunkSize + objstore.ChunkSize/2)
	req := httptest.NewRequest(http.MethodPut, "/work/files/ENG/reports/q3.bin", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	w := &pacedWriter{ResponseRecorder: httptest.NewRecorder(), log: log}
	a.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload answered %d: %s", w.Code, w.Body.String())
	}

	want := []string{
		"read-deadline", // the first chunk's window, before anything is read
		fmt.Sprintf("stored %d", objstore.ChunkSize), "read-deadline",
		fmt.Sprintf("stored %d", objstore.ChunkSize), "read-deadline",
		fmt.Sprintf("stored %d", objstore.ChunkSize/2), "read-deadline",
	}
	// The answer's own write is the response, not the body being read.
	var got []string
	for _, e := range log.all() {
		if !strings.HasPrefix(e, "write ") {
			got = append(got, e)
		}
	}
	if !equal(got, want) {
		t.Errorf("the upload went\n  %v\nwant\n  %v", got, want)
	}
	withinPace(t, log)
}

// A BODY THAT STOPS ARRIVING IS THE CLIENT'S, not the store's: it answers 400
// and records nothing. It answered 503 `unavailable`, which tells a client to
// retry a request whose own connection was what failed.
func TestAnUploadWhoseBodyStopsIsRefusedAsTheClients(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(&paceLog{})
	a := filesApp(t, fake)

	cut := io.MultiReader(bytes.NewReader(content(objstore.ChunkSize+10)),
		&failingReader{err: errors.New("i/o timeout")})
	req := httptest.NewRequest(http.MethodPut, "/work/files/ENG/reports/q3.bin", cut)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a body that stopped answered %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var answer map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer["error"] != string(httpjson.CodeUnreadableBody) ||
		!strings.Contains(answer["detail"], "i/o timeout") {
		t.Errorf("answer = %v, want unreadable_body naming the read's own failure", answer)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.puts != 0 {
		t.Errorf("a file was recorded from a body that never arrived")
	}
}

type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }

// A DOWNLOAD'S EVERY CHUNK HAS ITS OWN WINDOW TOO, opened once the chunk is in
// hand. The route copied with no write deadline, so a client that opened a
// download and never read it held the handler, its connection and every chunk
// read ahead for it; and a deadline set before a chunk was fetched would have
// spent the client's window on a walk down the ranking.
func TestADownloadWritesEachChunkInAWindowOfItsOwn(t *testing.T) {
	t.Parallel()
	log := &paceLog{}
	fake := newFakeFiles(log)
	data := content(2*objstore.ChunkSize + 100)
	var m objstore.Manifest
	for off := 0; off < len(data); off += objstore.ChunkSize {
		piece := data[off:min(off+objstore.ChunkSize, len(data))]
		h := objstore.HashOf(piece)
		fake.chunks[h] = piece
		m.Chunks = append(m.Chunks, objstore.Chunk{Hash: h, Size: int64(len(piece))})
	}
	file := tracker.File{Version: 2, Project: "ENG", Path: "reports/q3.bin", Size: int64(len(data))}
	for _, c := range m.Chunks {
		file.Chunks = append(file.Chunks, tracker.FileChunk{Hash: c.Hash, Size: c.Size})
	}
	fake.file = &file
	a := filesApp(t, fake)

	req := httptest.NewRequest(http.MethodGet, "/work/files/ENG/reports/q3.bin", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := &pacedWriter{ResponseRecorder: httptest.NewRecorder(), log: log}
	a.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("download answered %d with %d bytes, want 200 with %d", w.Code, w.Body.Len(), len(data))
	}

	// EVERY WRITE DIRECTLY FOLLOWS ITS OWN DEADLINE, with no fetch between
	// them: a deadline set before its bytes were in hand would have a walk
	// down the ranking inside it.
	events := log.all()
	deadlines := 0
	for i, e := range events {
		if e == "write-deadline" {
			deadlines++
			if i+1 >= len(events) || !strings.HasPrefix(events[i+1], "write ") {
				t.Errorf("deadline %d is not followed by its write: %v", deadlines, events)
			}
		}
		if strings.HasPrefix(e, "write ") && (i == 0 || events[i-1] != "write-deadline") {
			t.Errorf("a write at %d follows %q rather than its deadline: %v", i, events[max(i-1, 0)], events)
		}
	}
	if deadlines != len(m.Chunks) {
		t.Errorf("%d write deadlines for %d chunks: %v", deadlines, len(m.Chunks), events)
	}
	withinPace(t, log)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
