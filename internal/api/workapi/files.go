package workapi

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

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/paced"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- a project's file BYTES -------------------------------------------- //
//
// The listing is a question like every other read (`GET /worklet/files`, the
// `work_files` query). What is here is what cannot be one: a download STREAMS
// a file back and an upload streams one in — neither holding the file in
// memory and neither passing through a model. The rows go through the
// tracker's half — the SAME file reader and writer the file tools are handed
// ([builtin.WorkDeps.Files], [builtin.WorkDeps.FileWriter]) — and the bytes
// through this node's object store ([Halves.Objects]), the upload always
// bytes first: a file that is listed is a file that can be read.
//
// THREE VERBS NO TOOL MAKES, on this surface's terms for one: each is decided
// on the project its path names by the same rows the file tools are
// ([authz.ActionFileRead], [authz.ActionFileWrite], [authz.ActionFileRemove]),
// a write is attributed to the request's principal as every write here is
// ([Service.actor]), takes its operation from the request's `Idempotency-Key`
// ([operationKey]) bound to what it asks ([keyedOp]), leaves the
// `operator_acted` record a tool's call does ([operator.AuditGesture]), and
// answers a refusal the writer did not name for files through the one refusal
// mapping ([operator.FailWrite]).
//
// WHAT IS THE BYTES' OWN KEEPS ITS OWN CODE: a path no project keeps a file
// under, an If-Match that is not a version, a body that stopped arriving or
// outgrew the cap, an upload too old to be named, content the store will not
// vouch for — and the writer's three refusals about a FILE as a resource with
// an ETag: no project, no file, and a version moved past, which is HTTP's own
// `412` for a failed If-Match on the resource it names.

// filesPattern is the path every byte route answers on.
const filesPattern = "/worklet/files/{project}/{path...}"

// ObjectStore is this node's object store as the byte routes stream through
// it: an upload into a new object ([objstore.Store.Put]), a download out of
// one, checked against the row naming it ([objstore.Store.Open]).
//
// ITS OWN SEAM rather than [builtin.WorkDeps.Objects], which reads a file a
// page at a time for a tool that answers a model: a route streaming a
// gibibyte takes the object whole, as one reader.
type ObjectStore interface {
	Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error)
	Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error)
}

// servesFiles reports whether these halves hold the byte routes' whole
// surface: the tracker's file rows, the project read an upload is refused on
// before a byte is read, the file writer and the object store.
func (h Halves) servesFiles() bool {
	_, projects := h.Work.Reader.(builtin.ProjectReader)
	return projects && h.Work.Files != nil && h.Work.FileWriter != nil &&
		h.Objects != nil
}

// onFiles mounts a byte route.
func (s *Service) onFiles(h servedHandler) http.HandlerFunc {
	return s.on(Halves.servesFiles, h)
}

// fileRoutes mounts the byte routes.
//
// THE PROJECT IS IN THE PATH, so each route is decided there on it: the read
// on [authz.ActionFileRead], the upload on [authz.ActionFileWrite] — the
// tracker, as filing an item into the project is — and the removal on
// [authz.ActionFileRemove], which asks the chart who leads the project. The
// file tools ask the same rows of the same object.
func (s *Service) fileRoutes(mount mounter) {
	route := func(pattern string, p authz.Policy, h servedHandler) {
		mount(pattern, p, s.onFiles(h))
	}
	project := func(a authz.Action) authz.Policy {
		return authz.Policy{Action: a, Object: func(r *http.Request) authz.Object {
			return authz.Object{Kind: authz.KindProject,
				Container: worklet.ProjectKey(r.PathValue("project"))}
		}}
	}
	route("GET "+filesPattern, project(authz.ActionFileRead), (*served).getFile)
	// A PUT WITH NO If-Match WRITES A NEW FILE ([fileVersion]).
	route("PUT "+filesPattern, project(authz.ActionFileWrite).WhenAbsent(authz.CreatesWhenAbsent()),
		(*served).putFile)
	route("DELETE "+filesPattern, project(authz.ActionFileRemove), (*served).deleteFile)
}

// ---- the download ------------------------------------------------------- //

// downloadHead is how much of a file is read before the response starts —
// enough to know the content can be read at all, so a file whose bytes are
// unreachable answers 503 rather than a 200 with an empty body.
const downloadHead = 32 << 10

// getFile answers GET /worklet/files/{project}/{path...}: the file's bytes,
// streamed, as an attachment.
func (s *served) getFile(w http.ResponseWriter, r *http.Request) {
	fresh, err := statelog.ParseFreshness(queries.FromQuery(r.URL.Query()))
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
		return
	}
	fresh.Level = statelog.LevelFor(statelog.SurfaceDashboard, fresh.Level)
	detail, err := s.Work.Files.File(r.Context(), r.PathValue("project"),
		r.PathValue("path"), fresh)
	if err != nil {
		fileReadFailed(w, r, err)
		return
	}
	f := detail.File
	object, named := f.Content()
	if !named {
		// A LIVE FILE NAMING NO OBJECT is one the applier refuses to write,
		// so this is the engine's own fault: no retry reads it, which rules
		// out a 503, and nothing is gone, which rules out a 404 or a 410.
		log.Warn("api_file_names_no_object", "project", f.Project, "path", f.Path)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	body, err := s.Objects.Open(r.Context(), object)
	if err == nil {
		defer body.Close()
		head := make([]byte, min(int64(downloadHead), f.Size))
		_, err = io.ReadFull(body, head)
		if err == nil {
			s.sendFile(w, f, io.MultiReader(bytes.NewReader(head), body))
			return
		}
	}
	if errors.Is(err, objstore.ErrCorrupt) {
		// DAMAGED, NOT UNREACHABLE: the store answered, and what it holds
		// is not the content the file records. A retry reads the same
		// bytes, so this is no 503.
		log.Warn("api_file_content_corrupt", "project", f.Project, "path", f.Path,
			"error", err)
		httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeContentCorrupt,
			map[string]string{"detail": f.Path + " exists and the object store holds " +
				"content for it that is not what it records"})
		return
	}
	// UNREACHABLE, NOT DAMAGED: the store did not answer, which clears when
	// it answers again — the broker's own election at the soonest, the
	// default store being the data nodes' NATS bucket. The store's own words
	// are the log's.
	log.Warn("api_file_content_unavailable", "project", f.Project, "path", f.Path,
		"error", err)
	httpjson.UnavailableWith(w, httpjson.CodeContentUnavailable,
		httpjson.RetrySeconds(statelog.ElectionRetryHint), httpjson.Detail{
			"detail": f.Path + " exists and its content could not be read right now",
		})
}

// sendFile writes the file's headers and then its content, from src.
func (s *served) sendFile(w http.ResponseWriter, f worklet.File, src io.Reader) {
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
	if err := paced.Send(w, src, s.pace); err != nil {
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

// ---- the writes --------------------------------------------------------- //

// putFile answers PUT /worklet/files/{project}/{path...}: the body is the file,
// streamed into one new object and then recorded.
func (s *served) putFile(w http.ResponseWriter, r *http.Request) {
	project := worklet.ProjectKey(r.PathValue("project"))
	filePath, ok := filePathOf(w, r)
	if !ok {
		return
	}
	version, ok := fileVersion(w, r)
	if !ok {
		return
	}
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.actor(w, r, key)
	if !ok {
		return
	}
	// REFUSED BEFORE A BYTE IS READ where the refusal is already known: a
	// declared length over the cap, and a project that takes no files.
	// Either found after the body would have streamed up to a gibibyte into
	// the store for nothing, and left it there for a day.
	if r.ContentLength > worklet.MaxFileBytes {
		bytesFailed(w, r, fmt.Errorf("%w: the body declares %d bytes and a project's "+
			"files are at most %d", objstore.ErrTooLarge, r.ContentLength,
			int64(worklet.MaxFileBytes)), key)
		return
	}
	if !s.acceptsFiles(w, r, project) {
		return
	}
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(filePath))
	}
	// THE BYTES FIRST — see the file's head — paced as they arrive.
	object, err := s.Objects.Put(r.Context(),
		paced.NewBody(r.Body, http.NewResponseController(w), s.pace),
		worklet.MaxFileBytes, objstore.PutMeta{ContentType: contentType})
	if err != nil {
		bytesFailed(w, r, err, key)
		return
	}
	// NOTHING IS DELETED ON A REFUSAL BELOW: a write retried under one
	// operation can be refused here after another node committed it, and
	// the object would be that file's content. What no row names, the
	// collector removes a day later.
	//
	// THE OPERATION IS BOUND TO THE CONTENT, by its digest: the same key
	// sending the same bytes again is the retry an unknown answer asks for,
	// and the ledger answers it with the first copy's result — the second
	// object being one no row names — while the same key sending other
	// bytes is another write, which lands as asked.
	asked := map[string]any{"hash": object.Hash, "content_type": contentType,
		"if_match": version}
	opID, ok := keyedOp(w, r, key, "file-put", project+"/"+filePath, asked)
	if !ok {
		return
	}
	result, err := s.Work.FileWriter(actor).PutFile(r.Context(), opID, worklet.FilePut{
		Project: project, Path: filePath, ContentType: contentType,
		Object: object, IfMatch: version,
	})
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork,
		string(authz.ActionFileWrite), key, result.Outcome, result.Position, err)
	if err != nil {
		fileWriteFailed(w, r, err, key)
		return
	}
	answerFile(w, key, result, map[string]any{
		"project": project, "path": filePath, "size": object.Size,
		"hash": object.Hash, "content_type": contentType,
	})
}

// deleteFile answers DELETE /worklet/files/{project}/{path...}: the file is
// stamped removed and its object dropped.
func (s *served) deleteFile(w http.ResponseWriter, r *http.Request) {
	project := worklet.ProjectKey(r.PathValue("project"))
	filePath, ok := filePathOf(w, r)
	if !ok {
		return
	}
	version, ok := fileVersion(w, r)
	if !ok {
		return
	}
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.actor(w, r, key)
	if !ok {
		return
	}
	opID, ok := keyedOp(w, r, key, "file-remove", project+"/"+filePath,
		map[string]any{"if_match": version})
	if !ok {
		return
	}
	result, err := s.Work.FileWriter(actor).RemoveFile(r.Context(), opID, project,
		filePath, version)
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork,
		string(authz.ActionFileRemove), key, result.Outcome, result.Position, err)
	if err != nil {
		fileWriteFailed(w, r, err, key)
		return
	}
	answerFile(w, key, result, map[string]any{"project": project, "path": filePath})
}

// acceptsFiles reads the project an upload names, refusing — before a byte of
// the body is read — one there is none of and one that takes no files, in the
// writer's own sentence ([worklet.AcceptFiles]). It reports whether to go on.
//
// AT THE LEVEL EVERY DECISION HERE READS AT ([decisionRead]), and only an
// early refusal: the write's own decide is what actually judges the project.
func (s *served) acceptsFiles(w http.ResponseWriter, r *http.Request, project string) bool {
	projects, _ := s.Work.Reader.(builtin.ProjectReader)
	detail, err := projects.Project(r.Context(), worklet.ProjectDetailQuery{
		Project: project, Level: decisionRead.Level,
	})
	if err == nil {
		err = worklet.AcceptFiles(detail.Key, detail.Archived)
	}
	if err == nil {
		return true
	}
	if errors.Is(err, worklet.ErrInvalid) {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid,
			map[string]string{"detail": worklet.Sentence(err)})
		return false
	}
	fileReadFailed(w, r, err)
	return false
}

// filePathOf is the route's file path, normalised as the tracker keeps it —
// refused `400 bad_path` naming what is wrong with it.
func filePathOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	filePath, err := worklet.NormalizeFilePath(r.PathValue("path"))
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadPath,
			map[string]string{"detail": err.Error()})
		return "", false
	}
	return filePath, true
}

// fileVersion reads an If-Match header's version, zero when absent — a write
// that replaces whatever is there.
func fileVersion(w http.ResponseWriter, r *http.Request) (uint64, bool) {
	raw := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if raw == "" {
		return 0, true
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadIfMatch,
			map[string]string{"detail": "If-Match is the file's version, as its " +
				"ETag carries it"})
		return 0, false
	}
	return v, true
}

// answerFile answers a file write that was MADE, in the THREE-VALUED
// vocabulary every write here answers in ([answer]) — and its position and
// version only where it is known to have landed.
func answerFile(w http.ResponseWriter, key string, result worklet.WriteResult,
	body map[string]any) {

	body["outcome"] = string(result.Outcome)
	if result.Outcome != statelog.OutcomeUnknown {
		body["position"] = positionOf(result.Position)
		body["version"] = result.Version
	}
	answer(w, key, result.Outcome, result.Unvouched, body)
}

// ---- the refusals ------------------------------------------------------- //

// fileReadFailed answers a read a byte route took — the file, or the project
// an upload names — that could not be served: the two that are about the
// RESOURCE under their own codes, and everything else as every read here is
// answered ([readFailed]).
func fileReadFailed(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, worklet.ErrNoProject):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoProject,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, worklet.ErrNoFile):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoFile,
			map[string]string{"detail": err.Error()})
	default:
		readFailed(w, r, err)
	}
}

// bytesFailed answers an upload whose bytes did not reach the store: a body
// that stopped arriving is the CLIENT'S (400, never a 503 telling it to retry
// a request whose own connection failed), one over the cap is the one
// spelling of a 413 every JSON surface answers, and a store that did not take
// them is a condition that clears when it answers again — the broker's own
// election at the soonest, the default store being the data nodes' NATS
// bucket — with the store's own words in the log.
func bytesFailed(w http.ResponseWriter, r *http.Request, err error, key string) {
	switch {
	case errors.Is(err, paced.ErrBody):
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeUnreadableBody,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, objstore.ErrTooLarge):
		httpjson.FailWith(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge,
			map[string]string{"detail": err.Error()})
	default:
		log.WarnContext(r.Context(), "api_file_store_unavailable", "op_id", key,
			"error", err.Error())
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
			httpjson.RetrySeconds(statelog.ElectionRetryHint), httpjson.Detail{
				"detail": "the file's content could not be stored in the object " +
					"store right now, and nothing was recorded",
				"op_id": key,
			})
	}
}

// fileWriteFailed answers what the tracker's file writer refused: the
// refusals about the file as a resource under their own codes, and anything
// else through the one refusal mapping ([operator.FailWrite]) — so a
// forbidden write, a conflict, a node that could not decide and a fault of the
// node's own read here as they read on every write.
func fileWriteFailed(w http.ResponseWriter, r *http.Request, err error, key string) {
	switch {
	case errors.Is(err, worklet.ErrNoProject):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoProject,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, worklet.ErrNoFile):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoFile,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, worklet.ErrStaleVersion):
		httpjson.FailWith(w, http.StatusPreconditionFailed, httpjson.CodeVersionMoved,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, worklet.ErrUploadStale):
		// THE BYTES ARE STORED AND MAY NOT BE NAMED ANY MORE: the upload
		// took longer than a write may name its object after
		// ([objstore.RecordWithin]), and sending it again is the one thing
		// that lands — a new upload is a new object, under this key or
		// another.
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeUploadExpired,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, worklet.ErrInvalid):
		// THE WRITE CAN NEVER LAND AS SENT — an archived project, a
		// content type no row may hold — which is the request's to change.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid,
			map[string]string{"detail": worklet.Sentence(err)})
	default:
		failErr(w, r, err, key)
	}
}
