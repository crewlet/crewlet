package assistantapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/assistantapi"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval/approvaltest"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tabular"
)

// keptFile is one file the fake store keeps, keyed by its owner and
// conversation as the domain keys it.
type keptFile struct {
	owner, conversation uuid.UUID
	ref                 wire.FileRef
	content             []byte
}

// fileStore is assistant.Files over a map, owner-keyed exactly as the domain's
// is: the route's half is what these tests hold, the domain's is its own.
type fileStore struct {
	mu     sync.Mutex
	owners map[uuid.UUID]uuid.UUID // conversation -> owner
	files  map[uuid.UUID]keptFile
	stored []assistant.NewFile
	full   bool
	// fail, when set, is what every later Store answers.
	fail error
}

func newFileStore() *fileStore {
	return &fileStore{owners: map[uuid.UUID]uuid.UUID{}, files: map[uuid.UUID]keptFile{}}
}

func (s *fileStore) Store(_ context.Context, w assistant.Write, f assistant.NewFile) (wire.FileRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners[f.Conversation] != w.Owner {
		return wire.FileRef{}, fmt.Errorf("%w: conversation %s", assistant.ErrNotFound, f.Conversation)
	}
	if s.full {
		return wire.FileRef{}, fmt.Errorf("%w: %w", assistant.ErrInvalid, assistantdomain.ErrTooManyFiles)
	}
	if s.fail != nil {
		return wire.FileRef{}, s.fail
	}
	s.stored = append(s.stored, f)
	ref := wire.FileRef{FileID: f.ID, Name: f.Name, MediaType: f.MediaType,
		Size: int64(len(f.Content)), Download: wire.FilePath(f.Conversation, f.ID)}
	s.files[f.ID] = keptFile{owner: w.Owner, conversation: f.Conversation, ref: ref, content: f.Content}
	return ref, nil
}

func (s *fileStore) Open(ctx context.Context, owner, conversation, file uuid.UUID) (assistant.StoredFile, []byte, error) {
	stored, err := s.Stat(ctx, owner, conversation, file)
	if err != nil {
		return assistant.StoredFile{}, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return stored, s.files[file].content, nil
}

func (s *fileStore) Stat(_ context.Context, owner, conversation, file uuid.UUID) (assistant.StoredFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[file]
	if !ok || f.owner != owner || f.conversation != conversation {
		return assistant.StoredFile{}, fmt.Errorf("%w: file %s", assistant.ErrNotFound, file)
	}
	return assistant.StoredFile{FileRef: f.ref, Owner: f.owner, Conversation: f.conversation,
		Origin: wire.FileAttached}, nil
}

// keep puts a file in the store for owner's conversation, as an earlier
// upload would have.
func (s *fileStore) keep(owner, conversation uuid.UUID, name, mediaType string, content []byte) wire.FileRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.New()
	ref := wire.FileRef{FileID: id, Name: name, MediaType: mediaType, Size: int64(len(content)),
		Download: wire.FilePath(conversation, id)}
	s.files[id] = keptFile{owner: owner, conversation: conversation, ref: ref, content: content}
	return ref
}

// fileSite is the surface with a file store, its rig's person owning its
// conversation.
func fileSite(t *testing.T) (*site, *fileStore) {
	t.Helper()
	s := newSite(t, readTurn(t))
	files := newFileStore()
	files.owners[s.rig.Conversation] = s.rig.Person.ID
	surface, err := assistantapi.New(assistantapi.Options{Service: s.rig.Svc,
		Grants: grants{now: s.rig.Clock.Now}, Tables: tabular.Tables{}, Chart: authz.NoChart{},
		Files: files, Now: s.rig.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	s.mux = http.NewServeMux()
	if err := surface.Routes(s.mux); err != nil {
		t.Fatal(err)
	}
	s.surface = surface
	return s, files
}

// pngBytes is a PNG's signature and some bytes.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{7}, 64)...)

// upload sends content as p's upload named name, declared as contentType.
func (s *site) upload(p iam.Principal, name, contentType string, content []byte,
	tweak func(*http.Request)) *httptest.ResponseRecorder {
	path := "/assistant/conversations/" + s.rig.Conversation.String() + "/files?name=" + name
	req := httptest.NewRequestWithContext(iam.WithPrincipal(context.Background(), p),
		http.MethodPost, path, bytes.NewReader(content))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(opkey.Header, "0192f1a4-9b2d-7c03-a4e1-5f3b9d0c7a14")
	if tweak != nil {
		tweak(req)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// envelope decodes a refusal's code.
func envelope(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	code, _ := body["error"].(string)
	return code
}

// AN UPLOAD IS TYPED BY ITS BYTES: an image is kept as the type its bytes
// are, under an id its operation derives, and HTML named and declared as a PNG
// is refused `415 assistant_file_refused` and never reaches the store, as is a
// body declared as something no image is. Mutation: keep the declared type,
// or skip the sniff, and the HTML is kept.
func TestAnUploadIsTypedByItsBytes(t *testing.T) {
	t.Parallel()
	s, files := fileSite(t)
	rec := s.upload(s.rig.Person, "chart.png", "application/octet-stream", pngBytes, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a PNG upload = %d %s", rec.Code, rec.Body)
	}
	var stored wire.FileStored
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.File.MediaType != "image/png" || stored.File.Name != "chart.png" ||
		stored.File.Download != wire.FilePath(s.rig.Conversation, stored.File.FileID) {
		t.Errorf("the upload answered %+v", stored.File)
	}
	if len(files.stored) != 1 || files.stored[0].Origin != wire.FileAttached ||
		files.stored[0].Turn != uuid.Nil {
		t.Fatalf("the store was handed %+v", files.stored)
	}
	again := s.upload(s.rig.Person, "chart.png", "image/png", pngBytes, nil)
	var second wire.FileStored
	_ = json.Unmarshal(again.Body.Bytes(), &second)
	if second.File.FileID != stored.File.FileID {
		t.Errorf("a retried upload under one key is file %s, then %s", stored.File.FileID,
			second.File.FileID)
	}
	for name, c := range map[string]struct{ name, contentType string }{
		"html called a png":        {"evil.png", "image/png"},
		"html declared as text":    {"evil.png", "text/html"},
		"html declared as a form":  {"evil.png", "application/x-www-form-urlencoded"},
		"html with no declaration": {"evil.png", ""},
	} {
		rec := s.upload(s.rig.Person, c.name, c.contentType,
			[]byte("<!DOCTYPE html><html><script>alert(1)</script></html>"), nil)
		if rec.Code != http.StatusUnsupportedMediaType || envelope(t, rec) != "assistant_file_refused" {
			t.Errorf("%s = %d %s, want 415 assistant_file_refused", name, rec.Code, rec.Body)
		}
	}
	if len(files.stored) != 2 {
		t.Errorf("the store was handed %d files, want the two PNG uploads", len(files.stored))
	}
}

// AN UPLOAD PAST THE BOUND IS REFUSED BEFORE IT IS READ WHOLE: a declared
// length past wire.FileMaxBytes is `413 body_too_large` before a byte is read,
// and an undeclared one one byte past the bound, and neither reaches the store.
// Mutation: drop the declared-length check, or read the body without its
// bound, and the oversized upload is read or kept.
func TestAnUploadPastTheBoundIsRefused(t *testing.T) {
	t.Parallel()
	s, files := fileSite(t)
	big := append(slicesClone(pngBytes), make([]byte, wire.FileMaxBytes)...)
	read := &countingReader{r: bytes.NewReader(big)}
	declared := s.upload(s.rig.Person, "big.png", "image/png", big, func(r *http.Request) {
		r.Body = io.NopCloser(read)
	})
	if read.n != 0 {
		t.Errorf("a body declared past the bound was read for %d bytes before it was refused",
			read.n)
	}
	undeclared := s.upload(s.rig.Person, "big.png", "image/png", big, func(r *http.Request) {
		r.ContentLength = -1
	})
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"declared": declared, "undeclared": undeclared,
	} {
		if rec.Code != http.StatusRequestEntityTooLarge || envelope(t, rec) != "body_too_large" {
			t.Errorf("the %s oversized upload = %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(files.stored) != 0 {
		t.Error("an oversized upload reached the store")
	}
}

func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }

// countingReader counts the bytes a handler read.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// AN UPLOAD INTO A FULL CONVERSATION IS A CONFLICT, and an upload into
// somebody else's conversation is not found.
func TestAnUploadIntoAFullOrForeignConversationIsRefused(t *testing.T) {
	t.Parallel()
	s, files := fileSite(t)
	ada := evals.Person("ada", "ada", iam.GrantWorklet)
	if rec := s.upload(ada, "chart.png", "image/png", pngBytes, nil); rec.Code != http.StatusNotFound {
		t.Errorf("ada's upload into grace's conversation = %d %s", rec.Code, rec.Body)
	}
	files.full = true
	rec := s.upload(s.rig.Person, "chart.png", "image/png", pngBytes, nil)
	if rec.Code != http.StatusConflict || envelope(t, rec) != "assistant_file_limit" {
		t.Errorf("an upload into a full conversation = %d %s", rec.Code, rec.Body)
	}
}

// AN UPLOAD THE FILE STORE DID NOT TAKE IS TOLD BY ITS CAUSE, as every surface
// that stores a person's file tells it: a full store in the words that say
// who makes room, with no Retry-After, since no wait clears it; a store that
// did not take the bytes under an election's wait; a record the log refused in
// the state log's own words; a write nobody can account for as the unknown it
// is; and a fault as a fault. Each of the first three was `500
// internal_error`, saying the engine had broken. Mutation: drop the condition
// arm from fail and the first three rows answer 500.
func TestAnUploadTheStoreDidNotTakeIsToldByItsCause(t *testing.T) {
	t.Parallel()
	kept := func(cause error) error {
		said, _ := builtin.Condition(cause)
		return &assistant.Unkept{Said: said, Cause: cause}
	}
	refused := &statelog.Unavailable{Reason: statelog.ReasonLogFull,
		Detail: "raise stream.assistant_log_max_bytes"}
	unanswered := fmt.Errorf("assistantdomain: upload file x: %w: %w", objstore.ErrUnanswered,
		errors.New("nats: timeout"))
	refusedSaid, _ := builtin.ConditionSentence(refused)
	unansweredSaid, _ := builtin.ConditionSentence(unanswered)
	for name, c := range map[string]struct {
		err     error
		status  int
		code    string
		says    string
		retry   string
		outcome bool
	}{
		"a full store": {kept(fmt.Errorf("assistantdomain: upload file x: %w", objstore.ErrFull)),
			http.StatusServiceUnavailable, "unavailable", objstore.FullWords, "", false},
		"a store that did not take the bytes": {kept(unanswered), http.StatusServiceUnavailable,
			"unavailable", unansweredSaid,
			strconv.Itoa(int(statelog.ElectionRetryHint / time.Second)), false},
		"a record the log refused": {kept(refused), http.StatusServiceUnavailable, "unavailable",
			refusedSaid, "", false},
		"a write nobody can account for": {fmt.Errorf("%w: operation x", assistant.ErrOutcomeUnknown),
			http.StatusServiceUnavailable, "unavailable", "", "2", true},
		"a fault": {errors.New("open /var/lib/crewlet/objects: disk I/O error"),
			http.StatusInternalServerError, "internal_error", "", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, files := fileSite(t)
			files.fail = c.err
			rec := s.upload(s.rig.Person, "chart.png", "image/png", pngBytes, nil)
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != c.status || body["error"] != c.code ||
				(c.says != "" && body["detail"] != c.says) ||
				rec.Header().Get("Retry-After") != c.retry || (body["outcome"] == "unknown") != c.outcome {
				t.Errorf("answered %d %v (Retry-After %q), want %d %s saying %q (Retry-After %q)",
					rec.Code, body, rec.Header().Get("Retry-After"), c.status, c.code, c.says, c.retry)
			}
			if text := rec.Body.String(); strings.Contains(text, "nats") ||
				strings.Contains(text, "/var/lib") || strings.Contains(text, "assistant_log_max_bytes") {
				t.Errorf("the answer carried its error's own text: %s", text)
			}
		})
	}
}

// wantFileHeaders holds a download to every header that keeps a browser from
// rendering or keeping it.
func wantFileHeaders(t *testing.T, h http.Header, mediaType, disposition string) {
	t.Helper()
	for name, want := range map[string]string{
		"Content-Type":            mediaType,
		"Content-Disposition":     disposition,
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox",
		"Cache-Control":           "private, no-store",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// EVERY DOWNLOAD CARRIES EVERY SECURITY HEADER, as the type its bytes were
// sniffed as and always as an attachment, its name RFC 5987-encoded where it
// is not plain ASCII; and another person's file, or the owner's under another
// conversation, is not found. Mutation: drop any one header from serveFile, or
// key the read on the file alone, and this names it.
func TestEveryDownloadCarriesEverySecurityHeader(t *testing.T) {
	t.Parallel()
	s, files := fileSite(t)
	grace := s.rig.Person
	ref := files.keep(grace.ID, s.rig.Conversation, "Résumé chart.png", "image/png", pngBytes)
	rec := s.do(grace, http.MethodGet, ref.Download, nil, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Fatalf("grace's download = %d (%d bytes)", rec.Code, rec.Body.Len())
	}
	wantFileHeaders(t, rec.Header(), "image/png",
		`attachment; filename="R_sum_ chart.png"; filename*=UTF-8''R%C3%A9sum%C3%A9%20chart.png`)

	svg := files.keep(grace.ID, s.rig.Conversation, "logo.svg", "image/svg+xml",
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	rec = s.do(grace, http.MethodGet, svg.Download, nil, nil)
	wantFileHeaders(t, rec.Header(), "image/svg+xml", `attachment; filename="logo.svg"`)

	ada := evals.Person("ada", "ada", iam.GrantWorklet)
	for name, path := range map[string]string{
		"ada reading grace's file": ref.Download,
		"grace under another conversation": "/assistant/conversations/" +
			uuid.NewString() + "/files/" + ref.FileID.String(),
	} {
		who := grace
		if strings.HasPrefix(name, "ada") {
			who = ada
		}
		if rec := s.do(who, http.MethodGet, path, nil, nil); rec.Code != http.StatusNotFound ||
			envelope(t, rec) != "not_found" {
			t.Errorf("%s = %d %s, want 404 not_found", name, rec.Code, rec.Body)
		}
	}
}

// A NAME NO DOWNLOAD HEADER CAN CARRY IS NEVER WRITTEN INTO ONE: a kept name
// cannot hold a quote, but a row no build wrote could, and the header names a
// generic file rather than breaking its own syntax.
func TestADownloadsNameCannotBreakItsHeader(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"plain.png":            `attachment; filename="plain.png"`,
		`a"; filename="x.html`: `attachment; filename="download"`,
		"":                     `attachment; filename="download"`,
		"日本.png":               `attachment; filename="__.png"; filename*=UTF-8''%E6%97%A5%E6%9C%AC.png`,
	} {
		if got := assistantapi.ContentDisposition(name); got != want {
			t.Errorf("ContentDisposition(%q) = %q, want %q", name, got, want)
		}
	}
}

// THE FILE ROUTES ARE NEVER THE ASSISTANT'S: the guard refuses `/assistant/`
// to a grant before routing, and the authority table refuses each route to a
// principal riding one, so a turn can neither upload into a conversation nor
// read a file, its own conversation's included. Mutation: take the files out
// from under the assistant prefix and the first check goes red.
func TestTheFileRoutesAreNeverTheAssistants(t *testing.T) {
	t.Parallel()
	s, files := fileSite(t)
	ref := files.keep(s.rig.Person.ID, s.rig.Conversation, "a.png", "image/png", pngBytes)
	paths := []string{
		"/assistant/conversations/" + s.rig.Conversation.String() + "/files",
		ref.Download,
		"/assistant/audit/conversations/" + s.rig.Conversation.String() + "/files/" +
			ref.FileID.String() + "/read",
	}
	for _, path := range paths {
		if !auth.AssistRefusedPath(path) {
			t.Errorf("a grant may reach %s", path)
		}
	}
	assisted := s.rig.Person
	assisted.Via, assisted.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())), iam.AssistScopeRead
	if rec := s.do(assisted, http.MethodGet, ref.Download, nil, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a download through the assistant = %d %s", rec.Code, rec.Body)
	}
	if rec := s.upload(assisted, "a.png", "image/png", pngBytes, nil); rec.Code != http.StatusForbidden {
		t.Errorf("an upload through the assistant = %d %s", rec.Code, rec.Body)
	}
}

// A QUESTION CARRIES AT MOST FOUR ATTACHMENTS, each named once, refused as
// written before anything starts. Mutation: drop the route's bound and the
// fifth attachment reaches the orchestrator.
func TestAQuestionCarriesAtMostItsAttachments(t *testing.T) {
	t.Parallel()
	s, _ := fileSite(t)
	one := uuid.New()
	for name, c := range map[string]struct {
		attachments []uuid.UUID
		says        string
	}{
		"five": {[]uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()},
			fmt.Sprintf("at most %d attachments", wire.AttachmentsPerTurn)},
		"one twice": {[]uuid.UUID{one, one}, "attached twice"},
		"nil":       {[]uuid.UUID{uuid.Nil}, "names no file"},
	} {
		rec := s.do(s.rig.Person, http.MethodPost, "/assistant/conversations/"+
			s.rig.Conversation.String()+"/turns", wire.TurnRequest{
			ProtocolVersion: wire.ProtocolVersion, Input: wire.TurnInput{Text: "Look"},
			Attachments: c.attachments}, map[string]string{"Idempotency-Key": opKey})
		if rec.Code != http.StatusBadRequest || envelope(t, rec) != "invalid_body" ||
			!strings.Contains(rec.Body.String(), c.says) {
			t.Errorf("%s = %d %s, want 400 invalid_body saying %q", name, rec.Code, rec.Body,
				c.says)
		}
	}
}

// auditFiles is the audited download's domain half.
type auditFiles struct {
	audits  *audits
	owner   uuid.UUID
	content []byte
}

func (a auditFiles) AuditOpen(_ context.Context, _ assistantdomain.AuditReceipt,
	file uuid.UUID) (assistantdomain.File, []byte, error) {
	a.audits.mu.Lock()
	defer a.audits.mu.Unlock()
	if a.audits.unopened {
		return assistantdomain.File{}, nil, fmt.Errorf("%w: %w", assistantdomain.ErrAuditPending,
			assistantdomain.ErrNotFound)
	}
	a.audits.steps = append(a.audits.steps, "open")
	return assistantdomain.File{ID: file, Owner: a.owner, Name: "evidence.png",
		MediaType: "image/png", Size: int64(len(a.content))}, a.content, nil
}

// fileSurface is the decision routes with the audited download mounted.
func fileSurface(t *testing.T, unopened bool) *surface {
	t.Helper()
	s := &surface{fleet: approvaltest.NewFleet(), audits: &audits{owner: uuid.New(),
		unopened: unopened}, audit: &published{}}
	d, err := assistantapi.NewDecisions(assistantapi.DecisionsDeps{
		Approver: s.fleet.Node(t), Audits: s.audits, Audit: s.audit,
		Render: func(context.Context, assistantdomain.Transcript) (wire.Transcript, error) {
			return wire.Transcript{}, nil
		},
		Files: auditFiles{audits: s.audits, owner: s.audits.owner, content: pngBytes},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := d.Mount(authz.NewRouter(mux, authz.ContextGuard(authz.NoChart{}))); err != nil {
		t.Fatal(err)
	}
	s.handler = mux
	return s
}

const auditFilePath = "/assistant/audit/conversations/018f0000-0000-7000-8000-000000000001" +
	"/files/018f0000-0000-7000-8000-0000000000f1/read"

// A COMPLIANCE DOWNLOAD IS RECORDED AND AUDITED BEFORE IT OPENS, exactly as
// the transcript's compliance read is: the read is recorded beside the
// conversation first, the file opens only for that record, the runtime audit
// names the reader, the owner, the reason and the file, and the bytes are
// served with every download header. A record not yet applied here opens
// nothing and publishes nothing. Mutation: publish the event before the open,
// or drop the file from it, and this names it.
func TestAComplianceDownloadIsRecordedAndAudited(t *testing.T) {
	t.Parallel()
	s := fileSurface(t, false)
	grace := admin(time.Now().Add(time.Hour))
	raw, _ := json.Marshal(readBody("Legal hold 42"))
	r := httptest.NewRequest(http.MethodPost, auditFilePath, bytes.NewReader(raw))
	r = r.WithContext(approvaltest.As(r.Context(), grace))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(opkey.Header, uuid.Must(uuid.NewV7()).String())
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Fatalf("the compliance download = %d %s", rec.Code, rec.Body)
	}
	wantFileHeaders(t, rec.Header(), "image/png", `attachment; filename="evidence.png"`)
	if got := strings.Join(s.audits.steps, ","); got != "record,open" {
		t.Errorf("the download went %s, want record,open", got)
	}
	if len(s.audit.events) != 1 {
		t.Fatalf("%d runtime audit records, want one", len(s.audit.events))
	}
	read, ok := s.audit.events[0].Data.(*types.AssistantConversationRead)
	if !ok || read.Reason != "Legal hold 42" || read.ActorName != "grace" ||
		read.OwnerID != s.audits.owner.String() ||
		read.FileID != "018f0000-0000-7000-8000-0000000000f1" {
		t.Errorf("the runtime audit is %+v", s.audit.events[0].Data)
	}

	pending := fileSurface(t, true)
	if status, body := pending.send(t, grace, http.MethodPost, auditFilePath,
		readBody("Legal hold"), nil); status != http.StatusServiceUnavailable || body["op_id"] == nil {
		t.Errorf("a download whose record has not applied answered %d %v", status, body)
	}
	if len(pending.audit.events) != 0 {
		t.Error("a download that never opened was recorded as one")
	}
	if status, body := pending.send(t, approvaltest.Person(time.Now().Add(time.Hour)),
		http.MethodPost, auditFilePath, readBody("Legal hold"), nil); status != http.StatusForbidden ||
		body["error"] != "unauthorized" {
		t.Errorf("a person who is not an administrator answered %d %v", status, body)
	}
}
