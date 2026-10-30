package artifactupload

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// FileSpec names one path of the next version and where its bytes come from:
// exactly one of the sources.
type FileSpec struct {
	Path string `json:"path"`

	// Upload takes the bytes that arrived through the request's upload for
	// this path.
	Upload bool `json:"upload,omitempty"`

	// Carry names an object this artifact already holds, by digest.
	Carry objstore.Hash `json:"carry,omitempty"`

	// From carries the file at this path from that retained version.
	From int `json:"from,omitempty"`

	// Remove drops the path from the base version. Patch mode only.
	Remove bool `json:"remove,omitempty"`

	// Inline is the file's bytes in the request itself: an agent's tool
	// call, at most [artifacts.MaxInlineBytes] across the call. Never
	// decoded from a request body.
	Inline []byte `json:"-"`

	// Brand copies the adopted brand's file in this slot (a logo, or one
	// file of a face under [brandformat.FontFileSlot]) as a new object of
	// the version: an agent's `from_brand`, and a deck begun in the brand.
	// Never decoded from a request body.
	Brand brandformat.Slot `json:"-"`
}

// sources counts how many sources a spec names.
func (f FileSpec) sources() int {
	n := 0
	for _, set := range []bool{f.Upload, f.Carry != "", f.From != 0, f.Remove, f.Inline != nil,
		f.Brand != ""} {
		if set {
			n++
		}
	}
	return n
}

// PublishRequest is one new version of an artifact.
type PublishRequest struct {
	Artifact artifacts.ID `json:"artifact"`

	// Base is the head the caller decided against (If-Match), ignored when
	// Force.
	Base int `json:"base"`

	// Force publishes over a moved head: a person with presence only
	// (D08), the authority the caller decided.
	Force bool `json:"force,omitempty"`

	// RestoreFrom rolls back to an earlier version as a new one, every
	// entry carried; nothing else may then be named.
	RestoreFrom int `json:"restore_from,omitempty"`

	// Upload is the intent the new bytes came through, if any.
	Upload string `json:"upload,omitempty"`

	// Mode is whole (the full file set) or patch (relative to the base).
	Mode manifest.Mode `json:"mode"`

	Files []FileSpec `json:"files,omitempty"`

	// Edits apply to the base version's files, in order, all or nothing.
	Edits []edits.Edit `json:"edits,omitempty"`

	// Entry is the path served at the version's root, empty to keep the
	// base's.
	Entry string `json:"entry,omitempty"`

	Label string `json:"label,omitempty"`

	// Contract is empty (keep the pin), `latest`, or an exact version.
	Contract string `json:"contract,omitempty"`

	// Capabilities are the version's; nil keeps the head's, and an empty
	// set declares none.
	Capabilities []artifacts.Capability `json:"capabilities"`
}

// CreateRequest is a new artifact and its first version, its files named as a
// whole publish's.
type CreateRequest struct {
	Kind        artifacts.Kind `json:"kind"`
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Icon        string         `json:"icon,omitempty"`

	// Owner is the seat handle or machine login that owns it, and
	// OwnerKind its actor kind: the caller decides who that is.
	Owner     string        `json:"owner"`
	OwnerKind iam.ActorKind `json:"owner_kind"`

	SourceKey string            `json:"source_key,omitempty"`
	Shares    []artifacts.Share `json:"shares,omitempty"`

	// Folder is the folder the artifact is created in, by id, empty for the
	// top level: one the caller's surface already admitted filing into
	// (contributor on it, through the requester ceiling for a tool).
	Folder string `json:"folder,omitempty"`

	Upload       string                 `json:"upload,omitempty"`
	Files        []FileSpec             `json:"files,omitempty"`
	Entry        string                 `json:"entry,omitempty"`
	Label        string                 `json:"label,omitempty"`
	Contract     string                 `json:"contract,omitempty"`
	Capabilities []artifacts.Capability `json:"capabilities,omitempty"`

	// Starter begins the artifact from its kind's starter content (a deck's
	// example slides), which the engine holds; it names no files.
	Starter bool `json:"starter,omitempty"`

	// Doc is a doc's starter as its writer already wrote it: with Starter,
	// on a doc only.
	Doc *DocStarter `json:"doc,omitempty"`

	// Deck is the plan a slides starter is composed from: its theme and the
	// slides it opens with. Nil is the default plan; a slides starter only.
	Deck *slideformat.Plan `json:"deck,omitempty"`

	// Layout is a design's starter: the boards and notes the person laid
	// out, published as placeholder pages. Only with Starter, and only for a
	// design.
	Layout *canvasformat.Layout `json:"layout,omitempty"`
}

// StageNamespace scopes the derivation of a publish's own reservation from the
// operation that publishes, so the retry of a publish that wrote objects of its
// own finds the reservation, and the objects, its first attempt made.
var StageNamespace = uuid.MustParse("8b1f4e2c-7a9d-5c36-b0e1-2f4d6a8c9e57")

// Create makes an artifact and its first version through THE pipeline: the
// upload's bytes read back and verified, the linter, the copies of CDN files,
// the search text and the runtime contract, then the domain's decide.
func (s *Service) Create(ctx context.Context, c artifactsdomain.Call, req CreateRequest) (artifactsdomain.Published, Report, error) {
	start := s.now()
	out, report, err := s.create(ctx, c, req)
	s.observePublish(start, err)
	s.judgePublish(start, err)
	return out, report, err
}

func (s *Service) create(ctx context.Context, c artifactsdomain.Call, req CreateRequest) (artifactsdomain.Published, Report, error) {
	if !req.Kind.Valid() {
		return artifactsdomain.Published{}, Report{}, fmt.Errorf("%w: kind %q is not one of %s",
			ErrInvalid, req.Kind, words.Or(artifacts.Kinds))
	}
	// adopted is the brand a deck begun in it was composed from, which its
	// copies are then made from too: one adoption for the whole version.
	var adopted *artifactsdomain.Adoption
	if (req.Doc != nil || req.Deck != nil || req.Layout != nil) && !req.Starter {
		return artifactsdomain.Published{}, Report{}, fmt.Errorf("%w: a doc's written text, a "+
			"deck plan or a design's layout begins the artifact from its kind's starter: "+
			"send it with \"starter\": true", ErrInvalid)
	}
	if req.Starter {
		if req.Upload != "" || len(req.Files) > 0 || req.Entry != "" {
			return artifactsdomain.Published{}, Report{}, fmt.Errorf("%w: a create from the "+
				"kind's starter names no upload, files or entry: the starter is all of them", ErrInvalid)
		}
		files, entry, brand, err := s.starter(ctx, req)
		if err != nil {
			return artifactsdomain.Published{}, Report{}, err
		}
		req.Files, req.Entry, adopted = files, entry, brand
	}
	for _, f := range req.Files {
		if f.Carry != "" || f.From != 0 || f.Remove {
			return artifactsdomain.Published{}, Report{}, fmt.Errorf("%w: %q: a new artifact holds "+
				"nothing to carry or remove; name every file by its upload or inline", ErrInvalid, f.Path)
		}
	}
	if err := checkStrings(c.Authority.Actor.Kind, map[string]string{
		"the title": req.Title, "the description": req.Description, "the icon": req.Icon,
		"the label": req.Label, "the source key": req.SourceKey,
	}); err != nil {
		return artifactsdomain.Published{}, Report{}, err
	}
	if err := s.turns.publish(c.TurnID, "", c.OpKey); err != nil {
		return artifactsdomain.Published{}, Report{}, err
	}
	r, err := s.newRun(ctx, c, req.Kind, artifacts.ID{}, req.Upload, purposeCreate)
	if err != nil {
		return artifactsdomain.Published{}, Report{}, err
	}
	if done, ok := r.answered(); ok {
		return done.Published, done.Report, nil
	}
	r.brand = adopted
	in, err := r.compose(ctx, manifest.ModeWhole, req.Files, nil, req.Entry, req.Contract, req.Capabilities)
	if err != nil {
		return artifactsdomain.Published{}, Report{}, r.fail(ctx, err)
	}
	in.Label = req.Label
	published, err := s.writes.Create(ctx, c, artifactsdomain.CreateInput{
		Kind: req.Kind, Title: req.Title, Description: req.Description, Icon: req.Icon,
		Owner: req.Owner, OwnerKind: req.OwnerKind, SourceKey: req.SourceKey,
		First: in, Shares: req.Shares, Folder: req.Folder,
	})
	if err != nil {
		return published, Report{}, r.fail(ctx, err)
	}
	r.close(ctx, published, in.Manifest)
	return published, r.report(), nil
}

// Publish adds a version to an artifact through THE pipeline: the upload's
// bytes read back and verified, the edits against the base version, the
// linter, the copies of CDN files, the search text and the runtime contract,
// then the domain's decide.
//
// # A refused publish leaves nothing behind
//
// Every object the pipeline wrote for the publish (an edit's result, a linted
// page, a copied CDN file, inline content) is deleted when the publish is
// refused, and its reservation released; the upload the caller sent goes back
// to open, its files held for another try until its window closes. An outcome
// nobody could establish (a store that did not answer) leaves everything as it
// is, so the retry of the same operation continues where it stopped.
func (s *Service) Publish(ctx context.Context, c artifactsdomain.Call, req PublishRequest) (artifactsdomain.Published, Report, error) {
	start := s.now()
	out, report, err := s.publish(ctx, c, req)
	s.observePublish(start, err)
	s.judgePublish(start, err)
	return out, report, err
}

func (s *Service) publish(ctx context.Context, c artifactsdomain.Call, req PublishRequest) (artifactsdomain.Published, Report, error) {
	if req.Artifact.IsZero() {
		return artifactsdomain.Published{}, Report{}, fmt.Errorf("%w: a publish names its artifact", ErrInvalid)
	}
	if err := checkStrings(c.Authority.Actor.Kind, map[string]string{"the label": req.Label}); err != nil {
		return artifactsdomain.Published{}, Report{}, err
	}
	if err := s.turns.publish(c.TurnID, req.Artifact.String(), c.OpKey); err != nil {
		return artifactsdomain.Published{}, Report{}, err
	}
	if req.RestoreFrom != 0 {
		return s.restore(ctx, c, req)
	}
	if !req.Mode.Valid() {
		return artifactsdomain.Published{}, Report{}, fmt.Errorf("%w: mode %q is not whole or patch",
			ErrInvalid, req.Mode)
	}
	if len(req.Edits) > artifacts.MaxEditsPerPublish {
		return artifactsdomain.Published{}, Report{}, fmt.Errorf("%w: the publish names %d edits and "+
			"the maximum is %d", ErrInvalid, len(req.Edits), artifacts.MaxEditsPerPublish)
	}
	head, err := s.headFor(ctx, req.Artifact, req.Base, req.Force)
	if err != nil {
		return artifactsdomain.Published{}, Report{}, err
	}
	if head.Status == artifactsdomain.StatusTrashed {
		return artifactsdomain.Published{}, Report{}, fmt.Errorf("%w: %s", artifactsdomain.ErrTrashed, req.Artifact)
	}
	// THE RUN FIRST, so a retry of a publish that landed is answered with
	// what it landed, whatever the head has done since.
	r, err := s.newRun(ctx, c, head.Kind, req.Artifact, req.Upload, purposePublish)
	if err != nil {
		return artifactsdomain.Published{}, Report{}, err
	}
	if done, ok := r.answered(); ok {
		return done.Published, done.Report, nil
	}
	base := head.HeadNumber
	if !req.Force && req.Base != head.HeadNumber {
		// REFUSED BEFORE A BYTE IS WRITTEN: the domain would refuse the
		// record the same way, and an edit aimed at another version is
		// never re-aimed at the head.
		live, err := s.reads.Version(ctx, req.Artifact, head.HeadNumber) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return artifactsdomain.Published{}, Report{}, r.fail(ctx, err)
		}
		return artifactsdomain.Published{}, Report{}, r.fail(ctx, &artifactsdomain.VersionMoved{Head: head, Live: live})
	}
	r.headContract, r.headCaps = head.Contract, head.Capabilities
	if r.base, err = s.reads.Version(ctx, req.Artifact, base); err != nil {
		return artifactsdomain.Published{}, Report{}, r.fail(ctx, err)
	}
	r.hasBase = true
	in, err := r.compose(ctx, req.Mode, req.Files, req.Edits, req.Entry, req.Contract, req.Capabilities)
	if err != nil {
		return artifactsdomain.Published{}, Report{}, r.fail(ctx, err)
	}
	in.Artifact, in.Base, in.Force, in.Label = req.Artifact, req.Base, req.Force, req.Label
	decided := s.now()
	published, err := s.writes.Publish(ctx, c, in)
	if bytesFree(req) {
		s.judgeApplied(decided, err)
	}
	if err != nil {
		return published, Report{}, r.fail(ctx, err)
	}
	r.close(ctx, published, in.Manifest)
	return published, r.report(), nil
}

// headFor is the artifact's head as a publish based on base decides against.
//
// THIS NODE'S COPY FIRST, which costs no broker call: a head past the base is
// a moved head wherever it is read, and a head at the base is the base. Only a
// copy BEHIND the base (the caller published through a node ahead of this one)
// or a forced publish (which composes against whatever the head is) is read
// again at the linearizable level, so the base a publish composes against is
// never older than the one the caller saw.
func (s *Service) headFor(ctx context.Context, id artifacts.ID, base int, force bool) (artifactsdomain.Head, error) {
	if !force {
		head, _, err := s.reads.Artifact(ctx, id, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil || head.HeadNumber >= base {
			return head, err
		}
	}
	head, _, err := s.reads.Artifact(ctx, id, statelog.Freshness{Level: statelog.ReadLinearizable})
	return head, err
}

// restore is a rollback: version RestoreFrom's files as a new version, every
// entry carried, so no byte moves and nothing is linted again.
func (s *Service) restore(ctx context.Context, c artifactsdomain.Call, req PublishRequest) (artifactsdomain.Published, Report, error) {
	if req.Upload != "" || len(req.Files) > 0 || len(req.Edits) > 0 || req.Entry != "" {
		return artifactsdomain.Published{}, Report{}, fmt.Errorf("%w: a rollback names the version "+
			"it restores and nothing else", ErrInvalid)
	}
	if err := checkCapabilities(req.Capabilities); err != nil {
		return artifactsdomain.Published{}, Report{}, err
	}
	head, err := s.headFor(ctx, req.Artifact, req.Base, req.Force)
	if err != nil {
		return artifactsdomain.Published{}, Report{}, err
	}
	contract := req.Contract
	if contract != "" {
		// AN EMPTY CONTRACT KEEPS THE RESTORED VERSION'S PIN, which the
		// domain reads from that version; anything named is resolved here.
		caps := req.Capabilities
		if caps == nil {
			caps = head.Capabilities
		}
		if contract, err = s.resolveContract(head.Kind, contract, head.Contract, caps); err != nil {
			return artifactsdomain.Published{}, Report{}, err
		}
	}
	return s.writeOnly(ctx, c, artifactsdomain.PublishInput{
		Artifact: req.Artifact, Base: req.Base, Force: req.Force, RestoredFrom: req.RestoreFrom,
		Label: req.Label, Contract: contract, Capabilities: req.Capabilities,
	})
}

// writeOnly is a publish whose bytes are all carried, handed straight to the
// domain.
//
// EVERY BYTE IS CARRIED, so it is a bytes-free publish the latency objective
// counts.
func (s *Service) writeOnly(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.PublishInput) (artifactsdomain.Published, Report, error) {
	decided := s.now()
	published, err := s.writes.Publish(ctx, c, in)
	s.judgeApplied(decided, err)
	return published, Report{}, err
}

// AddAsset finalizes one asset of the caller's asset upload: its bytes read
// back and verified, then the domain's decide. An asset is never linted (a
// page reaches it under the artifact origin's own asset route, with the
// subresource policy) and never carried.
func (s *Service) AddAsset(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, upload, name string) (artifactsdomain.Asset, error) {
	start := s.now()
	asset, err := s.addAsset(ctx, c, id, upload, name)
	s.observePublish(start, err)
	return asset, err
}

func (s *Service) addAsset(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, upload, name string) (artifactsdomain.Asset, error) {
	principal, err := principalOf(c)
	if err != nil {
		return artifactsdomain.Asset{}, err
	}
	rec, err := s.ownRecord(ctx, principal, upload)
	if err != nil {
		return artifactsdomain.Asset{}, err
	}
	if rec.Purpose != purposeAsset || rec.Artifact != id.String() {
		return artifactsdomain.Asset{}, fmt.Errorf("%w: upload %s is not an asset upload for %s",
			ErrInvalid, upload, id)
	}
	p, err := manifest.CleanPath(name)
	if err != nil {
		return artifactsdomain.Asset{}, classify(err)
	}
	i := rec.fileAt(p)
	if i < 0 {
		return artifactsdomain.Asset{}, fmt.Errorf("%w: upload %s declares no asset %q", ErrInvalid, upload, p)
	}
	f := rec.Files[i]
	if f.State != fileNamed {
		if !rec.Expires.After(s.now()) {
			return artifactsdomain.Asset{}, fmt.Errorf("%w: %s", ErrUploadExpired, upload)
		}
		if f.State == fileDeleted || (f.Transport == transportEngine && f.State != fileStored) {
			return artifactsdomain.Asset{}, &UploadMissing{Paths: []string{p}}
		}
		if _, err := s.readBack(ctx, f); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return artifactsdomain.Asset{}, err
		}
	}
	asset, _, err := s.writes.AddAsset(ctx, c, artifactsdomain.AssetInput{
		Artifact: id, Name: p, ContentType: f.ContentType, Object: f.object(),
	})
	if err != nil {
		return asset, err
	}
	closed, err := s.updateRecord(context.WithoutCancel(ctx), upload, func(r *record) error {
		if j := r.fileAt(p); j >= 0 {
			r.Files[j].State = fileNamed
		}
		if r.heldBytes() == 0 {
			r.State = stateClosed
		}
		return nil
	})
	if err != nil {
		// THE ASSET IS RECORDED; only the account of it is not. Its bytes
		// stay held until the hold lapses, and a retry is answered by the
		// domain from its own record.
		log.WarnContext(ctx, "artifact_upload_record_failed", "upload", upload, "error", err.Error())
		return asset, nil
	}
	s.release(ctx, closed.ID)
	return asset, nil
}

// readBack reads one uploaded file's object through the store's verifying
// reader and checks its content, answering its bytes for a text file and its
// head for a binary one. A missing object is [*UploadMissing]; one whose bytes
// are not the declared ones is [*UploadMismatch].
func (s *Service) readBack(ctx context.Context, f file) ([]byte, error) {
	typ, err := typeOf(f.Path, f.ContentType)
	if err != nil {
		return nil, err
	}
	body, err := readObject(ctx, s.objects, f.object(), typ.Text)
	switch {
	case errors.Is(err, objstore.ErrNotFound):
		return nil, &UploadMissing{Paths: []string{f.Path}}
	case errors.Is(err, objstore.ErrCorrupt):
		return nil, &UploadMismatch{Path: f.Path, Reason: err.Error()}
	case err != nil:
		return nil, fmt.Errorf("artifactupload: read back %q: %w", f.Path, err)
	}
	if err := checkContent(f.Path, typ, headOf(body, typ.Text), f.Size); err != nil {
		return nil, err
	}
	if f.Transport == transportDirect {
		s.countBytes(f.Size, "direct")
	}
	return body, nil
}
