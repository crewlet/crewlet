package artifactupload

import (
	"context"
	"encoding/json"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// HeldBytes is every byte the reservation ledger holds at s's clock.
func HeldBytes(ctx context.Context, s *Service) (int64, error) {
	raw, _, err := s.res.UploadLedger(ctx)
	if err != nil || raw == nil {
		return 0, err
	}
	var l ledger
	if err := json.Unmarshal(raw, &l); err != nil {
		return 0, err
	}
	return l.totals("", s.now()).companyBytes, nil
}

// UploadState is how far an upload got, by its record: its state and each
// file's.
func UploadState(ctx context.Context, s *Service, upload string) (string, map[string]string, error) {
	r, _, err := s.readRecord(ctx, upload)
	if err != nil || r == nil {
		return "", nil, err
	}
	files := map[string]string{}
	for _, f := range r.Files {
		files[f.Path] = string(f.State)
	}
	return string(r.State), files, nil
}

// UploadKey is the key an upload minted for one of its files.
func UploadKey(ctx context.Context, s *Service, upload, path string) (string, error) {
	r, _, err := s.readRecord(ctx, upload)
	if err != nil || r == nil {
		return "", err
	}
	if i := r.fileAt(path); i >= 0 {
		return r.Files[i].Key.String(), nil
	}
	return "", nil
}

// Signed reports whether a binary type has a signature its bytes are held to.
func Signed(essence string) bool {
	_, ok := signatures[essence]
	return ok
}

// ClaimUpload claims an upload for a finalize under c's operation key, as a
// publish's first step does.
func ClaimUpload(ctx context.Context, s *Service, c artifactsdomain.Call, upload string, id artifacts.ID) error {
	_, err := s.claim(ctx, c, c.Authority.Actor.Name, upload, purposePublish, id)
	return err
}

// Brings is [run.brings] over a version whose files are new (true) or carried
// (false) from the version before.
func Brings(files map[string]bool, path string) bool {
	r := &run{files: map[string]*pending{}}
	for p, isNew := range files {
		r.files[p] = &pending{path: p, carried: !isNew}
	}
	return r.brings(path)
}

// ReasonOf is the refusals counter's reason for err.
func ReasonOf(err error) string { return reasonOf(err) }
