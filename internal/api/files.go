package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A project's file BYTES, over HTTP.
//
// The listing is a question like every other read on this surface (`GET
// /work/files`, the `work_files` query). What is here is what cannot be one:
// a download STREAMS a file back and an upload streams one in — neither
// holding the file in memory and neither passing through a model. The rows go
// through the tracker and the bytes through this node's object store, the
// upload always bytes first: a file that is listed is a file that can be read.
//
// A write is attributed to the OPERATOR on the request, for the purge's
// reason: a person's upload names that person in the file's history, never the
// process that relayed it.

// ProjectFiles is the file surface's reach into the engine.
type ProjectFiles interface {
	File(ctx context.Context, project, path string, fresh statelog.Freshness) (tracker.FileDetail, error)

	// AcceptsFiles refuses an upload into project before its body is
	// read: [tracker.ErrNoProject] for a project there is none of, and
	// [tracker.AcceptFiles]'s refusal for an archived one — the project
	// read at fresh, which the surface resolves.
	AcceptsFiles(ctx context.Context, project string, fresh statelog.Freshness) error

	// Open streams an object back, checked against the row naming it
	// ([objstore.Store.Open]); Put streams r into a new object
	// ([objstore.Store.Put]).
	Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error)
	Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error)

	PutFileAs(ctx context.Context, operator, opID string, put tracker.FilePut) (tracker.WriteResult, error)
	RemoveFileAs(ctx context.Context, operator, opID, project, path string,
		ifMatch uint64) (tracker.WriteResult, error)
}

// mountFiles registers the byte routes, where the engine serves files.
func (a *App) mountFiles(mux *http.ServeMux) {
	if a.files == nil {
		return
	}
	mux.Handle("GET /work/files/{project}/{path...}", http.HandlerFunc(a.serveFileDownload))
	mux.Handle("PUT /work/files/{project}/{path...}", http.HandlerFunc(a.serveFileUpload))
	mux.Handle("DELETE /work/files/{project}/{path...}", http.HandlerFunc(a.serveFileRemove))
}

// filePace is how long each mebibyte of a file is given to cross the
// connection, in either direction: an upload's to arrive, a download's to be
// taken by the client.
//
// [objstore.MiBPace] — THE object store's one floor, which every leg a file's
// bytes cross is held to, the client's included, so no leg can be retuned
// out from under another — and PER MEBIBYTE rather than per body. The
// server's ReadTimeout and WriteTimeout are deliberately unset (cmd/crewlet's
// apiIdleTimeout says why), so a route that streams a body bounds it itself:
// unbounded, a client could trickle a gibibyte upload a byte at a time, or
// open a download and never read it, and hold a handler and a connection
// slot as long as it liked. One deadline over the whole body cannot be the
// bound either, because a file of [tracker.MaxFileBytes] is a gibibyte and
// any single figure is either a cap on real uploads or no bound on a trickle.
//
// AND CHARGED ONLY FOR THE TIME THE CLIENT IS WAITED ON, never the server's
// own: storing what arrived can take a broker's publish acknowledgements or a
// bucket's part upload, and fetching what a download sends can wait on the
// store, and neither is the client's time to spend.
const filePace = objstore.MiBPace

// errFileBody is an upload whose body could not be read to its end — the
// client's connection, not the store, so it answers 400 rather than 503.
var errFileBody = errors.New("the file's body could not be read")

// pacedBody is an upload's body, every mebibyte of it read inside a budget of
// [filePace] — see [pacedBody.Read].
type pacedBody struct {
	r    io.Reader
	rc   *http.ResponseController
	pace time.Duration

	// left is how many bytes of the current mebibyte are still to arrive,
	// and budget how much of its pace is left to spend on them.
	left   int64
	budget time.Duration
}

// newPacedBody paces r at [filePace] a mebibyte.
func newPacedBody(r io.Reader, rc *http.ResponseController) *pacedBody {
	return &pacedBody{r: r, rc: rc, pace: filePace}
}

// Read reads the body, inside what is left of the current mebibyte's budget.
//
// THE STORE DRIVES THE READS — it asks for the next piece once it has done
// whatever it does with the last one — so the pace is kept HERE, on the time
// spent inside Read, which is the only time the client is the one being
// waited on. Each Read is given the rest of its mebibyte's budget as its
// deadline, and what it took is charged against it; a mebibyte that arrives
// opens the next one's whole budget. A deadline re-armed whole at every Read
// would admit a byte every twenty-nine seconds for ever, and one charged by
// the wall clock would bill the client for the store's own work.
//
// http.ErrNotSupported is ignored, as [httpjson.ReadBody] ignores it: a writer
// that cannot carry a deadline has no connection to bound — and the budget is
// still kept, so a trickle is refused here even where no deadline could be
// set.
//
// A failure is tagged as the client's — a body cut short, which net/http
// answers as io.ErrUnexpectedEOF, included. The end of the body, io.EOF
// itself, is passed through untouched: the store ends an object only there
// ([objstore.Fill]), and a tagged one would read as a failure.
func (b *pacedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		b.left, b.budget = objstore.MiB, b.pace
	}
	if b.budget <= 0 {
		return 0, fmt.Errorf("%w: a mebibyte of it took more than %v to arrive",
			errFileBody, b.pace)
	}
	start := time.Now()
	if err := b.rc.SetReadDeadline(start.Add(b.budget)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		return 0, fmt.Errorf("%w: %w", errFileBody, err)
	}
	n, err := b.r.Read(p)
	b.budget -= time.Since(start)
	b.left -= int64(n)
	if b.left <= 0 {
		// A READ THAT CROSSED INTO THE NEXT MEBIBYTE has paid for this
		// one; what it carried past the boundary counts toward the next,
		// which opens with its whole budget.
		b.left, b.budget = objstore.MiB+b.left, b.pace
	}
	if err == nil || err == io.EOF {
		return n, err
	}
	return n, fmt.Errorf("%w: %w", errFileBody, err)
}

// fileRefusal answers a file route's failure with the status its cause
// deserves.
func fileRefusal(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errFileBody):
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeUnreadableBody,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, tracker.ErrNoProject):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_project", "detail": err.Error()})
	case errors.Is(err, tracker.ErrNoFile):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_file", "detail": err.Error()})
	case errors.Is(err, tracker.ErrStaleVersion):
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{
			"error": "version_moved", "detail": err.Error(),
		})
	case errors.Is(err, tracker.ErrUploadStale):
		// THE BYTES ARE STORED AND MAY NOT BE NAMED ANY MORE: the
		// upload took longer than a write may name its object after
		// (objstore.RecordWithin), and sending it again is the one thing
		// that lands.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "upload_expired", "detail": err.Error(),
		})
	case errors.Is(err, tracker.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid", "detail": tracker.Sentence(err),
		})
	case errors.Is(err, objstore.ErrTooLarge):
		// THE ONE SPELLING OF A 413 every JSON surface answers.
		httpjson.FailWith(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge,
			map[string]string{"detail": err.Error()})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "unavailable", "detail": err.Error(),
		})
	}
}

// downloadHead is how much of a file is read before the response starts —
// enough to know the content can be read at all, so a file whose bytes are
// unreachable answers 503 rather than a 200 with an empty body.
const downloadHead = 32 << 10

// retiredContent is the sentence a file kept by an earlier build in chunks
// is answered with: listed, removable, writable again — and its content gone.
const retiredContent = "was saved by an earlier build that kept files in chunks, " +
	"and its content cannot be read any more; upload it again"

// serveFileDownload answers GET /work/files/{project}/{path...}: the file's
// bytes, streamed.
func (a *App) serveFileDownload(w http.ResponseWriter, r *http.Request) {
	fresh, err := tracker.ParseFreshness(queries.FromQuery(r.URL.Query()))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_params", "detail": err.Error()})
		return
	}
	fresh.Level = statelog.LevelFor(statelog.SurfaceDashboard, fresh.Level)
	detail, err := a.files.File(r.Context(), r.PathValue("project"), r.PathValue("path"), fresh)
	if err != nil {
		fileRefusal(w, err)
		return
	}
	f := detail.File
	object, named := f.Content()
	if !named {
		// GONE FOR GOOD rather than unavailable: a 503 tells a client to
		// retry, and no retry will ever read it.
		writeJSON(w, http.StatusGone, map[string]string{
			"error": "content_retired", "detail": f.Path + " " + retiredContent,
		})
		return
	}
	body, err := a.files.Open(r.Context(), object)
	if err == nil {
		defer body.Close()
		head := make([]byte, min(int64(downloadHead), f.Size))
		_, err = io.ReadFull(body, head)
		if err == nil {
			a.sendFile(w, f, io.MultiReader(bytes.NewReader(head), body))
			return
		}
	}
	if errors.Is(err, objstore.ErrCorrupt) {
		// DAMAGED, NOT UNREACHABLE: the store answered, and what it holds
		// is not the content the file records. A retry reads the same
		// bytes, so this is no 503.
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "content_corrupt",
			"detail": fmt.Sprintf("%s exists and the object store holds content for it "+
				"that is not what it records: %v", f.Path, err),
		})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": "content_unavailable",
		"detail": fmt.Sprintf("%s exists and its content could not be read "+
			"right now: %v", f.Path, err),
	})
}

// sendFile writes the file's headers and then its content, from src.
func (a *App) sendFile(w http.ResponseWriter, f tracker.File, src io.Reader) {
	contentType := f.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.Header().Set("ETag", strconv.Quote(strconv.FormatUint(f.Version, 10)))
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(f.Path)}))
	w.WriteHeader(http.StatusOK)
	if err := sendPaced(w, src); err != nil {
		// THE STATUS IS SENT, so the one honest signal left is a body
		// shorter than its Content-Length: aborting the handler closes
		// the connection rather than ending the response cleanly. The
		// store holds back the byte that would complete a file whose
		// content it could not verify, so a damaged object reaches here
		// too, never a whole response.
		log.Warn("api_file_download_cut", "project", f.Project, "path", f.Path, "error", err)
		panic(http.ErrAbortHandler)
	}
}

// sendUnit is how much of a download one write hands the client, each under
// its own [filePace] deadline: a mebibyte, the unit the pace is given per.
const sendUnit = objstore.MiB

// sendPaced writes src to the client [sendUnit] at a time, each under its own
// [filePace] write deadline, set once the unit is in hand — so the time spent
// waiting on the store for it is never the client's.
//
// Done only where src answers io.EOF itself ([objstore.Fill]): a source cut
// short is a failure, which the caller turns into a cut response rather than
// one ended as if it were whole.
func sendPaced(w http.ResponseWriter, src io.Reader) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, sendUnit)
	for {
		n, end, err := objstore.Fill(src, buf)
		if err != nil {
			return err
		}
		if n > 0 {
			if derr := rc.SetWriteDeadline(time.Now().Add(filePace)); derr != nil &&
				!errors.Is(derr, http.ErrNotSupported) {
				return derr
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if end {
			return nil
		}
	}
}

// operatorFor is the person a file write is attributed to, answering the
// refusal itself where the request carries none.
func operatorFor(w http.ResponseWriter, r *http.Request) (string, bool) {
	operator, ok := auth.OperatorFrom(r.Context())
	if !ok || operator == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":  "operator_required",
			"detail": "a file write is attributed to the person who made it, and this request carries no operator identity",
		})
		return "", false
	}
	return operator, true
}

// ifMatch reads an If-Match header's version, zero when absent.
func ifMatch(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	raw := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if raw == "" {
		return tracker.NoIfMatch, true
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "bad_if_match", "detail": "If-Match is the file's version, as its ETag carries it",
		})
		return 0, false
	}
	return v, true
}

// writeAnswer is a file write's answer: the three-valued outcome, and the
// position only where the write is known to have landed.
func writeAnswer(result tracker.WriteResult, extra map[string]any) map[string]any {
	extra["outcome"], extra["op_id"] = result.Outcome, result.OpID
	if result.Outcome != statelog.OutcomeUnknown {
		extra["position"] = result.Position
		extra["version"] = result.Version
	}
	return extra
}

// serveFileUpload answers PUT /work/files/{project}/{path...}: the body is the
// file, streamed into one new object and then recorded.
func (a *App) serveFileUpload(w http.ResponseWriter, r *http.Request) {
	operator, ok := operatorFor(w, r)
	if !ok {
		return
	}
	project := tracker.ProjectKey(r.PathValue("project"))
	filePath, err := tracker.NormalizeFilePath(r.PathValue("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_path", "detail": err.Error()})
		return
	}
	version, ok := ifMatch(w, r)
	if !ok {
		return
	}
	opID, ok := callerOpID(w, r, "file-"+project)
	if !ok {
		return
	}
	// REFUSED BEFORE A BYTE IS READ where the refusal is already known: a
	// declared length over the cap, and a project that takes no files.
	// Either found after the body would have streamed up to a gibibyte into
	// the store for nothing, and left it there for a day.
	if r.ContentLength > tracker.MaxFileBytes {
		fileRefusal(w, fmt.Errorf("%w: the body declares %d bytes and a project's "+
			"files are at most %d", objstore.ErrTooLarge, r.ContentLength, tracker.MaxFileBytes))
		return
	}
	// AT THE SURFACE'S OWN LEVEL, as every read here is: the read names
	// none on its own, and an absent one is refused by the reader rather
	// than guessed at. Only an early refusal — the write's decide is what
	// actually judges the project.
	accepts := statelog.Freshness{Level: statelog.LevelFor(statelog.SurfaceDashboard, "")}
	if refused := a.files.AcceptsFiles(r.Context(), project, accepts); refused != nil {
		fileRefusal(w, refused)
		return
	}
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(filePath))
	}
	// THE BYTES FIRST — see the file's head — paced as they arrive.
	object, err := a.files.Put(r.Context(), newPacedBody(r.Body, http.NewResponseController(w)),
		tracker.MaxFileBytes, objstore.PutMeta{ContentType: contentType})
	if err != nil {
		fileRefusal(w, err)
		return
	}
	// NOTHING IS DELETED ON A REFUSAL BELOW: a write retried under one
	// operation id can be refused here after another node committed it,
	// and the object would be that file's content. What no row names, the
	// collector removes a day later.
	result, err := a.files.PutFileAs(r.Context(), operator, opID, tracker.FilePut{
		Project: project, Path: filePath, ContentType: contentType,
		Object: object, IfMatch: version,
	})
	if err != nil {
		fileRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, writeAnswer(result, map[string]any{
		"project": project, "path": filePath, "size": object.Size,
		"hash": object.Hash, "content_type": contentType,
	}))
}

// serveFileRemove answers DELETE /work/files/{project}/{path...}.
func (a *App) serveFileRemove(w http.ResponseWriter, r *http.Request) {
	operator, ok := operatorFor(w, r)
	if !ok {
		return
	}
	project := tracker.ProjectKey(r.PathValue("project"))
	filePath, err := tracker.NormalizeFilePath(r.PathValue("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_path", "detail": err.Error()})
		return
	}
	version, ok := ifMatch(w, r)
	if !ok {
		return
	}
	opID, ok := callerOpID(w, r, "file-remove-"+project)
	if !ok {
		return
	}
	result, err := a.files.RemoveFileAs(r.Context(), operator, opID, project, filePath, version)
	if err != nil {
		fileRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, writeAnswer(result, map[string]any{
		"project": project, "path": filePath,
	}))
}
