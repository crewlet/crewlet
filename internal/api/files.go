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

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/paced"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
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
//
// # Two seams, because a download is a read
//
// A node serves its reads wherever it runs the tracker, and its writes only
// where it also hands out the tracker's writer — never in a maintenance mode,
// which hands no writer to any surface. One seam over both halves was served
// only where both were, so a node in a maintenance window listed a project's
// files (`work_files`, a read) and answered every download of them as a route
// it does not serve.

// FileReads is what a download reaches the engine for: the row, and the bytes
// it names.
type FileReads interface {
	File(ctx context.Context, project, path string, fresh statelog.Freshness) (tracker.FileDetail, error)

	// Open streams an object back, checked against the row naming it
	// ([objstore.Store.Open]).
	Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error)
}

// FileWrites is what an upload and a removal reach the engine for.
type FileWrites interface {
	// AcceptsFiles refuses an upload into project before its body is
	// read: [tracker.ErrNoProject] for a project there is none of, and
	// [tracker.AcceptFiles]'s refusal for an archived one — the project
	// read at fresh, which the surface resolves.
	AcceptsFiles(ctx context.Context, project string, fresh statelog.Freshness) error

	// Put streams r into a new object ([objstore.Store.Put]).
	Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error)

	PutFileAs(ctx context.Context, operator, opID string, put tracker.FilePut) (tracker.WriteResult, error)
	RemoveFileAs(ctx context.Context, operator, opID, project, path string,
		ifMatch uint64) (tracker.WriteResult, error)
}

// mountFiles registers the byte routes on every API, each asking the engine on
// every request whether it serves its half NOW — see [NativeRoutes].
func (a *App) mountFiles(mux httpjson.Router) {
	mux.Handle("GET /work/files/{project}/{path...}", servedBy(a.native.FileReads, a.serveFileDownload))
	mux.Handle("PUT /work/files/{project}/{path...}", servedBy(a.native.FileWrites, a.serveFileUpload))
	mux.Handle("DELETE /work/files/{project}/{path...}", servedBy(a.native.FileWrites, a.serveFileRemove))
}

// servedBy resolves a route's seam for one request and hands it to serve, or
// answers the mux's own 404 where this node serves none now — byte for byte
// what the route answered when it was left unmounted, because a route this
// node does not serve is absent rather than refused.
//
// ONE RESOLUTION PER REQUEST, at its top, so a request is served by one view
// of the runtime from its first read to its last.
func servedBy[S any](seam func() (S, bool), serve func(http.ResponseWriter, *http.Request, S)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := seam()
		if !ok {
			httpjson.NoRoute(w, r)
			return
		}
		serve(w, r, s)
	})
}

// fileRefusal answers a file route's failure with the status its cause
// deserves.
func fileRefusal(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, paced.ErrBody):
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

// serveFileDownload answers GET /work/files/{project}/{path...}: the file's
// bytes, streamed.
func (a *App) serveFileDownload(w http.ResponseWriter, r *http.Request, files FileReads) {
	fresh, err := tracker.ParseFreshness(queries.FromQuery(r.URL.Query()))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_params", "detail": err.Error()})
		return
	}
	fresh.Level = statelog.LevelFor(statelog.SurfaceDashboard, fresh.Level)
	detail, err := files.File(r.Context(), r.PathValue("project"), r.PathValue("path"), fresh)
	if err != nil {
		fileRefusal(w, err)
		return
	}
	f := detail.File
	object, named := f.Content()
	if !named {
		// A LIVE FILE NAMING NO OBJECT is one the applier refuses to write,
		// so this is the engine's own fault: no retry reads it, which rules
		// out a 503, and nothing is gone, which rules out a 404 or a 410.
		log.Warn("api_file_names_no_object", "project", f.Project, "path", f.Path)
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": stream.CodeQueryFailed})
		return
	}
	body, err := files.Open(r.Context(), object)
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
	if err := paced.Send(w, src, a.pace); err != nil {
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
func (a *App) serveFileUpload(w http.ResponseWriter, r *http.Request, files FileWrites) {
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
	if refused := files.AcceptsFiles(r.Context(), project, accepts); refused != nil {
		fileRefusal(w, refused)
		return
	}
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(filePath))
	}
	// THE BYTES FIRST — see the file's head — paced as they arrive.
	object, err := files.Put(r.Context(), paced.Body(r.Body, http.NewResponseController(w), a.pace),
		tracker.MaxFileBytes, objstore.PutMeta{ContentType: contentType})
	if err != nil {
		fileRefusal(w, err)
		return
	}
	// NOTHING IS DELETED ON A REFUSAL BELOW: a write retried under one
	// operation id can be refused here after another node committed it,
	// and the object would be that file's content. What no row names, the
	// collector removes a day later.
	result, err := files.PutFileAs(r.Context(), operator, opID, tracker.FilePut{
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
func (a *App) serveFileRemove(w http.ResponseWriter, r *http.Request, files FileWrites) {
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
	result, err := files.RemoveFileAs(r.Context(), operator, opID, project, filePath, version)
	if err != nil {
		fileRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, writeAnswer(result, map[string]any{
		"project": project, "path": filePath,
	}))
}
