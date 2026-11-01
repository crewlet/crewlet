package s3obj_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/aws/smithy-go"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/worklet"
)

// part is the part size every case here runs the backend at.
//
// SIXTY-FOUR KIBIBYTES rather than the production [s3obj.PartBytes], because
// every branch of a multipart upload — one put below a part, equal parts
// above it, the abort at a part's end or inside one — turns on a body's size
// RELATIVE to the part, and at eight mebibytes the cases moved hundreds of
// mebibytes through the fake bucket to reach them. The fake does not enforce
// S3's five-mebibyte floor on a part, so the small size is accepted here; the
// production size and that floor are pinned on their own
// ([TestEveryFileTheTrackerKeepsFitsOneUploadOfPartsS3Takes]).
const part = 64 << 10

// open is the backend over srv at the parts every case here runs at.
func open(t *testing.T, srv *s3fake.Server, prefix string) *s3obj.Backend {
	t.Helper()
	b := openFake(t, srv, prefix)
	s3obj.CutParts(b, part)
	return b
}

// openFake is the backend over srv as [s3obj.Open] builds it, at the
// production [s3obj.PartBytes] — what the benchmarks measure.
func openFake(t testing.TB, srv *s3fake.Server, prefix string) *s3obj.Backend {
	t.Helper()
	b, err := s3obj.Open(t.Context(), fakeConfig(srv, prefix))
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	return b
}

// fakeConfig is how a backend reaches srv: the bucket it serves, path-style,
// at the region the fake ignores, with credentials it never checks.
func fakeConfig(srv *s3fake.Server, prefix string) s3obj.Config {
	return s3obj.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: srv.Bucket(), Prefix: prefix,
		PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	}
}

// EVERY FILE THE TRACKER KEEPS FITS ONE UPLOAD OF PARTS S3 TAKES — the
// production part size, held where the cases above run at another.
//
// Eight mebibytes: at or above S3's five-mebibyte floor on every part but the
// last (a part under it is refused when the upload is completed, after every
// byte has been sent), and with S3's ten thousand parts an upload holds eighty
// gibibytes, past the largest file a project keeps. A part size that broke
// either would fail only against a real bucket, and the fake this suite runs
// against enforces neither.
func TestEveryFileTheTrackerKeepsFitsOneUploadOfPartsS3Takes(t *testing.T) {
	t.Parallel()
	if s3obj.PartBytes != 8*objstore.MiB {
		t.Errorf("a part is %d bytes; the size this suite and the docs reason "+
			"about is eight mebibytes", s3obj.PartBytes)
	}
	const s3SmallestPart = 5 * objstore.MiB
	if s3obj.PartBytes < s3SmallestPart {
		t.Errorf("a part of %d bytes is under S3's %d-byte floor, so every "+
			"multipart upload is refused at completion", s3obj.PartBytes, s3SmallestPart)
	}
	if most := int64(s3obj.MaxParts) * int64(s3obj.PartBytes); most < worklet.MaxFileBytes {
		t.Errorf("one upload holds at most %d bytes and a project keeps files of %d",
			most, int64(worklet.MaxFileBytes))
	}
}

// AN S3 BUCKET PASSES THE SUITE, over the wire the SDK speaks, with listings
// that cross page boundaries and objects long enough to go up in parts.
// LastModified is to the second, so a re-put is asked about a second later.
func TestContract(t *testing.T) {
	t.Parallel()
	var servers sync.Map
	objstoretest.Run(t, func(t *testing.T) objstore.Backend {
		srv := s3fake.Start(t, "files")
		b := open(t, srv, "crewlet/")
		servers.Store(objstore.Backend(b), srv)
		return b
	}, objstoretest.Options{Piece: part, Ranged: true, BatchDeletes: true, Stalling: stallingBackend,
		// A MULTIPART UPLOAD BEGUN AND NEVER COMPLETED, under the
		// prefix: what a process killed mid-upload leaves, named by
		// what follows the prefix.
		Unfinished: func(t *testing.T, b objstore.Backend) objstore.Pending {
			srv, _ := servers.Load(b)
			return objstore.Pending{Name: "files/unfinished",
				ID: srv.(*s3fake.Server).Begin("crewlet/files/unfinished")}
		}})
}

// AN UPLOAD OUTSIDE THE PREFIX IS NOBODY'S HERE: a bucket shared by several
// companies holds each one's unfinished uploads under its own prefix, and a
// backend that listed another's would have its collector abandon them.
func TestPendingKeepsToItsPrefix(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.PageSize = 2
	b := open(t, srv, "crewlet/")
	srv.Begin("somebody-else/files/theirs")
	var want []string
	for range 5 {
		want = append(want, srv.Begin("crewlet/files/ours"))
	}
	var got []string
	if err := b.Pending(t.Context(), func(p objstore.Pending) error {
		if p.Name != "files/ours" {
			t.Errorf("Pending named %q, want files/ours", p.Name)
		}
		got = append(got, p.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Pending listed %q across pages of two, want exactly %q", got, want)
	}
}

// AN ENTRY THE BUCKET DID NOT DATE IS NEVER HANDED ON. An S3-compatible
// gateway may leave <Initiated> out of an upload listing or <LastModified> out
// of an object listing, and an entry dated at the zero time reads as older
// than every grace: the collector would abandon an upload still in flight
// under its put, or judge an object by its key's clock alone. So each listing
// hands on everything it could date and then fails with ErrUndated naming
// what it left alone.
func TestAnUndatedEntryIsNeverHandedOn(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.PageSize = 1
	b := open(t, srv, "crewlet/")

	srv.Undated("crewlet/files/in-flight")
	srv.Begin("crewlet/files/in-flight")
	dated := srv.Begin("crewlet/files/dated")
	var uploads []string
	err := b.Pending(t.Context(), func(p objstore.Pending) error {
		if p.Started.IsZero() {
			t.Errorf("Pending handed on %s with no date", p.Name)
		}
		uploads = append(uploads, p.ID)
		return nil
	})
	if !errors.Is(err, objstore.ErrUndated) {
		t.Fatalf("Pending over an undated upload = %v, want ErrUndated", err)
	}
	if !slices.Equal(uploads, []string{dated}) {
		t.Errorf("Pending handed on %q, want only the dated upload %q", uploads, dated)
	}

	for _, name := range []string{"files/a", "files/b"} {
		if err := b.Put(t.Context(), name, bytes.NewReader([]byte(name)), objstore.PutMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	srv.Undated("crewlet/files/a")
	var listed []string
	err = b.List(t.Context(), func(info objstore.Info) error {
		if info.Written.IsZero() {
			t.Errorf("List handed on %s with no date", info.Name)
		}
		listed = append(listed, info.Name)
		return nil
	})
	if !errors.Is(err, objstore.ErrUndated) {
		t.Fatalf("List over an undated object = %v, want ErrUndated", err)
	}
	if !slices.Equal(listed, []string{"files/b"}) {
		t.Errorf("List handed on %q, want only the dated object", listed)
	}
}

// A BUCKET THAT CANNOT BE REACHED FAILS THE BOOT, not the first upload.
func TestAnUnreachableBucketIsRefusedAtOpen(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	_, err := s3obj.Open(t.Context(), s3obj.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "somebody-elses", PathStyle: true,
		AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	})
	if err == nil {
		t.Fatal("a bucket the endpoint does not have was opened")
	}
	for name, cfg := range map[string]s3obj.Config{
		"no bucket":  {Region: "us-east-1"},
		"no region":  {Bucket: "files"},
		"half a key": {Bucket: "files", Region: "us-east-1", AccessKeyID: "AKIAEXAMPLE"},
	} {
		if _, err := s3obj.Open(t.Context(), cfg); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

// THE PREFIX IS THE COMPANY'S CORNER OF A SHARED BUCKET: every key is put
// under it, and a listing never hands the collector a key outside it.
func TestTheBackendKeepsToItsPrefix(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	ours, theirs := open(t, srv, "acme/"), open(t, srv, "other/")
	for _, name := range []string{"one", "two"} {
		if err := ours.Put(t.Context(), name, bytes.NewReader([]byte(name)), objstore.PutMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := theirs.Put(t.Context(), "theirs", bytes.NewReader([]byte("x")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme/one", "acme/two", "other/theirs"}; !slices.Equal(srv.Keys(), want) {
		t.Fatalf("the bucket holds %v, want %v", srv.Keys(), want)
	}
	var listed []string
	if err := ours.List(t.Context(), func(info objstore.Info) error {
		listed = append(listed, info.Name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "two"}; !slices.Equal(listed, want) {
		t.Fatalf("the prefix listed %v, want %v", listed, want)
	}
}

// AN OBJECT LONGER THAN A PART GOES UP AS A MULTIPART UPLOAD of equal parts,
// and one that fits a part as a single put — the fake counts which, so a
// backend that sent everything as one request would fail here rather than
// pass every case without uploading a part.
func TestAnObjectLongerThanAPartGoesUpInParts(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "")
	long := bytes.Repeat([]byte("in parts "), (2*part+17)/9+1)
	if err := b.Put(t.Context(), "long", iotest.HalfReader(bytes.NewReader(long)), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Parts("long"); got != 3 {
		t.Fatalf("%d bytes went up in %d parts, want 3 of at most %d", len(long), got, part)
	}
	if err := b.Put(t.Context(), "short", bytes.NewReader([]byte("one request")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Parts("short"); got != 0 {
		t.Fatalf("an object under a part went up in %d parts, want one put", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	r, err := b.Get(ctx, "long", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, long) {
		t.Fatalf("the multipart object read back %d bytes, %v", len(got), err)
	}
	if len(srv.Uploads()) != 0 {
		t.Fatalf("a completed upload is still open: %v", srv.Uploads())
	}
}

// AN UPLOAD THAT FAILS PART WAY IS ABANDONED — whether the bucket refused a
// part or the body stopped arriving — because an upload nobody completes or
// aborts is billed for its parts and shown by no listing. A body CUT SHORT is
// one that stopped, at a part's end or inside one: read as the end, it was
// completed as an object of the parts that had arrived. The part is refused
// the way the client takes as final — a throttle or a server error it would
// ask again until the part's deadline.
func TestAFailedUploadIsAbandoned(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "")
	body := bytes.Repeat([]byte{7}, 3*part)

	srv.FailPart(2, http.StatusForbidden, "AccessDenied")
	if err := b.Put(t.Context(), "refused", bytes.NewReader(body), objstore.PutMeta{}); err == nil {
		t.Fatal("an upload whose second part was refused succeeded")
	}
	srv.FailPart(0, 0, "")

	for _, stop := range []struct {
		name string
		at   int
		err  error
	}{
		{"stopped", 3 * part / 2, errors.New("the client went away")},
		{"cut-at-a-part", part, io.ErrUnexpectedEOF},
		{"cut-in-a-part", 3 * part / 2, io.ErrUnexpectedEOF},
	} {
		stopped := io.MultiReader(bytes.NewReader(body[:stop.at]), iotest.ErrReader(stop.err))
		if err := b.Put(t.Context(), stop.name, stopped, objstore.PutMeta{}); !errors.Is(err, stop.err) {
			t.Fatalf("%s: an upload whose body stopped = %v, want the body's error", stop.name, err)
		}
	}
	if open := srv.Uploads(); len(open) != 0 {
		t.Fatalf("failed uploads were left open: %v", open)
	}
	if len(srv.Keys()) != 0 {
		t.Fatalf("failed uploads left objects: %v", srv.Keys())
	}
	if held := s3obj.SparesHeld(b); held != 0 {
		t.Fatalf("failed uploads kept %d of the node's spare part buffers", held)
	}
}

// A WRITE THE PROVIDER HAS NO ROOM FOR IS REFUSED AS FULL: a bucket whose
// provider refuses a write for want of room — MinIO's drives at their floor or
// its bucket quota spent, Ceph's gateway out of space or past a quota, any
// gateway's 507 — refuses the same write the same way until an operator makes
// room, so the put is answered as the store having none ([objstore.Full], over
// [objstore.ErrFull]), naming the provider's room ([s3obj.FullRemedy]) — on
// the PutObject, on a part, on the completion and on the upload's start alike
// — asked once, storing nothing and leaving no upload open. A refusal that is
// no want of room stays what it was.
//
// Mutation: drop Put's classification and every case fails; drop a code from
// fullCodes and its case fails; read only the status and the 400 and 403 cases
// fail.
func TestAWriteTheProviderHasNoRoomForIsRefusedAsFull(t *testing.T) {
	t.Parallel()
	small, large := bytes.Repeat([]byte{7}, part/2), bytes.Repeat([]byte{7}, 3*part)
	for _, c := range []struct {
		name   string
		body   []byte
		refuse func(srv *s3fake.Server)
		code   string
		// asked is the operation the refusal answers, asked only once.
		asked string
	}{
		{"MinIO's drives at their floor", small, func(srv *s3fake.Server) {
			srv.Fill(http.StatusInsufficientStorage, "XMinioStorageFull")
		}, "XMinioStorageFull", "PutObject"},
		{"MinIO's bucket quota", small, func(srv *s3fake.Server) {
			srv.Fill(http.StatusBadRequest, "XMinioAdminBucketQuotaExceeded")
		}, "XMinioAdminBucketQuotaExceeded", "PutObject"},
		{"Ceph out of space", small, func(srv *s3fake.Server) {
			srv.Fill(http.StatusInsufficientStorage, "InsufficientCapacity")
		}, "InsufficientCapacity", "PutObject"},
		{"Ceph's quota", small, func(srv *s3fake.Server) {
			srv.Fill(http.StatusForbidden, "QuotaExceeded")
		}, "QuotaExceeded", "PutObject"},
		{"a gateway's 507 by any code", small, func(srv *s3fake.Server) {
			srv.Fill(http.StatusInsufficientStorage, "NoSpaceLeft")
		}, "NoSpaceLeft", "PutObject"},
		{"a part", large, func(srv *s3fake.Server) {
			srv.Fill(http.StatusInsufficientStorage, "XMinioStorageFull")
		}, "XMinioStorageFull", ""},
		{"the completion", large, func(srv *s3fake.Server) {
			var landed sync.WaitGroup
			landed.Add(3)
			srv.PartHook = func(int32) { landed.Done() }
			go func() {
				landed.Wait()
				srv.Fill(http.StatusInsufficientStorage, "XMinioStorageFull")
			}()
		}, "XMinioStorageFull", "CompleteMultipartUpload"},
		{"the upload's start", large, func(srv *s3fake.Server) {
			srv.FailNext(1, http.StatusInsufficientStorage, "XMinioStorageFull")
		}, "XMinioStorageFull", "CreateMultipartUpload"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := s3fake.Start(t, "files")
			b := open(t, srv, "")
			// THE PROBE'S OWN REQUESTS AT OPEN are not the put's.
			before := srv.RequestsOf(c.asked)
			c.refuse(srv)
			err := b.Put(t.Context(), "refused", bytes.NewReader(c.body), objstore.PutMeta{})
			full, ok := errors.AsType[*objstore.Full](err)
			switch {
			case !ok || !errors.Is(err, objstore.ErrFull):
				t.Fatalf("a write the provider had no room for answered %v, want an objstore.Full over ErrFull",
					err)
			case full.Remedy != s3obj.FullRemedy:
				t.Errorf("the refusal named the room %q, want the provider's, %q", full.Remedy, s3obj.FullRemedy)
			case !strings.Contains(full.Err.Error(), c.code):
				t.Errorf("the refusal kept %q, which does not carry the provider's own answer %s", full.Err, c.code)
			}
			if c.asked != "" {
				if n := srv.RequestsOf(c.asked) - before; n != 1 {
					t.Errorf("the refused %s was asked %d times, want once: no wait makes room", c.asked, n)
				}
			}
			if keys := srv.Keys(); len(keys) != 0 {
				t.Errorf("a write the provider had no room for stored %v", keys)
			}
			if open := srv.Uploads(); len(open) != 0 {
				t.Errorf("a write the provider had no room for left its upload open: %v", open)
			}
		})
	}
	t.Run("a refusal that is no want of room", func(t *testing.T) {
		t.Parallel()
		srv := s3fake.Start(t, "files")
		b := open(t, srv, "")
		srv.Fill(http.StatusForbidden, "AccessDenied")
		err := b.Put(t.Context(), "refused", bytes.NewReader(small), objstore.PutMeta{})
		if err == nil || errors.Is(err, objstore.ErrFull) {
			t.Fatalf("a put refused AccessDenied answered %v, want a failure that is no full store", err)
		}
	})
}

// A PART IS GIVEN ITS SHARE OF THE LINK. An upload's parts in flight cross ONE
// link to the bucket together, so each moves at a share of it, and the floor
// every leg is held to ([objstore.MiBPace]) is the UPLOAD's: a part given only
// its own bytes at the floor misses its deadline on a link the upload as a
// whole crosses at the floor, where one part at a time had crossed it. So
// every part — a short last one too — is given its request's [s3obj.CallBudget]
// and the time its bytes take at the floor with every part an upload may keep
// in flight sharing that link: [s3obj.PartsInFlight] of them, whenever the
// others were sent, because an upload sends parts behind one already on its
// way — its first part goes alone, and three more join it as fast as the body
// arrives.
//
// THE ARITHMETIC RATHER THAN A THROTTLED BUCKET: the budget of a part of the
// production size is sixteen and a half minutes, which no case can wait out.
func TestAPartIsGivenItsShareOfTheLink(t *testing.T) {
	t.Parallel()
	for _, size := range []int64{1, objstore.MiB / 3, objstore.MiB, s3obj.PartBytes - 1, s3obj.PartBytes} {
		atFloor := time.Duration(size*s3obj.PartsInFlight) * objstore.MiBPace / objstore.MiB
		if got, want := s3obj.PartBudget(size), s3obj.CallBudget+atFloor; got < want {
			t.Errorf("a part of %d bytes is given %v; sharing the link with %d parts in flight "+
				"at the floor its bytes take %v, and its request %v more", size, got,
				s3obj.PartsInFlight, atFloor, s3obj.CallBudget)
		}
	}
}

// A REFUSED PART ENDS ITS UPLOAD AT ONCE: once the bucket's refusal of a part
// reaches the upload, the upload cancels the parts still in flight, begins no
// further read of its body and sends no other part, and fails with the refusal
// — never a cancelled sibling's error. With parts in flight together, an
// upload that noticed a failure only when it next needed a buffer went on
// reading the client's body and sending parts behind it — up to three parts,
// minutes of a slow client's body, for an upload already lost.
//
// THE MOMENT IS SEEN FROM THE BUCKET'S SIDE: the bucket refuses part 1 and
// holds part 2 unanswered, so the hold reports when the upload cancels its
// requests — which an upload does only once it has the refusal. The body hands
// over the first two parts freely and waits at the third for that report, so
// a read the upload begins after the report is one it began knowing it had
// failed; and the refusal of part 1 is itself held back until the body has
// reached its third part AND the bucket holds part 2 with its body read
// ([s3fake.Server.PartHook]), so it arrives while the upload is reading, and
// the cancellation it causes reaches a part the bucket is watching. Released
// at the third part alone, the refusal now and then cancelled part 2 while
// its body was still on its way, which no hold ever saw, and the case waited
// out its minute (three runs in thirty). An upload that never acts on the
// refusal is never reported, and the body gives up waiting after a minute,
// so it fails this case rather than hanging it.
func TestARefusedPartEndsItsUploadAtOnce(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "")
	srv.FailPart(1, http.StatusForbidden, "AccessDenied")
	reached, held := make(chan struct{}), make(chan struct{})
	var heldOnce sync.Once
	srv.PartHook = func(number int32) {
		switch number {
		case 1:
			for _, moment := range []chan struct{}{reached, held} {
				select {
				case <-moment:
				case <-time.After(time.Minute):
				}
			}
		case 2:
			heldOnce.Do(func() { close(held) })
		}
	}
	body := &gatedBody{body: make([]byte, 8*part), at: 2 * part, gate: srv.HoldPart(2), reached: reached}

	err := b.Put(t.Context(), "refused", body, objstore.PutMeta{})
	if apiErr, ok := errors.AsType[smithy.APIError](err); !ok || apiErr.ErrorCode() != "AccessDenied" {
		t.Fatalf("an upload whose first part was refused = %v; want the refusal itself", err)
	}
	if !body.waited {
		t.Fatal("the upload heard of the refusal before it asked for its third part, so " +
			"nothing staged a refusal arriving while it reads")
	}
	if !body.opened {
		t.Fatalf("the upload read on for a minute after its first part was refused, " +
			"its other parts still in flight")
	}
	if body.after > 0 {
		t.Fatalf("the upload began %d reads of its body after it had cancelled its parts "+
			"for a refusal", body.after)
	}
	if open := srv.Uploads(); len(open) != 0 {
		t.Fatalf("the refused upload was left open: %v", open)
	}
	if held := s3obj.SparesHeld(b); held != 0 {
		t.Fatalf("the refused upload kept %d of the node's spare part buffers", held)
	}
}

// gatedBody hands over body, and at offset at closes reached — where it is
// given one — and waits for gate to close — for a minute at most, opened
// saying which ended the wait — before it hands over the rest, a kibibyte a
// read. after counts the reads begun once the wait was over: the read that
// waited was asked for before, and answers with what it was asked for.
type gatedBody struct {
	body           []byte
	at             int
	gate           <-chan struct{}
	reached        chan struct{}
	read           int
	waited, opened bool
	after          int
}

func (g *gatedBody) Read(p []byte) (int, error) {
	if g.waited {
		g.after++
	}
	if g.read == len(g.body) {
		return 0, io.EOF
	}
	if g.read < g.at {
		p = p[:min(len(p), g.at-g.read)]
	} else {
		if !g.waited {
			g.waited = true
			if g.reached != nil {
				close(g.reached)
			}
			select {
			case <-g.gate:
				g.opened = true
			case <-time.After(time.Minute):
			}
		}
		p = p[:min(len(p), 1<<10)]
	}
	n := copy(p, g.body[g.read:])
	g.read += n
	return n, nil
}

// A LARGE UPLOAD KEEPS ITS PARTS IN FLIGHT TOGETHER — exactly
// [s3obj.PartsInFlight] of them, never more — so a bucket a round trip away
// costs an upload a round trip per that many parts rather than one per part,
// and the object it assembles is the body, in order, whatever order the parts
// landed in.
//
// The bucket holds every request for its latency, so the parts an upload has
// sent overlap at the bucket for as long as the upload keeps them in flight,
// and the most at once is what the bucket counts.
func TestALargeUploadKeepsItsPartsInFlightTogether(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.Latency = 200 * time.Millisecond
	b := open(t, srv, "")
	const parts = 3 * s3obj.PartsInFlight
	body := make([]byte, parts*part)
	for i := range body {
		body[i] = byte(i / part)
	}
	if err := b.Put(t.Context(), "wide", iotest.HalfReader(bytes.NewReader(body)), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if got := srv.MostPartsAtOnce(); got != s3obj.PartsInFlight {
		t.Fatalf("the upload had at most %d parts in flight at once, want %d", got, s3obj.PartsInFlight)
	}
	if got := srv.Parts("wide"); got != parts {
		t.Fatalf("%d bytes went up in %d parts, want %d", len(body), got, parts)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	r, err := b.Get(ctx, "wide", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("the object read back %d bytes, %v, not the body in order", len(got), err)
	}
	if held := s3obj.SparesHeld(b); held != 0 {
		t.Fatalf("a finished upload kept %d of the node's spare part buffers", held)
	}
}

// UPLOADS SHARE THE NODE'S SPARE PART BUFFERS, and none waits for one: past
// its own first buffer an upload takes a spare only while the node has one
// free, so however many large uploads run at once the parts in flight never
// exceed one per upload plus [s3obj.SpareParts] — and every upload still
// finishes on the buffers it got.
func TestUploadsShareTheNodesSpareParts(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.Latency = 200 * time.Millisecond
	b := open(t, srv, "")
	// MORE UPLOADS THAN THE SPARES CAN WIDEN, so the allowance runs out
	// while every one of them still wants more.
	const uploads = s3obj.SpareParts/(s3obj.PartsInFlight-1) + 2
	body := bytes.Repeat([]byte("shared "), 2*s3obj.PartsInFlight*part/7+1)
	errs := make([]error, uploads)
	var wg sync.WaitGroup
	for i := range uploads {
		wg.Go(func() {
			errs[i] = b.Put(t.Context(), fmt.Sprintf("shared-%d", i), bytes.NewReader(body), objstore.PutMeta{})
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if most, limit := srv.MostPartsAtOnce(), int64(uploads+s3obj.SpareParts); most > limit || most <= uploads {
		t.Fatalf("%d uploads had %d parts in flight at once; want more than one each and "+
			"at most one each plus the node's %d spares, %d", uploads, most, s3obj.SpareParts, limit)
	}
	if held := s3obj.SparesHeld(b); held != 0 {
		t.Fatalf("finished uploads kept %d of the node's spare part buffers", held)
	}
	for i := range uploads {
		if got := srv.Parts(fmt.Sprintf("shared-%d", i)); got == 0 {
			t.Fatalf("upload %d was not assembled from parts", i)
		}
	}
}

// THE MEDIA TYPE IS STORED WITH THE OBJECT, whichever way it went up, so an
// operator reading the bucket with their own tools sees what the bytes are.
func TestTheContentTypeIsStored(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "acme/")
	client := httpxtest.Pool(t)
	for name, size := range map[string]int{"small": 10, "large": part + 1} {
		if err := b.Put(t.Context(), name, bytes.NewReader(make([]byte, size)),
			objstore.PutMeta{ContentType: "text/csv"}); err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, srv.URL+"/files/acme/"+name, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); got != "text/csv" {
			t.Errorf("%s is served as %q, want the type it was put with", name, got)
		}
	}
}

// AN OBJECT PUT IN PARTS IS DATED BY ITS UPLOAD'S START, as Amazon dates one —
// which is why objstore.Info's Written is read as the earliest the object
// might have been stored rather than as when it was finished. The body holds
// its last byte back until the clock has crossed into a later second than the
// upload's start, so a backend or a fake dating the completion reports that
// later second and fails here — and a fake that did would let the collector's
// tests meet a gentler S3 than Amazon's.
func TestAnObjectPutInPartsIsDatedByItsUploadsStart(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "")
	before := time.Now().UTC().Truncate(time.Second)
	body := &secondCrossingReader{r: bytes.NewReader(make([]byte, part+1)), at: part}
	if err := b.Put(t.Context(), "long", body, objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Parts("long"); got != 2 {
		t.Fatalf("%d bytes went up in %d parts, want 2: this case is about an object put in parts",
			part+1, got)
	}
	if body.resumed.IsZero() {
		t.Fatal("the upload never asked for the bytes past its first part")
	}
	info, err := b.Stat(t.Context(), "long")
	if err != nil {
		t.Fatal(err)
	}
	if info.Written.Before(before) || !info.Written.Before(body.resumed) {
		t.Fatalf("an object whose upload began in [%v, %v) and finished at or after %v is "+
			"dated %v, want its upload's start", before, body.resumed, body.resumed, info.Written)
	}
	var listed []objstore.Info
	if err := b.List(t.Context(), func(info objstore.Info) error {
		listed = append(listed, info)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].Written.Equal(info.Written) {
		t.Fatalf("the listing says %+v, want the dating Stat gave: %v", listed, info.Written)
	}
}

// secondCrossingReader reads r, and on the first read at offset at — once the
// upload holding everything before it has begun — waits for the clock to
// reach a later second than the one it was asked in, recording that second.
type secondCrossingReader struct {
	r       io.Reader
	at      int64
	read    int64
	resumed time.Time
}

func (s *secondCrossingReader) Read(p []byte) (int, error) {
	if s.read < s.at {
		p = p[:min(int64(len(p)), s.at-s.read)]
	} else if s.resumed.IsZero() {
		next := time.Now().Truncate(time.Second).Add(time.Second + 50*time.Millisecond)
		time.Sleep(time.Until(next))
		s.resumed = time.Now().UTC().Truncate(time.Second)
	}
	n, err := s.r.Read(p)
	s.read += int64(n)
	return n, err
}

// A BUCKET BEHIND A PRIVATE CERTIFICATE AUTHORITY IS REACHED THROUGH
// AWS_CA_BUNDLE, and refused without it.
//
// What the bundle reaches is [s3obj.Open]'s own client — the engine's shared
// transport with the bundle added to its roots for the bucket's requests only
// — which no other case runs, because every other case reaches its fake over
// plain http. It is also what the benchmark's https arm rests on: a bucket on
// loopback with a certificate of its own, as real S3 is reached over TLS.
//
// NOT PARALLEL, because it sets the environment, and the process's
// environment is what Open reads the bundle from.
//
// THE REFUSAL IS AT ONCE: a certificate the node does not trust is refused the
// same way every time, so it is never asked again — where every other failure
// to reach the bucket is, until the request's deadline. Asked again, the check
// at open would end at its deadline with the refusal reduced to text beside
// it, so the refused half runs under a deadline of a few seconds.
func TestABucketBehindAPrivateAuthorityIsReachedThroughItsBundle(t *testing.T) {
	srv := s3fake.StartTLS(t, "files")

	t.Setenv("AWS_CA_BUNDLE", "")
	refusedCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := s3obj.Open(refusedCtx, fakeConfig(srv, "crewlet/"))
	var unverified *tls.CertificateVerificationError
	if !errors.As(err, &unverified) {
		t.Fatalf("Open with no bundle = %v; want the bucket's certificate refused", err)
	}

	t.Setenv("AWS_CA_BUNDLE", trustBundle(t, srv))
	b, err := s3obj.Open(t.Context(), fakeConfig(srv, "crewlet/"))
	if err != nil {
		t.Fatalf("Open with the bucket's authority in AWS_CA_BUNDLE: %v", err)
	}
	data := []byte("behind a private authority")
	if err := b.Put(t.Context(), "files/tls", bytes.NewReader(data), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	r, err := b.Get(ctx, "files/tls", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back %q, %v; want %q", got, err, data)
	}
}

// trustBundle writes srv's certificate as a PEM bundle and answers its path —
// what an operator points AWS_CA_BUNDLE at for a bucket behind an authority of
// their own.
func trustBundle(t testing.TB, srv *s3fake.Server) string {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE",
		Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return bundle
}

// A DIRECTORY BUCKET LISTS IN NO ORDER, SO ITS RANGES ARE REFUSED, unasked:
// an S3 Express One Zone bucket — every one named `…--x-s3` — answers a
// listing in no order and takes no StartAfter, and a range read from it would
// have every object past the first stray read as absent. Its caller asks
// about each object instead. The bucket is a real directory bucket to the
// SDK, opened through the session it asks the fake for.
func TestADirectoryBucketsRangesAreUnordered(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files--usw2-az1--x-s3")
	b := open(t, srv, "crewlet/")
	if n := srv.RequestsOf("CreateSession"); n < 1 {
		t.Fatalf("opening a directory bucket asked for %d sessions; the SDK does not take it for one", n)
	}
	err := b.ListRange(t.Context(), "", "files/z", func(objstore.Info) error {
		t.Error("a directory bucket's range listed an object")
		return nil
	})
	if !errors.Is(err, objstore.ErrUnordered) {
		t.Fatalf("ListRange on a directory bucket = %v, want ErrUnordered", err)
	}
	if n := srv.RequestsOf("ListObjectsV2"); n != 0 {
		t.Fatalf("refusing a directory bucket's range cost %d listings, want none", n)
	}
}

// A BATCH DELETION IS ONE DeleteObjects REQUEST, carrying the Content-MD5 the
// API long required of it — the fake, as many gateways do, refuses one
// without — and a key the bucket refuses is named back by the name it was
// handed, the rest of the batch gone: the caller learns which files it could
// not remove, never just that the request did not wholly succeed.
func TestABatchDeletionIsOneRequestNamingWhatItCouldNotRemove(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "crewlet/")
	names := []string{"files/a", "files/b", "files/c", "files/never-put"}
	for _, name := range names[:3] {
		if err := b.Put(t.Context(), name, bytes.NewReader([]byte(name)), objstore.PutMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	srv.RefuseDelete("crewlet/files/b", "AccessDenied")
	// THE OPEN'S PROBE DELETES ITS OWN OBJECT, and those deletions are
	// the open's, not the batch's.
	batches, singles := srv.RequestsOf("DeleteObjects"), srv.RequestsOf("DeleteObject")
	err := b.DeleteBatch(t.Context(), names)
	var refused *objstore.DeleteBatchError
	if !errors.As(err, &refused) || len(refused.Failed) != 1 || refused.Failed["files/b"] == nil {
		t.Fatalf("DeleteBatch with one key refused = %v; want a DeleteBatchError naming files/b alone", err)
	}
	if errors.Is(err, objstore.ErrNoBatch) {
		t.Fatalf("a batch the bucket answered key by key was read as refused whole: %v", err)
	}
	if got := srv.Keys(); !slices.Equal(got, []string{"crewlet/files/b"}) {
		t.Fatalf("after the batch the bucket holds %q, want only the refused key", got)
	}
	if n := srv.RequestsOf("DeleteObjects") - batches; n != 1 {
		t.Fatalf("a batch of %d names made %d DeleteObjects requests, want one", len(names), n)
	}
	if n := srv.RequestsOf("DeleteObject") - singles; n != 0 {
		t.Fatalf("a batch deletion made %d single deletions, want none", n)
	}
	// PAST THE BATCH THE API TAKES, refused before any request.
	if err := b.DeleteBatch(t.Context(), make([]string, objstore.MaxDeleteBatch+1)); err == nil {
		t.Fatal("a batch past MaxDeleteBatch was accepted")
	}
	if n := srv.RequestsOf("DeleteObjects") - batches; n != 1 {
		t.Fatalf("a batch past MaxDeleteBatch was sent (%d requests in all)", n)
	}
}

// A BATCH THE BUCKET REFUSES AS A REQUEST IS ErrNoBatch, AND ONLY THAT IS: a
// gateway that does not offer the verb answers it with a client error or 501,
// having removed nothing, and its caller deletes each name instead. A throttle
// — 503 SlowDown, or 429 — and a server error are the bucket SLOWING, never a
// missing verb: the batch is asked again until the bucket answers it or the
// caller's deadline passes, and is never ErrNoBatch — read as one, the caller
// would send a request for every key to a prefix that has just said to slow
// down. And a batch the bucket slowed for a while is deleted once it has
// answered.
func TestABatchTheBucketRefusesAsARequestIsNoBatch(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status  int
		code    string
		noBatch bool
	}{
		{http.StatusNotImplemented, "NotImplemented", true},
		{http.StatusBadRequest, "InvalidArgument", true},
		{http.StatusMethodNotAllowed, "MethodNotAllowed", true},
		{http.StatusForbidden, "AccessDenied", true},
		{http.StatusTooManyRequests, "TooManyRequests", false},
		{http.StatusServiceUnavailable, "SlowDown", false},
		{http.StatusInternalServerError, "InternalError", false},
	} {
		t.Run(c.code, func(t *testing.T) {
			t.Parallel()
			srv := s3fake.Start(t, "files")
			b := openRetrying(t, srv, "crewlet/", s3obj.Clock{})
			if err := b.Put(t.Context(), "files/a", bytes.NewReader([]byte("a")), objstore.PutMeta{}); err != nil {
				t.Fatal(err)
			}
			srv.RefuseBatches(c.status, c.code)
			ctx := t.Context()
			if !c.noBatch {
				// A BUCKET SLOWING FOR GOOD is asked again until the
				// caller's deadline, which a case cannot wait out at
				// the production thirty seconds.
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
			}
			err := b.DeleteBatch(ctx, []string{"files/a"})
			if err == nil {
				t.Fatalf("a batch the bucket refused with %d %s succeeded", c.status, c.code)
			}
			if got := errors.Is(err, objstore.ErrNoBatch); got != c.noBatch {
				t.Fatalf("DeleteBatch refused with %d %s = %v; ErrNoBatch %v, want %v",
					c.status, c.code, err, got, c.noBatch)
			}
			if !c.noBatch && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a batch the bucket kept slowing with %d %s = %v; want it asked again "+
					"until its deadline", c.status, c.code, err)
			}
			if got := srv.Keys(); !slices.Equal(got, []string{"crewlet/files/a"}) {
				t.Fatalf("a refused batch left the bucket holding %q", got)
			}
			if c.noBatch {
				return
			}

			// A FRESH CLIENT, so the batch is not held behind the limiter
			// the refusals above engaged.
			srv.RefuseBatches(0, "")
			srv.FailNext(1, c.status, c.code)
			if err := openRetrying(t, srv, "crewlet/", s3obj.Clock{}).DeleteBatch(t.Context(), []string{"files/a"}); err != nil {
				t.Fatalf("a batch the bucket slowed once with %d %s = %v; want it deleted",
					c.status, c.code, err)
			}
			if got := srv.Keys(); len(got) != 0 {
				t.Fatalf("a batch the bucket answered at last left it holding %q", got)
			}
		})
	}
}
