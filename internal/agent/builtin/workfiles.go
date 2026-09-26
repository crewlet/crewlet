package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The project file tools.
//
// A file is TWO THINGS in two places: a row in the tracker saying it exists
// and which chunks it is, and the chunks themselves in the object store. So
// these tools take both halves — [FileReader] and [FileWriter] for the row,
// [ObjectStore] for the bytes — and a write is always bytes first, row second:
// the other order would publish a file whose content is not anywhere yet.
//
// # What a model is handed
//
// Text, and a PAGE of it: a read answers at most [maxFileRead] bytes from an
// offset, and says where the next page starts, because a tool answer has a
// ceiling ([ToolAnswerBytes]) and a file does not. A file that is not text is
// described rather than dumped — unless the caller asks for base64, which is
// what a seat moving bytes between a file and a sandbox needs.

// FileReader is the tracker's read side for files.
type FileReader interface {
	Files(ctx context.Context, q tracker.FileQuery) (tracker.FileListing, error)
	File(ctx context.Context, project, path string, fresh statelog.Freshness) (tracker.FileDetail, error)
}

// FileWriter is the tracker's write side for files.
type FileWriter interface {
	PutFile(ctx context.Context, opID string, put tracker.FilePut) (tracker.WriteResult, error)
	RemoveFile(ctx context.Context, opID, project, path string, ifMatch uint64) (tracker.WriteResult, error)
}

// ObjectStore is the fleet's object store as these tools need it.
type ObjectStore interface {
	Put(ctx context.Context, h objstore.Hash, data []byte) (int, error)
	ReadAt(ctx context.Context, m objstore.Manifest, off, n int64) ([]byte, error)
}

// The read page.
//
// THIRTY-TWO KIBIBYTES BY DEFAULT AND FORTY-EIGHT AT MOST: the answer carries
// the page beside a dozen fields of metadata, and base64 grows a page by a
// third, so forty-eight is what keeps the largest answer inside
// [ToolAnswerBytes] with room for the envelope.
const (
	defaultFileRead = 32 << 10
	maxFileRead     = 48 << 10
)

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
	return tracker.ProjectKey(project), argString(args, "path"), ""
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

func (t *listProjectFiles) Name() string { return tracker.ListProjectFilesTool }

func (t *listProjectFiles) Description() string {
	return "List the files kept in a project — reports, specs, notes and " +
		"anything else its work produced — in path order, a page at a time. " +
		"Read one with read_project_file."
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
				"type":        "integer",
				"description": fmt.Sprintf("Files per page, at most %d.", tracker.MaxFilePage),
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
		return notInATurn(t.Name()), nil
	}
	if t.deps.Files == nil {
		return unconfigured(t.Name()), nil
	}
	project, _, refusal := t.deps.fileArgs(&actor, args)
	if refusal != "" {
		return failed(t.Name() + ": " + refusal), nil
	}
	listing, err := t.deps.Files.Files(ctx, tracker.FileQuery{
		Project: project, Folder: argString(args, "folder"),
		After: argString(args, "after"), Limit: argInt(args, "limit", 0),
		Freshness: statelog.Freshness{Level: seatReadLevel},
	})
	if err != nil {
		return failed(fileReadFailure(t.Name(), project, "", err)), nil
	}
	return jsonResult(map[string]any{
		"project": project, "count": len(listing.Files), "files": listing.Files,
		"next": listing.Next, "read_level": listing.Level, "complete": listing.Complete,
	})
}

type readProjectFile struct{ deps WorkDeps }

var _ tools.Callable = (*readProjectFile)(nil)

func (t *readProjectFile) Name() string { return tracker.ReadProjectFileTool }

func (t *readProjectFile) Description() string {
	return fmt.Sprintf("Read a file kept in a project. Text comes back as "+
		"text, a page at a time — at most %d KiB from `offset`, with "+
		"`next_offset` to continue — and a file that is not text is described "+
		"rather than returned unless you ask for `encoding: base64`.",
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
		return notInATurn(t.Name()), nil
	}
	if t.deps.Files == nil || t.deps.Objects == nil {
		return unconfigured(t.Name()), nil
	}
	project, filePath, refusal := t.deps.fileArgs(&actor, args)
	if refusal != "" {
		return failed(t.Name() + ": " + refusal), nil
	}
	filePath, err = tracker.NormalizeFilePath(filePath)
	if err != nil {
		return failed(fmt.Sprintf("%s: %v", t.Name(), err)), nil
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
		return failed(fileReadFailure(t.Name(), project, filePath, err)), nil
	}
	f := detail.File
	answer := map[string]any{
		"project": f.Project, "path": f.Path, "content_type": f.ContentType,
		"size": f.Size, "version": f.Version, "hash": f.Hash,
		"updated_by": f.UpdatedBy, "updated_at": f.UpdatedAt, "offset": offset,
	}
	page, err := t.deps.Objects.ReadAt(ctx, f.Manifest(), offset, int64(limit))
	if err != nil {
		return failed(fmt.Sprintf("%s: %s in %s exists, and its content could not "+
			"be read right now (%v). The file is NOT empty or missing — try again, "+
			"or say you could not read it.", t.Name(), f.Path, f.Project, err)), nil
	}
	if encoding == "base64" {
		answer["encoding"], answer["content"] = "base64", base64.StdEncoding.EncodeToString(page)
		return jsonResult(withNext(answer, offset, int64(len(page)), f.Size))
	}
	text, whole := textOf(page, offset+int64(len(page)) == f.Size)
	if !whole {
		answer["encoding"] = "binary"
		answer["note"] = "this is not text; read it with `encoding: base64` to get its bytes"
		return jsonResult(answer)
	}
	answer["encoding"], answer["content"] = "text", text
	return jsonResult(withNext(answer, offset, int64(len(text)), f.Size))
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

// fileReadFailure explains a file read that could not be served, keeping a
// file that is not there apart from a read that failed — the second must never
// read as the first.
func fileReadFailure(tool, project, filePath string, err error) string {
	switch {
	case errors.Is(err, tracker.ErrNoProject):
		return fmt.Sprintf("%s: there is no project %s. List them with %s.", tool,
			project, tracker.ListProjectsTool)
	case errors.Is(err, tracker.ErrNoFile):
		return fmt.Sprintf("%s: there is no file %s in %s. List them with %s.", tool,
			filePath, project, tracker.ListProjectFilesTool)
	}
	return readFailure(tool, err)
}

type writeProjectFile struct{ deps WorkDeps }

var _ tools.Callable = (*writeProjectFile)(nil)

func (t *writeProjectFile) Name() string { return tracker.WriteProjectFileTool }

func (t *writeProjectFile) Description() string {
	return "Write a file into a project, creating it or replacing what is " +
		"there. Use it for what the work produced — the report, the plan, the " +
		"notes — so everybody on the project finds it in one place. Pass the " +
		"`version` read_project_file gave you as `if_version` to refuse the " +
		"write if somebody changed the file since."
}

func (t *writeProjectFile) Parameters() map[string]any {
	return t.deps.operationParam(map[string]any{
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
		return notInATurn(t.Name()), nil
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
	filePath, err := tracker.NormalizeFilePath(rawPath)
	if err != nil {
		return failed(fmt.Sprintf("%s: %v", t.Name(), err)), nil
	}
	content := []byte(argString(args, "content"))
	encoding := strings.TrimSpace(argString(args, "encoding"))
	switch encoding {
	case "", "text":
		encoding = "text"
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
	// THE BYTES FIRST — see the file's head.
	manifest, err := objstore.Split(ctx, bytes.NewReader(content), tracker.MaxFileBytes,
		func(ctx context.Context, c objstore.Chunk, data []byte) error {
			_, putErr := t.deps.Objects.Put(ctx, c.Hash, data)
			return putErr
		})
	if err != nil {
		return failed(fmt.Sprintf("%s: %s was not written, and nothing was recorded: "+
			"its content could not be stored (%v)", t.Name(), filePath, err)), nil
	}
	opID := opIDFor(actor, t.Name(), "file", project+"/"+filePath, args)
	result, err := t.deps.FileWriter(actor).PutFile(ctx, opID, tracker.FilePut{
		Project: project, Path: filePath, ContentType: contentType, Manifest: manifest,
		IfMatch: uint64(max(argInt(args, "if_version", 0), 0)),
	})
	if err != nil {
		return failed(writeFailure(actor, t.Name(), err)), nil
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return failed(unknownWrite(actor, t.Name(),
			fmt.Sprintf("%s was written to %s", filePath, project), opID,
			result.Unvouched, unknownNext(result.Unvouched, sameCall(actor, t.Name()),
				fmt.Sprintf("Read %s in %s with %s", filePath, project, tracker.ReadProjectFileTool),
				"it writes the same content again, which changes nothing but the version"))), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(withOperation(map[string]any{
		"project": project, "path": filePath, "size": manifest.Size, "hash": manifest.Hash,
		"content_type": contentType, "outcome": string(result.Outcome),
		"position": positionOf(result.Position), "version": result.Version,
	}, actor))
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

func (t *removeProjectFile) Name() string { return tracker.RemoveProjectFileTool }

func (t *removeProjectFile) Description() string {
	return "Remove a file from a project. Its history says who removed it and " +
		"when; its content is deleted from storage, so write it again to bring " +
		"it back."
}

func (t *removeProjectFile) Parameters() map[string]any {
	return t.deps.operationParam(map[string]any{
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
		return notInATurn(t.Name()), nil
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
	filePath, err := tracker.NormalizeFilePath(rawPath)
	if err != nil {
		return failed(fmt.Sprintf("%s: %v", t.Name(), err)), nil
	}
	opID := opIDFor(actor, t.Name(), "file-remove", project+"/"+filePath, args)
	result, err := t.deps.FileWriter(actor).RemoveFile(ctx, opID, project, filePath,
		uint64(max(argInt(args, "if_version", 0), 0)))
	switch {
	case errors.Is(err, tracker.ErrNoFile):
		return failed(fileReadFailure(t.Name(), project, filePath, err)), nil
	case err != nil:
		return failed(writeFailure(actor, t.Name(), err)), nil
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return failed(unknownWrite(actor, t.Name(),
			fmt.Sprintf("%s was removed from %s", filePath, project), opID,
			result.Unvouched, unknownNext(result.Unvouched, sameCall(actor, t.Name()),
				fmt.Sprintf("List %s's files with %s", project, tracker.ListProjectFilesTool),
				"it is refused, because the file is already gone"))), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(withOperation(map[string]any{
		"project": project, "path": filePath, "outcome": string(result.Outcome),
		"position": positionOf(result.Position),
	}, actor))
}
