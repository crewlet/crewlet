package queries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
)

// THE ARTIFACTS' READ QUESTIONS: the gallery's listing, one artifact's head,
// its versions, one version's manifest, its history, its assets, who has
// opened it, who holds it, and the company's usage against its quota. Each is
// DECIDED PER ARTIFACT through the one admission the artifacts' write routes
// take ([artifactsapi.Gate]), which records the admin path's override before
// it answers, and each is SHAPED by the reader's role as those routes shape
// what they answer. The listing is everybody's question, FILTERED row by row
// by `get_artifact` rather than refused.

// ArtifactsReader is the artifacts estate's read side these questions ask: the
// estate router's, so a node whose own copy is out of service answers from a
// peer's.
type ArtifactsReader interface {
	artifactsapi.Lister
	artifactsapi.SeenByReads
	Version(ctx context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error)
	Versions(ctx context.Context, id artifacts.ID, cursor string, limit int) (
		artifactsdomain.VersionPage, error)
	History(ctx context.Context, id artifacts.ID, cursor string, limit int) (
		artifactsdomain.HistoryPage, error)
	Assets(ctx context.Context, id artifacts.ID) ([]artifactsdomain.Asset, error)
	Usage(ctx context.Context) (artifactsdomain.Usage, error)
}

// ArtifactsSource is what the artifacts' questions read: the admission and the
// reads.
type ArtifactsSource struct {
	Gate  artifactsapi.Gate
	Reads ArtifactsReader
}

// artifactsAnswers is the source as the questions answer from it, on the one
// clock every answer here reads ([Sources.clock]).
type artifactsAnswers struct {
	ArtifactsSource
	now func() time.Time
}

// registerArtifacts registers the artifacts' questions.
func registerArtifacts(r *Registry, src ArtifactsSource, now func() time.Time) {
	src.Gate.Now = now
	s := artifactsAnswers{ArtifactsSource: src, now: now}
	r.Register("artifacts", Everybody(), s.list)
	r.Register("artifact", DecidedBy(authz.ActionArtifactRead), s.artifact)
	r.Register("artifact_versions", DecidedBy(authz.ActionArtifactRead), s.versions)
	r.Register("artifact_version", DecidedBy(authz.ActionArtifactRead), s.version)
	r.Register("artifact_history", DecidedBy(authz.ActionArtifactRead), s.history)
	r.Register("artifact_assets", DecidedBy(authz.ActionArtifactRead), s.assets)
	r.Register("artifact_seen_by", DecidedBy(authz.ActionArtifactRead), s.seenBy)
	r.Register("artifact_access", DecidedBy(authz.ActionArtifactRead), s.access)
	// THE COMPANY'S ARTIFACTS against its plan quota: the deployment's own
	// machinery, so the usage verb's (admin's).
	r.Register("artifact_usage", DecidedBy(authz.ActionArtifactUsage), s.usage)
}

// artifactFailure is an admission's or an artifacts read's failure in this
// surface's vocabulary.
func artifactFailure(what string, err error) error {
	var refusal *artifactsapi.Refused
	switch {
	case err == nil:
		return nil
	case errors.Is(err, artifactsapi.ErrNoSuchArtifact):
		return fmt.Errorf("%w: no such artifact", ErrNotFound)
	case errors.As(err, &refusal):
		return refused(what, refusal.Decision)
	case errors.Is(err, artifactsapi.ErrRecordUnwritten), errors.Is(err, artifactsapi.ErrNoRoster):
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, artifactsdomain.ErrInvalid), errors.Is(err, artifactsdomain.ErrBadCursor):
		return fmt.Errorf("%w: %w", ErrBadParams, err)
	}
	return err
}

// artifactID is the artifact the `id` parameter names, answered as absent
// where it names none.
func artifactID(p Params) (artifacts.ID, error) {
	id, err := artifacts.ParseID(p.String("id"))
	if err != nil {
		return artifacts.ID{}, fmt.Errorf("%w: no artifact %q", ErrNotFound, p.String("id"))
	}
	return id, nil
}

// versionNumber is the version the `n` parameter names.
func versionNumber(p Params) (int, error) {
	n, ok := p.WholeInt("n")
	if !ok || n < 1 {
		return 0, fmt.Errorf("%w: n is a version number, 1 or more", ErrBadParams)
	}
	return int(n), nil
}

// read admits a read of the `id` parameter's artifact at version (zero for
// none).
func (s artifactsAnswers) read(ctx context.Context, what string, p Params, version int) (
	artifactsapi.Admission, error) {

	id, err := artifactID(p)
	if err != nil {
		return artifactsapi.Admission{}, err
	}
	asking, err := caller(ctx, what)
	if err != nil {
		return artifactsapi.Admission{}, err
	}
	a, err := s.Gate.Read(ctx, asking, id, version)
	return a, artifactFailure(what, err)
}

// list answers one page of the artifacts the caller can see: `scope=` mine,
// shared, company, agents or all (the default), `kind=`, `q=` (the title, the
// description and the head's text), `sort=` updated (the default), created,
// title or opened, `status=` live (the default) or trashed, `pinned=true`,
// `work_item=` (the artifacts made for that tracker item), `cursor=` and
// `limit=`.
func (s artifactsAnswers) list(ctx context.Context, p Params) (any, error) {
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	q := artifactsdomain.ListQuery{
		Scope: artifactsdomain.ListScope(p.String("scope")), Kind: artifacts.Kind(p.String("kind")),
		Status: artifactsdomain.Status(p.String("status")), Q: p.String("q"),
		Pinned: p.Bool("pinned", false), WorkItem: p.String("work_item"),
		Sort: artifactsdomain.ListSort(p.String("sort")), Cursor: p.String("cursor"),
		Limit: Clamp(p.Int("limit", 0), artifactsdomain.DefaultListLimit, artifactsdomain.MaxListLimit),
		Fresh: fresh,
	}
	asking, err := caller(ctx, "artifacts")
	if err != nil {
		return nil, err
	}
	page, err := s.Gate.List(ctx, s.Reads, asking, q)
	return page, artifactFailure("artifacts", err)
}

// artifact answers one artifact's head, its shares (to the owner and editors),
// the caller's role and whether they may publish.
func (s artifactsAnswers) artifact(ctx context.Context, p Params) (any, error) {
	id, err := artifactID(p)
	if err != nil {
		return nil, err
	}
	asking, err := caller(ctx, "artifact")
	if err != nil {
		return nil, err
	}
	detail, err := s.Gate.Detail(ctx, asking, id)
	return detail, artifactFailure("artifact", err)
}

// versions answers one page of an artifact's versions, newest first.
func (s artifactsAnswers) versions(ctx context.Context, p Params) (any, error) {
	a, err := s.read(ctx, "artifact_versions", p, 0)
	if err != nil {
		return nil, err
	}
	page, err := s.Reads.Versions(ctx, a.Head.ID, p.String("cursor"), p.Int("limit", 0))
	return page, artifactFailure("artifact_versions", err)
}

// version answers one version with its manifest.
func (s artifactsAnswers) version(ctx context.Context, p Params) (any, error) {
	n, err := versionNumber(p)
	if err != nil {
		return nil, err
	}
	a, err := s.read(ctx, "artifact_version", p, n)
	if err != nil {
		return nil, err
	}
	v, err := s.Reads.Version(ctx, a.Head.ID, n)
	return v, artifactFailure("artifact_version", err)
}

// history answers one page of an artifact's history, newest first, as the
// caller's role shows it.
func (s artifactsAnswers) history(ctx context.Context, p Params) (any, error) {
	a, err := s.read(ctx, "artifact_history", p, 0)
	if err != nil {
		return nil, err
	}
	page, err := s.Reads.History(ctx, a.Head.ID, p.String("cursor"), p.Int("limit", 0))
	if err != nil {
		return nil, artifactFailure("artifact_history", err)
	}
	return a.History(page), nil
}

// assets answers an artifact's live assets.
func (s artifactsAnswers) assets(ctx context.Context, p Params) (any, error) {
	a, err := s.read(ctx, "artifact_assets", p, 0)
	if err != nil {
		return nil, err
	}
	list, err := s.Reads.Assets(ctx, a.Head.ID)
	if err != nil {
		return nil, artifactFailure("artifact_assets", err)
	}
	if list == nil {
		list = []artifactsdomain.Asset{}
	}
	return map[string]any{"assets": list}, nil
}

// seenBy answers who has opened an artifact, to its owner and editors while
// the company shows it (D21).
func (s artifactsAnswers) seenBy(ctx context.Context, p Params) (any, error) {
	id, err := artifactID(p)
	if err != nil {
		return nil, err
	}
	asking, err := caller(ctx, "artifact_seen_by")
	if err != nil {
		return nil, err
	}
	seen, err := s.Gate.SeenBy(ctx, s.Reads, asking, id)
	return seen, artifactFailure("artifact_seen_by", err)
}

// access answers who holds an artifact and through what: the share dialog's
// list, in full to the owner and editors.
func (s artifactsAnswers) access(ctx context.Context, p Params) (any, error) {
	id, err := artifactID(p)
	if err != nil {
		return nil, err
	}
	asking, err := caller(ctx, "artifact_access")
	if err != nil {
		return nil, err
	}
	access, err := s.Gate.Access(ctx, asking, id)
	return access, artifactFailure("artifact_access", err)
}

// usage answers the company's artifacts counters against the plan quota in
// force on this node.
func (s artifactsAnswers) usage(ctx context.Context, _ Params) (any, error) {
	asking, err := caller(ctx, "artifact_usage")
	if err != nil {
		return nil, err
	}
	d := authz.Decide(ctx, asking, authz.ActionArtifactUsage,
		authz.Object{Kind: authz.KindArtifact}, s.Gate.Chart, s.now())
	switch {
	case d.Unknown():
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, d.Err)
	case !d.Allowed:
		return nil, refused("artifact_usage", d)
	}
	usage, err := s.Reads.Usage(ctx)
	if err != nil {
		return nil, artifactFailure("artifact_usage", err)
	}
	return map[string]any{"usage": usage, "limits": s.Gate.Limits()}, nil
}
