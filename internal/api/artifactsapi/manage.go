package artifactsapi

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
)

// THE GESTURES THAT MOVE NO BYTES: renaming, labelling a version, the share
// dialog's grant, revoke and transfer, the trash and its way back, the purge,
// an asset's finalize and removal, and the caller's own pin. Each admits its
// verb on the artifact first and calls the domain's writer with the authority
// it concluded; the writer decides the rest in its own snapshot.

// gesture admits action on the path's artifact, takes the request's operation
// key and builds the call, rendering every refusal itself. It reports whether
// the handler goes on.
func (s *Service) gesture(w http.ResponseWriter, r *http.Request, action authz.Action) (
	Admission, artifactsdomain.Call, string, bool) {

	a, ok := s.admit(w, r, action)
	if !ok {
		return Admission{}, artifactsdomain.Call{}, "", false
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return Admission{}, artifactsdomain.Call{}, "", false
	}
	c, err := a.call(originOf(a.Principal), s.gate.limits(), key, "")
	if err != nil {
		unavailable(w, err, "this node could not settle your role on the artifact")
		return Admission{}, artifactsdomain.Call{}, "", false
	}
	return a, c, key, true
}

// done answers a gesture's write.
func done(w http.ResponseWriter, r *http.Request, key, verb string, id artifacts.ID,
	out artifactsdomain.Written, err error) {

	if err != nil {
		writeFailed(w, r, key, verb, err)
		return
	}
	written(w, key, out.Result, httpjson.Detail{"artifact": id.String()})
}

// patchBody is the head's displayed fields; an absent field is unchanged.
type patchBody struct {
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Icon        *string `json:"icon,omitempty"`
}

// patch renames an artifact, or changes its description or icon (`PATCH
// /artifacts/{id}`), decided by `update_artifact`.
func (s *Service) patch(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactUpdate)
	if !ok {
		return
	}
	var body patchBody
	if !decode(w, r, &body, false) {
		return
	}
	out, err := s.writes.Patch(r.Context(), c, a.Head.ID, artifactsdomain.Patch{
		Title: body.Title, Description: body.Description, Icon: body.Icon})
	done(w, r, key, "update the artifact", a.Head.ID, out, err)
}

// labelBody is one version's label; empty clears it.
type labelBody struct {
	Label *string `json:"label"`
}

// label sets or clears one version's label (`PATCH
// /artifacts/{id}/versions/{n}`), decided by `update_artifact`. A labelled
// version is exempt from the retention that retires the oldest unlabelled
// ones.
func (s *Service) label(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactUpdate)
	if !ok {
		return
	}
	n, ok := pathVersion(w, r)
	if !ok {
		return
	}
	var body labelBody
	if !decode(w, r, &body, false) {
		return
	}
	if body.Label == nil {
		invalid(w, "name the label, or an empty one to clear it")
		return
	}
	out, err := s.writes.Patch(r.Context(), c, a.Head.ID, artifactsdomain.Patch{
		Label: &artifactsdomain.VersionLabel{Number: n, Label: *body.Label}})
	done(w, r, key, "label the version", a.Head.ID, out, err)
}

// share grants one audience a role (`PUT /artifacts/{id}/shares`), decided by
// `share_artifact`, the owner's. The record names the caller as the grantor; a
// share never grants above the sharer's own role, and an agent never shares
// with the whole company (the writer refuses both).
func (s *Service) share(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactShare)
	if !ok {
		return
	}
	var body shareInput
	if !decode(w, r, &body, false) {
		return
	}
	out, err := s.writes.Share(r.Context(), c, a.Head.ID,
		artifacts.Share{Audience: body.Audience, Role: body.Role})
	done(w, r, key, "share the artifact", a.Head.ID, out, err)
}

// unshare withdraws one audience's share (`DELETE
// /artifacts/{id}/shares/{audience}`, the audience as `seat:<handle>`,
// `unit:<key>` or `company`), decided by `unshare_artifact`. A share that is
// not there answers success and writes nothing.
func (s *Service) unshare(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactUnshare)
	if !ok {
		return
	}
	audience, err := artifacts.ParseAudience(r.PathValue("audience"))
	if err != nil {
		invalid(w, say(err))
		return
	}
	out, err := s.writes.Unshare(r.Context(), c, a.Head.ID, audience)
	done(w, r, key, "withdraw the share", a.Head.ID, out, err)
}

// transferBody names the seat ownership moves to.
type transferBody struct {
	To string `json:"to"`
}

// transferOwnership moves ownership to another seat (`POST
// /artifacts/{id}/transfer`), decided by `artifacts.transfer`: the owner's, a
// person's, with a person present.
func (s *Service) transferOwnership(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactTransfer)
	if !ok {
		return
	}
	var body transferBody
	if !decode(w, r, &body, false) {
		return
	}
	to := strings.TrimPrefix(strings.TrimSpace(body.To), "@")
	if to == "" {
		invalid(w, "name the seat ownership moves to, by its handle")
		return
	}
	out, err := s.writes.Transfer(r.Context(), c, a.Head.ID, to)
	done(w, r, key, "transfer the artifact", a.Head.ID, out, err)
}

// trash takes an artifact out of circulation (`DELETE /artifacts/{id}`),
// decided by `artifacts.trash`: its owner's, a person's. Every frame and
// ticket stops resolving at once; it is purged after the trash term unless it
// is restored.
func (s *Service) trash(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactTrash)
	if !ok {
		return
	}
	out, err := s.writes.Trash(r.Context(), c, a.Head.ID)
	done(w, r, key, "move the artifact to the trash", a.Head.ID, out, err)
}

// restore takes an artifact out of the trash (`POST /artifacts/{id}/restore`),
// decided by `artifacts.restore`.
func (s *Service) restore(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactRestore)
	if !ok {
		return
	}
	out, err := s.writes.Restore(r.Context(), c, a.Head.ID)
	done(w, r, key, "restore the artifact", a.Head.ID, out, err)
}

// purge destroys an artifact for good, its datastore first (`POST
// /artifacts/{id}/purge`): `artifacts.purge`, an administrator's, with a fresh
// proof of identity and a person present, decided where the route is mounted.
// The artifact is read first so an id nothing holds answers as one.
func (s *Service) purge(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactPurge)
	if !ok {
		return
	}
	out, err := s.writes.Purge(r.Context(), c, a.Head.ID)
	done(w, r, key, "purge the artifact", a.Head.ID, out, err)
}

// assetBody finalizes one file of an asset upload.
type assetBody struct {
	Upload string `json:"upload"`
	Name   string `json:"name"`
}

// addAsset finalizes one asset of the caller's asset upload into the
// artifact's asset store (`POST /artifacts/{id}/assets`), decided by
// `upload_artifact_asset`: its bytes read back and verified, then the domain's
// decide.
func (s *Service) addAsset(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactAsset)
	if !ok {
		return
	}
	if a.Head.Status == artifactsdomain.StatusTrashed {
		trashed(w)
		return
	}
	var body assetBody
	if !decode(w, r, &body, false) {
		return
	}
	pipe, ok := s.pipeline(w, r)
	if !ok {
		return
	}
	asset, err := pipe.AddAsset(r.Context(), c, a.Head.ID, body.Upload, body.Name)
	if err != nil {
		writeFailed(w, r, key, "add the asset", err)
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"asset": asset, "op_id": key})
}

// removeAsset removes one asset (`DELETE /artifacts/{id}/assets/{asset}`),
// decided by `upload_artifact_asset`.
func (s *Service) removeAsset(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactAsset)
	if !ok {
		return
	}
	asset, err := uuid.Parse(r.PathValue("asset"))
	if err != nil {
		gone(w, "that is not an asset id")
		return
	}
	out, err := s.writes.RemoveAsset(r.Context(), c, a.Head.ID, asset)
	done(w, r, key, "remove the asset", a.Head.ID, out, err)
}

// pin keeps an artifact on the caller's own pinned list (`PUT
// /artifacts/{id}/pin`), decided by `artifacts.pin`.
func (s *Service) pin(w http.ResponseWriter, r *http.Request) { s.setPin(w, r, true) }

// unpin drops it (`DELETE /artifacts/{id}/pin`).
func (s *Service) unpin(w http.ResponseWriter, r *http.Request) { s.setPin(w, r, false) }

// setPin is a pin or an unpin: the caller's own record, so the role is the
// read row's and nothing is shared.
func (s *Service) setPin(w http.ResponseWriter, r *http.Request, pinned bool) {
	a, c, key, ok := s.gesture(w, r, authz.ActionArtifactPin)
	if !ok {
		return
	}
	out, err := s.writes.Pin(r.Context(), c, a.Head.ID, pinned)
	done(w, r, key, "pin the artifact", a.Head.ID, out, err)
}
