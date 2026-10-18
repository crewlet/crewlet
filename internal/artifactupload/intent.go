package artifactupload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/objstore"
)

// UploadNamespace scopes the derivation of an upload's id from the operation
// that began it. FIXED for the life of the format: a new namespace would give
// a retry that straddles an upgrade a second upload.
var UploadNamespace = uuid.MustParse("3f6c2a90-5d1e-5b7a-9e44-6a8d0c2f1b73")

// VendorPrefix is the folder the pipeline copies CDN files into, inside every
// version that references one. It is the pipeline's: an author's file may not
// be published under it, so a copied file never shadows one, or the reverse.
const VendorPrefix = "_vendor/"

// Intend decides an upload before any byte moves: for an artifact's next
// version, for a create (a zero id), or into its asset store (req.Asset).
//
// # In one decision, before the first byte
//
// It holds every declared file to its path, its media type and its size, the
// publish to its file and byte bounds, finds which files the artifact already
// holds (those are carried and move no bytes), and reserves the bytes of the
// rest against the principal's and the company's pending budgets and the
// company's quota, counted over what its artifacts name and every other hold
// outstanding. Only then does it mint one key per file on the engine's clock
// and answer how each file is transferred: a presigned put straight to the
// bucket, bound to the file's exact length, type and digest, where the object
// store proved it can take one and the caller is not an agent's tool (a
// sandbox is never handed a credential); a put through the engine otherwise.
//
// The authority verb is the caller's to have decided. An intent with the
// caller's operation key is idempotent: the retry an unknown outcome asks for
// answers the same upload, keys and reservation.
func (s *Service) Intend(ctx context.Context, c artifactsdomain.Call, id artifacts.ID,
	req artifacts.UploadRequest) (artifacts.UploadIntent, error) {

	intent, err := s.intend(ctx, c, id, req)
	if err != nil && refused(err) {
		s.countRefusal(err)
	}
	return intent, err
}

func (s *Service) intend(ctx context.Context, c artifactsdomain.Call, id artifacts.ID,
	req artifacts.UploadRequest) (artifacts.UploadIntent, error) {

	principal, err := principalOf(c)
	if err != nil {
		return artifacts.UploadIntent{}, err
	}
	p := purposePublish
	switch {
	case req.Asset && id.IsZero():
		return artifacts.UploadIntent{}, fmt.Errorf("%w: an asset upload names its artifact", ErrInvalid)
	case req.Asset:
		p = purposeAsset
	case id.IsZero():
		p = purposeCreate
	}
	plan, err := s.planIntent(ctx, p, id, req)
	if err != nil {
		return artifacts.UploadIntent{}, err
	}
	if len(plan.Upload) == 0 {
		return artifacts.UploadIntent{Carried: carriedPaths(plan)}, nil
	}
	if err := checkStrings(c.Authority.Actor.Kind, declaredPaths(plan)); err != nil {
		return artifacts.UploadIntent{}, err
	}

	upload := uuid.NewString()
	if c.OpKey != "" {
		digest, err := requestDigest(id, req)
		if err != nil {
			return artifacts.UploadIntent{}, err
		}
		upload = uuid.NewSHA1(UploadNamespace, []byte(principal+"\n"+c.OpKey+"\n"+digest)).String()
		// A RETRY OF AN INTENT THAT WROTE ITS RECORD is answered from it:
		// the same keys, nothing reserved twice, nothing charged twice.
		if r, _, err := s.readRecord(ctx, upload); err != nil {
			return artifacts.UploadIntent{}, err
		} else if r != nil {
			if r.Principal != principal {
				return artifacts.UploadIntent{}, fmt.Errorf("%w: %s", ErrUploadNotFound, upload)
			}
			return s.intentFor(ctx, r)
		}
	}

	var total, largest int64
	for _, d := range plan.Upload {
		total += d.Size
		largest = max(largest, d.Size)
	}
	giveBack, err := s.turns.spend(c.TurnID, total)
	if err != nil {
		return artifacts.UploadIntent{}, err
	}
	usage, err := s.reads.Usage(ctx)
	if err != nil {
		giveBack()
		return artifacts.UploadIntent{}, fmt.Errorf("artifactupload: read the company's usage: %w", err)
	}

	now := s.now()
	r := &record{
		ID: upload, Principal: principal, Purpose: p, TurnID: c.TurnID, Created: now,
		Expires: now.Add(objstore.PresignTerm(total)), State: stateOpen,
		Carried: carriedPaths(plan),
	}
	if !id.IsZero() {
		r.Artifact = id.String()
	}
	direct := s.presign != nil && c.Authority.Origin != artifacts.OriginTool
	for _, d := range plan.Upload {
		t := transportEngine
		if direct {
			t = transportDirect
		}
		r.Files = append(r.Files, file{
			Path: d.Path, ContentType: d.ContentType, SHA256: d.SHA256, Size: d.Size,
			Key: s.objects.Mint(), Transport: t, State: filePending,
		})
	}
	h := hold{
		Upload: upload, Principal: principal, Bytes: total, Expires: r.Expires, Direct: direct,
		// THE LAST BYTE CAN LAND a transfer's pace after the window closes:
		// a put begun at the last moment is given its own time.
		Until: holdUntil(r.Expires.Add(objstore.PaceFor(largest))),
	}
	if err := s.reserve(ctx, h, usage.Bytes); err != nil {
		giveBack()
		return artifacts.UploadIntent{}, err
	}
	wrote, err := s.writeRecord(ctx, r, 0)
	if err != nil || wrote == 0 {
		// NOTHING MOVED UNDER THE HOLD: with no record written it goes again,
		// and an intent that lost to an identical one keeps the hold that one
		// answers for, and answers it.
		s.release(ctx, r.ID)
		giveBack()
		if err != nil {
			return artifacts.UploadIntent{}, err
		}
		winner, _, err := s.ownRecord(ctx, principal, upload)
		if err != nil {
			return artifacts.UploadIntent{}, err
		}
		return s.intentFor(ctx, winner)
	}
	return s.intentFor(ctx, r)
}

// planIntent holds a request's declared files to the rules a version or an
// asset store sets, and splits them into carried and uploaded.
func (s *Service) planIntent(ctx context.Context, p purpose, id artifacts.ID,
	req artifacts.UploadRequest) (manifest.CarryPlan, error) {

	n := len(req.Entries)
	switch {
	case n == 0:
		return manifest.CarryPlan{}, fmt.Errorf("%w: an upload declares at least one file", ErrInvalid)
	case p == purposeAsset && n > artifacts.MaxAssetsPerCall:
		return manifest.CarryPlan{}, fmt.Errorf("%w: an asset upload declares %d files and the "+
			"maximum is %d", ErrInvalid, n, artifacts.MaxAssetsPerCall)
	case n > artifacts.MaxFilesPerVersion:
		return manifest.CarryPlan{}, fmt.Errorf("%w: an upload declares %d files and a version "+
			"holds at most %d", ErrInvalid, n, artifacts.MaxFilesPerVersion)
	}
	declared := make([]manifest.Declared, n)
	for i, e := range req.Entries {
		declared[i] = manifest.Declared{Path: e.Path, SHA256: e.SHA256, Size: e.Size, ContentType: e.ContentType}
	}
	if p == purposeAsset {
		return planAssets(declared)
	}
	held := map[objstore.Hash]objstore.Object{}
	if p == purposePublish {
		found, err := s.reads.HeldObjects(ctx, id, manifest.Hashes(declared))
		if err != nil {
			return manifest.CarryPlan{}, fmt.Errorf("artifactupload: read what artifact %s holds: %w", id, err)
		}
		held = found
	}
	plan, err := manifest.PlanCarry(declared, held)
	if err != nil {
		return manifest.CarryPlan{}, classify(err)
	}
	var total int64
	for _, d := range plan.Upload {
		if err := authorPath(d.Path); err != nil {
			return manifest.CarryPlan{}, err
		}
		// THE FILE'S OWN BOUNDS, through the one entry constructor, so an
		// intent refuses before a byte moves exactly what the version would.
		probe := objstore.Object{Key: objstore.KeyAt(s.now()), Hash: d.SHA256, Size: d.Size}
		if _, err := manifest.NewEntry(d.Path, d.ContentType, probe, false); err != nil {
			return manifest.CarryPlan{}, classify(err)
		}
		total += d.Size
	}
	for _, e := range plan.Carried {
		if err := authorPath(e.Path); err != nil {
			return manifest.CarryPlan{}, err
		}
	}
	switch {
	case len(plan.Upload) > artifacts.MaxFilesPerPublish:
		return manifest.CarryPlan{}, fmt.Errorf("%w: the upload moves %d files and a publish "+
			"uploads at most %d; carry the files the artifact holds", ErrInvalid,
			len(plan.Upload), artifacts.MaxFilesPerPublish)
	case total > artifacts.MaxPublishBytes:
		return manifest.CarryPlan{}, fmt.Errorf("%w: the upload moves %d bytes and a publish "+
			"uploads at most %d", ErrInvalid, total, int64(artifacts.MaxPublishBytes))
	}
	return plan, nil
}

// planAssets holds an asset upload's files to the asset store's bounds. An
// asset is never carried: its object is its own upload's.
func planAssets(declared []manifest.Declared) (manifest.CarryPlan, error) {
	var plan manifest.CarryPlan
	seen := map[string]bool{}
	for _, d := range declared {
		name, err := manifest.CleanPath(d.Path)
		if err != nil {
			return manifest.CarryPlan{}, classify(err)
		}
		if seen[name] {
			return manifest.CarryPlan{}, fmt.Errorf("%w: %q is declared twice", ErrInvalid, name)
		}
		seen[name] = true
		if !d.SHA256.Valid() || d.Size < 0 {
			return manifest.CarryPlan{}, fmt.Errorf("%w: %q declares digest %q and %d bytes",
				ErrInvalid, name, d.SHA256, d.Size)
		}
		typ, err := mediatype.Resolve(name, d.ContentType)
		if err != nil {
			return manifest.CarryPlan{}, fmt.Errorf("%w: %w", ErrMediaType, err)
		}
		limit := int64(artifacts.MaxAssetBinaryBytes)
		if typ.Text {
			limit = artifacts.MaxAssetTextBytes
		}
		if d.Size > limit {
			return manifest.CarryPlan{}, fmt.Errorf("%w: asset %q is %d bytes and the maximum is %d",
				ErrInvalid, name, d.Size, limit)
		}
		plan.Upload = append(plan.Upload, manifest.Declared{Path: name, SHA256: d.SHA256,
			Size: d.Size, ContentType: typ.Served})
	}
	return plan, nil
}

// authorPath refuses an author's file under the pipeline's own folder.
func authorPath(p string) error {
	if strings.HasPrefix(strings.ToLower(p), VendorPrefix) {
		return fmt.Errorf("%w: %q is under %q, where the engine copies the CDN files a page "+
			"references; publish the file under another folder", ErrInvalid, p, VendorPrefix)
	}
	return nil
}

// intentFor is an upload's answer: how each file still to arrive is
// transferred. A presigned put is signed afresh, never past the upload's
// window.
func (s *Service) intentFor(ctx context.Context, r *record) (artifacts.UploadIntent, error) {
	now := s.now()
	if !r.Expires.After(now) {
		return artifacts.UploadIntent{}, fmt.Errorf("%w: %s", ErrUploadExpired, r.ID)
	}
	out := artifacts.UploadIntent{Upload: r.ID, ExpiresAt: r.Expires, Carried: r.Carried}
	for _, f := range r.Files {
		if f.State != filePending {
			continue
		}
		switch f.Transport {
		case transportDirect:
			term := min(objstore.PresignTerm(f.Size), r.Expires.Sub(now))
			signed, err := s.presign.PresignPut(ctx, f.Key, objstore.PresignRequest{
				Size: f.Size, ContentType: f.ContentType, SHA256: f.SHA256, Expires: term,
			})
			if err != nil {
				return artifacts.UploadIntent{}, fmt.Errorf("artifactupload: sign the put of %q: %w", f.Path, err)
			}
			out.Transfers = append(out.Transfers, artifacts.Transfer{Path: f.Path, Method: signed.Method,
				URL: signed.URL, Headers: signed.Headers, Direct: true})
		case transportEngine:
			out.Transfers = append(out.Transfers, artifacts.Transfer{Path: f.Path, Method: http.MethodPut,
				URL: TransferPath(r.ID, f.Path)})
		}
	}
	return out, nil
}

// TransferPath is the API path an engine-streamed file is put to, relative to
// the API's own origin: `/artifacts/uploads/<upload>/<path>`, each segment
// escaped. The API mounts the route this names.
func TransferPath(upload, path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return "/artifacts/uploads/" + url.PathEscape(upload) + "/" + strings.Join(segments, "/")
}

// Upload is what an upload is for, as a surface re-decides the caller's
// authority on a transfer before the bytes move.
type Upload struct {
	ID string

	// Artifact is the artifact it is for, zero for a create.
	Artifact artifacts.ID

	// Asset reports an upload into the asset store.
	Asset bool

	ExpiresAt time.Time
}

// Lookup is the caller's own upload: another principal's answers
// [ErrUploadNotFound], exactly as one that never existed.
func (s *Service) Lookup(ctx context.Context, c artifactsdomain.Call, upload string) (Upload, error) {
	principal, err := principalOf(c)
	if err != nil {
		return Upload{}, err
	}
	r, _, err := s.ownRecord(ctx, principal, upload)
	if err != nil {
		return Upload{}, err
	}
	out := Upload{ID: r.ID, Asset: r.Purpose == purposeAsset, ExpiresAt: r.Expires}
	if r.Artifact != "" {
		if out.Artifact, err = artifacts.ParseID(r.Artifact); err != nil {
			return Upload{}, fmt.Errorf("artifactupload: upload %s names artifact %q: %w", r.ID, r.Artifact, err)
		}
	}
	return out, nil
}

// principalOf is the name a call's reservations are kept under: the stamped
// actor's, which is the seat handle or the machine login a record names.
func principalOf(c artifactsdomain.Call) (string, error) {
	name := strings.TrimSpace(c.Authority.Actor.Name)
	if name == "" || !c.Authority.Actor.Kind.Valid() {
		return "", fmt.Errorf("%w: the call names no actor: every reservation is held by one",
			ErrInvalid)
	}
	return name, nil
}

// requestDigest is what an intent asks, in one string: what an idempotent
// retry must ask again to be answered the same upload.
func requestDigest(id artifacts.ID, req artifacts.UploadRequest) (string, error) {
	raw, err := json.Marshal(struct {
		ID  string                  `json:"id"`
		Req artifacts.UploadRequest `json:"req"`
	}{id.String(), req})
	if err != nil {
		return "", fmt.Errorf("artifactupload: digest the upload request: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func carriedPaths(plan manifest.CarryPlan) []string {
	out := make([]string, 0, len(plan.Carried))
	for _, e := range plan.Carried {
		out = append(out, e.Path)
	}
	return out
}

func declaredPaths(plan manifest.CarryPlan) map[string]string {
	out := map[string]string{}
	for i, d := range plan.Upload {
		out[fmt.Sprintf("path %d", i)] = d.Path
	}
	return out
}

// classify answers a refusal of the manifest grammar or the media table as
// this package's own.
func classify(err error) error {
	switch {
	case errors.Is(err, mediatype.ErrRefused):
		return fmt.Errorf("%w: %w", ErrMediaType, err)
	case errors.Is(err, manifest.ErrInvalid), errors.Is(err, manifest.ErrReserved):
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return err
}
