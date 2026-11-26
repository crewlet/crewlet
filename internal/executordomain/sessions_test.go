package executordomain_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// sessionStore is a session store over an in-memory object store and a
// keyring of the given keys, the first active.
func sessionStore(t *testing.T, ids ...string) (*executordomain.SessionStore, *memobj.Backend) {
	t.Helper()
	at := func() time.Time { return time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC) }
	backend := memobj.NewAt(at)
	objects, err := objstore.NewStoreAt(backend, at)
	if err != nil {
		t.Fatal(err)
	}
	return over(t, objects, ids...), backend
}

// over is a session store over objects, under a keyring of the given keys, the
// first active.
func over(t *testing.T, objects *objstore.Store, ids ...string) *executordomain.SessionStore {
	t.Helper()
	ring := secrets.Keyring{ActiveID: ids[0], Keys: map[string][]byte{}}
	for _, id := range ids {
		ring.Keys[id] = bytes.Repeat([]byte(id[len(id)-1:]), 32)
	}
	sealer, err := secrets.NewStreamSealer(ring)
	if err != nil {
		t.Fatal(err)
	}
	store, err := executordomain.NewSessionStore(objects, sealer)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// archive is a session archive of n random bytes carrying a marker.
func archive(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return append([]byte("transcript-marker:"), b...)
}

// readAll opens and reads a session whole.
func readAll(t *testing.T, store *executordomain.SessionStore, s executordomain.Session) ([]byte, error) {
	t.Helper()
	r, err := store.Open(t.Context(), s)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

// storedBytes is what the backend holds under name, read past every check.
//
// UNDER A DEADLINE, which every backend's read refuses to go without
// (objstore.CheckRead): a stream with none could hang on a stalled store.
func storedBytes(t *testing.T, backend *memobj.Backend, name string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	body, err := backend.Get(ctx, name, 0, -1)
	if err != nil {
		t.Fatalf("read %s off the backend: %v", name, err)
	}
	defer func() { _ = body.Close() }()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read %s off the backend: %v", name, err)
	}
	return out
}

// A SESSION IS SEALED AT REST AND OPENS ONLY WHERE ITS ROW NAMES IT: the stored
// bytes hold nothing of the archive in the clear, and the object named by
// another turn, seat or object key is ErrUnreadable, since the associated data
// binds it to `executor/<seat>/<turn>/<object key>`.
//
// Mutation: leave the turn out of the associated data and the object opens
// under another turn's row.
func TestASessionOpensOnlyWhereItsRowNamesIt(t *testing.T) {
	t.Parallel()
	store, backend := sessionStore(t, "k1")
	turn := statelog.NewOpID(time.Now(), "")
	plain := archive(t, 3*secrets.StreamChunkBytes+17)
	put, err := store.Put(t.Context(), "ana", turn, io.NopCloser(bytes.NewReader(plain)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if put.ContentKey != "k1" || put.Object.Size != secrets.SealedStreamSize(int64(len(plain)), "k1") {
		t.Fatalf("Put answered %+v, want the object sealed under k1 at its sealed size", put)
	}
	stored := storedBytes(t, backend, put.Object.Key.Name())
	if bytes.Contains(stored, []byte("transcript-marker:")) {
		t.Fatal("the stored object carries the archive in the clear")
	}
	own := executordomain.Session{Handle: "ana", Turn: turn, Object: put.Object}
	got, err := readAll(t, store, own)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("the session read back (%d bytes, %v), want the archive", len(got), err)
	}
	// THE SAME BYTES UNDER ANOTHER KEY: what a row naming another capture of the
	// same seat and turn would open, were the object's own key not bound in.
	moved := objstore.KeyAt(time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC))
	if err := backend.Put(t.Context(), moved.Name(), bytes.NewReader(stored), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]executordomain.Session{
		"another turn": {Handle: "ana", Turn: statelog.NewOpID(time.Now(), ""), Object: put.Object},
		"another seat": {Handle: "ben", Turn: turn, Object: put.Object},
		"another capture's key": {Handle: "ana", Turn: turn, Object: objstore.Object{
			Key: moved, Hash: put.Object.Hash, Size: put.Object.Size}},
	} {
		if _, err := readAll(t, store, other); !errors.Is(err, executordomain.ErrUnreadable) {
			t.Errorf("the object named by %s: %v, want ErrUnreadable", name, err)
		}
	}
}

// stalled is an archive whose reads block until it is closed: a box's stream
// that stopped sending, which no check of a context between reads interrupts.
type stalled struct {
	reading, closed chan struct{}
	once, entered   sync.Once
}

func newStalled() *stalled {
	return &stalled{reading: make(chan struct{}), closed: make(chan struct{})}
}

func (s *stalled) Read([]byte) (int, error) {
	s.entered.Do(func() { close(s.reading) })
	<-s.closed
	return 0, errors.New("the stream was closed under the read")
}

func (s *stalled) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// A PUT ENDS WITH ITS CONTEXT EVEN WHILE THE ARCHIVE IS BLOCKED: the caller
// giving up closes the archive, which is what ends a read blocked on it, so
// the sealing goroutine Put owns never outlives the call.
//
// Mutation: drop the context.AfterFunc that closes the archive and Put waits
// on its goroutine for ever, blocked in the archive's read.
func TestAPutEndsWithItsContextWhileTheArchiveIsBlocked(t *testing.T) {
	t.Parallel()
	store, _ := sessionStore(t, "k1")
	archive := newStalled()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-archive.reading
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, "ana", statelog.NewOpID(time.Now(), ""), archive)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a put whose archive never finished answered success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Put outlived its context while the archive's read was blocked")
	}
}

// A SESSION NOBODY CAN READ IS "NO SESSION", NOT A FAILED LAUNCH: an object the
// store no longer holds, a row naming none, and one sealed under a key the
// keyring has dropped are all ErrUnreadable, which a restorer starts fresh on.
//
// Mutation: return the object store's ErrNotFound unwrapped and a restorer
// fails the launch over a session the collector already took.
func TestASessionNobodyCanReadIsNoSession(t *testing.T) {
	t.Parallel()
	backend := memobj.NewAt(time.Now)
	objects, err := objstore.NewStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	store := over(t, objects, "k1")
	turn := statelog.NewOpID(time.Now(), "")
	put, err := store.Put(t.Context(), "ana", turn, io.NopCloser(bytes.NewReader(archive(t, 100))))
	if err != nil {
		t.Fatal(err)
	}
	row := executordomain.Session{Handle: "ana", Turn: turn, Object: put.Object}
	// THE SAME STORE, the old key dropped from the ring.
	rotated := over(t, objects, "k2")
	if _, err := readAll(t, rotated, row); !errors.Is(err, executordomain.ErrUnreadable) {
		t.Errorf("a session under a dropped key: %v, want ErrUnreadable", err)
	}
	if _, err := readAll(t, store, executordomain.Session{Handle: "ana", Turn: turn}); !errors.Is(err,
		executordomain.ErrUnreadable) {
		t.Errorf("a row naming no object: %v, want ErrUnreadable", err)
	}
	if err := backend.Delete(t.Context(), put.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(t, store, row); !errors.Is(err, executordomain.ErrUnreadable) {
		t.Errorf("a collected object: %v, want ErrUnreadable", err)
	}
}

// AN ARCHIVE PAST ITS BOUND IS REFUSED WHOLE, never stored cut: a cut archive
// is a session no harness resumes.
//
// Mutation: drop the limit from SessionStore.seal and the oversized archive is
// stored.
func TestAnArchivePastItsBoundIsRefused(t *testing.T) {
	t.Parallel()
	store, _ := sessionStore(t, "k1")
	big := io.NopCloser(io.LimitReader(zeros{}, executorkeep.SessionMaxBytes+1))
	if _, err := store.Put(t.Context(), "ana", statelog.NewOpID(time.Now(), ""), big); !errors.Is(err,
		executorkeep.ErrSessionTooLarge) {
		t.Fatalf("an archive a byte past its bound: %v, want ErrSessionTooLarge", err)
	}
}

// zeros is an endless reader of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// THE CAP IS STATED ON SEALED BYTES AND HOLDS ONE SESSION AT ITS BOUND under the
// longest key id a header can name, which is what every upload is limited to
// and what the object store's pacing pin reads.
func TestTheSealedCapHoldsASessionAtItsBound(t *testing.T) {
	t.Parallel()
	if executorkeep.SealedMaxBytes < executorkeep.SessionMaxBytes {
		t.Fatalf("the sealed cap %d is below the archive's own %d",
			executorkeep.SealedMaxBytes, executorkeep.SessionMaxBytes)
	}
	if got := secrets.SealedStreamSize(executorkeep.SessionMaxBytes,
		string(bytes.Repeat([]byte("k"), secrets.MaxStreamKeyIDBytes))); got != executorkeep.SealedMaxBytes {
		t.Fatalf("an archive at its bound seals to %d and the cap is %d", got,
			executorkeep.SealedMaxBytes)
	}
}

// THE SEAT BUDGET'S FLOOR HOLDS ONE SESSION AT ITS BOUND, AND NO MORE: a budget
// Tier B accepts never refuses outright a capture the session bound admits, a
// budget that holds one is never refused at boot (the floor is that session
// rounded up to a whole mebibyte), and the default is no smaller than the floor.
// internal/config cannot import this package, so they are held together here.
//
// Mutation: raise SessionMaxBytes to 64 MiB without moving the config floor,
// raise the floor to 64 MiB, or the default to 94 MiB, and this fails.
func TestTheSeatBudgetFloorHoldsOneSessionAtItsBound(t *testing.T) {
	t.Parallel()
	if config.SessionBytesPerSeatFloor < executorkeep.SealedMaxBytes {
		t.Errorf("max_bytes_per_seat accepts %d bytes and one sealed session is up to %d",
			config.SessionBytesPerSeatFloor, executorkeep.SealedMaxBytes)
	}
	if past := config.SessionBytesPerSeatFloor - executorkeep.SealedMaxBytes; past >= 1<<20 {
		t.Errorf("max_bytes_per_seat's floor of %d bytes is %d past one sealed session at its bound, "+
			"so it refuses budgets that hold one", config.SessionBytesPerSeatFloor, past)
	}
	if config.DefaultSessionBytesPerSeat < config.SessionBytesPerSeatFloor {
		t.Errorf("the default budget of %d bytes is below the field's own floor of %d",
			config.DefaultSessionBytesPerSeat, config.SessionBytesPerSeatFloor)
	}
	// AT THE DEFAULT, A CAPTURE AT THE KEPT BOUND MEETS THE SEAT'S REFUSAL: the
	// kept bound admits more than the default budget holds, so the budget,
	// never the bound, is what a seat's large reads are kept within
	// (sandbox's TestACaptureAtTheKeptBoundIsHeldToItsSeatsBudget).
	if config.DefaultSessionBytesPerSeat >= executorkeep.SessionKeptMaxBytes {
		t.Errorf("the default budget of %d bytes holds a capture at the kept bound (%d), which its doc says "+
			"the budget refuses", config.DefaultSessionBytesPerSeat, executorkeep.SessionKeptMaxBytes)
	}
}

// A SESSION SEALED TO A SPOOL UPLOADS AS THE OBJECT ITS SEAL MINTED, AND READS
// BACK AS THE ARCHIVE: the spool on a node's disk holds nothing in the clear,
// the upload stores its size under the key the seal minted and bound in, the
// object opens only under its own seat and turn, and an archive past its bound
// is refused at the seal, before any upload.
//
// Mutation: spool in the clear, mint the upload's key anew, or let the seal pass
// an oversized archive, and a check goes red.
func TestASessionSealedToASpoolUploadsAsTheObjectItsSealMinted(t *testing.T) {
	t.Parallel()
	store, backend := sessionStore(t, "k1")
	turn := statelog.NewOpID(time.Now(), "")
	plain := archive(t, 2*secrets.StreamChunkBytes+5)
	var spool bytes.Buffer
	sealed, err := store.Seal("ana", turn, bytes.NewReader(plain), &spool)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(spool.Bytes(), []byte("transcript-marker:")) {
		t.Fatal("the spool carries the archive in the clear")
	}
	if sealed.Size != int64(spool.Len()) || sealed.ContentKey != "k1" ||
		sealed.Size != secrets.SealedStreamSize(int64(len(plain)), "k1") {
		t.Fatalf("the seal answered %+v for a spool of %d bytes", sealed, spool.Len())
	}
	spooled := bytes.Clone(spool.Bytes())
	put, err := store.Upload(t.Context(), sealed, &spool)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if put.Object.Key.Name() != sealed.Key.Name() || put.Object.Size != sealed.Size || put.ContentKey != "k1" {
		t.Fatalf("the upload answered %+v, want the key the seal minted, at the spool's size", put)
	}
	if stored := storedBytes(t, backend, put.Object.Key.Name()); !bytes.Equal(stored, spooled) {
		t.Fatal("the object holds other bytes than the spool")
	}
	got, err := readAll(t, store, executordomain.Session{Handle: "ana", Turn: turn, Object: put.Object})
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("the session read back (%d bytes, %v), want the archive", len(got), err)
	}
	if _, err := readAll(t, store, executordomain.Session{Handle: "ben", Turn: turn,
		Object: put.Object}); !errors.Is(err, executordomain.ErrUnreadable) {
		t.Errorf("the object named by another seat: %v, want ErrUnreadable", err)
	}
	if _, err := store.Seal("ana", turn, io.LimitReader(zeros{}, executorkeep.SessionMaxBytes+1),
		io.Discard); !errors.Is(err, executorkeep.ErrSessionTooLarge) {
		t.Errorf("an archive a byte past its bound was sealed: %v, want ErrSessionTooLarge", err)
	}
}

// A STORED SESSION OPENS ONLY FOR ITS OWN ROW: the object is checked against
// the row by the object store and opened under the row's own associated data,
// so it is ErrUnreadable for another turn's, seat's or object's row, whatever
// bytes it holds. A row naming an object the store no longer holds is
// ErrUnreadable at once.
//
// Mutation: open the object under an associated data that leaves the turn or
// the seat out, or skip the object store's check, and a row goes red.
func TestAStoredSessionOpensOnlyForItsOwnRow(t *testing.T) {
	t.Parallel()
	store, _ := sessionStore(t, "k1")
	turn := statelog.NewOpID(time.Now(), "")
	plain := archive(t, 2*secrets.StreamChunkBytes+5)
	put, err := store.Put(t.Context(), "ana", turn, io.NopCloser(bytes.NewReader(plain)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	own := executordomain.Session{Handle: "ana", Turn: turn, Object: put.Object}
	if got, err := readAll(t, store, own); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("the session opened for its own row read (%d bytes, %v), want the archive", len(got), err)
	}
	gone := objstore.Object{Key: objstore.KeyAt(time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)),
		Hash: put.Object.Hash, Size: put.Object.Size}
	for name, other := range map[string]executordomain.Session{
		"another turn":   {Handle: "ana", Turn: statelog.NewOpID(time.Now(), ""), Object: put.Object},
		"another seat":   {Handle: "ben", Turn: turn, Object: put.Object},
		"another object": {Handle: "ana", Turn: turn, Object: gone},
	} {
		if _, err := readAll(t, store, other); !errors.Is(err, executordomain.ErrUnreadable) {
			t.Errorf("the session opened for %s: %v, want ErrUnreadable", name, err)
		}
	}
}

// A SUMMARY COPIED UNDER A FRESH KEY OPENS AS THE SAME WORDS, UNDER ITS OWN: the
// copy is a new object, sealed again under the turn and its new key, so the
// row naming it opens it and the one naming the old key still opens the old;
// a summary gone copies nothing and says it is unreadable.
//
// Mutation: copy the sealed bytes as they are, and the copy does not open
// under its own key.
func TestASummaryCopiedUnderAFreshKeyOpensAsTheSameWords(t *testing.T) {
	t.Parallel()
	store, backend := sessionStore(t, "k1")
	turn := statelog.NewOpID(time.Now(), "")
	const words = "## Next Move\n\nRerun the cache test."
	put, err := store.PutSummary(t.Context(), "ana", turn, bytes.NewReader([]byte(words)))
	if err != nil {
		t.Fatalf("PutSummary: %v", err)
	}
	copied, err := store.CopySummary(t.Context(), "ana", turn, put.Object)
	if err != nil || copied.Object.Key == put.Object.Key || copied.ContentKey != "k1" {
		t.Fatalf("CopySummary answered %+v (%v), want a new object sealed under k1", copied, err)
	}
	for name, object := range map[string]objstore.Object{"the copy": copied.Object, "the original": put.Object} {
		body, err := store.OpenSummary(t.Context(), executordomain.Session{Handle: "ana", Turn: turn,
			Summary: &executorkeep.Summary{Object: object}})
		if err != nil {
			t.Fatalf("%s does not open: %v", name, err)
		}
		got, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil || string(got) != words {
			t.Errorf("%s reads %q (%v), want the summary's words", name, got, err)
		}
	}
	if err := backend.Delete(t.Context(), put.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CopySummary(t.Context(), "ana", turn, put.Object); !errors.Is(err,
		executordomain.ErrUnreadable) {
		t.Errorf("a summary gone copied as %v, want ErrUnreadable", err)
	}
}
