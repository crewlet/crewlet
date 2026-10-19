package artifactsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/api/paced"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// HOW BYTES BECOME A VERSION, over HTTP: an upload intent decided before any
// byte moves, the engine-streamed transfer leg, then the create or the publish
// that names the upload, every one through the one pipeline
// (internal/artifactupload) the agent tools and the CLI take too.

// MaxBodyBytes bounds one JSON request body on this surface: a publish's file
// list and its edits. An edit's text cannot match in a file larger than the
// largest entry ([artifacts.MaxEntryBytes]), so a body past it carries no edit
// set that could apply; everything else a body names is a few hundred bytes a
// file.
const MaxBodyBytes = artifacts.MaxEntryBytes

// decode reads a JSON body of at most [MaxBodyBytes] into out, refusing a
// field the route does not take: a typo would otherwise read as a request for
// less. An absent body decodes as nothing where empty allows it.
func decode(w http.ResponseWriter, r *http.Request, out any, empty bool) bool {
	return decodeWithin(w, r, out, empty, MaxBodyBytes)
}

// decodeWithin is [decode] for a route whose body is bounded tighter than a
// publish's: a body past limit is refused before it is read whole.
func decodeWithin(w http.ResponseWriter, r *http.Request, out any, empty bool, limit int64) bool {
	body, err := httpjson.ReadBody(w, r, limit)
	if err != nil {
		httpjson.Refuse(w, err)
		return false
	}
	if empty && len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
		return false
	}
	return true
}

// ---- upload intents ------------------------------------------------------ //

// creatorCall is the caller context of a create: no artifact yet, so the
// creator holds owner on what they make.
func (s *Service) creatorCall(p iam.Principal, key, workItem string) artifactsdomain.Call {
	return artifactsdomain.Call{
		Authority: artifacts.Authority{
			Actor: iam.ActorFor(p), Role: artifacts.RoleOwner, Limits: s.gate.limits(),
			Origin: originOf(p), WorkItem: workItem,
		},
		OpKey: key,
	}
}

// intendCreate answers an upload intent for a new artifact (`POST
// /artifacts/uploads`), decided by `create_artifact` where it is mounted.
func (s *Service) intendCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionArtifactCreate)
	if !ok {
		return
	}
	var req artifacts.UploadRequest
	if !decode(w, r, &req, false) {
		return
	}
	if req.Asset {
		invalid(w, "an asset upload names its artifact: POST /artifacts/{id}/uploads")
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	pipe, ok := s.pipeline(w, r)
	if !ok {
		return
	}
	intent, err := pipe.Intend(r.Context(), s.creatorCall(p, key, ""), artifacts.ID{}, req)
	if err != nil {
		writeFailed(w, r, key, "begin the upload", err)
		return
	}
	httpjson.Write(w, http.StatusCreated, intentAnswer{UploadIntent: intent, OpID: key})
}

// intentAnswer is an upload intent and the operation it was decided under.
type intentAnswer struct {
	artifacts.UploadIntent
	OpID string `json:"op_id"`
}

// intendPublish answers an upload intent for an artifact's next version, or
// into its asset store (`POST /artifacts/{id}/uploads`): `publish_artifact`
// for a version, `upload_artifact_asset` for assets, each on the artifact.
func (s *Service) intendPublish(w http.ResponseWriter, r *http.Request) {
	var req artifacts.UploadRequest
	if _, ok := pathID(w, r); !ok {
		return
	}
	if !decode(w, r, &req, false) {
		return
	}
	action := authz.ActionArtifactPublish
	if req.Asset {
		action = authz.ActionArtifactAsset
	}
	a, ok := s.admit(w, r, action)
	if !ok {
		return
	}
	if a.Head.Status == artifactsdomain.StatusTrashed {
		trashed(w)
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	c, err := a.call(originOf(a.Principal), s.gate.limits(), key, "")
	if err != nil {
		unavailable(w, err, "this node could not settle your role on the artifact")
		return
	}
	pipe, ok := s.pipeline(w, r)
	if !ok {
		return
	}
	intent, err := pipe.Intend(r.Context(), c, a.Head.ID, req)
	if err != nil {
		writeFailed(w, r, key, "begin the upload", err)
		return
	}
	httpjson.Write(w, http.StatusCreated, intentAnswer{UploadIntent: intent, OpID: key})
}

// transfer is the engine-streamed leg (`PUT
// /artifacts/uploads/{upload}/files/{path...}`): the body is one declared
// file's bytes, put under the key its intent minted, paced as it arrives.
//
// THE INTENT'S OWN VERB, DECIDED AGAIN: the upload names what it is for, and
// the caller must still hold that verb on it now (a create, the artifact's
// publish, or its assets), so a share revoked since the intent stops the
// bytes.
//
// NO OPERATION KEY: a client follows the transfer the intent handed it
// verbatim, and the upload's record makes the put idempotent by itself (a
// file is claimed before a byte is read, and a second put of it is refused
// `upload_consumed`).
func (s *Service) transfer(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionArtifactPublish)
	if !ok {
		return
	}
	pipe, ok := s.pipeline(w, r)
	if !ok {
		return
	}
	upload := r.PathValue("upload")
	lookup := s.creatorCall(p, "", "")
	u, err := pipe.Lookup(r.Context(), lookup, upload)
	if err != nil {
		writeFailed(w, r, "", "transfer the file", err)
		return
	}
	c := lookup
	if u.Artifact.IsZero() {
		d := authz.Decide(r.Context(), p, authz.ActionArtifactCreate,
			authz.Object{Kind: authz.KindArtifact}, s.gate.Chart, s.gate.decidedAt(r.Context()))
		if d.Unknown() || !d.Allowed {
			operator.RefuseDecision(w, r, authz.ActionArtifactCreate, d)
			return
		}
	} else {
		action := authz.ActionArtifactPublish
		if u.Asset {
			action = authz.ActionArtifactAsset
		}
		a, err := s.gate.Admit(r.Context(), p, action, u.Artifact)
		if err != nil {
			refuseAdmission(w, r, action, err)
			return
		}
		if c, err = a.call(originOf(p), s.gate.limits(), "", ""); err != nil {
			unavailable(w, err, "this node could not settle your role on the artifact")
			return
		}
	}
	size := r.ContentLength
	if size < 0 {
		size = -1
	}
	body := paced.NewBody(r.Body, http.NewResponseController(w), s.pace)
	if err := pipe.Transfer(r.Context(), c, upload, r.PathValue("path"), body, size); err != nil {
		if errors.Is(err, paced.ErrBody) {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeUnreadableBody,
				map[string]string{"detail": err.Error()})
			return
		}
		writeFailed(w, r, "", "transfer the file", err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"upload": upload, "path": r.PathValue("path"),
		"stored": true})
}

// ---- create and publish -------------------------------------------------- //

// shareInput is one audience and the role it is granted: a share as a caller
// asks for it, whose grantor the record names.
type shareInput struct {
	Audience artifacts.Audience `json:"audience"`
	Role     artifacts.Role     `json:"role"`
}

// createBody is a new artifact and its first version.
type createBody struct {
	Kind         artifacts.Kind            `json:"kind"`
	Title        string                    `json:"title"`
	Description  string                    `json:"description,omitempty"`
	Icon         string                    `json:"icon,omitempty"`
	SourceKey    string                    `json:"source_key,omitempty"`
	Upload       string                    `json:"upload,omitempty"`
	Files        []artifactupload.FileSpec `json:"files,omitempty"`
	Entry        string                    `json:"entry,omitempty"`
	Label        string                    `json:"label,omitempty"`
	Contract     string                    `json:"contract,omitempty"`
	Capabilities []artifacts.Capability    `json:"capabilities,omitempty"`
	Shares       []shareInput              `json:"shares,omitempty"`

	// Starter begins the artifact from its kind's starter content, which the
	// engine holds (a deck's example slides); it names no upload or files.
	Starter bool `json:"starter,omitempty"`

	// Doc is a doc's starter as its writer already wrote it (`{"body": ...}`),
	// sent with Starter.
	Doc *artifactupload.DocStarter `json:"doc,omitempty"`

	// Deck is the plan a slides starter is composed from: its theme and the
	// slides it opens with ([slideformat.Plan]). Absent is the default plan.
	Deck *slideformat.Plan `json:"deck,omitempty"`

	// Layout is a design's starter: the boards and notes the person laid
	// out, each board published as a placeholder page of its size.
	Layout *canvasformat.Layout `json:"layout,omitempty"`

	// WorkItem is the tracker item the artifact is made for, which lists it
	// there. It confers nothing.
	WorkItem string `json:"work_item,omitempty"`

	// Folder is the folder the artifact is created in, by id; absent is the
	// top level. Filing into it asks contributor on it.
	Folder string `json:"folder,omitempty"`
}

// publishedAnswer is a version a create or a publish landed.
type publishedAnswer struct {
	Artifact  artifacts.ID    `json:"artifact"`
	Label     artifacts.Label `json:"label"`
	Number    int             `json:"number"`
	VersionID string          `json:"version_id"`
	Warnings  []lint.Finding  `json:"warnings,omitempty"`

	// Files counts the version's files by where their bytes came from, which
	// only the manifest that landed can say: a file the upload sent may
	// still be one the artifact held ([artifactupload.FileCounts]).
	Files *artifactupload.FileCounts `json:"files,omitempty"`

	OpID     string            `json:"op_id"`
	Outcome  statelog.Outcome  `json:"outcome"`
	Position statelog.Position `json:"position"`
}

// published answers a version that landed: 201 for a create applied here, 200
// for a publish, 202 for a version durable and not yet applied on this node,
// 503 carrying the operation for one nobody can account for.
func published(w http.ResponseWriter, key string, out artifactsdomain.Published,
	report artifactupload.Report, created bool) {

	answer := publishedAnswer{Artifact: out.Artifact, Label: out.Artifact.Label(), Number: out.Number,
		VersionID: out.VersionID.String(), OpID: key, Outcome: out.Result.Outcome,
		Position: out.Result.Position, Warnings: report.Warnings, Files: report.Files}
	switch out.Result.Outcome {
	case statelog.OutcomeUnknown:
		written(w, key, out.Result, httpjson.Detail{"artifact": out.Artifact.String()})
	case statelog.OutcomePending:
		httpjson.Write(w, http.StatusAccepted, answer)
	default:
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		httpjson.Write(w, status, answer)
	}
}

// create makes an artifact and its first version (`POST /artifacts`), owned
// by the caller, decided by `create_artifact` where it is mounted.
func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionArtifactCreate)
	if !ok {
		return
	}
	var body createBody
	if !decode(w, r, &body, false) {
		return
	}
	item, err := checkWorkItem(body.WorkItem)
	if err != nil {
		invalid(w, err.Error())
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	pipe, ok := s.pipeline(w, r)
	if !ok {
		return
	}
	shares := make([]artifacts.Share, len(body.Shares))
	audiences := make([]artifacts.Audience, len(body.Shares))
	for i, sh := range body.Shares {
		shares[i] = artifacts.Share{Audience: sh.Audience, Role: sh.Role}
		audiences[i] = sh.Audience
	}
	if !s.audiencesKnown(w, r, audiences...) {
		return
	}
	if body.Folder != "" {
		if _, err = s.gate.AdmitFolder(r.Context(), s.reads, p, authz.ActionArtifactFolderFile,
			body.Folder); err != nil {
			refuseFolderAdmission(w, r, authz.ActionArtifactFolderFile, err, false)
			return
		}
	}
	actor := iam.ActorFor(p)
	out, report, err := pipe.Create(r.Context(), s.creatorCall(p, key, item), artifactupload.CreateRequest{
		Kind: body.Kind, Title: body.Title, Description: body.Description, Icon: body.Icon,
		Owner: actor.Name, OwnerKind: actor.Kind, SourceKey: body.SourceKey, Shares: shares,
		Folder: body.Folder,
		Upload: body.Upload, Files: body.Files, Entry: body.Entry, Label: body.Label,
		Contract: body.Contract, Capabilities: body.Capabilities, Starter: body.Starter,
		Doc: body.Doc, Deck: body.Deck, Layout: body.Layout,
	})
	if err != nil {
		writeFailed(w, r, key, "create the artifact", err)
		return
	}
	published(w, key, out, report, true)
}

// publishBody is one new version of an artifact.
type publishBody struct {
	Upload       string                    `json:"upload,omitempty"`
	Mode         manifest.Mode             `json:"mode,omitempty"`
	Files        []artifactupload.FileSpec `json:"files,omitempty"`
	Edits        []edits.Edit              `json:"edits,omitempty"`
	Entry        string                    `json:"entry,omitempty"`
	Label        string                    `json:"label,omitempty"`
	Contract     string                    `json:"contract,omitempty"`
	Capabilities []artifacts.Capability    `json:"capabilities"`

	// Force publishes over a head that moved since the caller read it: a
	// person with presence only (D08), and the version is marked forced.
	Force bool `json:"force,omitempty"`

	// WorkItem is the tracker item the version is made for.
	WorkItem string `json:"work_item,omitempty"`
}

// errNoIfMatch is a publish that names no base and does not force.
var errNoIfMatch = errors.New(`a publish names the version it was based on: send ` +
	`If-Match: "<n>" with the head number you read, or force it`)

// ifMatch is the base version a request's If-Match names: `"<n>"`, the form
// an artifact's head number takes as an ETag. Absent is zero and false.
func ifMatch(w http.ResponseWriter, r *http.Request) (int, bool, bool) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, false, true
	}
	unquoted, err := strconv.Unquote(raw)
	n, convErr := strconv.Atoi(unquoted)
	if err != nil || convErr != nil || n < 1 || strconv.Itoa(n) != unquoted {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadIfMatch, map[string]string{
			"detail": `If-Match carries the artifact's head number as its ETag does: "3"`})
		return 0, false, false
	}
	return n, true, true
}

// publish adds a version to an artifact (`POST /artifacts/{id}/versions`):
// `publish_artifact` on it, and `artifacts.publish.force` beside it for a
// forced one. A publish based on a head that has moved is `412
// version_moved`, carrying the live head and its entry's text to merge onto.
func (s *Service) publish(w http.ResponseWriter, r *http.Request) {
	a, ok := s.admit(w, r, authz.ActionArtifactPublish)
	if !ok {
		return
	}
	var body publishBody
	if !decode(w, r, &body, false) {
		return
	}
	base, named, ok := ifMatch(w, r)
	if !ok {
		return
	}
	if !named && !body.Force {
		httpjson.FailWith(w, http.StatusPreconditionRequired, httpjson.CodeBadIfMatch,
			map[string]string{"detail": errNoIfMatch.Error()})
		return
	}
	item, err := checkWorkItem(body.WorkItem)
	if err != nil {
		invalid(w, err.Error())
		return
	}
	if body.Force {
		if _, err := s.gate.Decide(r.Context(), a.Principal, authz.ActionArtifactForce, a.Head); err != nil {
			refuseAdmission(w, r, authz.ActionArtifactForce, err)
			return
		}
	}
	if a.Head.Status == artifactsdomain.StatusTrashed {
		trashed(w)
		return
	}
	mode := body.Mode
	if mode == "" {
		mode = manifest.ModeWhole
	}
	s.runPublish(w, r, a, item, artifactupload.PublishRequest{
		Artifact: a.Head.ID, Base: base, Force: body.Force, Upload: body.Upload, Mode: mode,
		Files: body.Files, Edits: body.Edits, Entry: body.Entry, Label: body.Label,
		Contract: body.Contract, Capabilities: body.Capabilities,
	})
}

// restoreBody is a rollback's own fields.
type restoreBody struct {
	Label    string `json:"label,omitempty"`
	WorkItem string `json:"work_item,omitempty"`
}

// restoreVersion rolls an artifact back to version n as a new version, every
// entry carried (`POST /artifacts/{id}/versions/{n}/restore`), decided by
// `publish_artifact` and based, like every publish, on the head the caller
// read.
func (s *Service) restoreVersion(w http.ResponseWriter, r *http.Request) {
	a, ok := s.admit(w, r, authz.ActionArtifactPublish)
	if !ok {
		return
	}
	n, ok := pathVersion(w, r)
	if !ok {
		return
	}
	var body restoreBody
	if !decode(w, r, &body, true) {
		return
	}
	base, named, ok := ifMatch(w, r)
	if !ok {
		return
	}
	if !named {
		httpjson.FailWith(w, http.StatusPreconditionRequired, httpjson.CodeBadIfMatch,
			map[string]string{"detail": errNoIfMatch.Error()})
		return
	}
	item, err := checkWorkItem(body.WorkItem)
	if err != nil {
		invalid(w, err.Error())
		return
	}
	if a.Head.Status == artifactsdomain.StatusTrashed {
		trashed(w)
		return
	}
	s.runPublish(w, r, a, item, artifactupload.PublishRequest{
		Artifact: a.Head.ID, Base: base, RestoreFrom: n, Label: body.Label,
	})
}

// runPublish publishes req as the caller a admitted, and answers it.
func (s *Service) runPublish(w http.ResponseWriter, r *http.Request, a Admission, workItem string,
	req artifactupload.PublishRequest) {

	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	c, err := a.call(originOf(a.Principal), s.gate.limits(), key, workItem)
	if err != nil {
		unavailable(w, err, "this node could not settle your role on the artifact")
		return
	}
	pipe, ok := s.pipeline(w, r)
	if !ok {
		return
	}
	out, report, err := pipe.Publish(r.Context(), c, req)
	var moved *artifactsdomain.VersionMoved
	switch {
	case errors.As(err, &moved):
		s.versionMoved(w, r, key, a, moved)
	case err != nil:
		writeFailed(w, r, key, "publish the version", err)
	default:
		published(w, key, out, report, false)
	}
}

// movedEntry is the live entry's text a version_moved answer inlines, so the
// caller merges onto it without a second read.
type movedEntry struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	Text        string `json:"text"`

	// Truncated reports an entry longer than
	// [artifacts.MaxConflictEntryText], whose text is its first part; the
	// source route serves the whole of it.
	Truncated bool `json:"truncated,omitempty"`
}

// versionMoved answers a publish based on a head that has moved: `412`,
// carrying the live head (as the caller is shown it), the live version's
// manifest and, where the entry is text, up to
// [artifacts.MaxConflictEntryText] of it.
//
// THE ANSWER IS A READ OF THE LIVE VERSION, so a publisher only the admin path
// admitted is recorded as reading it before anything of it is sent, exactly
// as a read through [Gate.Content] is; a record that cannot be written answers
// 503 with nothing of the version, and nothing was published either way.
func (s *Service) versionMoved(w http.ResponseWriter, r *http.Request, key string, a Admission,
	moved *artifactsdomain.VersionMoved) {

	if err := s.gate.recordElevated(r.Context(), a, moved.Live.Number); err != nil {
		log.WarnContext(r.Context(), "artifacts_elevated_read_unrecorded", "artifact",
			moved.Head.ID.String(), "version", moved.Live.Number, "error", err.Error())
		unavailable(w, err, "nothing was published: the artifact moved past your base, and "+
			"its live version is shown to you only as an administrator's read, whose record "+
			"could not be written")
		return
	}
	head := shapeHead(moved.Head, a.Role(), a.Elevated())
	// ITS FOLDER ONLY WHERE THE PUBLISHER SEES IT, as every read of a head
	// names it; a folder this node could not decide on is left out, which
	// shows less rather than more.
	shown := folderShowing{g: s.gate, reads: s.reads, p: a.Principal}
	folder, err := shown.folder(r.Context(), head.Folder)
	if err != nil {
		log.WarnContext(r.Context(), "artifacts_moved_folder_undecided", "artifact",
			moved.Head.ID.String(), "error", err.Error())
	}
	head.Folder = folder
	detail := httpjson.Detail{
		"detail": say(moved), "op_id": key,
		"head": head,
		"live": moved.Live,
	}
	if entry, err := s.entryText(r.Context(), moved.Live, artifacts.MaxConflictEntryText); err != nil {
		// THE REFUSAL STANDS WITHOUT THE TEXT: the head and the manifest
		// are what a merge decides on, and the source route reads the
		// entry again.
		log.WarnContext(r.Context(), "artifacts_moved_entry_unread", "artifact",
			moved.Head.ID.String(), "version", moved.Live.Number, "error", err.Error())
		detail["entry_unavailable"] = true
	} else if entry != nil {
		detail["entry"] = entry
	}
	httpjson.FailWithFields(w, http.StatusPreconditionFailed, httpjson.CodeVersionMoved, detail)
}

// errNotText is an entry whose bytes are not text: nothing a merge or a
// dashboard renders as source.
var errNotText = errors.New("artifactsapi: the entry is not a text file")

// entryText is up to limit bytes of version v's entry, read back through the
// object store and checked against its digest when whole; nil for a version
// with no entry (a native sheet) or an entry that is not text.
func (s *Service) entryText(ctx context.Context, v artifactsdomain.Version, limit int64) (*movedEntry, error) {
	entry, ok := entryOf(v)
	if !ok {
		return nil, nil
	}
	t, known := mediatype.For(entry.Path)
	if !known || !t.Text {
		return nil, nil
	}
	text, truncated, err := s.readText(ctx, entry, limit)
	if err != nil {
		return nil, err
	}
	return &movedEntry{Path: entry.Path, ContentType: entry.ContentType, Text: text,
		Truncated: truncated}, nil
}

// entryOf is a version's entry file.
func entryOf(v artifactsdomain.Version) (artifacts.Entry, bool) {
	if v.Manifest.Entry == "" {
		return artifacts.Entry{}, false
	}
	for _, e := range v.Manifest.Files {
		if e.Path == v.Manifest.Entry {
			return e, true
		}
	}
	return artifacts.Entry{}, false
}

// readText reads up to limit bytes of entry e as UTF-8 text. A whole read is
// verified against the entry's digest by the store's own reader; a cut one
// stops on a rune boundary.
func (s *Service) readText(ctx context.Context, e artifacts.Entry, limit int64) (string, bool, error) {
	body, err := s.objects.Open(ctx, e.Object)
	if err != nil {
		return "", false, err
	}
	defer body.Close()
	truncated := e.Object.Size > limit
	var raw []byte
	if truncated {
		raw = make([]byte, limit)
		if _, err := io.ReadFull(body, raw); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return "", false, err
		}
		// A CUT MID-RUNE loses at most the rune's leading bytes.
		for i := 0; i < utf8.UTFMax-1 && len(raw) > 0 && !utf8.Valid(raw); i++ {
			raw = raw[:len(raw)-1]
		}
	} else if raw, err = io.ReadAll(body); err != nil {
		return "", false, err
	}
	if !utf8.Valid(raw) {
		return "", false, errNotText
	}
	return string(raw), truncated, nil
}
