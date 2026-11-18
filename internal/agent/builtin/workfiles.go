package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The project file tools. A file is a tracker row naming an object plus the
// object, so these tools take [FileReader] and [FileWriter] for the row and
// [ObjectStore] for the bytes, and a write puts the bytes first: the row first
// would publish a file whose content is nowhere yet. A read answers a page of
// text, or base64 when asked; a file that is not text is described.
//
// The object store's failures are told in fixed words, its error only in the
// node's log ([objectFailure]), since it names endpoints, buckets and keys.
// Wrong content ([objstore.ErrCorrupt]) or a row naming none is damage no retry
// reads past ([tools.RefusalInternalError]); no room ([objstore.Full]) is
// [tools.RefusalStoreFull] naming [objstore.Full.Remedy], since a retry is
// refused the same; else the store is not answering ([tools.RefusalUnavailable]).

// FileReader is the tracker's read side for files.
type FileReader interface {
	Files(ctx context.Context, q worklet.FileQuery) (worklet.FileListing, error)
	File(ctx context.Context, project, path string, fresh statelog.Freshness) (worklet.FileDetail, error)
}

// FileWriter is the tracker's write side for files.
type FileWriter interface {
	PutFile(ctx context.Context, opID string, put worklet.FilePut) (worklet.WriteResult, error)
	RemoveFile(ctx context.Context, opID, project, path string, ifMatch uint64) (worklet.WriteResult, error)
}

// ObjectStore is the fleet's object store as these tools need it: a write
// streams the content into a new object ([objstore.Store.Put]), a read takes
// a page of one ([objstore.Store.ReadAt]).
type ObjectStore interface {
	Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error)
	ReadAt(ctx context.Context, o objstore.Object, off, n int64) ([]byte, error)
}

// The read page: 32 KiB by default and 48 at most, since base64 grows a page by
// a third and 48 keeps the largest answer, metadata and envelope included,
// inside [ToolAnswerBytes].
const (
	defaultFileRead = 32 << 10
	maxFileRead     = 48 << 10
)

// The listing page a seat asks the tracker for ([fileListPage]): 50 by default
// and 100 at most, below the tracker's own ([worklet.DefaultFilePage],
// [worklet.MaxFilePage]). A row is about 420 bytes with a short path, so
// [ToolAnswerBytes] holds about 150; 100 rows (~41 KiB) leave room for the
// envelope and longer paths. 50 is the tracker's other listings' default
// ([worklet.PageDefault]).
const (
	defaultFileList = 50
	maxFileList     = 100
)

// fileListPage is the page a seat's listing asks the tracker for: the default
// for none, and never more than its answer carries.
func fileListPage(asked int) int {
	switch {
	case asked <= 0:
		return defaultFileList
	case asked > maxFileList:
		return maxFileList
	default:
		return asked
	}
}

// fileArgs is a project and a path as a caller names them, the project
// defaulting to the caller's unit's.
func (d WorkDeps) fileArgs(actor *Actor, args map[string]any) (string, string, string) {
	project := strings.TrimSpace(argString(args, "project"))
	if project == "" {
		project = d.defaultProject(*actor)
	}
	if project == "" {
		return "", "", "name the project the file is in"
	}
	return worklet.ProjectKey(project), argString(args, "path"), ""
}

// normalizePath is a file's path as the tracker keeps it
// ([worklet.NormalizeFilePath]), or its refusal in the tracker's sentence
// ([worklet.Sentence]), never its error, which opens with the package's name.
// Every tool naming a file holds its path to this one rule.
func normalizePath(tool, raw string) (string, *tools.Result) {
	p, err := worklet.NormalizeFilePath(raw)
	if err != nil {
		r := failed(fmt.Sprintf("%s: %s.", tool, worklet.Sentence(err)))
		return "", &r
	}
	return p, nil
}

func projectParameter() map[string]any {
	return map[string]any{
		"type": "string",
		"description": "The project's key, e.g. `ENG`. Omit for your own " +
			"team's project.",
	}
}

func filePathParameter(what string) map[string]any {
	return map[string]any{
		"type": "string",
		"description": what + " Folders are separated by `/`, e.g. " +
			"`reports/2026/q3.md`.",
	}
}

type listProjectFiles struct{ deps WorkDeps }

var _ tools.Callable = (*listProjectFiles)(nil)

func (t *listProjectFiles) Name() string { return worklet.ListProjectFilesTool }

func (t *listProjectFiles) Description() string {
	return "List the files kept in a project — reports, specs, notes and " +
		"anything else its work produced — in path order, a page at a time. " +
		"Read one with " + worklet.ReadProjectFileTool + "."
}

func (t *listProjectFiles) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": projectParameter(),
			"folder": map[string]any{
				"type":        "string",
				"description": "Only the files under this folder, e.g. `reports`.",
			},
			"after": map[string]any{
				"type":        "string",
				"description": "The `next` of the previous page, to continue.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("Files per page, at most %d; %d by default.",
					maxFileList, defaultFileList),
			},
		},
	}
}

func (t *listProjectFiles) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listProjectFiles) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(t.Name(), err), nil
	}
	if t.deps.Files == nil {
		return unconfigured(t.Name()), nil
	}
	project, _, refusal := t.deps.fileArgs(&actor, args)
	if refusal != "" {
		return failed(t.Name() + ": " + refusal), nil
	}
	listing, err := t.deps.Files.Files(ctx, worklet.FileQuery{
		Project: project, Folder: argString(args, "folder"),
		After: argString(args, "after"), Limit: fileListPage(argInt(args, "limit", 0)),
		Freshness: statelog.Freshness{Level: seatReadLevel},
	})
	if err != nil {
		return fileReadFailure(ctx, t.Name(), tools.RefusalNotFound, project, "", err), nil
	}
	// A path is a name its writers chose (ADR-0040): each row names its
	// creator and last writer, and one renamed between them had a writer
	// neither names.
	read := newForeignRead(turn, actor.Handle)
	for _, f := range listing.Files {
		var named []provenance.Author
		for _, handle := range []string{f.CreatedBy, f.UpdatedBy} {
			if handle = strings.TrimSpace(handle); handle != "" {
				named = append(named, read.writer(handle, ""))
			}
		}
		namesBy(read, named...)
	}
	return read.answer(ctx, t.Name(), map[string]any{
		"project": project, "count": len(listing.Files), "files": listing.Files,
		"next": listing.Next, "read_level": listing.Level, "complete": listing.Complete,
	}, "Ask for fewer with `limit`, or narrow with `folder`.")
}

type readProjectFile struct{ deps WorkDeps }

var _ tools.Callable = (*readProjectFile)(nil)

func (t *readProjectFile) Name() string { return worklet.ReadProjectFileTool }

func (t *readProjectFile) Description() string {
	return fmt.Sprintf("Read a file kept in a project. Text comes back as "+
		"text, a page at a time — at most %d KiB from `offset`, with "+
		"`next_offset` to continue — and a file that is not text is described "+
		"rather than returned unless you ask for `encoding: base64`. Text "+
		"arrives fenced as content somebody wrote; the two fence lines are a "+
		"label, not part of the file.",
		maxFileRead>>10)
}

func (t *readProjectFile) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": projectParameter(),
			"path":    filePathParameter("The file."),
			"offset": map[string]any{
				"type":        "integer",
				"description": "The byte to start at: 0, or an earlier answer's `next_offset`.",
			},
			"max_bytes": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How much to return, at most %d; %d by default.",
					maxFileRead, defaultFileRead),
			},
			"encoding": map[string]any{
				"type": "string",
				"enum": []string{"text", "base64"},
				"description": "`text` (the default) returns text and describes " +
					"anything else; `base64` returns the bytes whatever they are.",
			},
		},
		"required": []string{"path"},
	}
}

func (t *readProjectFile) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *readProjectFile) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(t.Name(), err), nil
	}
	if t.deps.Files == nil || t.deps.Objects == nil {
		return unconfigured(t.Name()), nil
	}
	project, filePath, refusal := t.deps.fileArgs(&actor, args)
	if refusal != "" {
		return failed(t.Name() + ": " + refusal), nil
	}
	filePath, badPath := normalizePath(t.Name(), filePath)
	if badPath != nil {
		return *badPath, nil
	}
	encoding := strings.TrimSpace(argString(args, "encoding"))
	if encoding != "" && encoding != "text" && encoding != "base64" {
		return failed(fmt.Sprintf("%s: encoding is `text` or `base64`, not %q", t.Name(), encoding)), nil
	}
	offset := int64(argInt(args, "offset", 0))
	limit := argInt(args, "max_bytes", defaultFileRead)
	if offset < 0 || limit <= 0 || limit > maxFileRead {
		return failed(fmt.Sprintf("%s: offset is 0 or more and max_bytes is 1..%d",
			t.Name(), maxFileRead)), nil
	}
	detail, err := t.deps.Files.File(ctx, project, filePath,
		statelog.Freshness{Level: seatReadLevel})
	if err != nil {
		return fileReadFailure(ctx, t.Name(), tools.RefusalNotFound, project, filePath, err), nil
	}
	f := detail.File
	// THE CONTENT IS SOMEBODY'S WORDS, labelled for the handle this call
	// reads as; every other field is defused on the way out.
	read := newForeignRead(turn, actor.Handle)
	writers, partial := fileWriters(read, f)
	narrow := "Narrow what you asked for."
	answer := map[string]any{
		"project": f.Project, "path": f.Path, "content_type": f.ContentType,
		"size": f.Size, "version": f.Version, "hash": f.Hash,
		"updated_by": f.UpdatedBy, "updated_at": f.UpdatedAt, "offset": offset,
	}
	object, unnamed := fileContent(ctx, t.Name(), f)
	if unnamed != nil {
		return *unnamed, nil
	}
	page, err := t.deps.Objects.ReadAt(ctx, object, offset, int64(limit))
	if err != nil {
		return fileContentFailure(ctx, t.Name(), f, err), nil
	}
	if encoding == "base64" {
		// Not fenced, since base64 is not prose and its alphabet holds no
		// bracket, but labelled with whose bytes they are.
		read.attribute(writers, partial)
		answer["encoding"], answer["content"] = "base64", base64.StdEncoding.EncodeToString(page)
		return read.answer(ctx, t.Name(), withNext(answer, offset, int64(len(page)), f.Size), narrow)
	}
	text, whole := textOf(page, offset+int64(len(page)) == f.Size)
	if !whole {
		answer["encoding"] = "binary"
		answer["note"] = "this is not text; read it with `encoding: base64` to get its bytes"
		return read.answer(ctx, t.Name(), answer, narrow)
	}
	// THE OFFSETS COUNT THE FILE'S BYTES, taken before the fence is put
	// round the page, so `next_offset` still points into the file.
	answer["encoding"], answer["content"] = "text", text
	read.fence(fieldPath{}.key("content"), provenance.SourceProjectFile, writers, partial, text)
	return read.answer(ctx, t.Name(), withNext(answer, offset, int64(len(text)), f.Size), narrow)
}

// copySource is a project's file a copy reads from ([WorkDeps.copyFrom]): the
// normalised project and path, the file's row, and the object holding its bytes.
type copySource struct {
	project, filePath string
	file              worklet.File
	object            objstore.Object
}

// copyFrom decides and reads the project file another write copies (an
// artifact's `from_project_file`) by read_project_file's rules, so a failed
// copy says what the read would: Worklet run, that tool's verb asked of the
// caller, the path held to [normalizePath], the row read at a seat's level and
// its content named. A missing project or file is [tools.RefusalInvalid], since
// a copy only names its source ([fileReadFailure]).
//
// The bytes are the caller's to read, from [WorkDeps.Objects] under its own
// bound, each failure told with [fileContentFailure]: what a copy may hold is
// the write's to say, not the read's.
func (d WorkDeps) copyFrom(ctx context.Context, tool, project, filePath string) (copySource, *tools.Result) {
	if d.Files == nil || d.Objects == nil {
		r := refusedBy(tools.RefusalUnavailable, waitless(), tool+" cannot read project files: this "+
			"company does not run Worklet.")
		return copySource{}, &r
	}
	// read_project_file's verb, asked as its gate asks it: a caller who may
	// not read a project's files may not copy one either.
	if refusal := askAuthority(ctx, d.Authorize, authz.Action(worklet.ReadProjectFileTool),
		authz.Object{}); refusal != nil {
		return copySource{}, refusal
	}
	key := worklet.ProjectKey(project)
	// Held to the path rule before the tracker is read: a path the reader
	// refuses would be told as this node's fault.
	at, refusal := normalizePath(tool, filePath)
	if refusal != nil {
		return copySource{}, refusal
	}
	detail, err := d.Files.File(ctx, key, at, seatRead)
	if err != nil {
		r := fileReadFailure(ctx, tool, tools.RefusalInvalid, key, at, err)
		return copySource{}, &r
	}
	object, refusal := fileContent(ctx, tool, detail.File)
	if refusal != nil {
		return copySource{}, refusal
	}
	return copySource{project: key, filePath: at, file: detail.File, object: object}, nil
}

// fileWriters are who wrote a file's content, and whether writers the row does
// not name had a hand in it. A put sends the whole content, so a file written
// once is wholly its creator's; a rewritten one names only its first and latest
// writer, and the latest may have sent earlier words back unchanged (as
// [fencePage] says of a page), so it is partial.
func fileWriters(read *foreignRead, f worklet.File) ([]provenance.Author, bool) {
	created, updated := strings.TrimSpace(f.CreatedBy), strings.TrimSpace(f.UpdatedBy)
	var writers []provenance.Author
	partial := false
	rewritten := created != updated || !f.CreatedAt.Equal(f.UpdatedAt)
	if rewritten && created != "" {
		writers = append(writers, read.writer(created, ""))
	}
	if updated != "" {
		writers = append(writers, read.writer(updated, ""))
	} else {
		partial = true
	}
	if rewritten {
		partial = true
	}
	return writers, partial
}

// textOf is a page as text, and false when it is not text. A page that ends
// inside a character is cut back to the character before, so the next page
// starts on one — unless the page is the end of the file, where a broken
// character is the file's own and makes it not text.
func textOf(page []byte, last bool) (string, bool) {
	if !last {
		for cut := 0; cut < utf8.UTFMax && len(page) > 0 && !utf8.Valid(page); cut++ {
			page = page[:len(page)-1]
		}
	}
	if !utf8.Valid(page) || bytes.IndexByte(page, 0) >= 0 {
		return "", false
	}
	return string(page), true
}

// withNext says where the next page starts, when there is one.
func withNext(answer map[string]any, offset, read, size int64) map[string]any {
	if next := offset + read; next < size {
		answer["next_offset"] = next
		answer["truncated"] = true
	}
	return answer
}

// fileReadFailure explains a file read that could not be served, never telling
// a failed read as a file that is not there; the tracker's sentinel rides
// beneath the class. absent, the class of a missing project or file, is the
// call's own statement ([refusedBy]): [tools.RefusalNotFound] to a call ABOUT
// the file (a read, a listing, a removal), [tools.RefusalInvalid] to one that
// only NAMES it, as a copy's source ([WorkDeps.copyFrom]), where a not-found
// would read as the artifact gone. The words are the same either way.
func fileReadFailure(ctx context.Context, tool string, absent tools.Refusal,
	project, filePath string, err error) tools.Result {

	switch {
	case errors.Is(err, worklet.ErrNoProject):
		return refusedBy(absent, err, fmt.Sprintf("%s: there is no "+
			"project %s. List them with %s.", tool, project, worklet.ListProjectsTool))
	case errors.Is(err, worklet.ErrNoFile):
		return refusedBy(absent, err, fmt.Sprintf("%s: there is no "+
			"file %s in %s. List them with %s.", tool, filePath, project,
			worklet.ListProjectFilesTool))
	}
	return readFailure(ctx, tool, err)
}

// fileContent is the object holding a file's bytes ([worklet.File.Content]),
// or the refusal of a live file whose row names none: the applier refuses to
// write one, so it is the engine's fault, never a file that is gone or a read a
// retry fixes.
func fileContent(ctx context.Context, tool string, f worklet.File) (objstore.Object, *tools.Result) {
	object, named := f.Content()
	if !named {
		r := faulted(ctx, tool, fmt.Errorf("builtin: %s in %s names no object",
			f.Path, f.Project), fmt.Sprintf("%s: %s in %s exists, and its row names "+
			"no content the object store could return. Trying again will not help — "+
			"tell a person the file needs writing again; this node's log has the details.",
			tool, f.Path, f.Project))
		return objstore.Object{}, &r
	}
	return object, nil
}

// fileContentFailure is a read of a file's content the object store could not
// serve, told as the file's head says ([objectFailure]).
func fileContentFailure(ctx context.Context, tool string, f worklet.File, err error) tools.Result {
	if errors.Is(err, objstore.ErrCorrupt) {
		// Not a failed read: the store answered with bytes that are not the
		// file's, and a retry reads the same bytes.
		return objectFailure(ctx, tool, tools.RefusalInternalError, err,
			fmt.Sprintf("%s: %s in %s exists, and the object store holds content "+
				"for it that is not what was written. Trying again will not help — do "+
				"not use what it holds, and tell a person the file needs restoring or "+
				"writing again; this node's log has the details.",
				tool, f.Path, f.Project))
	}
	return objectFailure(ctx, tool, tools.RefusalUnavailable, err,
		fmt.Sprintf("%s: %s in %s exists, and its content could not be read "+
			"from the object store right now. The file is NOT empty or missing — "+
			"try again, or say you could not read it.",
			tool, f.Path, f.Project))
}

// objectFailure is the failed result of a call the object store could not
// serve: the caller's class and sentence, the error beneath the class for
// [errors.Is] and in the node's log, never in the sentence (the file's head).
func objectFailure(ctx context.Context, tool string, class tools.Refusal, err error,
	sentence string) tools.Result {

	log.WarnContext(ctx, "builtin_tool_object_store_failed", "tool", tool,
		"class", string(class), "error", err.Error())
	return refusedBy(class, err, sentence)
}

type writeProjectFile struct{ deps WorkDeps }

var _ tools.Callable = (*writeProjectFile)(nil)

func (t *writeProjectFile) Name() string { return worklet.WriteProjectFileTool }

func (t *writeProjectFile) Description() string {
	return "Write a file into a project, creating it or replacing what is " +
		"there. Use it for what the work produced — the report, the plan, the " +
		"notes — so everybody on the project finds it in one place. Pass the " +
		"`version` " + worklet.ReadProjectFileTool + " gave you as `if_version` to refuse the " +
		"write if somebody changed the file since."
}

func (t *writeProjectFile) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": projectParameter(),
			"path":    filePathParameter("Where the file goes."),
			"content": map[string]any{
				"type":        "string",
				"description": "The whole content — text, or base64 with `encoding: base64`.",
			},
			"encoding": map[string]any{
				"type": "string",
				"enum": []string{"text", "base64"},
			},
			"content_type": map[string]any{
				"type":        "string",
				"description": "e.g. `text/markdown`. Omit to take it from the extension.",
			},
			"if_version": map[string]any{
				"type":        "integer",
				"description": "Refuse the write if the file is no longer at this version.",
			},
		},
		"required": []string{"path", "content"},
	})
}

func (t *writeProjectFile) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *writeProjectFile) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(t.Name(), err), nil
	}
	if t.deps.FileWriter == nil || t.deps.Objects == nil {
		return unconfigured(t.Name()), nil
	}
	actor, denied := t.deps.bindOperation(actor, t.Name(), args)
	if denied != "" {
		return failed(denied), nil
	}
	project, rawPath, refusal := t.deps.fileArgs(&actor, args)
	if refusal != "" {
		return failed(t.Name() + ": " + refusal), nil
	}
	filePath, badPath := normalizePath(t.Name(), rawPath)
	if badPath != nil {
		return *badPath, nil
	}
	content := []byte(argString(args, "content"))
	encoding := strings.TrimSpace(argString(args, "encoding"))
	switch encoding {
	case "", "text":
		encoding = "text"
		// Text read_project_file answered arrives fenced, and a copy of it
		// sent back whole would store the fence in the file; see [unfence].
		content = []byte(unfence(turn, string(content)))
	case "base64":
		decoded, decodeErr := base64.StdEncoding.DecodeString(string(content))
		if decodeErr != nil {
			return failed(fmt.Sprintf("%s: the content is not base64: %v", t.Name(), decodeErr)), nil
		}
		content = decoded
	default:
		return failed(fmt.Sprintf("%s: encoding is `text` or `base64`, not %q", t.Name(), encoding)), nil
	}
	contentType := strings.TrimSpace(argString(args, "content_type"))
	if contentType == "" {
		contentType = guessContentType(filePath, encoding)
	}
	ifVersion := uint64(max(argInt(args, "if_version", 0), 0))
	if same, ok := t.unchanged(ctx, project, filePath, contentType, content, ifVersion); ok {
		return jsonResult(withOperation(map[string]any{
			"project": project, "path": filePath, "size": same.Size, "hash": same.Hash,
			"content_type": same.ContentType, "outcome": "unchanged",
			"version": same.Version,
		}, actor))
	}
	// THE BYTES FIRST — see the file's head.
	object, err := t.deps.Objects.Put(ctx, bytes.NewReader(content), worklet.MaxFileBytes,
		objstore.PutMeta{ContentType: contentType})
	var full *objstore.Full
	switch {
	case errors.Is(err, objstore.ErrTooLarge):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s: %s was not "+
			"written: its content is over the %d bytes a project's file may hold",
			t.Name(), filePath, int64(worklet.MaxFileBytes))), nil
	case errors.As(err, &full):
		// NO ROOM, WHICH NO RETRY MAKES — see the file's head.
		return objectFailure(ctx, t.Name(), tools.RefusalStoreFull, err,
			fmt.Sprintf("%s: %s was not written, and nothing was recorded: the "+
				"object store has no room left for its content. Making the call "+
				"again is refused the same until %s — say so, and say the file "+
				"was not written.", t.Name(), filePath, full.Remedy)), nil
	case err != nil:
		// A store that did not take the bytes is a condition, invited back
		// ([objstore.ErrUnanswered]); anything else is this node's fault, as
		// every surface storing a file tells it.
		if why, ok := Condition(err); ok {
			return objectFailure(ctx, t.Name(), tools.RefusalUnavailable, err,
				fmt.Sprintf("%s: %s was not written, and nothing was recorded: its "+
					"content could not be stored (%s). %s", t.Name(), filePath, why,
					conditionNext(err, "Try again shortly.", stuckWrite))), nil
		}
		return faulted(ctx, t.Name(), err, fmt.Sprintf("%s: %s was not written, and "+
			"nothing was recorded: %s.", t.Name(), filePath, faultSaid)), nil
	}
	// Nothing is deleted on a refusal below: a retry under one operation can
	// be refused on one node after another committed it, making the object
	// that file's content. The collector removes what no row names.
	opID := opIDFor(actor, t.Name(), "file", project+"/"+filePath, args)
	result, err := t.deps.FileWriter(actor).PutFile(ctx, opID, worklet.FilePut{
		Project: project, Path: filePath, ContentType: contentType, Object: object,
		IfMatch: ifVersion,
	})
	if errors.Is(err, worklet.ErrUploadStale) {
		// The upload's state, which no wait changes: a conflict, as the
		// artifacts' stale upload and the file route's 409 `upload_expired`
		// are, which the same call fixes by uploading anew under a fresh key.
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s: %s was not "+
			"written: %s. Make the same call again.", t.Name(), filePath,
			worklet.Sentence(err))), nil
	}
	if err != nil {
		return writeFailure(ctx, actor, t.Name(), err), nil
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return unknownWrite(actor, t.Name(),
			fmt.Sprintf("%s was written to %s", filePath, project), opID,
			result.Unvouched, unknownNext(result.Unvouched, sameCall(actor, t.Name()),
				fmt.Sprintf("Read %s in %s with %s", filePath, project, worklet.ReadProjectFileTool),
				"it writes the same content again, which changes nothing")), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(withOperation(map[string]any{
		"project": project, "path": filePath, "size": object.Size, "hash": object.Hash,
		"content_type": contentType, "outcome": string(result.Outcome),
		"position": positionOf(result.Position), "version": result.Version,
	}, actor))
}

// unchanged answers the live file at project/filePath when writing content
// there would change nothing: the same bytes and type, and no if_version it has
// moved past. Every upload is an object of its own (ADR-0026), so a re-save
// would store a second copy until the collector removes the first; the content
// is in memory, so the check is one digest against the row's. Anything this
// read cannot settle (no reader, a failed read) is written.
func (t *writeProjectFile) unchanged(ctx context.Context, project, filePath,
	contentType string, content []byte, ifVersion uint64) (worklet.File, bool) {

	if t.deps.Files == nil {
		return worklet.File{}, false
	}
	detail, err := t.deps.Files.File(ctx, project, filePath,
		statelog.Freshness{Level: seatReadLevel})
	if err != nil {
		return worklet.File{}, false
	}
	f := detail.File
	if _, named := f.Content(); !named || f.Hash != objstore.HashOf(content) ||
		f.ContentType != contentType || (ifVersion != 0 && ifVersion != f.Version) {
		return worklet.File{}, false
	}
	return f, true
}

// guessContentType is a content type from a path's extension, or the plain
// default for how the content arrived.
func guessContentType(filePath, encoding string) string {
	if t := mime.TypeByExtension(path.Ext(filePath)); t != "" {
		return t
	}
	switch strings.ToLower(path.Ext(filePath)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".yaml", ".yml":
		return "application/yaml"
	}
	if encoding == "base64" {
		return "application/octet-stream"
	}
	return "text/plain; charset=utf-8"
}

type removeProjectFile struct{ deps WorkDeps }

var _ tools.Callable = (*removeProjectFile)(nil)

func (t *removeProjectFile) Name() string { return worklet.RemoveProjectFileTool }

func (t *removeProjectFile) Description() string {
	return "Remove a file from a project. Its history says who removed it and " +
		"when; its content is deleted from storage, so write it again to bring " +
		"it back."
}

func (t *removeProjectFile) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": projectParameter(),
			"path":    filePathParameter("The file."),
			"if_version": map[string]any{
				"type":        "integer",
				"description": "Refuse the removal if the file is no longer at this version.",
			},
		},
		"required": []string{"path"},
	})
}

func (t *removeProjectFile) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *removeProjectFile) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(t.Name(), err), nil
	}
	if t.deps.FileWriter == nil {
		return unconfigured(t.Name()), nil
	}
	actor, denied := t.deps.bindOperation(actor, t.Name(), args)
	if denied != "" {
		return failed(denied), nil
	}
	project, rawPath, refusal := t.deps.fileArgs(&actor, args)
	if refusal != "" {
		return failed(t.Name() + ": " + refusal), nil
	}
	filePath, badPath := normalizePath(t.Name(), rawPath)
	if badPath != nil {
		return *badPath, nil
	}
	// The project it is kept in decides, once the call has settled which
	// (the `inTool` entries in authority.go). A colleague may keep a file in
	// a project but not remove one: that deletes the content, the lead's call.
	if denied := t.deps.mayWrite(ctx, authz.ActionFileRemove, authz.Object{
		Kind: authz.KindProject, Container: project,
	}); denied != nil {
		return *denied, nil
	}
	opID := opIDFor(actor, t.Name(), "file-remove", project+"/"+filePath, args)
	result, err := t.deps.FileWriter(actor).RemoveFile(ctx, opID, project, filePath,
		uint64(max(argInt(args, "if_version", 0), 0)))
	switch {
	case errors.Is(err, worklet.ErrNoFile):
		return fileReadFailure(ctx, t.Name(), tools.RefusalNotFound, project, filePath, err), nil
	case err != nil:
		return writeFailure(ctx, actor, t.Name(), err), nil
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return unknownWrite(actor, t.Name(),
			fmt.Sprintf("%s was removed from %s", filePath, project), opID,
			result.Unvouched, unknownNext(result.Unvouched, sameCall(actor, t.Name()),
				fmt.Sprintf("List %s's files with %s", project, worklet.ListProjectFilesTool),
				"it is refused, because the file is already gone")), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(withOperation(map[string]any{
		"project": project, "path": filePath, "outcome": string(result.Outcome),
		"position": positionOf(result.Position),
	}, actor))
}
