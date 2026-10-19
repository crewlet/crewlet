package artifactsapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FOLDERS: the tree a reader sees, a folder's create, rename, move and
// removal, and filing an artifact (ADR-0047: an artifact lives in one folder,
// and a folder grants nothing).
//
// # Two objects, never one
//
// Whether a caller SEES a folder, and whether they may FILE into it, is
// decided on its top-level folder's owner and shares
// ([artifactsdomain.FolderObject]), so a tree is seen whole or not at all and
// a breadcrumb never names a folder its reader cannot see. Whether they may
// MANAGE it (rename, move, remove, take an artifact out of it) is decided on
// the folder's own owner beside those shares
// ([artifactsdomain.FolderManagerObject]), whose rows ask for owner: its owner,
// the owner's lead, an administrator. Every decision on a folder asks the
// first before anything else, and a caller it does not show the folder is
// answered exactly as for a folder that does not exist.
//
// # Filing asks the artifact and the folder
//
// Filing an artifact is an edit of where it lives: editor on the artifact
// (`move_artifact`) AND contributor on the destination
// (`artifacts.folder.file`), two decisions, both of which must admit. Taking
// an artifact out of a folder to the top level is `move_artifact` too, or the
// folder's manager's `artifacts.folder.unfile`, for an artifact they can read.
// Nothing about the artifact's access changes either way.

// FolderReads is the artifacts estate's folder reads every folder decision and
// the tree read ask: the estate router's.
type FolderReads interface {
	Folders(ctx context.Context, v artifactsdomain.Viewer, fresh statelog.Freshness) (
		artifactsdomain.FolderTree, error)
	Folder(ctx context.Context, id string, fresh statelog.Freshness) (
		artifactsdomain.Folder, statelog.Position, error)
}

// FolderWrites is the artifacts estate's folder writes and an artifact's move.
type FolderWrites interface {
	CreateFolders(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.FolderCreateInput) (
		artifactsdomain.FolderCreated, error)
	RenameFolder(ctx context.Context, c artifactsdomain.Call, id, name string) (artifactsdomain.Written, error)
	MoveFolder(ctx context.Context, c artifactsdomain.Call, id, parent string) (artifactsdomain.Written, error)
	RemoveFolder(ctx context.Context, c artifactsdomain.Call, id string) (artifactsdomain.Written, error)
	Move(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, folder string) (
		artifactsdomain.Written, error)
}

// ErrNoSuchFolder is a folder this caller is answered as absent: one that does
// not exist, one that was removed, and one they cannot see, all alike.
var ErrNoSuchFolder = errors.New("artifactsapi: no such folder")

// FolderRole is what a reader may do with a folder, as the tree tells them: the
// one field a client gates its gestures on, decided by the authority table
// rather than guessed from who owns what.
type FolderRole string

const (
	// FolderRoleOwner manages the folder: renames, moves and removes it,
	// and takes any artifact they can read out of it. Its owner, the
	// owner's lead, or an administrator.
	FolderRoleOwner FolderRole = "owner"

	// FolderRoleContributor files into the folder and creates folders in
	// it.
	FolderRoleContributor FolderRole = "contributor"

	// FolderRoleViewer sees the folder and what in it they can open.
	FolderRoleViewer FolderRole = "viewer"
)

// FolderAnswer is one folder of the tree as a reader is shown it: the folder,
// what sits directly in it that they can see, and their role. Its shares are
// answered to the folder's managers alone, as an artifact's are to its owner
// and editors.
type FolderAnswer struct {
	artifactsdomain.FolderItem
	Role FolderRole `json:"role"`
}

// FoldersAnswer is the tree a reader sees (`artifact_folders`, `GET
// /artifact-folders`).
type FoldersAnswer struct {
	Folders  []FolderAnswer     `json:"folders"`
	TopLevel int                `json:"top_level"`
	Level    statelog.ReadLevel `json:"read_level"`
	Position statelog.Position  `json:"position"`
	Complete bool               `json:"complete"`
}

// managerVerb reports a verb decided on the folder's own owner rather than its
// top-level folder's.
func managerVerb(action authz.Action) bool {
	switch action {
	case authz.ActionArtifactFolderRename, authz.ActionArtifactFolderMove,
		authz.ActionArtifactFolderRemove, authz.ActionArtifactFolderUnfile:
		return true
	}
	return false
}

// FolderAdmission is what a decision on a folder that admitted concluded.
type FolderAdmission struct {
	Folder    artifactsdomain.Folder
	Decision  authz.Decision
	Principal iam.Principal
}

// call is a folder write's caller context: the decision's authority (elevated
// where only the admin path admitted it), the limits in force and the
// request's operation key.
func (a FolderAdmission) call(limits artifacts.Limits, key string) artifactsdomain.Call {
	role := a.Decision.Standing.Role
	if !role.Valid() {
		role = ""
	}
	return artifactsdomain.Call{
		Authority: artifacts.Authority{
			Actor: iam.ActorFor(a.Principal), Role: role, Elevated: a.Decision.Elevated,
			Limits: limits, Origin: originOf(a.Principal),
		},
		OpKey: key,
	}
}

// readFolder is one live folder, answering an absent or removed one as
// [ErrNoSuchFolder].
func (g Gate) readFolder(ctx context.Context, reads FolderReads, id string) (artifactsdomain.Folder, error) {
	f, _, err := reads.Folder(ctx, id, decisionRead)
	switch {
	case errors.Is(err, artifactsdomain.ErrFolderNotFound):
		return artifactsdomain.Folder{}, ErrNoSuchFolder
	case err != nil:
		return artifactsdomain.Folder{}, err
	case f.Status != artifactsdomain.FolderLive:
		return artifactsdomain.Folder{}, ErrNoSuchFolder
	}
	return f, nil
}

// AdmitFolder decides action on folder id for p: [ErrNoSuchFolder] for a folder
// the caller is answered as absent, a [*Refused] for one the table refuses,
// and any other error for one this node could not decide.
func (g Gate) AdmitFolder(ctx context.Context, reads FolderReads, p iam.Principal, action authz.Action,
	id string) (FolderAdmission, error) {

	f, err := g.readFolder(ctx, reads, id)
	if err != nil {
		return FolderAdmission{}, err
	}
	return g.DecideFolder(ctx, p, action, f)
}

// DecideFolder is [Gate.AdmitFolder] on a folder the caller already read. The
// folder must be one the caller sees ([authz.Visible], which the admin path
// never satisfies, so an administrator is never shown a private folder by an
// override nobody recorded); a folder they do not see is [ErrNoSuchFolder].
func (g Gate) DecideFolder(ctx context.Context, p iam.Principal, action authz.Action,
	f artifactsdomain.Folder) (FolderAdmission, error) {

	now, manageOthers := g.now(), g.settings().AgentsManageOthers
	shown, err := authz.Visible(ctx, p, artifactsdomain.FolderObject(f, manageOthers), g.Chart, now)
	switch {
	case err != nil:
		return FolderAdmission{}, err
	case !shown:
		return FolderAdmission{}, ErrNoSuchFolder
	}
	object := artifactsdomain.FolderObject(f, manageOthers)
	if managerVerb(action) {
		object = artifactsdomain.FolderManagerObject(f, manageOthers)
	}
	d := authz.Decide(ctx, p, action, object, g.Chart, now)
	switch {
	case d.Unknown():
		return FolderAdmission{}, d.Err
	case !d.Allowed:
		// SEEN, SO NEVER ANSWERED AS ABSENT: a relation below the verb is
		// a refusal naming what it needs.
		return FolderAdmission{}, &Refused{Action: action, Decision: d}
	}
	return FolderAdmission{Folder: f, Decision: d, Principal: p}, nil
}

// folderRole is the role a reader is shown on a folder they see, from the
// decisions its gestures take.
func (g Gate) folderRole(ctx context.Context, p iam.Principal, f artifactsdomain.Folder) (FolderRole, error) {
	now, manageOthers := g.now(), g.settings().AgentsManageOthers
	manage := authz.Decide(ctx, p, authz.ActionArtifactFolderRename,
		artifactsdomain.FolderManagerObject(f, manageOthers), g.Chart, now)
	switch {
	case manage.Unknown():
		return "", manage.Err
	case manage.Allowed:
		return FolderRoleOwner, nil
	}
	file := authz.Decide(ctx, p, authz.ActionArtifactFolderFile,
		artifactsdomain.FolderObject(f, manageOthers), g.Chart, now)
	switch {
	case file.Unknown():
		return "", file.Err
	case file.Allowed:
		return FolderRoleContributor, nil
	}
	return FolderRoleViewer, nil
}

// shapeFolder is a folder as p is shown it: its shares to its managers alone.
func shapeFolder(f artifactsdomain.Folder, role FolderRole) artifactsdomain.Folder {
	if role != FolderRoleOwner {
		f.Shares = nil
	}
	return f
}

// Folders answers the tree p sees: every folder the estate's filter keeps that
// the authority table shows p, each with p's role, and how many artifacts p
// can see that sit in no folder they see.
func (g Gate) Folders(ctx context.Context, reads FolderReads, p iam.Principal, fresh statelog.Freshness) (
	FoldersAnswer, error) {

	v, err := g.viewerFor(ctx, p)
	if err != nil {
		return FoldersAnswer{}, err
	}
	tree, err := reads.Folders(ctx, v, fresh)
	if err != nil {
		return FoldersAnswer{}, err
	}
	out := FoldersAnswer{Folders: make([]FolderAnswer, 0, len(tree.Folders)), TopLevel: tree.TopLevel,
		Level: tree.Level, Position: tree.Position, Complete: tree.Complete}
	now, manageOthers := g.now(), g.settings().AgentsManageOthers
	for _, item := range tree.Folders {
		shown, err := authz.Visible(ctx, p, artifactsdomain.FolderObject(item.Folder, manageOthers), g.Chart, now)
		if err != nil {
			return FoldersAnswer{}, err
		}
		if !shown {
			continue
		}
		role, err := g.folderRole(ctx, p, item.Folder)
		if err != nil {
			return FoldersAnswer{}, err
		}
		item.Folder = shapeFolder(item.Folder, role)
		out.Folders = append(out.Folders, FolderAnswer{FolderItem: item, Role: role})
	}
	return out, nil
}

// refuseFolderAdmission renders a folder decision that did not admit: the
// folder a path names answered as `not_found` (byte for byte an unknown id's),
// a folder a body names as `folder_not_found`.
func refuseFolderAdmission(w http.ResponseWriter, r *http.Request, action authz.Action, err error, inPath bool) {
	var refused *Refused
	switch {
	case errors.Is(err, ErrNoSuchFolder) && inPath:
		notFound(w)
	case errors.Is(err, ErrNoSuchFolder):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeFolderNotFound)
	case errors.As(err, &refused):
		refuseFolderDecision(w, r, action, refused.Decision)
	default:
		unavailable(w, err, "this node could not decide who may act on this folder")
	}
}

// refuseFolderDecision renders a refusal of the authority table on a folder:
// for a caller whose relation is below the verb's role, the role named.
func refuseFolderDecision(w http.ResponseWriter, r *http.Request, action authz.Action, d authz.Decision) {
	if d.Err == nil && d.Reason == authz.ReasonRole && d.Role.Valid() {
		detail := authz.RefusalDetail(d.Reason, d.Grants)
		detail["role"] = string(d.Role)
		detail["detail"] = folderRoleSentence(d.Role)
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeUnauthorized, detail)
		return
	}
	operator.RefuseDecision(w, r, action, d)
}

// folderRoleSentence says whom to ask for the role a folder verb needs.
func folderRoleSentence(role artifacts.Role) string {
	if role == artifacts.RoleOwner {
		return "only the folder's owner, their lead or an administrator can do this"
	}
	return "this needs the " + string(role) + " role on the folder; its top-level folder's " +
		"owner decides who has it"
}

// pathFolder is the folder id the path names.
func pathFolder(r *http.Request) string { return r.PathValue("id") }

// folderCreateBody is one folder, or a path of them with its missing parents.
type folderCreateBody struct {
	// Name is one folder's name; Path is a path of names separated by "/",
	// created with its missing parents when Parents is set. Exactly one.
	Name    string `json:"name,omitempty"`
	Path    string `json:"path,omitempty"`
	Parents bool   `json:"parents,omitempty"`

	// Parent is the folder it is created in, by id; absent is the top
	// level, where Visibility is required and a person creates it.
	Parent     string                           `json:"parent,omitempty"`
	Visibility artifactsdomain.FolderVisibility `json:"visibility,omitempty"`
}

// names is the chain a create body names.
func (b folderCreateBody) names() ([]string, error) {
	switch {
	case (b.Name == "") == (b.Path == ""):
		return nil, errors.New("name one folder (`name`) or a path of folders (`path`), not both")
	case b.Name != "":
		return []string{b.Name}, nil
	}
	return strings.Split(strings.TrimSuffix(b.Path, artifactsdomain.ListDelimiter),
		artifactsdomain.ListDelimiter), nil
}

// foldersCreatedAnswer is what a folder create answers.
type foldersCreatedAnswer struct {
	Folders  []artifactsdomain.Folder `json:"folders"`
	Created  int                      `json:"created"`
	OpID     string                   `json:"op_id"`
	Outcome  statelog.Outcome         `json:"outcome"`
	Position statelog.Position        `json:"position"`
}

// createFolder creates one folder, or a path with its missing parents (`POST
// /artifact-folders`), decided by `create_artifact_folder` where it is mounted
// and by `artifacts.folder.file` on the parent. A top-level folder is a
// person's, and states its visibility (the writer refuses an agent's).
func (s *Service) createFolder(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionArtifactFolderCreate)
	if !ok {
		return
	}
	var body folderCreateBody
	if !decodeWithin(w, r, &body, false, maxFolderBodyBytes) {
		return
	}
	names, err := body.names()
	if err != nil {
		invalid(w, err.Error())
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	c := s.creatorCall(p, key, "")
	if body.Parent != "" {
		a, admitErr := s.gate.AdmitFolder(r.Context(), s.reads, p, authz.ActionArtifactFolderFile, body.Parent)
		if admitErr != nil {
			refuseFolderAdmission(w, r, authz.ActionArtifactFolderFile, admitErr, false)
			return
		}
		c = a.call(s.gate.limits(), key)
	}
	actor := iam.ActorFor(p)
	out, err := s.writes.CreateFolders(r.Context(), c, artifactsdomain.FolderCreateInput{
		Parent: body.Parent, Names: names, Parents: body.Parents, Visibility: body.Visibility,
		Owner: actor.Name, OwnerKind: actor.Kind,
	})
	if err != nil {
		writeFailed(w, r, key, "create the folder", err)
		return
	}
	answer := foldersCreatedAnswer{Folders: make([]artifactsdomain.Folder, 0, len(out.Folders)),
		Created: out.Created, OpID: key, Outcome: out.Result.Outcome, Position: out.Result.Position}
	for _, f := range out.Folders {
		role, roleErr := s.gate.folderRole(r.Context(), p, f)
		if roleErr != nil {
			unavailable(w, roleErr, "the folders were created, and this node could not decide what "+
				"you may do with them; read the tree again")
			return
		}
		answer.Folders = append(answer.Folders, shapeFolder(f, role))
	}
	switch {
	case out.Result.Outcome == statelog.OutcomeUnknown:
		written(w, key, out.Result, nil)
	case out.Result.Outcome == statelog.OutcomePending:
		httpjson.Write(w, http.StatusAccepted, answer)
	case out.Created > 0:
		httpjson.Write(w, http.StatusCreated, answer)
	default:
		httpjson.Write(w, http.StatusOK, answer)
	}
}

// maxFolderBodyBytes bounds a folder route's body: a path of
// [artifacts.MaxFolderDepth] names of [artifacts.MaxFolderNameBytes] each, a
// parent id and a few words, with room for JSON escaping, rounded up.
const maxFolderBodyBytes = 4 << 10

// folderPatchBody renames a folder, moves it, or both. A move that would make
// the folder visible to somebody who cannot see it now is confirmed.
type folderPatchBody struct {
	Name         *string `json:"name,omitempty"`
	Parent       *string `json:"parent,omitempty"`
	ConfirmWiden bool    `json:"confirm_widen,omitempty"`
}

// patchFolder renames or moves a folder (`PATCH /artifact-folders/{id}`):
// `artifacts.folder.rename` or `artifacts.folder.move` on it, and
// `artifacts.folder.file` on the new parent. A move into a tree whose audience
// reaches somebody the folder's present one does not is `409 folder_widens`
// until it is confirmed.
//
// ONE GESTURE A REQUEST. A rename and a move are two records, and a body
// naming both could leave the rename applied under a move the domain then
// refused (a name taken at the destination, the depth, the limit, a cycle),
// which no decision taken here beforehand can rule out. So a body names one
// or the other, and every decision is taken before its one write.
func (s *Service) patchFolder(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionArtifactFolderRename)
	if !ok {
		return
	}
	var body folderPatchBody
	if !decodeWithin(w, r, &body, false, maxFolderBodyBytes) {
		return
	}
	switch {
	case body.Name == nil && body.Parent == nil:
		invalid(w, "name the folder's new name or its new parent")
		return
	case body.Name != nil && body.Parent != nil:
		invalid(w, "a rename and a move are two requests: rename the folder, then move it")
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	ctx, id := r.Context(), pathFolder(r)
	var rename, move FolderAdmission
	if body.Name != nil {
		a, err := s.gate.AdmitFolder(ctx, s.reads, p, authz.ActionArtifactFolderRename, id)
		if err != nil {
			refuseFolderAdmission(w, r, authz.ActionArtifactFolderRename, err, true)
			return
		}
		rename = a
	}
	if body.Parent != nil {
		a, err := s.gate.AdmitFolder(ctx, s.reads, p, authz.ActionArtifactFolderMove, id)
		if err != nil {
			refuseFolderAdmission(w, r, authz.ActionArtifactFolderMove, err, true)
			return
		}
		into, ok := s.moveDestination(w, r, p, a.Folder, *body.Parent)
		if !ok {
			return
		}
		if !body.ConfirmWiden && widens(a.Folder, into) {
			httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeFolderWidens, httpjson.Detail{
				"detail": "moving it there makes this folder and the folders in it visible by name to " +
					"people who cannot see them now (the artifacts in them keep their own access); " +
					"send confirm_widen to move it anyway",
				"op_id": key})
			return
		}
		move = a
	}
	var res statelog.Result
	if body.Name != nil {
		out, err := s.writes.RenameFolder(ctx, rename.call(s.gate.limits(), key), id, *body.Name)
		if err != nil {
			writeFailed(w, r, key, "rename the folder", err)
			return
		}
		res = out.Result
	}
	if body.Parent != nil {
		out, err := s.writes.MoveFolder(ctx, move.call(s.gate.limits(), key), id, *body.Parent)
		if err != nil {
			writeFailed(w, r, key, "move the folder", err)
			return
		}
		res = out.Result
	}
	s.folderPatched(w, r, p, key, id, res)
}

// audience is who sees a tree: its top-level folder's owner and shares.
type audience struct {
	owner  string
	shares []artifacts.Share
}

// moveDestination is the audience a folder moved under parent sees it with,
// decided by `artifacts.folder.file` on a new parent, and the audience it has
// now for a move to the top level (which keeps it). It renders a refusal
// itself and reports whether the handler goes on.
func (s *Service) moveDestination(w http.ResponseWriter, r *http.Request, p iam.Principal,
	f artifactsdomain.Folder, parent string) (audience, bool) {

	if parent == "" {
		return audience{owner: f.Owner, shares: f.Shares}, true
	}
	a, err := s.gate.AdmitFolder(r.Context(), s.reads, p, authz.ActionArtifactFolderFile, parent)
	if err != nil {
		refuseFolderAdmission(w, r, authz.ActionArtifactFolderFile, err, false)
		return audience{}, false
	}
	return audience{owner: a.Folder.RootOwner, shares: a.Folder.Shares}, true
}

// widens reports whether a folder now seen by its tree's audience would, under
// into, be seen by somebody that audience does not reach: the company where it
// was not shared with the company, or an owner or an audience the present one
// does not name. A tree shared with the whole company already reaches
// everybody, so nothing widens it.
func widens(f artifactsdomain.Folder, into audience) bool {
	now := audience{owner: f.RootOwner, shares: f.Shares}
	reaches := func(a audience, target artifacts.Audience) bool {
		return slices.ContainsFunc(a.shares, func(s artifacts.Share) bool { return s.Audience == target }) ||
			(target.Kind == artifacts.AudienceSeat && target.ID == a.owner)
	}
	company := artifacts.Audience{Kind: artifacts.AudienceCompany}
	switch {
	case reaches(now, company):
		return false
	case reaches(into, company):
		return true
	case !reaches(now, artifacts.Audience{Kind: artifacts.AudienceSeat, ID: into.owner}):
		return true
	}
	for _, s := range into.shares {
		if !reaches(now, s.Audience) {
			return true
		}
	}
	return false
}

// folderPatchedAnswer is what a rename or a move answers.
type folderPatchedAnswer struct {
	Folder   *artifactsdomain.Folder `json:"folder,omitempty"`
	OpID     string                  `json:"op_id"`
	Outcome  statelog.Outcome        `json:"outcome"`
	Position statelog.Position       `json:"position"`
}

// folderPatched answers a rename or a move with the folder as it is now, read
// at least as far as the write, where the write applied here.
func (s *Service) folderPatched(w http.ResponseWriter, r *http.Request, p iam.Principal, key, id string,
	res statelog.Result) {

	if res.Outcome == statelog.OutcomeUnknown {
		written(w, key, res, httpjson.Detail{"folder": id})
		return
	}
	answer := folderPatchedAnswer{OpID: key, Outcome: res.Outcome, Position: res.Position}
	status := http.StatusOK
	if res.Outcome == statelog.OutcomePending {
		status = http.StatusAccepted
	} else {
		fresh := decisionRead
		fresh.MinPosition = res.Position
		f, _, err := s.reads.Folder(r.Context(), id, fresh)
		if err == nil {
			if role, err := s.gate.folderRole(r.Context(), p, f); err == nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				shaped := shapeFolder(f, role)
				answer.Folder = &shaped
			}
		}
	}
	httpjson.Write(w, status, answer)
}

// removeFolder removes a folder (`DELETE /artifact-folders/{id}`), decided by
// `artifacts.folder.remove`, a person's: everything in it moves up to its
// parent, nothing is trashed and nobody loses access. The answer carries no
// counts: a count of what moved would include artifacts the remover cannot
// see.
func (s *Service) removeFolder(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionArtifactFolderRemove)
	if !ok {
		return
	}
	id := pathFolder(r)
	a, err := s.gate.AdmitFolder(r.Context(), s.reads, p, authz.ActionArtifactFolderRemove, id)
	if err != nil {
		refuseFolderAdmission(w, r, authz.ActionArtifactFolderRemove, err, true)
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	out, err := s.writes.RemoveFolder(r.Context(), a.call(s.gate.limits(), key), id)
	if err != nil {
		writeFailed(w, r, key, "remove the folder", err)
		return
	}
	written(w, key, out.Result, httpjson.Detail{"folder": id})
}

// moveBody files one artifact; an empty folder is the top level.
type moveBody struct {
	Folder string `json:"folder"`
}

// moveArtifact files one artifact (`PUT /artifacts/{id}/folder`): editor on it
// AND contributor on the destination, or, to the top level, editor on it or
// the source folder's manager.
func (s *Service) moveArtifact(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, ok := caller(w, r, authz.ActionArtifactMove)
	if !ok {
		return
	}
	var body moveBody
	if !decodeWithin(w, r, &body, false, maxFolderBodyBytes) {
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	c, err := s.admitMove(r.Context(), p, id, body.Folder, key)
	if err != nil {
		s.refuseMove(w, r, err)
		return
	}
	out, err := s.writes.Move(r.Context(), c, id, body.Folder)
	if err != nil {
		writeFailed(w, r, key, "move the artifact", err)
		return
	}
	written(w, key, out.Result, httpjson.Detail{"artifact": id.String(), "folder": body.Folder})
}

// moveRefusal is a move's refusal: the verb whose decision refused, and why.
type moveRefusal struct {
	action authz.Action
	err    error
	// folder says the refusal is about the destination folder.
	folder bool
}

func (e *moveRefusal) Error() string { return e.err.Error() }
func (e *moveRefusal) Unwrap() error { return e.err }

// admitMove decides filing artifact id in folder for p and answers the call
// the write is stamped with: [ErrNoSuchArtifact] for an artifact the caller is
// answered as absent, and a [*moveRefusal] for every other decision that did
// not admit.
func (s *Service) admitMove(ctx context.Context, p iam.Principal, id artifacts.ID, folder, key string) (
	artifactsdomain.Call, error) {

	a, err := s.gate.Admit(ctx, p, authz.ActionArtifactMove, id)
	var refused *Refused
	switch {
	case err == nil:
		if folder != "" {
			if _, fileErr := s.gate.AdmitFolder(ctx, s.reads, p, authz.ActionArtifactFolderFile,
				folder); fileErr != nil {
				return artifactsdomain.Call{}, &moveRefusal{action: authz.ActionArtifactFolderFile,
					err: fileErr, folder: true}
			}
		}
		return a.call(originOf(p), s.gate.limits(), key, "")
	case errors.As(err, &refused) && folder == "":
		// TO THE TOP LEVEL, THE FOLDER'S MANAGER may take out anything in
		// it they can read, though they could not edit it.
		read, rerr := s.gate.Read(ctx, p, id, 0)
		if rerr != nil || read.Head.Folder == "" {
			return artifactsdomain.Call{}, &moveRefusal{action: authz.ActionArtifactMove, err: err}
		}
		fa, ferr := s.gate.AdmitFolder(ctx, s.reads, p, authz.ActionArtifactFolderUnfile, read.Head.Folder)
		if ferr != nil {
			return artifactsdomain.Call{}, &moveRefusal{action: authz.ActionArtifactMove, err: err}
		}
		c := fa.call(s.gate.limits(), key)
		c.Authority.Role = read.Role()
		return c, nil
	case errors.As(err, &refused):
		return artifactsdomain.Call{}, &moveRefusal{action: authz.ActionArtifactMove, err: err}
	}
	return artifactsdomain.Call{}, err
}

// refuseMove renders a move's refusal.
func (s *Service) refuseMove(w http.ResponseWriter, r *http.Request, err error) {
	var refusal *moveRefusal
	switch {
	case errors.As(err, &refusal) && refusal.folder:
		refuseFolderAdmission(w, r, refusal.action, refusal.err, false)
	case errors.As(err, &refusal):
		refuseAdmission(w, r, refusal.action, refusal.err)
	default:
		refuseAdmission(w, r, authz.ActionArtifactMove, err)
	}
}

// movesBody files up to [artifacts.MaxMovesPerCall] artifacts.
type movesBody struct {
	Items []moveItem `json:"items"`
}

// moveItem is one artifact and the folder it is filed in.
type moveItem struct {
	Artifact string `json:"artifact"`
	Folder   string `json:"folder"`
}

// MoveResult is one item's outcome in a bulk move: `moved`, or the code of the
// refusal that stopped it, with what to do about it.
type MoveResult struct {
	Artifact string `json:"artifact"`
	Outcome  string `json:"outcome"`
	Message  string `json:"message,omitempty"`
}

// moveMoved is a bulk item that landed (or was already there).
const moveMoved = "moved"

// moveArtifacts files several artifacts (`POST /artifacts/folder-moves`),
// EACH DECIDED AND WRITTEN ALONE, as one move each: one item's refusal is
// named in its result and stops nothing else, and a refusal is never applied
// silently. The request's operation key covers every item: each item's
// operation is derived from it, the artifact and the folder.
func (s *Service) moveArtifacts(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionArtifactMove)
	if !ok {
		return
	}
	var body movesBody
	if !decode(w, r, &body, false) {
		return
	}
	switch {
	case len(body.Items) == 0:
		invalid(w, "name at least one artifact to move")
		return
	case len(body.Items) > artifacts.MaxMovesPerCall:
		invalid(w, fmt.Sprintf("a request moves at most %d artifacts; send the rest in another",
			artifacts.MaxMovesPerCall))
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	results := make([]MoveResult, len(body.Items))
	for i, item := range body.Items {
		results[i] = s.moveOne(r, p, item, key)
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"results": results})
}

// moveOne decides and writes one bulk item.
func (s *Service) moveOne(r *http.Request, p iam.Principal, item moveItem, key string) MoveResult {
	out := MoveResult{Artifact: item.Artifact}
	id, err := artifacts.ParseID(item.Artifact)
	if err != nil {
		out.Outcome, out.Message = string(httpjson.CodeNotFound), "there is no such artifact"
		return out
	}
	c, err := s.admitMove(r.Context(), p, id, item.Folder, key)
	if err == nil {
		var written artifactsdomain.Written
		written, err = s.writes.Move(r.Context(), c, id, item.Folder)
		if err == nil && written.Result.Outcome == statelog.OutcomeUnknown {
			out.Outcome = string(httpjson.CodeUnavailable)
			out.Message = "whether the move landed is unknown; send the request again with the same key"
			return out
		}
	}
	if err == nil {
		out.Outcome = moveMoved
		return out
	}
	out.Outcome, out.Message = moveOutcome(err)
	return out
}

// moveOutcome is a bulk item's refusal as a code and a sentence.
func moveOutcome(err error) (string, string) {
	var (
		refused *Refused
		refusal *moveRefusal
	)
	folder := errors.As(err, &refusal) && refusal.folder
	switch {
	case errors.Is(err, ErrNoSuchArtifact):
		return string(httpjson.CodeNotFound), "there is no such artifact, or it is not shared with you"
	case errors.Is(err, ErrNoSuchFolder), errors.Is(err, artifactsdomain.ErrFolderNotFound):
		return string(httpjson.CodeFolderNotFound), "there is no such folder, or it is not one you can see"
	case errors.As(err, &refused) && folder:
		return string(httpjson.CodeUnauthorized), folderRoleSentence(refused.Decision.Role)
	case errors.As(err, &refused):
		return string(httpjson.CodeUnauthorized), "moving it needs the editor role on the artifact; " +
			"its owner can share it with you at that role"
	case errors.Is(err, artifactsdomain.ErrTrashed):
		return string(httpjson.CodeArtifactTrashed), "it is in the trash; its owner restores it first"
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return string(httpjson.CodeNotFound), "there is no such artifact"
	case errors.Is(err, artifactsdomain.ErrInvalid):
		return string(httpjson.CodeInvalid), say(err)
	}
	return string(httpjson.CodeUnavailable), "this node could not move it right now; send the " +
		"request again with the same key"
}
