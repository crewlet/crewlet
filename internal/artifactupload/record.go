package artifactupload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE UPLOAD RECORD: one per upload, in the fleet's coordination store, saying
// which keys the engine minted for it, how far each file got, and whether a
// publish has claimed it. Every change is one compare-and-set on the record, so
// two nodes taking a transfer of one file, or a finalize of one upload, cannot
// both win.

// recordVersion is the record's format, written into every record so a later
// build can tell what it is reading.
const recordVersion = 1

// purpose is what an upload is for.
type purpose string

const (
	// purposeCreate is an upload for a new artifact's first version.
	purposeCreate purpose = "create"

	// purposePublish is an upload for a new version of an artifact.
	purposePublish purpose = "publish"

	// purposeAsset is an upload into an artifact's asset store.
	purposeAsset purpose = "asset"

	// purposeStage is the pipeline's own reservation for the objects a
	// publish writes itself (edited text, linted pages, copied CDN files,
	// a tool's inline content) where no upload carries them.
	purposeStage purpose = "stage"
)

// recordState is how far an upload got.
type recordState string

const (
	// stateOpen takes transfers, and a finalize may claim it.
	stateOpen recordState = "open"

	// stateFinalizing is claimed by one publish (its operation key): a
	// retry of that publish may continue it, and nothing else may use it.
	stateFinalizing recordState = "finalizing"

	// stateClosed is used: its outcome answers a retry of the publish that
	// used it.
	stateClosed recordState = "closed"
)

// transport is how a file's bytes reach the store.
type transport string

const (
	transportEngine    transport = "engine"
	transportDirect    transport = "direct"
	transportGenerated transport = "generated"
)

// fileState is how far one file got.
type fileState string

const (
	// filePending holds no bytes the engine knows of: an engine-streamed
	// file not yet transferred, or a direct one the client may be putting.
	filePending fileState = "pending"

	// fileStreaming is a transfer in flight through the engine, claimed so
	// no second put of the key can start beside it.
	fileStreaming fileState = "streaming"

	// fileStored holds its bytes: an engine put that completed, or a
	// direct put a finalize read back.
	fileStored fileState = "stored"

	// fileNamed is named by an applied record: its bytes are the domain's
	// to count, and its reservation is released.
	fileNamed fileState = "named"

	// fileDeleted was deleted by the engine, which knew no put of it could
	// land after: its reservation is released.
	fileDeleted fileState = "deleted"
)

// file is one key an upload minted.
type file struct {
	Path        string        `json:"path"`
	ContentType string        `json:"content_type"`
	SHA256      objstore.Hash `json:"sha256"`
	Size        int64         `json:"size"`
	Key         objstore.Key  `json:"key"`
	Transport   transport     `json:"transport"`
	State       fileState     `json:"state"`
}

// object is the object the file's row would name.
func (f file) object() objstore.Object {
	return objstore.Object{Key: f.Key, Hash: f.SHA256, Size: f.Size}
}

// held reports whether the file's bytes are reserved: anything but named or
// deleted.
func (f file) held() bool { return f.State != fileNamed && f.State != fileDeleted }

// outcome is what the publish that closed an upload answered, which a retry of
// that publish is answered with.
type outcome struct {
	Published artifactsdomain.Published `json:"published"`
	Warnings  []lint.Finding            `json:"warnings,omitempty"`
}

// record is one upload.
type record struct {
	V int `json:"v"`

	ID        string  `json:"id"`
	Principal string  `json:"principal"`
	Purpose   purpose `json:"purpose"`

	// Artifact is the artifact the upload is for, empty for a create.
	Artifact string `json:"artifact,omitempty"`

	// TurnID is the agent turn that began it, for the per-turn guardrails.
	TurnID string `json:"turn,omitempty"`

	Created time.Time `json:"created"`

	// Expires ends the transfer window: no transfer starts after it and no
	// finalize claims the upload after it.
	Expires time.Time `json:"expires"`

	State       recordState `json:"state"`
	FinalizeKey string      `json:"finalize_key,omitempty"`

	// Claimed is when the run holding the finalize took it, zero once that
	// run ended: what keeps a second run of the same operation from racing
	// the first ([claimLease]).
	Claimed time.Time `json:"claimed,omitzero"`
	Outcome *outcome  `json:"outcome,omitempty"`

	// Carried are the declared paths the artifact already held, which an
	// intent answered without a transfer.
	Carried []string `json:"carried,omitempty"`

	Files []file `json:"files"`
}

// fileAt is the index of the upload's file at path, -1 for none.
func (r *record) fileAt(path string) int {
	return slices.IndexFunc(r.Files, func(f file) bool { return f.Path == path })
}

// heldBytes is every byte the record's files still hold.
func (r *record) heldBytes() int64 {
	var n int64
	for _, f := range r.Files {
		if f.held() {
			n += f.Size
		}
	}
	return n
}

// recordAttempts bounds one gesture's compare-and-set rounds on one record: a
// record is contended only by the transfers of its own files and the publish
// that finalizes it, so sixteen rounds lost in a row is a store that is not
// answering rather than a race.
const recordAttempts = 16

// errLostRecord is a record write that lost every round.
var errLostRecord = errors.New("artifactupload: the upload record kept changing under this write; try again")

// readRecord is the upload's record and the revision it was read at, nil for
// none.
func (s *Service) readRecord(ctx context.Context, id string) (*record, uint64, error) {
	raw, rev, err := s.res.UploadRecord(ctx, id)
	if err != nil {
		return nil, 0, fmt.Errorf("artifactupload: read upload %s: %w", id, err)
	}
	if raw == nil {
		return nil, 0, nil
	}
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, 0, fmt.Errorf("artifactupload: upload %s's record is not one this build reads: %w", id, err)
	}
	if r.V > recordVersion {
		return nil, 0, fmt.Errorf("artifactupload: upload %s was recorded by a newer build (format %d); "+
			"finish it on a node running that build", id, r.V)
	}
	return &r, rev, nil
}

// writeRecord writes r at rev (0 creating it), answering the revision written
// and zero for a lost race.
func (s *Service) writeRecord(ctx context.Context, r *record, rev uint64) (uint64, error) {
	r.V = recordVersion
	raw, err := json.Marshal(r)
	if err != nil {
		return 0, fmt.Errorf("artifactupload: encode upload %s: %w", r.ID, err)
	}
	wrote, err := s.res.SwapUploadRecord(ctx, r.ID, raw, rev)
	if err != nil {
		return 0, fmt.Errorf("artifactupload: write upload %s: %w", r.ID, err)
	}
	return wrote, nil
}

// updateRecord applies change to the upload's current record and writes it,
// reading again on a lost race. change may refuse, which writes nothing. It
// answers the record as written.
func (s *Service) updateRecord(ctx context.Context, id string, change func(*record) error) (*record, error) {
	for range recordAttempts {
		r, rev, err := s.readRecord(ctx, id)
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, fmt.Errorf("%w: %s", ErrUploadNotFound, id)
		}
		if err := change(r); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return nil, err
		}
		wrote, err := s.writeRecord(ctx, r, rev)
		if err != nil {
			return nil, err
		}
		if wrote != 0 {
			return r, nil
		}
	}
	return nil, errLostRecord
}

// ownRecord is the principal's upload: another principal's, or none, is
// [ErrUploadNotFound] alike, so an upload id tells a caller nothing about an
// upload that is not theirs.
func (s *Service) ownRecord(ctx context.Context, principal, id string) (*record, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: no upload is named", ErrUploadNotFound)
	}
	r, _, err := s.readRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil || r.Principal != principal {
		return nil, fmt.Errorf("%w: %s", ErrUploadNotFound, id)
	}
	return r, nil
}
