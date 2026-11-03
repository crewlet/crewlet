package code_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A BUNDLE IS LAID OUT FROM THE FILES A GIT WROTE: each section's size from
// its file and the object count from the index's fan-out — its LAST entry,
// which counts every object, never the first, which counts those whose id
// begins 00 — under the id and the checksum its writer names, with the object
// unnamed until the bytes are stored. A file missing fails it, naming which.
func TestLayBundleReadsTheThreeFilesAGitWrote(t *testing.T) {
	t.Parallel()
	packPath, b := written(t)
	ref, err := code.LayBundle("p1", packPath, b.Checksum)
	if err != nil {
		t.Fatalf("lay out the bundle of a pack git wrote: %v", err)
	}
	want := code.PackRef{PackID: "p1", Size: int64(len(b.Bytes)), IndexBytes: b.IndexBytes,
		ReverseBytes: b.ReverseBytes, GitChecksum: b.Checksum, Objects: b.Objects}
	if *ref != want {
		t.Errorf("LayBundle = %+v, want %+v (the pack's header counts %d objects)", *ref, want, b.Objects)
	}
	if ref.PackBytes() != b.PackBytes {
		t.Errorf("the laid-out pack section is %d bytes and git wrote %d", ref.PackBytes(), b.PackBytes)
	}

	rev := strings.TrimSuffix(packPath, ".pack") + ".rev"
	if err := os.Remove(rev); err != nil {
		t.Fatal(err)
	}
	if _, err := code.LayBundle("p1", packPath, b.Checksum); err == nil || !strings.Contains(err.Error(), ".rev") {
		t.Errorf("a bundle with no .rev was laid out (%v), want a refusal naming the .rev", err)
	}
}

// AN INDEX'S HEADER IS READ ONCE, and what cannot be read is refused naming
// the file: the count is the fan-out's last entry and the file is left at the
// first object id, where the host's id stream reads on; a header cut short, a
// bad magic, another version, a count past 1<<31 and no file at all are each
// refused, with no file left open.
func TestOpenIdxReadsTheCountAndRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	packPath, b := written(t)
	idx := strings.TrimSuffix(packPath, ".pack") + ".idx"
	count, f, err := code.OpenIdx(idx)
	if err != nil {
		t.Fatalf("open the index of a pack git wrote: %v", err)
	}
	first := make([]byte, 20)
	_, err = io.ReadFull(f, first)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(idx)
	if err != nil {
		t.Fatal(err)
	}
	const names = 8 + 256*4 // the header and the fan-out; the ids follow
	if count != b.Objects || !bytes.Equal(first, body[names:names+20]) {
		t.Errorf("OpenIdx counted %d objects (the pack holds %d) and left the file at %x, want the "+
			"first id %x", count, b.Objects, first, body[names:names+20])
	}

	header := func(magic []byte, version, last uint32) []byte {
		h := append(slices.Clone(magic), binary.BigEndian.AppendUint32(nil, version)...)
		for range 255 {
			h = binary.BigEndian.AppendUint32(h, 0)
		}
		return binary.BigEndian.AppendUint32(h, last)
	}
	magic := []byte{0xff, 't', 'O', 'c'}
	dir := t.TempDir()
	for _, tc := range []struct {
		name, says string
		body       []byte
	}{
		{"a header cut short", "header", magic},
		{"a bad magic", "not a version-2 pack index", header([]byte("PACK"), 2, 3)},
		{"another version", "not a version-2 pack index", header(magic, 3, 3)},
		{"a count past 1<<31", "past 1<<31", header(magic, 2, 1<<31+1)},
		{"no file", "no such file", nil},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".idx")
		if tc.body != nil {
			if err := os.WriteFile(path, tc.body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		count, f, err := code.OpenIdx(path)
		if f != nil {
			_ = f.Close()
		}
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.says) || f != nil {
			t.Errorf("%s: OpenIdx = %d, %v, %v; want a refusal naming %s and saying %q, with no file",
				tc.name, count, f, err, path, tc.says)
		}
	}
}

// EVERY NODE NAMES A COMPACTION'S OUTPUTS ALIKE, IN EVERY BUILD: an output's
// id is a pure function of the compaction's id and the output's place — no
// clock, no randomness, nothing of the node — so the build that names the
// outputs and the committer that checks them agree without asking each other.
// The literals were computed apart from this package (SHA-256 over an empty
// name, the compaction's id and the place, each closed by a zero byte, laid
// out as a UUIDv7 at the compaction id's millisecond), so a change to what
// the digest takes, in what order, or at which instant fails here rather than
// as two builds of one rolling upgrade refusing each other's outputs. An id
// that carries no instant names its outputs at the Unix epoch, and as alike.
func TestEveryNodeNamesACompactionsOutputsAlike(t *testing.T) {
	t.Parallel()
	const compaction = "01a1753a-6eab-7a1b-8c3d-4e5f60718293" // minted 2026-10-25T22:42:06.123Z
	for _, tc := range []struct {
		compaction string
		n          int
		want       string
	}{
		{compaction, 0, "01a1753a-6eab-701b-adcd-1ecc70e47bd6"},
		{compaction, 1, "01a1753a-6eab-762c-ba84-773ed87f5671"},
		{compaction, 15, "01a1753a-6eab-747b-84a4-9e5810e6756b"},
		{"", 0, "00000000-0000-73d0-89f6-dc618e4cfdbb"},
		{"not-an-operation-id", 0, "00000000-0000-790a-939f-64b799c4a4ee"},
	} {
		if got := code.DeriveOutputPackID(tc.compaction, tc.n); got != tc.want {
			t.Errorf("output %d of compaction %q is named %s, want %s", tc.n, tc.compaction, got, tc.want)
		}
	}
}

// A COMPACTION'S OUTPUTS TAKE IDS OF THEIR OWN, AT ITS INSTANT: one id a
// place, and none another compaction's, each in the shape every stored pack's
// id has — a bare UUIDv7 in its canonical spelling, as a push's — and
// carrying the instant the compaction's id carries, never the clock of the
// call that derives it.
func TestACompactionsOutputsTakeIDsOfTheirOwnAtItsInstant(t *testing.T) {
	t.Parallel()
	minted := time.Date(2026, 10, 25, 22, 42, 6, 123_000_000, time.UTC)
	named := map[string]string{}
	for _, compaction := range []string{statelog.NewOpID(minted, ""), statelog.NewOpID(minted, "")} {
		for n := range 3 {
			id := code.DeriveOutputPackID(compaction, n)
			what := fmt.Sprintf("output %d of compaction %s", n, compaction)
			if other, taken := named[id]; taken {
				t.Errorf("%s is named %s, as %s is", what, id, other)
			}
			named[id] = what
			u, err := uuid.Parse(id)
			at, carries := statelog.OpMintedAt(id)
			if err != nil || u.Version() != 7 || u.String() != id || !carries || !at.Equal(minted) {
				t.Errorf("%s is named %q (minted %v, %t), want a UUIDv7 in its canonical spelling "+
					"minted at the compaction's %v", what, id, at, carries, minted)
			}
		}
	}
}

// stored is a store over memory, and the backend that holds its bytes.
func stored(t *testing.T) (*objstore.Store, *memobj.Backend) {
	t.Helper()
	backend := memobj.New()
	store, err := objstore.NewStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	return store, backend
}

// A BUNDLE IS STORED AS IT WAS LAID OUT: the three files back to back, under
// the bundle's content type, read back whole through the store's own check,
// and the row it would name holds to the seal every installing cache reads.
func TestStoreBundleStoresWhatItLaysOut(t *testing.T) {
	t.Parallel()
	packPath, b := written(t)
	store, _ := stored(t)
	ref, err := code.LayBundle("p1", packPath, b.Checksum)
	if err != nil {
		t.Fatal(err)
	}
	if err := code.StoreBundle(t.Context(), store, packPath, ref, nil); err != nil {
		t.Fatalf("store the bundle of a pack git wrote: %v", err)
	}
	if ref.Object.IsZero() || ref.Hash != objstore.HashOf(b.Bytes) {
		t.Fatalf("the stored bundle is named %s with digest %s, want a key and %s", ref.Object, ref.Hash,
			objstore.HashOf(b.Bytes))
	}
	r, err := store.Open(t.Context(), ref.Content())
	if err != nil {
		t.Fatalf("open the stored bundle: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("read the stored bundle: %v", err)
	}
	if !bytes.Equal(got, b.Bytes) {
		t.Errorf("the store holds %d bytes that are not the %d of the pack, index and reverse index "+
			"back to back", len(got), len(b.Bytes))
	}
	if err := code.CheckBundle(*ref, mustSeal(t, *ref, got)); err != nil {
		t.Errorf("the row the stored bundle names is not the one its bytes describe: %v", err)
	}
}

// mustSeal is the seal of a bundle's whole bytes.
func mustSeal(t *testing.T, ref code.PackRef, body []byte) code.BundleSeal {
	t.Helper()
	s, err := seal(t, ref, body, len(body))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A BUNDLE ITS OWN SEAL REFUSES is stored and never named: the reverse index's
// magic is the case because only the seal reads it — every size still matches
// what the object count says. The bytes are in the store, under the key the
// refusal names for whoever logs it, the row the writer holds is left naming
// nothing, and the refusal is the seal's (ErrBadBundle) rather than a store
// that failed.
func TestStoreBundleRefusesASealItsRuleRefuses(t *testing.T) {
	t.Parallel()
	packPath, b := written(t)
	store, backend := stored(t)
	ref, err := code.LayBundle("p1", packPath, b.Checksum)
	if err != nil {
		t.Fatal(err)
	}
	rev := strings.TrimSuffix(packPath, ".pack") + ".rev"
	if err := os.Chmod(rev, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(rev, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{'X'}, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	laid := *ref

	err = code.StoreBundle(t.Context(), store, packPath, ref, nil)
	var bad *code.BadBundle
	if !errors.As(err, &bad) || !errors.Is(err, code.ErrBadBundle) ||
		!strings.Contains(err.Error(), "the reverse index section begins") {
		t.Fatalf("storing a bundle whose reverse index is damaged answered %v, want a *BadBundle "+
			"naming the reverse index", err)
	}
	if *ref != laid {
		t.Errorf("a refused bundle left the row naming %+v, want it as laid out, %+v", *ref, laid)
	}
	info, err := backend.Stat(t.Context(), bad.Object.Name())
	if err != nil || info.Size != ref.Size {
		t.Errorf("the refused bundle's bytes under %s: %+v, %v; want the %d bytes it stored", bad.Object,
			info, err, ref.Size)
	}
}

// recording is a Putter that reads what it is handed `chunk` bytes a Read, as
// a backend takes its parts, noting how many bytes it had before each one,
// and keeps it in memory — or answers refuse, having read nothing.
type recording struct {
	chunk  int
	refuse error

	got         []byte
	before      []int64
	deadline    time.Time
	hasDeadline bool
	limit       int64
	meta        objstore.PutMeta
}

func (p *recording) Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error) {
	p.deadline, p.hasDeadline = ctx.Deadline()
	p.limit, p.meta = limit, m
	if p.refuse != nil {
		return objstore.Object{}, p.refuse
	}
	buf := make([]byte, p.chunk)
	for {
		p.before = append(p.before, int64(len(p.got)))
		n, err := r.Read(buf)
		p.got = append(p.got, buf[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return objstore.Object{}, err
		}
	}
	return objstore.Object{Key: objstore.KeyAt(time.Now()), Hash: objstore.HashOf(p.got),
		Size: int64(len(p.got))}, nil
}

// THE STORE IS HANDED THE BUNDLE UNDER ITS OWN BOUND: the put runs under
// UploadDeadline of the bundle's size, limited at that size and typed as a
// bundle; the hit is asked before EVERY read the store makes, at the bytes
// handed over so far, and an error from it ends the upload as that error with
// nothing named; and a store with no room is answered as the store said it
// (objstore.Full), for the caller to name the room.
func TestStoreBundleHandsTheStoreTheBundleUnderItsBound(t *testing.T) {
	t.Parallel()
	packPath, b := written(t)
	lay := func() *code.PackRef {
		ref, err := code.LayBundle("p1", packPath, b.Checksum)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}

	ref := lay()
	p := &recording{chunk: 1000}
	var hits []int64
	began := time.Now()
	err := code.StoreBundle(t.Context(), p, packPath, ref, func(_ context.Context, at int64) error {
		hits = append(hits, at)
		return nil
	})
	ended := time.Now()
	if err != nil {
		t.Fatalf("store through a recording store: %v", err)
	}
	if !bytes.Equal(p.got, b.Bytes) || p.limit != ref.Size || p.meta.ContentType != code.BundleContentType {
		t.Errorf("the store was handed %d bytes limited at %d as %q, want the %d-byte bundle at its size "+
			"as %q", len(p.got), p.limit, p.meta.ContentType, len(b.Bytes), code.BundleContentType)
	}
	bound := code.UploadDeadline(ref.Size)
	if !p.hasDeadline || p.deadline.Before(began.Add(bound)) || p.deadline.After(ended.Add(bound)) {
		t.Errorf("the put ran under a deadline of %v (set: %v), want %v from its start", p.deadline,
			p.hasDeadline, bound)
	}
	if !slices.Equal(hits, p.before) || len(hits) < 3 || hits[len(hits)-1] != ref.Size {
		t.Errorf("the hit was asked at %v, and the store read at %v: want one hit before every read, "+
			"at the bytes handed over, the last at the bundle's %d", hits, p.before, ref.Size)
	}

	// Past the first read and inside the bundle, whose index alone is over a
	// kilobyte (code.IndexBytesFor).
	const failAt = 1000
	injected := errors.New("injected")
	ref, p = lay(), &recording{chunk: 1000}
	laid := *ref
	err = code.StoreBundle(t.Context(), p, packPath, ref, func(_ context.Context, at int64) error {
		if at >= failAt {
			return injected
		}
		return nil
	})
	if !errors.Is(err, injected) || *ref != laid || int64(len(p.got)) >= ref.Size {
		t.Errorf("a hit that failed at %d bytes ended the upload with %v after %d bytes, the row %+v; "+
			"want the hit's error, the upload stopped and nothing named", failAt, err, len(p.got), *ref)
	}

	full := &objstore.Full{Remedy: "the bucket is given room", Err: fmt.Errorf("the provider's own answer")}
	ref = lay()
	err = code.StoreBundle(t.Context(), &recording{chunk: 1000, refuse: full}, packPath, ref, nil)
	var answered *objstore.Full
	if !errors.As(err, &answered) || answered.Remedy != full.Remedy || !errors.Is(err, objstore.ErrFull) {
		t.Errorf("a store with no room was answered %v, want its own *objstore.Full", err)
	}
}
