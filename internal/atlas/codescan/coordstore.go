package codescan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
)

// Scans is the coordination store's bucket of scan standings
// (coord.AtlasCodeScans), as this package reads it: every standing filed
// under the host it is read from, a Gitlet repository's under
// coord.AtlasCodeGitlet and a forge's under its kind.
type Scans interface {
	AtlasCodeScans(ctx context.Context, host string) (map[string][]byte, error)
	AtlasCodeScan(ctx context.Context, host, repo string) ([]byte, bool, error)
	PutAtlasCodeScan(ctx context.Context, host, repo string, value []byte) error
	DeleteAtlasCodeScan(ctx context.Context, host, repo string) error
}

// encode is a repository's standing as stored. Every text it holds is
// bounded where it is made: a voice to an atlas key's 512 bytes, the stamp
// to a digest ([stampOf]) and the error to collect.MaxErrorLength, so its
// widest, its error escaped six bytes a byte, is 13,763 bytes of the 16,384
// it is held to (coord.SmallValueBytes; TestTheWidestCodeScanStandingFitsItsBound).
func encode(st Status) ([]byte, error) {
	value, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("codescan: encode the standing of %s: %w", st.Name, err)
	}
	return value, nil
}

// stampOf is what a standing keeps of a forge listing's stamp
// ([collect.CodeRepository.Stamp]): its SHA-256, since a stamp is only ever
// compared, and the vendor's own text has no bound (a branch's name in it).
// None for none.
func stampOf(stamp string) string {
	if stamp == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(stamp))
	return hex.EncodeToString(sum[:])
}

// CoordStore is each Gitlet repository's standing on the fleet's
// coordination store, filed under coord.AtlasCodeGitlet by its id.
type CoordStore struct{ scans Scans }

// NewCoordStore wraps the fleet's standings, refusing a nil backend: a nil
// wrapped is a crash on the first pass.
func NewCoordStore(scans Scans) (*CoordStore, error) {
	if scans == nil {
		return nil, errors.New("codescan: NewCoordStore needs the fleet's coordination backend (scans is nil), " +
			"which is where each repository's standing is recorded")
	}
	return &CoordStore{scans: scans}, nil
}

// Load reads every Gitlet repository's standing, by its id. One this build
// cannot decode is skipped: the repository is read again and its standing
// rewritten.
func (s *CoordStore) Load(ctx context.Context) (map[string]Status, error) {
	raw, err := s.scans.AtlasCodeScans(ctx, coord.AtlasCodeGitlet)
	if err != nil {
		return nil, fmt.Errorf("codescan: read the scan standings: %w", err)
	}
	out := make(map[string]Status, len(raw))
	for repo, value := range raw {
		if st, ok := decode(ctx, repo, value); ok {
			out[repo] = st
		}
	}
	return out, nil
}

// Get reads one repository's standing by its key, false where none is
// recorded or none this build can decode.
func (s *CoordStore) Get(ctx context.Context, repo string) (Status, bool, error) {
	value, found, err := s.scans.AtlasCodeScan(ctx, coord.AtlasCodeGitlet, repo)
	if err != nil {
		return Status{}, false, fmt.Errorf("codescan: read the standing of %s: %w", repo, err)
	}
	if !found {
		return Status{}, false, nil
	}
	st, ok := decode(ctx, repo, value)
	return st, ok, nil
}

// decode is repo's standing as recorded, false where this build cannot read
// it.
func decode(ctx context.Context, repo string, value []byte) (Status, bool) {
	var st Status
	if err := json.Unmarshal(value, &st); err != nil {
		log.WarnContext(ctx, "atlas_code_standing_undecodable", "repo", repo, "error", err,
			"detail", "this repository is read again and its standing rewritten")
		return Status{}, false
	}
	// THE KEY IS AUTHORITATIVE, not the field inside the document.
	st.Repo = repo
	return st, true
}

// Save records one repository's standing.
func (s *CoordStore) Save(ctx context.Context, st Status) error {
	if st.Repo == "" {
		return errors.New("codescan: cannot record a standing for no repository")
	}
	value, err := encode(st)
	if err != nil {
		return err
	}
	if err := s.scans.PutAtlasCodeScan(ctx, coord.AtlasCodeGitlet, st.Repo, value); err != nil {
		return fmt.Errorf("codescan: record the standing of %s: %w", st.Repo, err)
	}
	return nil
}

// Forget drops a repository's standing.
func (s *CoordStore) Forget(ctx context.Context, repo string) error {
	if err := s.scans.DeleteAtlasCodeScan(ctx, coord.AtlasCodeGitlet, repo); err != nil {
		return fmt.Errorf("codescan: forget the standing of %s: %w", repo, err)
	}
	return nil
}
