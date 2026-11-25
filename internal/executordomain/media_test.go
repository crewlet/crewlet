package executordomain_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// textFrame is b as a text frame of a session's text object.
func textFrame(b []byte) []byte {
	out := binary.AppendUvarint([]byte{executorkeep.FrameText}, uint64(len(b)))
	return append(out, b...)
}

// mediaFrame is the frame naming the member digest is the SHA-256 of.
func mediaFrame(digest string) []byte {
	raw, err := hex.DecodeString(digest)
	if err != nil {
		panic(err)
	}
	return append([]byte{executorkeep.FrameMedia}, raw...)
}

// keepMedia seals member for handle's seat to a spool and uploads it, as a
// capture keeps a media member.
func keepMedia(t *testing.T, store *executordomain.SessionStore, handle string,
	member []byte) executorkeep.MediaObject {

	t.Helper()
	var spool bytes.Buffer
	sealed, err := store.SealMedia(handle, bytes.NewReader(member), &spool)
	if err != nil {
		t.Fatalf("SealMedia: %v", err)
	}
	if sum := sha256.Sum256(member); sealed.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("the seal names digest %s, and the member's is %x", sealed.Digest, sum)
	}
	if bytes.Contains(spool.Bytes(), member[:64]) {
		t.Fatal("the media spool carries the member in the clear")
	}
	kept, err := store.UploadMedia(t.Context(), sealed, &spool)
	if err != nil {
		t.Fatalf("UploadMedia: %v", err)
	}
	return kept
}

// keepFramed stores frames as the text object of handle's turn, naming media.
func keepFramed(t *testing.T, store *executordomain.SessionStore, handle, turn string, frames []byte,
	media ...executorkeep.MediaObject) executordomain.Session {

	t.Helper()
	put, err := store.Put(t.Context(), handle, turn, io.NopCloser(bytes.NewReader(frames)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return executordomain.Session{Handle: handle, Turn: turn, Object: put.Object, Media: media}
}

// AN OPEN SPLICES EVERY MEDIA WHERE ITS FRAME STANDS: a session whose row names
// media is stored as text frames and media frames, and Open hands out the
// archive the take wrote, byte for byte — each member in its frame's place,
// as often as frames name it, whatever order the row lists them in and however
// small the reads that drain it. A member's object is sealed under
// `executor-media/<seat>/<object key>`, never in the clear, so a later turn of
// the seat carrying it opens it too.
//
// Mutation: skip a media frame, splice a member once however often it is
// framed, or hand out a frame's head, and the archive differs.
func TestAnOpenSplicesEveryMediaWhereItsFrameStands(t *testing.T) {
	t.Parallel()
	store, _ := sessionStore(t, "k1")
	pdf, image := archive(t, 2*secrets.StreamChunkBytes+11), archive(t, secrets.StreamChunkBytes+3)
	texts := [][]byte{archive(t, 700), archive(t, secrets.StreamChunkBytes+1), nil, archive(t, 5)}
	keptPDF, keptImage := keepMedia(t, store, "ana", pdf), keepMedia(t, store, "ana", image)
	frames := slices.Concat(textFrame(texts[0]), mediaFrame(keptPDF.Digest), textFrame(texts[1]),
		mediaFrame(keptImage.Digest), textFrame(texts[2]), mediaFrame(keptPDF.Digest), textFrame(texts[3]))
	want := slices.Concat(texts[0], pdf, texts[1], image, texts[2], pdf, texts[3])

	for _, turn := range []string{statelog.NewOpID(time.Now(), ""), statelog.NewOpID(time.Now(), "")} {
		row := keepFramed(t, store, "ana", turn, frames, keptImage, keptPDF)
		if got, err := readAll(t, store, row); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the spliced session of turn %s read back %d bytes (%v), want the %d the take wrote",
				turn, len(got), err, len(want))
		}
		r, err := store.Open(t.Context(), row)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(iotest.OneByteReader(r))
		_ = r.Close()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the spliced session read a byte at a time came back %d bytes (%v)", len(got), err)
		}
	}
	whole := keepFramed(t, store, "ana", statelog.NewOpID(time.Now(), ""), want)
	if got, err := readAll(t, store, whole); err != nil || !bytes.Equal(got, want) {
		t.Errorf("a session kept whole read back %d bytes (%v), want it as it was put", len(got), err)
	}
}

// A MISSING MEDIA OBJECT IS UNREADABLE, at the read that reaches its frame: the
// collector or a lost store took it, and the session is no session, which a
// restorer reads as a fresh start (LossRestoreFailed) rather than hand its
// harness an archive with a hole. What the read handed out before the frame
// is provisional, as any open's is until io.EOF.
//
// Mutation: answer a missing member as an empty one, or return the object
// store's ErrNotFound unwrapped, and it reads otherwise.
func TestAMissingMediaIsUnreadable(t *testing.T) {
	t.Parallel()
	store, backend := sessionStore(t, "k1")
	member := archive(t, 1000)
	kept := keepMedia(t, store, "ana", member)
	row := keepFramed(t, store, "ana", statelog.NewOpID(time.Now(), ""),
		slices.Concat(textFrame([]byte("before")), mediaFrame(kept.Digest)), kept)
	if err := backend.Delete(t.Context(), kept.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}
	got, err := readAll(t, store, row)
	if !errors.Is(err, executordomain.ErrUnreadable) || !strings.Contains(err.Error(), kept.Digest) {
		t.Fatalf("a session whose media is gone: %v, want ErrUnreadable naming the media", err)
	}
	if !bytes.Equal(got, []byte("before")) {
		t.Errorf("the read handed out %q before the gone media, want what stood before its frame", got)
	}
}

// MEDIA SEALED FOR ANOTHER SEAT DOES NOT OPEN: the associated data binds a
// media object to its seat and its own key, so a row naming another seat's
// media, or a media object's bytes copied under another key, is ErrUnreadable.
//
// Mutation: leave the seat or the key out of MediaAAD, and a row opens.
func TestMediaSealedForAnotherSeatDoesNotOpen(t *testing.T) {
	t.Parallel()
	store, backend := sessionStore(t, "k1")
	member := archive(t, 1000)
	theirs := keepMedia(t, store, "ben", member)
	row := keepFramed(t, store, "ana", statelog.NewOpID(time.Now(), ""), mediaFrame(theirs.Digest), theirs)
	if _, err := readAll(t, store, row); !errors.Is(err, executordomain.ErrUnreadable) {
		t.Errorf("another seat's media: %v, want ErrUnreadable", err)
	}
	ours := keepMedia(t, store, "ana", member)
	moved := objstore.KeyAt(time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC))
	if err := backend.Put(t.Context(), moved.Name(), bytes.NewReader(storedBytes(t, backend,
		ours.Object.Key.Name())), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	copied := ours
	copied.Object.Key = moved
	row = keepFramed(t, store, "ana", statelog.NewOpID(time.Now(), ""), mediaFrame(copied.Digest), copied)
	if _, err := readAll(t, store, row); !errors.Is(err, executordomain.ErrUnreadable) {
		t.Errorf("a media object's bytes under another key: %v, want ErrUnreadable", err)
	}
}

// MEDIA WHOSE BYTES ARE NOT ITS DIGEST IS UNREADABLE: a frame names a member by
// its SHA-256, and the open holds the member it spliced to it once read
// whole, so a row pairing a digest with another member's object never hands a
// harness bytes the take did not write.
//
// Mutation: skip the digest's check at the member's end, and the other member
// is spliced in.
func TestMediaWhoseBytesAreNotItsDigestIsUnreadable(t *testing.T) {
	t.Parallel()
	store, _ := sessionStore(t, "k1")
	kept := keepMedia(t, store, "ana", archive(t, 1000))
	other := sha256.Sum256([]byte("another member"))
	misnamed := kept
	misnamed.Digest = hex.EncodeToString(other[:])
	row := keepFramed(t, store, "ana", statelog.NewOpID(time.Now(), ""), mediaFrame(misnamed.Digest), misnamed)
	if _, err := readAll(t, store, row); !errors.Is(err, executordomain.ErrUnreadable) {
		t.Errorf("media whose bytes are not its digest: %v, want ErrUnreadable", err)
	}
}

// A FRAME NAMING MEDIA THE ROW DOES NOT IS UNREADABLE, as is a text object whose
// frames do not parse: a frame of a kind no build writes, or frames cut inside
// one. The row's media set is what the collector keeps, so a member it does not
// name may be gone, and a cut frame is a session the take did not write.
//
// Mutation: open any member a frame names by its digest alone, or read a cut
// frame as the frames' end, and a case reads.
func TestAFrameNamingMediaTheRowDoesNotIsUnreadable(t *testing.T) {
	t.Parallel()
	store, _ := sessionStore(t, "k1")
	kept, unnamed := keepMedia(t, store, "ana", archive(t, 1000)), keepMedia(t, store, "ana", archive(t, 900))
	for name, frames := range map[string][]byte{
		"a member the row does not name": slices.Concat(mediaFrame(kept.Digest), mediaFrame(unnamed.Digest)),
		"a frame no build writes":        slices.Concat(textFrame([]byte("text")), []byte("X")),
		"a text frame cut short":         textFrame([]byte("text"))[:4],
		"a media frame cut short":        mediaFrame(kept.Digest)[:20],
	} {
		row := keepFramed(t, store, "ana", statelog.NewOpID(time.Now(), ""), frames, kept)
		if _, err := readAll(t, store, row); !errors.Is(err, executordomain.ErrUnreadable) {
			t.Errorf("%s: %v, want ErrUnreadable", name, err)
		}
	}
}

// A MEDIA MEMBER PAST THE KEPT BOUND IS REFUSED WHOLE: one capture keeps at most
// executorkeep.SessionKeptMaxBytes in all, so no single member is sealed past
// it.
//
// Mutation: seal media under the text object's bound, and a member above it is
// refused that the kept bound holds; drop the bound, and the oversized one is
// sealed.
func TestAMediaMemberPastTheKeptBoundIsRefused(t *testing.T) {
	t.Parallel()
	store, _ := sessionStore(t, "k1")
	if _, err := store.SealMedia("ana", io.LimitReader(zeros{}, executorkeep.SessionMaxBytes+1),
		io.Discard); err != nil {
		t.Errorf("a member past the text object's bound and within the kept one was refused: %v", err)
	}
	if _, err := store.SealMedia("ana", io.LimitReader(zeros{}, executorkeep.SessionKeptMaxBytes+1),
		io.Discard); !errors.Is(err, executorkeep.ErrSessionTooLarge) {
		t.Errorf("a member a byte past the kept bound: %v, want ErrSessionTooLarge", err)
	}
}
