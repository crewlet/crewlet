package workapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/api/paced"
	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
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
//
// And on a CLOCK OF THE SUITE'S OWN ([workapi.Options.PaceClock]), moved by
// exactly the time a fake store's work takes, never slept through — so every
// deadline is measured exactly. The clock starts years from the wall clock,
// which is what keeps that honest: a route that read the wall clock instead of
// its pace clock would set deadlines decades from where this one stands.

// paceClock is a clock that moves only when a case says time passed.
type paceClock struct {
	mu sync.Mutex
	at time.Time
}

func newPaceClock() *paceClock {
	return &paceClock{at: time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)}
}

func (c *paceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *paceClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// paceLog is the order things happened in, shared by the fake store and the
// capturing writer.
type paceLog struct {
	clock *paceClock

	mu     sync.Mutex
	events []string
	// out is how far past the moment it was set each deadline was, on the
	// pace clock.
	out []time.Duration
}

func (l *paceLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *paceLog) deadline(kind string, at time.Time) {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, kind)
	l.out = append(l.out, at.Sub(now))
}

func (l *paceLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// pacedWriter is a recorder that can carry a read and a write deadline, which
// is what http.NewResponseController looks for, and logs each one it is set.
// Each write takes the client take on the pace clock.
type pacedWriter struct {
	*httptest.ResponseRecorder
	log  *paceLog
	take time.Duration
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
	w.log.clock.advance(w.take)
	return w.ResponseRecorder.Write(p)
}

// fakeFiles is a project's files over a real object store on the in-memory
// backend, so every read is checked against its row exactly as a node's is —
// the file rows and the file writer the tracker's half hands the surface, the
// project an upload is refused on, and the object store it streams through.
type fakeFiles struct {
	store   *objstore.Store
	backend *memobj.Backend

	// clock is the surface's pace clock, which the store's work moves.
	clock *paceClock

	// work is the store's own time between two reads of an upload's body
	// — a broker's acknowledgements, a bucket's part — which no client
	// may be charged for; and readWork its time to hand a download each
	// piece of a file, which no client may be charged for either.
	work, readWork time.Duration

	mu       sync.Mutex
	file     *worklet.File
	puts     []worklet.FilePut
	removals []string
	opIDs    []string
	objects  int
	refuse   error
	archived bool
	result   statelog.Result

	// readErr fails every read of a file's row, as a node that could not
	// serve it answers.
	readErr error

	// putLimit, where set, is the cap the store holds an upload to in place
	// of the surface's own, so a body can pass it without a gibibyte.
	putLimit int64
}

func newFakeFiles(t *testing.T) *fakeFiles {
	t.Helper()
	backend := memobj.New()
	s, err := objstore.NewStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeFiles{store: s, backend: backend, clock: newPaceClock()}
}

// paceLog is a log of the pace this fake's surface keeps.
func (f *fakeFiles) paceLog() *paceLog { return &paceLog{clock: f.clock} }

// holding stores content as the file at reports/q3.bin, and answers its row.
func (f *fakeFiles) holding(t *testing.T, content []byte) worklet.File {
	t.Helper()
	o, err := f.store.Put(t.Context(), bytes.NewReader(content), worklet.MaxFileBytes, objstore.PutMeta{})
	if err != nil {
		t.Fatal(err)
	}
	file := worklet.File{Version: 2, Project: "ENG", Path: "reports/q3.bin",
		Hash: o.Hash, Size: o.Size, Object: o.Key}
	f.file = &file
	return file
}

func (f *fakeFiles) Files(context.Context, worklet.FileQuery) (worklet.FileListing, error) {
	return worklet.FileListing{}, errors.New("fake: the listing is the read surface's")
}

func (f *fakeFiles) File(context.Context, string, string, statelog.Freshness) (worklet.FileDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return worklet.FileDetail{}, f.readErr
	}
	if f.file == nil {
		return worklet.FileDetail{}, worklet.ErrNoFile
	}
	return worklet.FileDetail{File: *f.file}, nil
}

func (f *fakeFiles) Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
	rc, err := f.store.Open(ctx, o)
	if err != nil || f.readWork == 0 {
		return rc, err
	}
	return struct {
		io.Reader
		io.Closer
	}{working{r: rc, work: f.readWork, clock: f.clock}, rc}, nil
}

func (f *fakeFiles) Put(ctx context.Context, r io.Reader, limit int64,
	m objstore.PutMeta) (objstore.Object, error) {
	f.mu.Lock()
	f.objects++
	if f.putLimit > 0 {
		limit = f.putLimit
	}
	f.mu.Unlock()
	return f.store.Put(ctx, working{r: r, work: f.work, clock: f.clock}, limit, m)
}

func (f *fakeFiles) PutFile(_ context.Context, opID string, put worklet.FilePut) (
	worklet.WriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opIDs = append(f.opIDs, opID)
	if f.refuse != nil {
		return worklet.WriteResult{}, f.refuse
	}
	f.puts = append(f.puts, put)
	return f.written(opID), nil
}

func (f *fakeFiles) RemoveFile(_ context.Context, opID, project, path string, _ uint64) (
	worklet.WriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opIDs = append(f.opIDs, opID)
	if f.refuse != nil {
		return worklet.WriteResult{}, f.refuse
	}
	f.removals = append(f.removals, project+"/"+path)
	return f.written(opID), nil
}

// written is what the next write answers: the case's result, applied by
// default.
func (f *fakeFiles) written(opID string) worklet.WriteResult {
	res := f.result
	if res.Outcome == "" {
		res.Outcome = statelog.OutcomeApplied
	}
	res.OpID, res.Version = opID, 3
	return worklet.WriteResult{Result: res}
}

// projects is the tracker's read side as an upload's early refusal reads it:
// the one project, ENG, archived where the case says so. Every other method
// panics through the nil interface it embeds.
type projects struct {
	builtin.WorkReader
	f *fakeFiles
}

func (p projects) Projects(context.Context, worklet.ProjectQuery) (worklet.ProjectListing, error) {
	return worklet.ProjectListing{}, errors.New("fake: not in this suite")
}

// Project refuses a read naming no level, as the tracker's reader does — the
// surface resolves one, and a route that forgot would refuse every upload.
func (p projects) Project(_ context.Context, q worklet.ProjectDetailQuery) (
	worklet.ProjectDetail, error) {
	if q.Level == "" {
		return worklet.ProjectDetail{}, errors.New("fake: this project read names no level")
	}
	if q.Project != "ENG" {
		return worklet.ProjectDetail{}, fmt.Errorf("%w: %s", worklet.ErrNoProject, q.Project)
	}
	return worklet.ProjectDetail{Key: "ENG", Archived: p.f.archived}, nil
}

// halves are the tracker's file half and the object store over this fake.
func (f *fakeFiles) halves() workapi.Halves {
	return workapi.Halves{
		Work: builtin.WorkDeps{
			Reader: projects{f: f},
			Files:  f,
			FileWriter: func(builtin.Actor) builtin.FileWriter {
				return f
			},
		},
		Objects: f,
	}
}

// working is the store's side of an upload: it does its own work before it
// asks the body for more — on the pace clock, which the work moves by its
// length.
type working struct {
	r     io.Reader
	work  time.Duration
	clock *paceClock
}

func (w working) Read(p []byte) (int, error) {
	w.clock.advance(w.work)
	// A PIECE AT A TIME, as a broker's messages or a bucket's reads are,
	// so a mebibyte takes many reads and the work between them adds up.
	return w.r.Read(p[:min(len(p), 256<<10)])
}

// fileSurface is the write surface serving fake's files, keeping their pace
// on fake's clock, and the audit it records into.
type fileSurface struct {
	t     *testing.T
	mux   *http.ServeMux
	audit *auditLog
}

func filesSurface(t *testing.T, fake *fakeFiles, c authz.Chart) *fileSurface {
	t.Helper()
	return filesSurfaceOver(t, serving(fake.halves()), c, fake.clock.Now)
}

// filesSurfaceOver is a surface over a halves source a case states.
func filesSurfaceOver(t *testing.T, halves func() (workapi.Halves, bool),
	c authz.Chart, pace func() time.Time) *fileSurface {
	t.Helper()
	r := &rig{t: t, audit: &auditLog{}}
	svc, err := workapi.New(workapi.Options{
		Halves: halves, Chart: c, Operator: r.operator(c, halves),
		Audit: r.audit, PaceClock: pace,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	return &fileSurface{t: t, mux: mux, audit: r.audit}
}

// serve runs one request through the surface as who, answering what recovered
// from the handler.
func (s *fileSurface) serve(w http.ResponseWriter, req *http.Request,
	who iam.Principal) (panicked any) {
	defer func() { panicked = recover() }()
	s.mux.ServeHTTP(w, req.WithContext(iam.WithPrincipal(req.Context(), who)))
	return nil
}

// content is n bytes of no repeating shape.
func content(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i / 7)
	}
	return out
}

// upload PUTs body to reports/q3.bin as an administrator, through w, under
// the headers given in pairs.
func (s *fileSurface) upload(body io.Reader, w http.ResponseWriter, headers ...string) {
	s.t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/worklet/files/ENG/reports/q3.bin", body)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	if aborted := s.serve(w, req, admin("ana")); aborted != nil {
		s.t.Fatal(aborted)
	}
}

// download GETs reports/q3.bin as an administrator, answering what recovered
// from the handler.
func (s *fileSurface) download(w http.ResponseWriter) (panicked any) {
	req := httptest.NewRequest(http.MethodGet, "/worklet/files/ENG/reports/q3.bin", nil)
	return s.serve(w, req, admin("ana"))
}

// answerOf decodes a JSON answer.
func answerOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var answer map[string]any
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
	fake := newFakeFiles(t)
	log := fake.paceLog()
	fake.work = time.Second
	s := filesSurface(t, fake, chart{})

	body := content(objstore.MiB + objstore.MiB/2)
	w := &pacedWriter{ResponseRecorder: httptest.NewRecorder(), log: log}
	s.upload(bytes.NewReader(body), w)
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
	// have taken out of the client's budget — and the client, a reader in
	// memory, took no time at all. So every deadline is the WHOLE pace,
	// exactly: one charged from the mebibyte's first read would be a
	// second short for every read before it.
	for i, d := range log.out {
		if d != objstore.MiBPace {
			t.Errorf("read deadline %d was set %v out, want the whole %v — the store's "+
				"own work was charged to the client", i, d, objstore.MiBPace)
		}
	}
	if spent := time.Duration(len(log.out)) * fake.work; spent < 4*time.Second {
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
//
// TOLD IN A PERSON'S WORDS ([paced.Unread]): the detail was the read's own
// failure as Go spells it — "the body could not be read: unexpected EOF", "…:
// i/o timeout" — which goes to the log. Mutation: answer err.Error() again
// and both rows go red.
func TestAnUploadWhoseBodyStopsIsRefusedAsTheClients(t *testing.T) {
	t.Parallel()
	for _, stop := range []error{errors.New("i/o timeout"), io.ErrUnexpectedEOF} {
		t.Run(stop.Error(), func(t *testing.T) {
			t.Parallel()
			fake := newFakeFiles(t)
			s := filesSurface(t, fake, chart{})

			cut := io.MultiReader(bytes.NewReader(content(objstore.MiB+10)),
				&failingReader{err: stop})
			rec := httptest.NewRecorder()
			s.upload(cut, rec)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("a body that stopped answered %d, want 400: %s", rec.Code, rec.Body.String())
			}
			answer := answerOf(t, rec)
			detail, _ := answer["detail"].(string)
			if answer["error"] != string(httpjson.CodeUnreadableBody) ||
				detail != paced.Unread || strings.Contains(detail, stop.Error()) {
				t.Errorf("answer = %v, want unreadable_body saying %q and not "+
					"the read's own failure", answer, paced.Unread)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.puts) != 0 {
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
		s := filesSurface(t, fake, chart{})
		req := httptest.NewRequest(http.MethodPut, "/worklet/files/ENG/big.bin",
			&failingReader{err: errors.New("the body was read")})
		req.ContentLength = worklet.MaxFileBytes + 1
		rec := httptest.NewRecorder()
		if aborted := s.serve(rec, req, admin("ana")); aborted != nil {
			t.Fatal(aborted)
		}
		answer := answerOf(t, rec)
		if rec.Code != http.StatusRequestEntityTooLarge ||
			answer["error"] != string(httpjson.CodeBodyTooLarge) ||
			answer["detail"] != tooLargeSaid {
			t.Fatalf("a declared length over the cap answered %d: %s", rec.Code, rec.Body.String())
		}
		if fake.objects != 0 {
			t.Error("the body was streamed into the store before the refusal")
		}
	})
	t.Run("an archived project", func(t *testing.T) {
		t.Parallel()
		fake := newFakeFiles(t)
		fake.archived = true
		s := filesSurface(t, fake, chart{})
		rec := httptest.NewRecorder()
		s.upload(bytes.NewReader([]byte("x")), rec)
		answer := answerOf(t, rec)
		detail, _ := answer["detail"].(string)
		// THE PROJECT'S STATE, answered as every surface answers it: `409
		// conflict`, never a request to fix, and in its code's own message,
		// since nobody has worded it for a person yet (ADR-0060).
		if rec.Code != http.StatusConflict || answer["error"] != "conflict" || detail != "" {
			t.Fatalf("an upload into an archived project answered %d: %s", rec.Code, rec.Body.String())
		}
		if fake.objects != 0 {
			t.Error("the body was streamed into the store before the refusal")
		}
	})
	t.Run("a project there is none of", func(t *testing.T) {
		t.Parallel()
		fake := newFakeFiles(t)
		s := filesSurface(t, fake, chart{})
		req := httptest.NewRequest(http.MethodPut, "/worklet/files/NOPE/a.bin",
			&failingReader{err: errors.New("the body was read")})
		rec := httptest.NewRecorder()
		if aborted := s.serve(rec, req, admin("ana")); aborted != nil {
			t.Fatal(aborted)
		}
		if rec.Code != http.StatusNotFound ||
			answerOf(t, rec)["error"] != string(httpjson.CodeNoProject) {
			t.Fatalf("an upload into no project answered %d: %s", rec.Code, rec.Body.String())
		}
		// IN THE TRACKER'S WORDS, never its package's name.
		if strings.Contains(rec.Body.String(), "worklet:") {
			t.Errorf("the refusal tells a person the package's name: %s", rec.Body.String())
		}
		if fake.objects != 0 {
			t.Error("the body was streamed into the store before the refusal")
		}
	})
}

// fullBucket is a backend with no room left: a put reads its content and is
// refused as a backend refuses one for room ([objstore.Full]), storing
// nothing, naming a room of the case's own ([caseRemedy]) — so a hint naming
// it spliced the store's remedy rather than a spelling of the route's own.
type fullBucket struct{ objstore.Backend }

// caseRemedy is the room [fullBucket] names.
const caseRemedy = "an operator makes room in this case's own bucket"

func (fullBucket) Put(_ context.Context, name string, r io.Reader, _ objstore.PutMeta) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	return &objstore.Full{Remedy: caseRemedy, Err: fmt.Errorf(
		"natsobj: put %s: OBJ_crewlet_files holds as many bytes as it reserves", name)}
}

// fullS3 is an S3 bucket whose provider has no room left — MinIO's drives at
// their floor — through the production backend: every write is refused `507
// XMinioStorageFull`.
func fullS3(t *testing.T) objstore.Backend {
	t.Helper()
	srv := s3fake.Start(t, "files")
	b, err := s3obj.Open(t.Context(), s3obj.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "files", PathStyle: true,
		AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	})
	if err != nil {
		t.Fatalf("opening the S3 fake: %v", err)
	}
	srv.Fill(http.StatusInsufficientStorage, "XMinioStorageFull")
	return b
}

// AN UPLOAD THE OBJECT STORE HAS NO ROOM FOR IS `507 store_full`, NAMING THE
// ROOM AN OPERATOR MAKES: the store refuses the same upload the same way until
// then, so the answer is HTTP's own status for a server that cannot store what
// a request needs, with no Retry-After — never the 503 that asked a client to
// send the same gibibyte again in two seconds. Nothing is recorded, and the
// store's own words, naming its bucket, stay in the node's log. Over the
// fleet's own bucket's twin, and over S3 whose provider has no room.
//
// Mutation: answer the full store through the catch-all (503 unavailable) and
// this fails on the status, the code and the Retry-After; splice a room of the
// route's own and it fails on the hint; drop s3obj's classification and the
// s3fake arm fails.
func TestAnUploadTheStoreHasNoRoomForIsRefusedAsFull(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name    string
		backend func(t *testing.T, fake *fakeFiles) objstore.Backend
		// remedy is the room the store names, and said its own words.
		remedy, said string
	}{
		{"the nats twin", func(_ *testing.T, fake *fakeFiles) objstore.Backend {
			return fullBucket{fake.backend}
		}, caseRemedy, "OBJ_crewlet_files"},
		{"s3fake", func(t *testing.T, _ *fakeFiles) objstore.Backend { return fullS3(t) },
			s3obj.FullRemedy, "XMinioStorageFull"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeFiles(t)
			store, err := objstore.NewStore(arm.backend(t, fake))
			if err != nil {
				t.Fatal(err)
			}
			fake.store = store
			s := filesSurface(t, fake, chart{})
			rec := httptest.NewRecorder()
			s.upload(bytes.NewReader([]byte("no room")), rec)

			answer := answerOf(t, rec)
			hint, _ := answer["hint"].(string)
			switch {
			case rec.Code != http.StatusInsufficientStorage || answer["error"] != string(httpjson.CodeStoreFull):
				t.Fatalf("an upload the store had no room for answered %d: %s, want 507 store_full",
					rec.Code, rec.Body.String())
			case rec.Header().Get("Retry-After") != "":
				t.Errorf("an upload the store had no room for asked its client back in %s seconds",
					rec.Header().Get("Retry-After"))
			case !strings.Contains(hint, arm.remedy):
				t.Errorf("an upload the store had no room for answered the hint %q, which does not name %q",
					hint, arm.remedy)
			case strings.Contains(rec.Body.String(), arm.said):
				t.Errorf("the object store's own words reached the client: %s", rec.Body.String())
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.puts) != 0 {
				t.Errorf("a file was recorded whose content the store had no room for")
			}
		})
	}
}

// THE WRITER'S REFUSALS ABOUT THE FILE KEEP THEIR OWN CODES, and every other
// refusal reads as it reads on every write here.
//
// A version moved past is HTTP's own `412` on the resource its ETag names, and
// an upload too old to be named is `409 upload_expired` — the bytes are stored
// and the write may no longer name them, and sending the request again is the
// one thing that lands, where a 503 would invite the same doomed retry of the
// record alone. A conflict the domain names, the project's state and a value
// it refuses go through the one refusal mapping ([operator.Fail]), as they
// would from any tool; the byte routes used to
// answer every refusal they did not name with a 503, so a fault told a client
// to retry for ever. And each is told in the writer's sentence: the codes of
// their own answered the error's text, which opens with the package's name.
//
// Mutation: answer any of the file's own codes with err.Error() again and its
// row names the package.
func TestTheWritersRefusalsReadAsTheFileRoutesDocument(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		refuse error
		status int
		code   httpjson.Code
	}{
		{"a version moved past", fmt.Errorf("%w: reports/q3.bin is at 4", worklet.ErrStaleVersion),
			http.StatusPreconditionFailed, httpjson.CodeVersionMoved},
		{"an upload too old to name", fmt.Errorf("%w: minted 13h ago", worklet.ErrUploadStale),
			http.StatusConflict, httpjson.CodeUploadExpired},
		{"a project gone since the early read", fmt.Errorf("%w: ENG", worklet.ErrNoProject),
			http.StatusNotFound, httpjson.CodeNoProject},
		{"a race the domain names", fmt.Errorf("%w: another write landed", statelog.ErrConflict),
			http.StatusConflict, httpjson.CodeConflict},
		// THE ONE RULE, BY THE ONE MAPPING: a project archived since the
		// early read is its state, `409 conflict`, and a content type no
		// row may hold is the request's, `422 invalid` — this route
		// answered both `400 invalid` from an arm of its own.
		{"a project archived since the early read", worklet.AcceptFiles("ENG", true),
			http.StatusConflict, httpjson.CodeConflict},
		{"a value the writer refuses", &worklet.Refusal{Mark: worklet.ErrInvalid,
			Err: errors.New("the content type spans lines")},
			http.StatusUnprocessableEntity, httpjson.CodeInvalid},
		{"a fault of the node's own", errors.New("disk I/O error"),
			http.StatusInternalServerError, httpjson.CodeInternalError},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeFiles(t)
			fake.refuse = c.refuse
			s := filesSurface(t, fake, chart{})
			rec := httptest.NewRecorder()
			s.upload(bytes.NewReader([]byte("late")), rec)
			if rec.Code != c.status || answerOf(t, rec)["error"] != string(c.code) {
				t.Fatalf("answered %d %s, want %d %s", rec.Code, rec.Body.String(),
					c.status, c.code)
			}
			if c.status == http.StatusInternalServerError &&
				strings.Contains(rec.Body.String(), "disk") {
				t.Errorf("a fault's own words reached the client: %s", rec.Body.String())
			}
			if detail, _ := answerOf(t, rec)["detail"].(string); strings.Contains(detail,
				"worklet:") || strings.Contains(detail, "statelog") {
				t.Errorf("the detail a person reads names a package: %s", detail)
			}
		})
	}
}

// A PATH THE TRACKER REFUSES IS TOLD IN ITS SENTENCE: `400 bad_path` naming
// what is wrong with it, and never the error's text, which opens with the
// package's name ("worklet: the path … ends in a slash").
//
// Mutation: answer the refusal with err.Error() again and the detail names
// the package.
func TestARefusedPathIsToldInTheTrackersSentence(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	s := filesSurface(t, fake, chart{})
	req := httptest.NewRequest(http.MethodPut, "/worklet/files/ENG/reports/",
		bytes.NewReader([]byte("x")))
	rec := httptest.NewRecorder()
	if aborted := s.serve(rec, req, admin("ana")); aborted != nil {
		t.Fatal(aborted)
	}
	answer := answerOf(t, rec)
	detail, _ := answer["detail"].(string)
	if rec.Code != http.StatusBadRequest || answer["error"] != string(httpjson.CodeBadPath) ||
		!strings.Contains(detail, "ends in a slash") || strings.Contains(detail, "worklet") {
		t.Errorf("a refused path answered %d %v, want 400 bad_path in the "+
			"tracker's sentence", rec.Code, answer)
	}
}

// A RETRY UNDER THE SAME KEY IS THE SAME OPERATION ONLY FOR THE SAME BYTES.
//
// The byte routes take their operation from the request's `Idempotency-Key`,
// as every write on this surface does — they took an `op_id` query parameter
// once, which no other write read — and the operation is bound to what the
// request asks: the same content under the same key is the retry an unknown
// answer asks for, so it derives the same operation and the ledger answers it
// with the first copy's result; other content under that key is another
// write, which must land rather than be answered with the first one's outcome.
// And the answer names the key the retry must send, carrying the key's
// instant.
//
// Mutations: bind the operation to the key alone and the second content
// collapses into the first; derive it per request and the retry is a second
// write.
func TestARetryOfAnUploadIsTheSameOperationOnlyForTheSameBytes(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	s := filesSurface(t, fake, chart{})
	key := statelog.NewOpID(time.Now(), "")

	sent := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		s.upload(strings.NewReader(body), rec, opkey.Header, key)
		if rec.Code != http.StatusOK {
			t.Fatalf("upload answered %d: %s", rec.Code, rec.Body.String())
		}
		return answerOf(t, rec)
	}
	first := sent("first draft")
	retried := sent("first draft")
	other := sent("second draft")

	fake.mu.Lock()
	ops := append([]string(nil), fake.opIDs...)
	fake.mu.Unlock()
	if len(ops) != 3 {
		t.Fatalf("three uploads wrote %d records: %v", len(ops), ops)
	}
	if ops[0] != ops[1] {
		t.Errorf("the same bytes under the same key derived %q and %q — a retry "+
			"would be a second write", ops[0], ops[1])
	}
	if ops[2] == ops[0] {
		t.Errorf("other bytes under the same key derived the first upload's "+
			"operation %q — the ledger would answer it with nothing written", ops[0])
	}
	for _, op := range ops {
		assertMintedWith(t, op, key)
	}
	if first["op_id"] != retried["op_id"] || first["op_id"] != other["op_id"] {
		t.Errorf("answers named %v, %v and %v, want the one key every retry sends",
			first["op_id"], retried["op_id"], other["op_id"])
	}
}

// AN UPLOAD AND A REMOVAL ARE RECORDED, and a removal is the project's lead's.
//
// The byte routes make their writes without a tool, so they record the
// `operator_acted` row a tool's call leaves themselves — under the
// operation the answer named, which is what a reader of the trail looks the
// call up by. And a removal takes the file out of the project, which is its
// lead's or the deployment's — never every colleague who may add one, who is
// refused and recorded as refused.
//
// Mutation: audit the file's address in place of the operation and the row
// names a request no answer ever named; decide the removal on the upload's
// verb and the colleague's removal lands.
func TestTheFileWritesAreRecordedAndTheRemovalIsTheLeads(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	s := filesSurface(t, fake, chart{})

	put := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/worklet/files/ENG/notes.md", strings.NewReader("hi"))
	if aborted := s.serve(put, req, colleague("bo")); aborted != nil {
		t.Fatal(aborted)
	}
	if put.Code != http.StatusOK {
		t.Fatalf("a colleague's upload answered %d: %s", put.Code, put.Body.String())
	}
	refused := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/worklet/files/ENG/notes.md", nil)
	if aborted := s.serve(refused, req, colleague("bo")); aborted != nil {
		t.Fatal(aborted)
	}
	if refused.Code != http.StatusForbidden {
		t.Fatalf("a colleague's removal answered %d: %s", refused.Code, refused.Body.String())
	}
	removed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/worklet/files/ENG/notes.md", nil)
	if aborted := s.serve(removed, req, colleague("cto")); aborted != nil {
		t.Fatal(aborted)
	}
	if removed.Code != http.StatusOK {
		t.Fatalf("the project lead's removal answered %d: %s", removed.Code, removed.Body.String())
	}
	if len(fake.removals) != 1 || fake.removals[0] != "ENG/notes.md" {
		t.Errorf("removals = %v, want the lead's alone", fake.removals)
	}

	// A REFUSED DOWNLOAD IS A READ, and leaves no row — as a read refused
	// through the operator's dispatch leaves none.
	read := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/worklet/files/ENG/notes.md", nil)
	if aborted := s.serve(read, req, person("ext")); aborted != nil {
		t.Fatal(aborted)
	}
	if read.Code != http.StatusForbidden {
		t.Fatalf("a download by a person holding no grant answered %d: %s",
			read.Code, read.Body.String())
	}

	acted := s.audit.acted()
	if len(acted) != 3 {
		t.Fatalf("three file writes and a refused read left %d audit records: %+v",
			len(acted), acted)
	}
	want := []struct {
		tool    authz.Action
		outcome types.AuditOutcome
		op      any
		actor   string
	}{
		{authz.ActionFileWrite, types.AuditApplied, answerOf(t, put)["op_id"], "bo"},
		{authz.ActionFileRemove, types.AuditRefused, nil, "bo"},
		{authz.ActionFileRemove, types.AuditApplied, answerOf(t, removed)["op_id"], "cto"},
	}
	for i, w := range want {
		got := acted[i]
		if got.Transport != types.TransportWork || got.Tool != string(w.tool) ||
			got.Outcome != w.outcome || got.ActorName != w.actor {
			t.Errorf("record %d is %+v, want %s %s by %s over %s", i, got, w.tool,
				w.outcome, w.actor, types.TransportWork)
		}
		if w.op != nil && got.RequestID != w.op {
			t.Errorf("record %d names request %q, the answer named %v", i,
				got.RequestID, w.op)
		}
	}
}

// THE BYTE ROUTES ARE SERVED BY THE HALVES THEIR REQUEST FINDS, for
// [TestARouteIsServedByTheHalvesItsRequestFinds]' reason: before a node's
// first company they are `503 no_active_revision`, and on a node whose company
// keeps its tracker here but which runs no object store they are the route's
// absence — the one wait cannot bring.
//
// Mutation: take the object store as the work half's, and the second phase
// answers as though the bytes were served.
func TestTheByteRoutesAreServedByTheHalvesTheirRequestFinds(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	full := fake.halves()
	noStore := full
	noStore.Objects = nil
	for _, c := range []struct {
		name   string
		halves workapi.Halves
		up     bool
		status int
		code   httpjson.Code
	}{
		{"before any company", workapi.Halves{}, false,
			http.StatusServiceUnavailable, httpjson.CodeNoActiveRevision},
		{"no object store", noStore, true, http.StatusNotFound, httpjson.CodeNoRoute},
	} {
		halves := func() (workapi.Halves, bool) { return c.halves, c.up }
		s := filesSurfaceOver(t, halves, chart{}, fake.clock.Now)
		rec := httptest.NewRecorder()
		s.upload(strings.NewReader("x"), rec)
		if rec.Code != c.status || answerOf(t, rec)["error"] != string(c.code) {
			t.Errorf("%s: answered %d %s, want %d %s", c.name, rec.Code,
				rec.Body.String(), c.status, c.code)
		}
		if fake.objects != 0 {
			t.Errorf("%s: the body was stored", c.name)
		}
	}
}

// A FILE'S BYTES ARE SERVED WHERE NO WRITER IS. A node in a maintenance mode
// hands no writer to any surface and serves its reads, the file listing among
// them — so the download, a read, is served by the halves that hold the rows
// and the object store, while the upload and the removal, which write, answer
// the mux's own 404 and reach no store. One predicate over both halves answered
// every download on such a node as a route it does not serve.
//
// Mutation: gate the download on servesFileWrites and the download is a 404.
func TestAFilesBytesAreServedWhereNoWriterIs(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	data := []byte("the quarter's report")
	fake.holding(t, data)
	stored := fake.objects
	reads := fake.halves()
	reads.Work.FileWriter = nil
	s := filesSurfaceOver(t, serving(reads), chart{}, fake.clock.Now)

	rec := httptest.NewRecorder()
	if aborted := s.download(rec); aborted != nil {
		t.Fatal(aborted)
	}
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("a node with no writer answered the download %d %q, want 200 "+
			"and the file", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.upload(strings.NewReader("x"), rec)
	if rec.Code != http.StatusNotFound || answerOf(t, rec)["error"] != string(httpjson.CodeNoRoute) {
		t.Errorf("a node with no writer answered the upload %d %s, want the "+
			"mux's own 404", rec.Code, rec.Body.String())
	}
	if fake.objects != stored {
		t.Error("a node with no writer stored an upload's body")
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/worklet/files/ENG/reports/q3.bin", nil)
	if aborted := s.serve(rec, req, admin("ana")); aborted != nil {
		t.Fatal(aborted)
	}
	if rec.Code != http.StatusNotFound || answerOf(t, rec)["error"] != string(httpjson.CodeNoRoute) {
		t.Errorf("a node with no writer answered the removal %d %s, want the "+
			"mux's own 404", rec.Code, rec.Body.String())
	}
}

type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }

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
		s := filesSurface(t, fake, chart{})
		rec := httptest.NewRecorder()
		aborted := s.download(rec)
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
		s := filesSurface(t, fake, chart{})
		rec := httptest.NewRecorder()
		if aborted := s.download(rec); aborted != nil {
			t.Fatal(aborted)
		}
		if rec.Code != http.StatusInternalServerError || answerOf(t, rec)["error"] != "content_corrupt" {
			t.Fatalf("a damaged small file answered %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// A LIVE FILE NAMING NO OBJECT IS THE ENGINE'S FAULT: the applier refuses to
// write one, so a download that meets one is a 500 — never a 503 a client
// retries for ever, and never a 404 or 410 for a file that is listed.
func TestADownloadOfALiveFileNamingNoObjectIsAFault(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	fake.file = &worklet.File{Version: 1, Project: "ENG", Path: "reports/q3.bin",
		Hash: objstore.HashOf([]byte("no object")), Size: 9}
	s := filesSurface(t, fake, chart{})
	rec := httptest.NewRecorder()
	if aborted := s.download(rec); aborted != nil {
		t.Fatal(aborted)
	}
	if rec.Code != http.StatusInternalServerError ||
		answerOf(t, rec)["error"] != string(httpjson.CodeInternalError) {
		t.Fatalf("a live file naming no object answered %d: %s", rec.Code, rec.Body.String())
	}
}

// A DOWNLOAD NO DATA NODE COULD SERVE IS A 503, NEVER A 500. The file's row is
// read through the estate's router, which answers that no data node served it
// while every copy is out of service or a peer is restarting — which clears
// when one serves again. The route answered it as a fault of this node: `500`
// with no Retry-After, so a client gave up on a file it could have had in a
// few seconds.
//
// Mutation: drop the ErrUnserved arm from builtin.Condition and this answers
// 500.
func TestADownloadNoDataNodeServedIsUnavailable(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	fake.readErr = &estate.ErrUnserved{Detail: "data-b: out_of_service"}
	s := filesSurface(t, fake, chart{})
	rec := httptest.NewRecorder()
	if aborted := s.download(rec); aborted != nil {
		t.Fatal(aborted)
	}
	if rec.Code != http.StatusServiceUnavailable ||
		answerOf(t, rec)["error"] != string(httpjson.CodeUnavailable) {
		t.Fatalf("a download no data node served answered %d: %s", rec.Code, rec.Body.String())
	}
	if after := rec.Header().Get("Retry-After"); after != strconv.Itoa(authz.RetryUndecidedSeconds) {
		t.Errorf("Retry-After = %q, want this node's undecided hint", after)
	}
	if strings.Contains(rec.Body.String(), "data-b") {
		t.Errorf("the router's detail reached the client: %s", rec.Body.String())
	}
}

// A DOWNLOAD FLOORED ON ANOTHER LOG IS THE REQUEST'S MISTAKE. The reader
// refuses a `min_position` on another domain's log before it waits for
// anything, and every node refuses it the same, so it is `400 bad_params`
// naming the log, as every read surface answers it: never a fault of this
// node — and in the framework's words without its name, which the error
// carries in the middle of its sentence.
//
// Mutation: drop the foreign-position arm from readFailed and the download is
// a `500 internal_error`; answer it with the error's own text and the
// package's name reaches the person.
func TestADownloadFlooredOnAnotherLogIsBadParams(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	floor := statelog.Position{Stream: topics.PagesLogStream, Generation: 1, Seq: 4}
	fake.readErr = fmt.Errorf("this read's floor: %w", floor.On(topics.TrackerLogStream))
	s := filesSurface(t, fake, chart{})
	rec := httptest.NewRecorder()
	if aborted := s.download(rec); aborted != nil {
		t.Fatal(aborted)
	}
	if rec.Code != http.StatusBadRequest ||
		answerOf(t, rec)["error"] != string(httpjson.CodeBadParams) {
		t.Fatalf("a download floored on another log answered %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), topics.PagesLogStream) {
		t.Errorf("the refusal does not name the log the floor is on: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "statelog:") {
		t.Errorf("the refusal tells a person the package's name: %s", rec.Body.String())
	}
}

// A DOWNLOAD'S EVERY MEBIBYTE HAS ITS OWN WINDOW, spent only while the client
// is taking its bytes. The route once copied with no write deadline, so a
// client that opened a download and never read it held the handler and its
// connection; and a deadline set before the bytes were read from the store,
// or a window charged for the store's own time, would have spent the
// client's pace on the store. Here the store takes ten seconds to hand over
// each piece — far more than a mebibyte's pace in all — and the client a
// second to take each write: every deadline is the window its mebibyte has
// left after the client's seconds and none of the store's, it directly
// precedes its write, and the response ends through the Writer, under what
// the last mebibyte had left.
func TestADownloadWritesEachMebibyteInAWindowOfItsOwn(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	fake.readWork = 10 * time.Second
	log := fake.paceLog()
	data := content(2*objstore.MiB + 100)
	fake.holding(t, data)
	s := filesSurface(t, fake, chart{})

	w := &pacedWriter{ResponseRecorder: httptest.NewRecorder(), log: log, take: time.Second}
	if aborted := s.download(w); aborted != nil {
		t.Fatal(aborted)
	}
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("download answered %d with %d bytes, want 200 with %d", w.Code, w.Body.Len(), len(data))
	}

	// EVERY WRITE DIRECTLY FOLLOWS ITS OWN DEADLINE, so nothing the store
	// does lands between a deadline and the write it bounds; and what the
	// window has left at each is the pace less a second for every write
	// before it in its mebibyte.
	events := log.all()
	log.mu.Lock()
	out := append([]time.Duration(nil), log.out...)
	log.mu.Unlock()
	var want []time.Duration
	written, inMebibyte := 0, 0
	for i, e := range events {
		size, isWrite := strings.CutPrefix(e, "write ")
		if !isWrite {
			continue
		}
		if i == 0 || events[i-1] != "write-deadline" {
			t.Fatalf("a write at %d follows %q rather than its deadline: %v", i, events[max(i-1, 0)], events)
		}
		n, err := strconv.Atoi(size)
		if err != nil {
			t.Fatal(err)
		}
		if written%objstore.MiB == 0 {
			inMebibyte = 0
		}
		want = append(want, objstore.MiBPace-time.Duration(inMebibyte)*w.take)
		written += n
		inMebibyte++
	}
	if events[len(events)-1] != "write-deadline" {
		t.Errorf("the download's last event is %q, want the deadline it ended under", events[len(events)-1])
	}
	want = append(want, objstore.MiBPace-time.Duration(inMebibyte)*w.take)
	if !slices.Equal(out, want) {
		t.Errorf("write deadlines set %v out, want %v: each mebibyte's whole pace less "+
			"the client's second at each write before it, none of the store's ten", out, want)
	}
}

// tooLargeSaid is what a person is told about a file over the cap.
const tooLargeSaid = "This file is over 1 GiB, the most a project's file can hold."

// A FILE OVER THE CAP IS TOLD IN A PERSON'S WORDS, whether its length was
// declared or the store found it while the body streamed: the detail was the
// store's error — "objstore: the object is over its size limit (1073741824
// bytes)", a package's name and the cap in bytes. Mutation: answer
// err.Error() for it again and this goes red.
func TestAFileOverTheCapIsToldInAPersonsWords(t *testing.T) {
	t.Parallel()
	fake := newFakeFiles(t)
	fake.putLimit = 4
	s := filesSurface(t, fake, chart{})
	rec := httptest.NewRecorder()
	s.upload(bytes.NewReader([]byte("five!")), rec)
	answer := answerOf(t, rec)
	if rec.Code != http.StatusRequestEntityTooLarge ||
		answer["error"] != string(httpjson.CodeBodyTooLarge) ||
		answer["detail"] != tooLargeSaid {
		t.Fatalf("a body the store found over the cap answered %d %v, want 413 "+
			"saying %q", rec.Code, answer, tooLargeSaid)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.puts) != 0 {
		t.Error("a file was recorded from a body over the cap")
	}
}
