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
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A project's file BYTES, over HTTP.
//
// The listing is a question like every other read on this surface (`GET
// /work/files`, the `work_files` query). What is here is what cannot be one:
// a download STREAMS a file back chunk by chunk, and an upload streams one in —
// neither holding the file in memory and neither passing through a model. The
// rows go through the tracker and the bytes through this node's object client,
// the upload always bytes first: a file that is listed is a file that can be
// read.
//
// A write is attributed to the OPERATOR on the request, for the purge's
// reason: a person's upload names that person in the file's history, never the
// process that relayed it.

// ProjectFiles is the file surface's reach into the engine.
type ProjectFiles interface {
	File(ctx context.Context, project, path string, fresh statelog.Freshness) (tracker.FileDetail, error)
	Open(ctx context.Context, m objstore.Manifest) (io.ReadCloser, error)
	PutChunk(ctx context.Context, h objstore.Hash, data []byte) (int, error)
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

// fileRefusal answers a file route's failure with the status its cause
// deserves.
func fileRefusal(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tracker.ErrNoProject):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_project", "detail": err.Error()})
	case errors.Is(err, tracker.ErrNoFile):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_file", "detail": err.Error()})
	case errors.Is(err, tracker.ErrStaleVersion):
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{
			"error": "version_moved", "detail": err.Error(),
		})
	case errors.Is(err, objstore.ErrTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": "too_large", "detail": err.Error(),
		})
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
	body, err := a.files.Open(r.Context(), f.Manifest())
	if err != nil {
		fileRefusal(w, err)
		return
	}
	defer body.Close()
	head := make([]byte, min(int64(downloadHead), f.Size))
	if _, err := io.ReadFull(body, head); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "content_unavailable",
			"detail": fmt.Sprintf("%s exists and its content could not be read "+
				"right now: %v", f.Path, err),
		})
		return
	}
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
	if _, err := io.Copy(w, io.MultiReader(bytes.NewReader(head), body)); err != nil {
		// THE STATUS IS SENT, so the one honest signal left is a body
		// shorter than its Content-Length: aborting the handler closes
		// the connection rather than ending the response cleanly.
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
// file, streamed into the object store a chunk at a time and then recorded.
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
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(filePath))
	}
	// THE BYTES FIRST — see the file's head.
	manifest, err := objstore.Split(r.Context(), r.Body, tracker.MaxFileBytes,
		func(ctx context.Context, c objstore.Chunk, data []byte) error {
			_, putErr := a.files.PutChunk(ctx, c.Hash, data)
			return putErr
		})
	if err != nil {
		fileRefusal(w, err)
		return
	}
	result, err := a.files.PutFileAs(r.Context(), operator, opID, tracker.FilePut{
		Project: project, Path: filePath, ContentType: contentType,
		Manifest: manifest, IfMatch: version,
	})
	if err != nil {
		fileRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, writeAnswer(result, map[string]any{
		"project": project, "path": filePath, "size": manifest.Size,
		"hash": manifest.Hash, "content_type": contentType,
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
