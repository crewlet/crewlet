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
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The byte routes: an upload streamed into one object and then recorded, a
// download streamed back out of one, checked against its row — and the pace
// each is held to, which charges the client only for the time it is waited
// on.
//
// The pacing is asserted by CAPTURING the deadlines rather than waiting for
// one to fire, for the reason internal/api/httpjson's own deadline test gives:
// net/http already guarantees a deadline fires, and what is this package's
// own is that one is set at all, how far out, and when.

// paceLog is the order things happened in, shared by the fake store and the
// capturing writer.
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

// fakeFiles is a project's files over a real object store on the in-memory
// backend, so every read is checked against its row exactly as a node's is.
type fakeFiles struct {
	store   *objstore.Store
	backend *memobj.Backend

	// work is the store's own time between two reads of an upload's body
	// — a broker's acknowledgements, a bucket's part — which no client
	// may be charged for.
	work time.Duration

	mu        sync.Mutex
	file      *tracker.File
	puts      int
	objects   int
	refuse    error
	notAccept error
}

func newFakeFiles(t *testing.T) *fakeFiles {
	t.Helper()
	backend := memobj.New()
	s, err := objstore.NewStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeFiles{store: s, backend: backend}
}

// holding stores content as the file at reports/q3.bin, and answers its row.
func (f *fakeFiles) holding(t *testing.T, content []byte) tracker.File {
	t.Helper()
	o, err := f.store.Put(t.Context(), bytes.NewReader(content), tracker.MaxFileBytes, objstore.PutMeta{})
	if err != nil {
		t.Fatal(err)
	}
	file := tracker.File{Version: 2, Project: "ENG", Path: "reports/q3.bin",
		Hash: o.Hash, Size: o.Size, Object: o.Key}
	f.file = &file
	return file
}

func (f *fakeFiles) File(context.Context, string, string, statelog.Freshness) (tracker.FileDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return tracker.FileDetail{}, tracker.ErrNoFile
	}
	return tracker.FileDetail{File: *f.file}, nil
}

// AcceptsFiles refuses a read naming no level, as the tracker's reader does —
// the surface resolves one, and a route that forgot would refuse every upload.
func (f *fakeFiles) AcceptsFiles(_ context.Context, _ string, fresh statelog.Freshness) error {
	if fresh.Level == "" {
		return errors.New("fake: this project read names no level")
	}
	return f.notAccept
}

func (f *fakeFiles) Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
	return f.store.Open(ctx, o)
}

func (f *fakeFiles) Put(ctx context.Context, r io.Reader, limit int64,
	m objstore.PutMeta) (objstore.Object, error) {
	f.mu.Lock()
	f.objects++
	f.mu.Unlock()
	return f.store.Put(ctx, working{r: r, work: f.work}, limit, m)
}

func (f *fakeFiles) PutFileAs(_ context.Context, _, _ string, put tracker.FilePut) (tracker.WriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return tracker.WriteResult{}, f.refuse
	}
	f.puts++
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (f *fakeFiles) RemoveFileAs(context.Context, string, string, string, string, uint64) (tracker.WriteResult, error) {
	return tracker.WriteResult{}, errors.New("not in this suite")
}

// working is the store's side of an upload: it does its own work before it
// asks the body for more.
type working struct {
	r    io.Reader
	work time.Duration
}

func (w working) Read(p []byte) (int, error) {
	time.Sleep(w.work)
	// A PIECE AT A TIME, as a broker's messages or a bucket's reads are,
	// so a mebibyte takes many reads and the work between them adds up.
	return w.r.Read(p[:min(len(p), 256<<10)])
}

// filesApp is an authenticated node serving fake's files.
func filesApp(t *testing.T, fake api.ProjectFiles) *api.App {
	t.Helper()
	b := closedPosture()
	return newApp(t, api.Options{Bootstrap: &b, Files: fake})
}

// content is n bytes of no repeating shape.
func content(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i / 7)
	}
	return out
}

// upload PUTs body to reports/q3.bin, through w when one is given.
func upload(t *testing.T, a *api.App, body io.Reader, w http.ResponseWriter) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/work/files/ENG/reports/q3.bin", body)
	req.Header.Set("Authorization", "Bearer secret")
	a.ServeHTTP(w, req)
}

// answerOf decodes a JSON refusal.
func answerOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var answer map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("answer %q: %v", rec.Body.String(), err)
	}
	return answer
}

// AN UPLOAD IS CHARGED ONLY FOR THE TIME THE CLIENT IS WAITED ON. The store
// drives the reads now — it asks for the next piece once it has done its own
// work with the last — so a deadline measured by the wall clock would bill
// the client for every broker acknowledgement and every bucket part, and a
// slow store would fail a fast client. Every deadline the body is read under
// is what is left of its mebibyte's budget after the time spent INSIDE reads,
// which for a client that answers at once is all of it.
func TestAnUploadIsChargedOnlyWhileTheClientIsWaitedOn(t *testing.T) {
	t.Parallel()
	log := &paceLog{}
	fake := newFakeFiles(t)
	fake.work = time.Second
	a := filesApp(t, fake)

	body := content(objstore.MiB + objstore.MiB/2)
	w := &pacedWriter{ResponseRecorder: httptest.NewRecorder(), log: log}
	upload(t, a, bytes.NewReader(body), w)
	if w.Code != http.StatusOK {
		t.Fatalf("upload answered %d: %s", w.Code, w.Body.String())
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.out) < 6 {
		t.Fatalf("%d read deadlines for a mebibyte and a half in quarters: %v", len(log.out), log.events)
	}
	// THE STORE WORKED A SECOND BEFORE EVERY READ — three seconds into the
	// first mebibyte by its fourth read, which a wall-clock charge would
	// have taken out of the client's budget. Two seconds of slack is for
	// a loaded machine's scheduling, which lands inside a read too.
	var spent time.Duration
	for i, d := range log.out {
		if d < objstore.MiBPace-2*time.Second || d > objstore.MiBPace+time.Second {
			t.Errorf("read deadline %d was set %v out, want the whole %v — the store's "+
				"own work was charged to the client", i, d, objstore.MiBPace)
		}
		spent += fake.work
	}
	if spent < 4*time.Second {
		t.Fatalf("the store worked for %v in all, too little for a deadline charged "+
			"by the wall clock to show", spent)
	}
}

// A BODY THAT STOPS ARRIVING IS THE CLIENT'S, not the store's: it answers 400
// and records nothing. It answered 503 `unavailable`, which tells a client to
// retry a request whose own connection was what failed.
//
// AND A BODY CUT SHORT IS ONE THAT STOPPED, never one that ended: net/http
// answers a client that went away mid-body with io.ErrUnexpectedEOF, which an
// upload once took for the end of the file and RECORDED — the half that
// arrived, as the file, answered 200.
func TestAnUploadWhoseBodyStopsIsRefusedAsTheClients(t *testing.T) {
	t.Parallel()
	for _, stop := range []error{errors.New("i/o timeout"), io.ErrUnexpectedEOF} {
		t.Run(stop.Error(), func(t *testing.T) {
			t.Parallel()
			fake := newFakeFiles(t)
			a := filesApp(t, fake)

			cut := io.MultiReader(bytes.NewReader(content(objstore.MiB+10)),
				&failingReader{err: stop})
			rec := httptest.NewRecorder()
			upload(t, a, cut, rec)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("a body that stopped answered %d, want 400: %s", rec.Code, rec.Body.String())
			}
			answer := answerOf(t, rec)
			if answer["error"] != string(httpjson.CodeUnreadableBody) ||
				!strings.Contains(answer["detail"], stop.Error()) {
				t.Errorf("answer = %v, want unreadable_body naming the read's own failure", answer)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.puts != 0 {
				t.Errorf("a file was recorded from a body that never arrived")
			}
		})
	}
}

// AN UPLOAD REFUSED BY WHAT IS ALREADY KNOWN IS REFUSED BEFORE A BYTE IS
// READ: a declared length over the cap is the one spelling of a 413 every
// JSON surface answers, and a project that takes no files is named — rather
// than a gibibyte streamed into the store first, only to be refused by the
// write and left there for a day.
func TestAnUploadRefusedByWhatIsKnownReadsNothing(t *testing.T) {
	t.Parallel()
	t.Run("a declared length over the cap", func(t *testing.T) {
		t.Parallel()
		fake := newFakeFiles(t)
		a := filesApp(t, fake)
		req := httptest.NewRequest(http.MethodPut, "/work/files/ENG/big.bin",
			&failingReader{err: errors.New("the body was read")})
		req.ContentLength = tracker.MaxFileBytes + 1
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge ||
			answerOf(t, rec)["error"] != string(httpjson.CodeBodyTooLarge) {
			t.Fatalf("a declared length over the cap answered %d: %s", rec.Code, rec.Body.String())
		}
		if fake.objects != 0 {
			t.Error("the body was streamed into the store before the refusal")
		}
	})
	t.Run("an archived project", func(t *testing.T) {
		t.Parallel()
		fake := newFakeFiles(t)
		fake.notAccept = tracker.AcceptFiles("ENG", true)
		a := filesApp(t, fake)
		rec := httptest.NewRecorder()
		upload(t, a, bytes.NewReader([]byte("x")), rec)
		answer := answerOf(t, rec)
		if rec.Code != http.StatusBadRequest || answer["error"] != "invalid" ||
			!strings.Contains(answer["detail"], "archived") {
			t.Fatalf("an upload into an archived project answered %d: %s", rec.Code, rec.Body.String())
		}
		if fake.objects != 0 {
			t.Error("the body was streamed into the store before the refusal")
		}
	})
}

// AN UPLOAD TOO OLD TO BE NAMED IS `409 upload_expired`: the bytes are stored
// and the write may no longer name them, and sending the request again is
// the one thing that lands. A 503 would invite the same doomed retry of the
// record alone.
func TestAnUploadTooOldToNameIsExpired(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	fake.refuse = fmt.Errorf("%w: minted 13h ago", tracker.ErrUploadStale)
	a := filesApp(t, fake)
	rec := httptest.NewRecorder()
	upload(t, a, bytes.NewReader([]byte("late")), rec)
	if rec.Code != http.StatusConflict || answerOf(t, rec)["error"] != "upload_expired" {
		t.Fatalf("a stale upload answered %d: %s", rec.Code, rec.Body.String())
	}
}

type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }

// download GETs reports/q3.bin, answering what recovered from the handler.
func download(t *testing.T, a *api.App, w http.ResponseWriter) (panicked any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/work/files/ENG/reports/q3.bin", nil)
	req.Header.Set("Authorization", "Bearer secret")
	defer func() { panicked = recover() }()
	a.ServeHTTP(w, req)
	return nil
}

// A DOWNLOAD OF AN OBJECT THAT IS NOT ITS ROW'S ENDS SHORT OF ITS
// CONTENT-LENGTH: the store holds back the byte that would complete the file
// until the whole of it is verified, so the response is aborted with that byte
// unsent — never a whole response a client would take for the file. And a
// file small enough for its damage to show before the status is sent is
// answered as damaged, never as a 503 inviting a retry that reads the same
// bytes.
func TestADownloadOfADamagedObjectNeverEndsWhole(t *testing.T) {
	t.Parallel()
	t.Run("damage found after the status is sent", func(t *testing.T) {
		t.Parallel()
		fake := newFakeFiles(t)
		data := content(objstore.MiB + 100)
		file := fake.holding(t, data)
		damaged := append([]byte(nil), data...)
		damaged[len(damaged)-1] ^= 0xff
		fake.backend.Corrupt(file.Object.Name(), damaged)
		a := filesApp(t, fake)
		rec := httptest.NewRecorder()
		aborted := download(t, a, rec)
		if err, _ := aborted.(error); !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("a damaged download ended with %v (status %d), want the response aborted",
				aborted, rec.Code)
		}
		if rec.Body.Len() >= len(data) {
			t.Fatalf("the response carried %d of %d bytes — a damaged file reached the "+
				"client whole", rec.Body.Len(), len(data))
		}
	})
	t.Run("damage found in the head", func(t *testing.T) {
		t.Parallel()
		fake := newFakeFiles(t)
		file := fake.holding(t, []byte("a small report"))
		fake.backend.Corrupt(file.Object.Name(), []byte("a small rep0rt"))
		a := filesApp(t, fake)
		rec := httptest.NewRecorder()
		if aborted := download(t, a, rec); aborted != nil {
			t.Fatal(aborted)
		}
		if rec.Code != http.StatusInternalServerError || answerOf(t, rec)["error"] != "content_corrupt" {
			t.Fatalf("a damaged small file answered %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// A FILE AN EARLIER BUILD KEPT IN CHUNKS IS GONE, NOT UNAVAILABLE: `410
// content_retired`, because a 503 tells a client to retry and no retry will
// ever read it.
func TestADownloadOfARetiredFileIsGone(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	fake.file = &tracker.File{Version: 1, Project: "ENG", Path: "reports/q3.bin",
		Hash: objstore.HashOf([]byte("kept in chunks")), Size: 14}
	a := filesApp(t, fake)
	rec := httptest.NewRecorder()
	if aborted := download(t, a, rec); aborted != nil {
		t.Fatal(aborted)
	}
	if rec.Code != http.StatusGone || answerOf(t, rec)["error"] != "content_retired" {
		t.Fatalf("a retired file answered %d: %s", rec.Code, rec.Body.String())
	}
}

// A DOWNLOAD'S EVERY MEBIBYTE HAS ITS OWN WINDOW, opened once its bytes are in
// hand. The route once copied with no write deadline, so a client that opened
// a download and never read it held the handler and its connection; and a
// deadline set before the bytes were read from the store would have spent the
// client's window on the store.
func TestADownloadWritesEachMebibyteInAWindowOfItsOwn(t *testing.T) {
	t.Parallel()
	log := &paceLog{}
	fake := newFakeFiles(t)
	data := content(2*objstore.MiB + 100)
	fake.holding(t, data)
	a := filesApp(t, fake)

	w := &pacedWriter{ResponseRecorder: httptest.NewRecorder(), log: log}
	if aborted := download(t, a, w); aborted != nil {
		t.Fatal(aborted)
	}
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("download answered %d with %d bytes, want 200 with %d", w.Code, w.Body.Len(), len(data))
	}

	// EVERY WRITE DIRECTLY FOLLOWS ITS OWN DEADLINE.
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
	if deadlines != 3 {
		t.Errorf("%d write deadlines for two mebibytes and a hundred bytes: %v", deadlines, events)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	for i, d := range log.out {
		if d < objstore.MiBPace-time.Second || d > objstore.MiBPace+time.Second {
			t.Errorf("deadline %d was set %v out, want %v", i, d, objstore.MiBPace)
		}
	}
}
