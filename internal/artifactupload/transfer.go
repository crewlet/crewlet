package artifactupload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/objstore"
)

// Transfer is the engine-streamed leg: the bytes of one file of the caller's
// upload, put under the key its intent minted.
//
// # A name is put once
//
// The file is CLAIMED on its upload's record before a byte is read (one
// compare-and-set, so two nodes handed the same file cannot both stream it),
// and the put is checked against what the intent declared as it completes: a
// stream of any other length or digest is deleted at once, nothing being able
// to name it, and the file may be sent again. A put the store failed leaves
// nothing behind, and the claim is given back. A file already stored is
// refused ([ErrUploadConsumed]), so no second put of its key ever runs, and a
// file the intent sends straight to the bucket is not taken here. A node that
// dies mid-stream leaves its file claimed, since nothing can say how much of
// the put landed: the upload's other files go on, that one is sent in a new
// upload, and the old one's hold covers whatever the dead put stored.
//
// size is the length the caller was told the body has, or -1 where it was
// not: a known size that is not the declared one is refused before a byte is
// read. The put runs no later than the upload's window plus the file's own
// pace, which the reservation's hold already covers.
func (s *Service) Transfer(ctx context.Context, c artifactsdomain.Call, upload, path string,
	body io.Reader, size int64) error {

	err := s.transfer(ctx, c, upload, path, body, size)
	if err != nil && refused(err) {
		s.countRefusal(err)
	}
	return err
}

func (s *Service) transfer(ctx context.Context, c artifactsdomain.Call, upload, path string,
	body io.Reader, size int64) error {

	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	if _, _, err := s.ownRecord(ctx, principal, upload); err != nil {
		return err
	}
	p, err := manifest.CleanPath(path)
	if err != nil {
		return classify(err)
	}
	var (
		claimed file
		expires time.Time
	)
	_, err = s.updateRecord(ctx, upload, func(r *record) error {
		switch {
		case !r.Expires.After(s.now()):
			return fmt.Errorf("%w: %s", ErrUploadExpired, upload)
		case r.State != stateOpen:
			return fmt.Errorf("%w: upload %s is claimed by a publish", ErrUploadConsumed, upload)
		}
		i := r.fileAt(p)
		if i < 0 {
			return fmt.Errorf("%w: upload %s declares no file %q", ErrInvalid, upload, p)
		}
		f := &r.Files[i]
		switch {
		case f.Transport != transportEngine:
			return fmt.Errorf("%w: %q of upload %s goes straight to the bucket on its signed URL",
				ErrInvalid, p, upload)
		case f.State == fileStreaming:
			return fmt.Errorf("%w: %q of upload %s is being transferred already", ErrUploadConsumed, p, upload)
		case f.State != filePending:
			return fmt.Errorf("%w: %q of upload %s has arrived already", ErrUploadConsumed, p, upload)
		case size >= 0 && size != f.Size:
			return &UploadMismatch{Path: p, Reason: fmt.Sprintf("the body is %d bytes and the upload "+
				"declared %d", size, f.Size)}
		}
		f.State = fileStreaming
		claimed, expires = *f, r.Expires
		return nil
	})
	if err != nil {
		return err
	}

	want := claimed.object()
	// THE PUT ENDS NO LATER THAN ITS HOLD ASSUMED: the window, and the
	// file's own pace for a transfer begun at its end.
	putCtx, cancel := context.WithDeadline(ctx, expires.Add(objstore.PaceFor(claimed.Size)))
	defer cancel()
	stored, err := s.objects.PutMinted(putCtx, claimed.Key, body, claimed.Size,
		objstore.PutMeta{ContentType: claimed.ContentType})
	next := fileStored
	switch {
	case errors.Is(err, objstore.ErrTooLarge):
		next, err = filePending, &UploadMismatch{Path: p, Reason: fmt.Sprintf("the body is longer than "+
			"the %d bytes the upload declared", claimed.Size)}
	case err != nil:
		next = filePending
	case stored != want:
		// STORED, AND NOT WHAT WAS DECLARED: nothing can name these bytes,
		// and no other put of the key can be running (the claim), so they
		// go now rather than wait a day for the collector.
		next = filePending
		reason := fmt.Sprintf("the body hashes to %s over %d bytes and the upload declared %s over %d",
			stored.Hash, stored.Size, claimed.SHA256, claimed.Size)
		if derr := s.objects.Delete(context.WithoutCancel(ctx), claimed.Key); derr != nil {
			// THE CLAIM STAYS: a put that might find the bytes still there
			// would be a second put of the key.
			next = fileStreaming
			log.WarnContext(ctx, "artifact_upload_delete_failed", "upload", upload,
				"key", claimed.Key.String(), "error", derr.Error())
		}
		err = &UploadMismatch{Path: p, Reason: reason}
	}
	if next == fileStored {
		s.countBytes(stored.Size, "engine")
	}
	if _, uerr := s.updateRecord(context.WithoutCancel(ctx), upload, func(r *record) error {
		if i := r.fileAt(p); i >= 0 && r.Files[i].State == fileStreaming {
			r.Files[i].State = next
		}
		return nil
	}); uerr != nil {
		// THE CLAIM COULD NOT BE SETTLED: the file stays claimed, which
		// refuses a second transfer rather than allowing one beside bytes
		// whose state nobody recorded, and the upload's hold covers it
		// until it lapses.
		return errors.Join(err, fmt.Errorf("artifactupload: record the transfer of %q: %w", p, uerr))
	}
	return err
}
