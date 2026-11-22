package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/googleplay"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// growthMedia is this node's [googleplay.Media]: a file an agent names from
// an artifact, read for an upload as that agent seat may read the artifact.
type growthMedia struct{ engine *Engine }

var _ googleplay.Media = growthMedia{}

// Open reads ref (artifact:<id>/<path>) for handle's upload, at most limit
// bytes.
func (m growthMedia) Open(ctx context.Context, handle, ref string, limit int64) (googleplay.File, error) {
	e := m.engine
	objects := e.Objects()
	if e.router == nil || objects == nil {
		return googleplay.File{}, growth.Refuse(growth.RefusedUnavailable, "this node reads no artifact for an "+
			"upload. No vendor was reached.")
	}
	r := mediaReader{artifacts: e.router.Artifacts(), objects: objects, chart: ChartAuthorityOf(e),
		org: e.currentOrg(), manageOthers: e.artifactsManageOthers(), ceiling: builtin.CallCeiling}
	return r.open(ctx, handle, ref, limit, time.Now())
}

// mediaArtifacts is the artifacts read side an upload reads through.
type mediaArtifacts interface {
	Artifact(ctx context.Context, id artifacts.ID, fresh statelog.Freshness) (artifactsdomain.Head,
		statelog.Position, error)
	Version(ctx context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error)
	Assets(ctx context.Context, id artifacts.ID) ([]artifactsdomain.Asset, error)
}

// mediaObjects streams a stored object, its every byte checked against the
// digest its row carries (objstore.Store.Open).
type mediaObjects interface {
	Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error)
}

// mediaReader is what [growthMedia] reads through, made per call from the
// running company.
type mediaReader struct {
	artifacts    mediaArtifacts
	objects      mediaObjects
	chart        authz.Chart
	org          *org.Organization
	manageOthers bool
	// ceiling is the requester ceiling the gate bound to the call
	// (builtin.CallCeiling), false outside any gated call.
	ceiling func(ctx context.Context) (authz.Ceiling, bool, error)
}

// mediaPrefix starts every file reference an upload takes.
const mediaPrefix = "artifact:"

// open reads ref for handle's upload, decided as get_artifact decides a read:
// for the seat and whoever its turn works for, so nobody has an agent publish
// what they may not open. The ask and the apply run inside the seat's gated
// call; the card an administrator opens reads the file outside any call, to
// show its digest, and is decided for the seat alone. An administrator's
// override admits no upload: it is a person's read, never an agent's.
func (r mediaReader) open(ctx context.Context, handle, ref string, limit int64, now time.Time) (googleplay.File,
	error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(ref), mediaPrefix)
	id, file, split := strings.Cut(raw, "/")
	artifact, err := artifactID(id)
	if !ok || !split || err != nil {
		return googleplay.File{}, growth.Refuse(growth.RefusedInvalid, fmt.Sprintf("%q names no artifact file: "+
			"write artifact:<artifact id>/<path in it>. No vendor was reached.", ref))
	}
	clean, err := manifest.CleanPath(file)
	if err != nil {
		return googleplay.File{}, growth.Refuse(growth.RefusedInvalid, fmt.Sprintf("%q is not a path in an "+
			"artifact. No vendor was reached.", file))
	}
	h, _, err := r.artifacts.Artifact(ctx, artifact, statelog.Freshness{
		Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)})
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return googleplay.File{}, noArtifact(ref)
	case err != nil:
		return googleplay.File{}, fmt.Errorf("engine: read artifact %s for an upload: %w", artifact, err)
	}
	if err = r.mayRead(ctx, handle, h, ref, now); err != nil {
		return googleplay.File{}, err
	}
	if h.Status == artifactsdomain.StatusTrashed || !h.PurgeBegunAt.IsZero() {
		return googleplay.File{}, growth.Refuse(growth.RefusedConflict, fmt.Sprintf("artifact %s is in the "+
			"trash, and nothing of it is uploaded until its owner (%s) restores it. No vendor was reached.",
			artifact, h.Owner))
	}
	entry, err := r.entry(ctx, h, clean)
	if err != nil {
		return googleplay.File{}, err
	}
	if entry.Object.Size > limit {
		return googleplay.File{}, tooLarge(ref, entry.Object.Size, limit)
	}
	rc, err := r.objects.Open(ctx, entry.Object)
	if err != nil {
		return googleplay.File{}, fmt.Errorf("engine: open %s for an upload: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return googleplay.File{}, fmt.Errorf("engine: read %s for an upload: %w", ref, err)
	}
	if int64(len(data)) > limit {
		return googleplay.File{}, tooLarge(ref, int64(len(data)), limit)
	}
	sum := sha256.Sum256(data)
	return googleplay.File{Ref: ref, Name: path.Base(clean), ContentType: entry.ContentType, Data: data,
		SHA256: hex.EncodeToString(sum[:])}, nil
}

// mayRead refuses a seat that may not read h, in the same words as an
// artifact nothing holds, so an id is no oracle.
func (r mediaReader) mayRead(ctx context.Context, handle string, h artifactsdomain.Head, ref string,
	now time.Time) error {
	var seat *org.Role
	if r.org != nil {
		seat = r.org.AgentSeatByHandle(handle)
	}
	if seat == nil {
		return noArtifact(ref)
	}
	ceiling := authz.Ceiling{Kind: authz.CeilingNone}
	if r.ceiling != nil {
		bound, ok, err := r.ceiling(ctx)
		if err != nil {
			return growth.Refuse(growth.RefusedUnavailable, "who this turn works for could not be read right now. "+
				"Nothing was uploaded; call again. No vendor was reached.")
		}
		if ok {
			ceiling = bound
		}
	}
	object := authz.Object{Kind: authz.KindArtifact, ID: h.ID.String(), Owner: h.Owner, Shares: h.Shares,
		OwnerIsAgent: h.OwnerKind == iam.ActorAgent, AgentsManageOthers: r.manageOthers}
	dec := authz.DecideWithin(ctx, turnctx.SeatPrincipal(r.org, seat), ceiling, authz.ActionArtifactRead, object,
		r.chart, now)
	switch {
	case dec.Unknown():
		return growth.Refuse(growth.RefusedUnavailable, "access to the artifact could not be checked right now. "+
			"Nothing was uploaded; call again. No vendor was reached.")
	case dec.Elevated:
		return growth.Refuse(growth.RefusedForbidden, "the artifact is not shared with this agent, so it is not "+
			"uploaded. Ask its owner to share it with the agent. No vendor was reached.")
	case !dec.Allowed:
		return noArtifact(ref)
	}
	return nil
}

// entry is the file at clean: one of the live version's files, else an asset
// named so.
func (r mediaReader) entry(ctx context.Context, h artifactsdomain.Head, clean string) (artifacts.Entry, error) {
	if h.HeadNumber > 0 {
		v, err := r.artifacts.Version(ctx, h.ID, h.HeadNumber)
		if err != nil && !errors.Is(err, artifactsdomain.ErrNotFound) {
			return artifacts.Entry{}, fmt.Errorf("engine: read artifact %s's live version: %w", h.ID, err)
		}
		if e, ok := manifest.EntryAt(v.Manifest, clean); err == nil && ok {
			return e, nil
		}
	}
	assets, err := r.artifacts.Assets(ctx, h.ID)
	if err != nil {
		return artifacts.Entry{}, fmt.Errorf("engine: read artifact %s's assets: %w", h.ID, err)
	}
	for _, a := range assets {
		if a.Name == clean {
			return artifacts.Entry{Path: a.Name, ContentType: a.ContentType, Object: a.Object}, nil
		}
	}
	return artifacts.Entry{}, growth.Refuse(growth.RefusedNotFound, fmt.Sprintf("artifact %s holds no file %q in "+
		"its live version or its assets; %s lists its files. No vendor was reached.", h.ID, clean,
		authz.ActionArtifactRead))
}

// artifactID reads an artifact's id or its 26-character host label.
func artifactID(raw string) (artifacts.ID, error) {
	if len(raw) == artifacts.LabelLen {
		return artifacts.ParseLabel(raw)
	}
	return artifacts.ParseID(raw)
}

// noArtifact is the one answer for an artifact nothing holds and one the seat
// may not open.
func noArtifact(ref string) error {
	return growth.Refuse(growth.RefusedNotFound, fmt.Sprintf("there is no artifact file %q this agent can open. "+
		"Check the id, or find it with %s. No vendor was reached.", ref, authz.ActionArtifactList))
}

// tooLarge refuses a file over the upload's limit.
func tooLarge(ref string, size, limit int64) error {
	return growth.Refuse(growth.RefusedInvalid, fmt.Sprintf("%s is %d bytes, more than the %d this upload takes. "+
		"No vendor was reached.", ref, size, limit))
}
