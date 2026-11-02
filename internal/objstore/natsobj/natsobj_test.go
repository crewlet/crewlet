package natsobj_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// suite is what the broker's object store promises the suite: messages of
// [natsobj.MessageBytes] and a digest it keeps.
//
// THE SLOW LISTING IS THE SUITE'S OWN SECOND. It used to be twelve, to outlast
// two of the ordered consumer's five-second heartbeats — but the vendored
// client arms no heartbeat between the reads of a listing at all, so twelve
// seconds of a slow visitor tripped nothing and proved nothing about the one
// thing it was for: a listing whose consumer is lost part way. That is staged
// directly instead, by deleting the consumer under it
// ([TestAListingWhoseConsumerIsLostFinishesOnItsReplacement]).
var suite = objstoretest.Options{
	Piece:      natsobj.MessageBytes,
	Digest:     true,
	Unfinished: unfinished,
	Spoil:      spoil,
}

// clients is the client each backend a case opened was opened on, so the
// suite's unfinished upload can be left on the same broker.
var clients sync.Map

// unfinished leaves what a put killed before its last message leaves: pieces
// on a subject of their own and no metadata naming them — so it has no name,
// and its ID is the pieces' identifier.
func unfinished(t *testing.T, b objstore.Backend) objstore.Pending {
	t.Helper()
	c, ok := clients.Load(b)
	if !ok {
		t.Fatal("the backend was not opened through open()")
	}
	var raw [11]byte
	_, _ = rand.Read(raw[:])
	nuid := strings.ToUpper(hex.EncodeToString(raw[:]))
	for i := range 2 {
		if _, err := c.(jetstream.JetStream).Publish(t.Context(), "$O."+natsobj.Bucket+".C."+nuid,
			bytes.Repeat([]byte{byte(i)}, 1000)); err != nil {
			t.Fatalf("leave an unfinished upload: %v", err)
		}
	}
	return objstore.Pending{ID: nuid}
}

// spoil leaves under name what no get could read: metadata that does not
// decode, published over whatever name held — as a client of the bucket's
// documented format writing garbage, or a disk returning it, would.
func spoil(t *testing.T, b objstore.Backend, name string) {
	t.Helper()
	c, ok := clients.Load(b)
	if !ok {
		t.Fatal("the backend was not opened through open()")
	}
	subject := "$O." + natsobj.Bucket + ".M." + base64.URLEncoding.EncodeToString([]byte(name))
	if _, err := c.(jetstream.JetStream).Publish(t.Context(), subject,
		[]byte("not an object's metadata")); err != nil {
		t.Fatalf("spoil %s: %v", name, err)
	}
}

// THE BROKER'S OWN OBJECT STORE PASSES THE SUITE, on a member's own client.
func TestContract(t *testing.T) {
	t.Parallel()
	opts := suite
	opts.Stalling = stallingMember
	objstoretest.Run(t, func(t *testing.T) objstore.Backend {
		return open(t, memberClient(t))
	}, opts)
}

// AND THROUGH A LEAF: a node without `data` runs no JetStream and reaches the
// members' across its leaf link, and its uploads and downloads go through
// exactly that.
func TestContractThroughALeaf(t *testing.T) {
	t.Parallel()
	opts := suite
	opts.Stalling = stallingLeaf
	objstoretest.Run(t, func(t *testing.T) objstore.Backend {
		return open(t, leafClient(t))
	}, opts)
}

// EVERY MESSAGE OF A PUT BUT ITS LAST IS FULL, however few bytes at a time the
// reader yields: a put that sent a message per read of its body would store a
// body arriving a byte at a time as a message per byte.
func TestEveryMessageButTheLastIsFull(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	data := bytes.Repeat([]byte("full messages "), 3*natsobj.MessageBytes/14+3)
	if err := b.Put(t.Context(), "trickled", iotest.OneByteReader(bytes.NewReader(data)),
		objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	pieces := (len(data) + natsobj.MessageBytes - 1) / natsobj.MessageBytes
	if got := messages(t, client); got != uint64(pieces)+1 {
		t.Fatalf("%d bytes a byte at a time made %d messages, want %d of %d bytes and "+
			"the one naming them", len(data), got, pieces, natsobj.MessageBytes)
	}
	r, err := b.Get(deadline(t), "trickled", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the trickled object read back %d bytes, %v", len(got), err)
	}
}

// A PUT WHOSE READER FAILS LEAVES NOT ONE MESSAGE: the pieces it sent are on
// a subject nothing names, which no listing would ever show the collector.
func TestAFailedPutLeavesNoMessages(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	dropped := errors.New("the client went away")
	body := io.MultiReader(bytes.NewReader(make([]byte, 3*natsobj.MessageBytes/2)),
		iotest.ErrReader(dropped))
	if err := b.Put(t.Context(), "dropped", body, objstore.PutMeta{}); !errors.Is(err, dropped) {
		t.Fatalf("Put = %v, want the reader's error", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelled := &cancelAfter{r: bytes.NewReader(make([]byte, 3*natsobj.MessageBytes)),
		after: 3 * natsobj.MessageBytes / 2, cancel: cancel}
	if err := b.Put(ctx, "cancelled", cancelled, objstore.PutMeta{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put whose context ended mid-stream = %v", err)
	}
	if got := messages(t, client); got != 0 {
		t.Fatalf("failed puts left %d messages in %s", got, natsobj.Stream)
	}
}

// cancelAfter cancels its context once it has yielded after bytes, and keeps
// yielding.
type cancelAfter struct {
	r      io.Reader
	after  int
	read   int
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.read += n; c.read >= c.after {
		c.cancel()
	}
	return n, err
}

// A DOWNLOAD WHOSE CONSUMER IS LOST PART WAY IS FINISHED BY ITS REPLACEMENT,
// byte for byte, and within [lostConsumerBound].
//
// A get is an ordered consumer of the object's messages, and the broker can
// take that consumer away under a reader — reaped while the reader was slow,
// dropped in a leader change. The client then makes a new one from the last
// message the reader took, once two of the broker's heartbeats have gone
// missing — the path every one of those failures goes down, and every pull a
// clustered fleet drops at a consumer's creation too — and the one this case
// stages: the consumer is deleted a few messages into a 4 MiB read, past what
// the read held in hand, so the rest can only come from a replacement. This
// replaced a read paced over twelve seconds to outlast two of the consumer's
// heartbeats; the vendored client arms none between reads, so that read
// tripped nothing and never asserted that a replacement was made.
func TestAReadWhoseConsumerIsLostFinishesOnItsReplacement(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	data := bytes.Repeat([]byte("read across a lost consumer "), 4<<20/28)
	if err := b.Put(t.Context(), "lost", bytes.NewReader(data), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	r, err := b.Get(deadline(t), "lost", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var got bytes.Buffer
	buf := make([]byte, natsobj.MessageBytes/2)
	var (
		deleted []string
		lost    time.Time
	)
	for {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("a read whose consumer was deleted failed at %d bytes: %v", got.Len(), err)
		}
		if deleted == nil && got.Len() >= 3*natsobj.MessageBytes {
			deleted, lost = dropConsumers(t, client), time.Now()
		}
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("a read across a lost consumer answered %d bytes, want %d exactly", got.Len(), len(data))
	}
	replacedBy(t, client, deleted)
	if took := time.Since(lost); took > lostConsumerBound {
		t.Fatalf("the read finished %v after its consumer was lost, past the %v a lost "+
			"consumer may cost", took, lostConsumerBound)
	}
}

// A LISTING ENDS EVEN WHEN THE NEWEST METADATA CANNOT BE READ. The library's
// own watch learns it has seen everything only from a message it decoded, so
// one that ended on garbage never finished — and the collector, whose context
// has no deadline, waited on it for ever. And it hands the garbled name on,
// holding nothing, so the collector can judge and clear it like any other.
func TestAListingEndsPastMetadataItCannotRead(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	if err := b.Put(t.Context(), "readable", bytes.NewReader([]byte("x")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	garbled := "$O." + natsobj.Bucket + ".M." + base64.URLEncoding.EncodeToString([]byte("garbled"))
	if _, err := client.Publish(t.Context(), garbled, []byte("not an object's metadata")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	listed := map[string]objstore.Info{}
	if err := b.List(ctx, func(info objstore.Info) error {
		listed[info.Name] = info
		return nil
	}); err != nil {
		t.Fatalf("a listing ending on unreadable metadata: %v", err)
	}
	if len(listed) != 2 || listed["readable"].Size != 1 || listed["garbled"].Size != 0 ||
		listed["garbled"].Written.IsZero() {
		t.Fatalf("listed %+v, want the readable object and the garbled name, dated and "+
			"holding nothing", listed)
	}
	// THE SUBJECT A NAME IS SPELLED IN IS NOT ALWAYS ONE: metadata on a
	// subject that decodes to no name is stepped over, and the listing goes
	// ON past it — to an object stored after it, so the stepped-over message
	// is not the listing's last and the walk has to read beyond it rather
	// than end on it.
	if _, err := client.Publish(t.Context(), "$O."+natsobj.Bucket+".M.!",
		[]byte("on no name at all")); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(t.Context(), "after", bytes.NewReader([]byte("yz")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	clear(listed)
	if err := b.List(ctx, func(info objstore.Info) error {
		listed[info.Name] = info
		return nil
	}); err != nil || len(listed) != 3 || listed["after"].Size != 2 {
		t.Fatalf("a listing past metadata on a subject spelling no name = %+v, %v; want "+
			"readable, garbled and after", listed, err)
	}
	// AND IT LEAVES NO CONSUMER BEHIND for the broker to reap later: a
	// collection lists the bucket every hour.
	stream, err := client.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Consumers != 0 {
		t.Fatalf("a finished listing left %d consumers on %s", info.State.Consumers, natsobj.Stream)
	}
}

// A DELETE LEAVES NOT ONE MESSAGE — no delete marker in the metadata's place —
// and a get's consumer is gone once the get is closed. The library's delete
// writes a marker no later put replaces, because no name is ever put twice,
// so every object the collector ever deleted would be a message each hourly
// listing ships for ever.
func TestADeleteLeavesNoMessage(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	data := bytes.Repeat([]byte("gone "), 3*natsobj.MessageBytes/5)
	if err := b.Put(t.Context(), "files/gone", bytes.NewReader(data), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	r, err := b.Get(deadline(t), "files/gone", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
	_ = r.Close()
	if err := b.Delete(t.Context(), "files/gone"); err != nil {
		t.Fatal(err)
	}
	if got := messages(t, client); got != 0 {
		t.Fatalf("a deleted object left %d messages in %s", got, natsobj.Stream)
	}
	if got := consumers(t, client); got != 0 {
		t.Fatalf("a closed get left %d consumers on %s", got, natsobj.Stream)
	}
}

// A DELETE INTERRUPTED BETWEEN ITS TWO PURGES LEAVES PIECES NOTHING NAMES, and
// they are pending — found, dated and abandoned like the pieces of a put that
// died — while the pieces of every object that still has its metadata are
// not, a delete marker's excepted: the delete that wrote one purged its
// pieces, so pieces still under it are leftovers too.
func TestPiecesNoMetadataNamesArePending(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	for _, name := range []string{"files/kept", "files/interrupted"} {
		if err := b.Put(t.Context(), name, bytes.NewReader(bytes.Repeat([]byte(name), 1000)),
			objstore.PutMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	stream, err := client.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	nuidOf := func(name string) string {
		t.Helper()
		library, err := client.ObjectStore(t.Context(), natsobj.Bucket)
		if err != nil {
			t.Fatal(err)
		}
		info, err := library.GetInfo(t.Context(), name, jetstream.GetObjectInfoShowDeleted())
		if err != nil {
			t.Fatal(err)
		}
		return info.NUID
	}
	interrupted := nuidOf("files/interrupted")
	// THE FIRST HALF OF A DELETE, and then the process is gone.
	if err := stream.Purge(t.Context(), jetstream.WithPurgeSubject("$O."+natsobj.Bucket+".M."+
		base64.URLEncoding.EncodeToString([]byte("files/interrupted")))); err != nil {
		t.Fatal(err)
	}
	// AND A MARKER WHOSE PIECES SURVIVED: the metadata says deleted, and the
	// pieces are still there.
	marker := objstore.KeyAt(time.Now()).Name()
	if err := b.Put(t.Context(), marker, bytes.NewReader([]byte("marked")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	survived := nuidOf(marker)
	library, err := client.ObjectStore(t.Context(), natsobj.Bucket)
	if err != nil {
		t.Fatal(err)
	}
	info, err := library.GetInfo(t.Context(), marker)
	if err != nil {
		t.Fatal(err)
	}
	info.Deleted, info.Size, info.Chunks, info.Digest = true, 0, 0, ""
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Publish(t.Context(), "$O."+natsobj.Bucket+".M."+
		base64.URLEncoding.EncodeToString([]byte(marker)), raw); err != nil {
		t.Fatal(err)
	}

	var got []objstore.Pending
	if err := b.Pending(t.Context(), func(p objstore.Pending) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, p := range got {
		if p.Name != "" || p.Started.IsZero() {
			t.Errorf("pending pieces answered as %+v, want no name and when they began", p)
		}
		ids = append(ids, p.ID)
	}
	slices.Sort(ids)
	want := []string{interrupted, survived}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("Pending = %q, want the interrupted delete's and the marker's pieces %q", ids, want)
	}
	for _, p := range got {
		if err := b.Abandon(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	r, err := b.Get(deadline(t), "files/kept", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if data, err := io.ReadAll(r); err != nil || len(data) != len("files/kept")*1000 {
		t.Fatalf("an object with its metadata lost its pieces to the sweep: %d bytes, %v", len(data), err)
	}
}

// AN ABANDONED ID IS ONE SUBJECT TOKEN, or nothing is purged: spliced into the
// purge's filter, a wildcard or a dot would take every object's pieces.
func TestAbandonRefusesAnIDThatIsNotOneToken(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	if err := b.Put(t.Context(), "files/kept", bytes.NewReader([]byte("kept")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", ">", "*", "a.b", "a b"} {
		if err := b.Abandon(t.Context(), objstore.Pending{ID: id}); err == nil {
			t.Errorf("Abandon(%q) was accepted", id)
		}
	}
	if got := messages(t, client); got != 2 {
		t.Fatalf("the object holds %d messages after the refused abandons, want its 2", got)
	}
}

// A DELETE MARKER THE LIBRARY'S OWN DELETE LEFT IS LISTED, AND CLEARED BY AN
// ORDINARY DELETE. `nats object rm`, or any other client of the bucket's
// documented format, deletes through the library, which keeps the marker for
// ever; the collector can only remove what a listing shows it, so the listing
// shows the name, as an object of no bytes, and the delete it makes purges the
// marker like any other metadata.
func TestAMarkerTheLibrarysDeleteLeftIsListedAndCleared(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	name := objstore.KeyAt(time.Now()).Name()
	if err := b.Put(t.Context(), name, bytes.NewReader([]byte("marked")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	library, err := client.ObjectStore(t.Context(), natsobj.Bucket)
	if err != nil {
		t.Fatal(err)
	}
	if err := library.Delete(t.Context(), name); err != nil {
		t.Fatalf("the library's own delete: %v", err)
	}
	if got := messages(t, client); got != 1 {
		t.Fatalf("the library's delete left %d messages, want its one marker", got)
	}
	if _, err := b.Stat(t.Context(), name); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Stat of a marker = %v, want ErrNotFound", err)
	}
	var listed []objstore.Info
	if err := b.List(t.Context(), func(info objstore.Info) error {
		listed = append(listed, info)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != name || listed[0].Size != 0 || listed[0].Written.IsZero() {
		t.Fatalf("the listing of a marker = %+v, want its name, no bytes and when it was left", listed)
	}
	if err := b.Delete(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	if got := messages(t, client); got != 0 {
		t.Fatalf("a delete of a marker left %d messages", got)
	}
}

// A RANGE IS FOUND BY THE MESSAGES' OWN SIZES, never by assuming each is
// [natsobj.MessageBytes]: an object some other writer cut into pieces of its
// own — here a byte a message, through the library's put with no filler —
// reads back from any offset exactly.
func TestARangeIsFoundByTheMessagesOwnSizes(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	library, err := client.ObjectStore(t.Context(), natsobj.Bucket)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("an object stored a byte at a time, by somebody else's writer")
	if _, err := library.Put(t.Context(), jetstream.ObjectMeta{Name: "ragged"},
		iotest.OneByteReader(bytes.NewReader(data))); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ off, n int64 }{{0, 5}, {1, 1}, {17, 9}, {int64(len(data)) - 3, -1}} {
		r, err := b.Get(deadline(t), "ragged", c.off, c.n)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		end := int64(len(data))
		if c.n >= 0 {
			end = c.off + c.n
		}
		if err != nil || !bytes.Equal(got, data[c.off:end]) {
			t.Fatalf("Get(%d, %d) = %q, %v; want %q", c.off, c.n, got, err, data[c.off:end])
		}
	}
	if got := consumers(t, client); got != 0 {
		t.Fatalf("ranged gets left %d consumers", got)
	}
}

// AN OBJECT WHOSE PIECES ARE GONE ENDS BEFORE IT BEGINS — which the store
// reads as an object holding fewer bytes than its row says — while the object
// whose metadata went too is not found: the two are told apart, so a deletion
// racing a read is never reported as damage.
func TestAnObjectWhosePiecesAreGoneReadsEmpty(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	if err := b.Put(t.Context(), "hollow", bytes.NewReader(make([]byte, 1000)), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	stream, err := client.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Purge(t.Context(), jetstream.WithPurgeSubject("$O."+natsobj.Bucket+".C.>")); err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, 500} {
		r, err := b.Get(deadline(t), "hollow", off, -1)
		if err != nil {
			t.Fatalf("Get at %d of an object with no pieces: %v", off, err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || len(got) != 0 {
			t.Fatalf("Get at %d of an object with no pieces = %d bytes, %v; want none", off, len(got), err)
		}
	}
	if err := b.Delete(t.Context(), "hollow"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(deadline(t), "hollow", 0, -1); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Get of a deleted object = %v, want ErrNotFound", err)
	}
}

// A LISTING WHOSE CONSUMER IS LOST PART WAY IS FINISHED BY ITS REPLACEMENT,
// visiting every name exactly once, and within [lostConsumerBound].
//
// The listing is an ordered consumer over the metadata, holding
// [natsobj.WalkAhead] messages at a time, so the bucket holds more names than
// that: the consumer is deleted under the tenth, and everything past what the
// listing already held can only come from the replacement the client makes
// from the last name visited. A name visited twice or not at all is the
// collector deleting or keeping the wrong object. Read slowly instead, as this
// suite used to read it for twelve seconds, nothing was lost: the vendored
// client arms no heartbeat between a listing's reads.
//
// THE NAMES ARE METADATA ALONE, published straight to the bucket's subjects: a
// listing reads nothing else, and five hundred uploads would cost seconds to
// stage what five hundred messages stage in a fraction of one.
func TestAListingWhoseConsumerIsLostFinishesOnItsReplacement(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	const extra = 50
	names := natsobj.WalkAhead + extra
	for i := range names {
		name := fmt.Sprintf("files/listed-%04d", i)
		info, err := json.Marshal(jetstream.ObjectInfo{ObjectMeta: jetstream.ObjectMeta{Name: name},
			Bucket: natsobj.Bucket, NUID: fmt.Sprintf("NUID%04d", i), Size: 1, Chunks: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Publish(t.Context(), "$O."+natsobj.Bucket+".M."+
			base64.URLEncoding.EncodeToString([]byte(name)), info); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]int{}
	var (
		deleted, last []string
		lost          time.Time
	)
	if err := b.List(t.Context(), func(info objstore.Info) error {
		seen[info.Name]++
		switch len(seen) {
		case 10:
			deleted, lost = dropConsumers(t, client), time.Now()
		case names:
			last = consumerNames(t, client)
		}
		return nil
	}); err != nil {
		t.Fatalf("a listing whose consumer was deleted: %v", err)
	}
	if took := time.Since(lost); took > lostConsumerBound {
		t.Fatalf("the listing finished %v after its consumer was lost, past the %v a lost "+
			"consumer may cost", took, lostConsumerBound)
	}
	if len(seen) != names {
		t.Fatalf("the listing visited %d names of %d", len(seen), names)
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("the listing visited %s %d times", name, n)
		}
	}
	replaced := false
	for _, name := range last {
		replaced = replaced || !slices.Contains(deleted, name)
	}
	if !replaced {
		t.Fatalf("the listing's last names came from %v, and its consumer %v was deleted "+
			"under the tenth: nothing stages the replacement this case is for", last, deleted)
	}
	if got := consumers(t, client); got != 0 {
		t.Fatalf("a listing that replaced its consumer left %d behind", got)
	}
}

// lostConsumerBound is the most a consumer lost under a read may cost it: the
// quiet second the read waits before it waits again ([natsobj.QuietWait]), the
// two heartbeats the client then waits out before it replaces the consumer
// ([natsobj.PullHeartbeat]), and room for a loaded runner to make the
// replacement and hand over what it reads first. A lost consumer is the one
// case the heartbeats are for, so the bound is theirs; what holds THEM is
// [TestAReadsHeartbeatOutlastsItsHandAtTheFloor].
const lostConsumerBound = natsobj.QuietWait + 2*natsobj.PullHeartbeat + 5*time.Second

// A READ'S HEARTBEATS OUTLAST ITS HAND CROSSING A LINK AT THE FLOOR, TWICE
// OVER — AND A CONSUMER LOST UNDER IT IS REPLACED INSIDE THE STALL.
//
// The client replaces a read's consumer when two heartbeats go missing while
// it waits, and hears of a heartbeat or a message only once the whole of it
// has arrived — on a compressed link, once the whole block the broker flushed
// has, and a get's first pull puts its whole hand on the wire in one flush. So
// on a link at [objstore.MiBPace], the slowest every object store leg accepts,
// a healthy read can hear nothing for as long as its hand takes to cross, and
// two heartbeats must outlast that or the reader is replaced before its hand
// arrives, and again before its replacement's, and fails a minute later at
// [objstore.Stall] having read nothing. Twice over, the room the stall
// gives a mebibyte.
//
// And a consumer the broker loses under a read costs a quiet second, two
// heartbeats and then its replacement's first hand — which must fit in the
// stall, or the read the replacement was made for is ended before it arrives.
// That is what bounds the hand: at eight messages, a mebibyte, the two could
// not both hold.
func TestAReadsHeartbeatOutlastsItsHandAtTheFloor(t *testing.T) {
	t.Parallel()
	hand := time.Duration(natsobj.ReadAhead*natsobj.MessageBytes) * objstore.MiBPace / objstore.MiB
	if 2*natsobj.PullHeartbeat < 2*hand {
		t.Errorf("two heartbeats of %v are under twice the %v a hand of %d messages takes to "+
			"cross a link at the floor: a read at the floor is replaced before its hand arrives",
			natsobj.PullHeartbeat, hand, natsobj.ReadAhead)
	}
	if lost := natsobj.QuietWait + 2*natsobj.PullHeartbeat + hand; lost > objstore.Stall {
		t.Errorf("a consumer lost under a read at the floor costs a quiet wait, two heartbeats "+
			"and a hand, %v, past the %v a read waits before it is ended as stalled",
			lost, objstore.Stall)
	}
}

// A READ ACROSS A LINK NEAR THE FLOOR FINISHES ON THE CONSUMER IT BEGAN WITH.
//
// A leaf reaches the bucket across its link to a member, and the link is held
// to a quarter above [objstore.MiBPace]'s floor towards the leaf. The link is
// compressed, as a leaf's is by default, so what the member flushes at once
// arrives as one block: the read's first hand, [natsobj.ReadAhead] messages,
// in twelve seconds and nothing of it sooner, then what its next pull asked
// for. The object is a hand and a half, so the read waits on the link twice.
// It also asks its consumer about its first message a second in — which the
// consumer answers, behind the hand, as being read — and must not remake it.
// At a heartbeat whose two went missing in either wait, the read's consumer
// was replaced before its messages arrived, and again before each of the
// replacement's, which queued behind what its predecessor had been sent: the
// read never completed a message and failed when its deadline ran out.
//
// A QUARTER ABOVE THE FLOOR rather than at it, so a scheduler running late on
// a loaded runner cannot push the link under the floor, where no heartbeat is
// promised to hold; the floor itself is held by the arithmetic
// ([TestAReadsHeartbeatOutlastsItsHandAtTheFloor]).
func TestAReadAcrossALinkNearTheFloorKeepsItsConsumer(t *testing.T) {
	t.Parallel()
	member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: t.TempDir(),
		FleetAuth: linkFleetAuth()})
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(member.Shutdown)
	gate := holdGate(t)
	floor := int64(objstore.MiB) * int64(time.Second) / int64(objstore.MiBPace)
	gate.slow(floor * 5 / 4)
	gate.open(fmt.Sprintf("127.0.0.1:%d", member.LeafPort()))
	leaf := client(t, startLeaf(t, gate.port()))
	awaitLink(t, leaf)
	b := open(t, leaf)
	data := make([]byte, 3*natsobj.ReadAhead*natsobj.MessageBytes/2)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(t.Context(), "slow", bytes.NewReader(data), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	// EVERY CONSUMER THE READ MAKES IS COUNTED, nothing made deaf: the get
	// waits for its first message before it returns, so a consumer it
	// remade in that wait is gone before the reader is handed back.
	made := &deafStream{}
	natsobj.SwapStream(b, func(s jetstream.Stream) jetstream.Stream { made.Stream = s; return made })

	// THE MEMBER'S OWN CLIENT names the consumers, so asking adds nothing to
	// the slow link the read is waiting on.
	direct := client(t, member)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	r, err := b.Get(ctx, "slow", 0, -1)
	if err != nil {
		t.Fatalf("a get across a link a quarter above the floor: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("a read across a link a quarter above the floor failed after %d bytes of %d: %v",
			len(got), len(data), err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("a read across a slow link answered %d bytes, want %d exactly", len(got), len(data))
	}
	made.mu.Lock()
	began := made.names
	made.mu.Unlock()
	if ended := consumerNames(t, direct); len(began) != 1 || !slices.Equal(began, ended) {
		t.Fatalf("the read made the consumers %v and ended on %v: a link above the floor "+
			"remade or replaced the consumer it began with", began, ended)
	}
}

// A READ WHOSE FIRST PULL NEVER REACHED ITS CONSUMER REMAKES THE CONSUMER, A
// SECOND IN — a whole get, a ranged get's walk to its offset, and a listing.
//
// On a clustered fleet a new consumer's first pull is dropped one read in 25
// to one in 1,500 ([natsobj.QuietWait]), and nothing tells the reader:
// it hears only heartbeats, and waiting on them cost the read two of them —
// thirty seconds at a heartbeat that honours the floor. The read asks its
// consumer instead, and one that holds no pull and has delivered nothing is
// made again. A [deafStream] stages exactly that: the first consumer it is
// asked for is made on the broker as asked, so what the read asks it is the
// broker's answer, but its pull goes elsewhere and never reaches it.
func TestAReadWhoseFirstPullNeverArrivedRemakesItsConsumer(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("asked again "), 3*natsobj.MessageBytes/12)
	for _, c := range []struct {
		name string
		// makes is how many consumers the read makes when none is deaf: a
		// ranged get walks to its offset on one and reads on another.
		makes int32
		read  func(t *testing.T, b *natsobj.Backend) error
	}{
		{"get", 1, func(t *testing.T, b *natsobj.Backend) error {
			return readsBack(t, b, 0, data)
		}},
		{"ranged", 2, func(t *testing.T, b *natsobj.Backend) error {
			return readsBack(t, b, natsobj.MessageBytes+5, data[natsobj.MessageBytes+5:])
		}},
		{"listing", 1, func(t *testing.T, b *natsobj.Backend) error {
			var names []string
			err := b.List(t.Context(), func(info objstore.Info) error {
				names = append(names, info.Name)
				return nil
			})
			if err == nil && !slices.Equal(names, []string{"asked"}) {
				return fmt.Errorf("the listing named %v, want the one object", names)
			}
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			client := memberClient(t)
			b := open(t, client)
			if err := b.Put(t.Context(), "asked", bytes.NewReader(data), objstore.PutMeta{}); err != nil {
				t.Fatal(err)
			}
			deaf := &deafStream{}
			deaf.deaf.Store(1)
			natsobj.SwapStream(b, func(s jetstream.Stream) jetstream.Stream { deaf.Stream = s; return deaf })
			began := time.Now()
			if err := c.read(t, b); err != nil {
				t.Fatalf("a read whose first pull never reached its consumer: %v", err)
			}
			if took, bound := time.Since(began), natsobj.QuietWait+5*time.Second; took > bound {
				t.Fatalf("the read took %v, past the %v asking its consumer and remaking it may "+
					"cost: it waited for its heartbeats", took, bound)
			}
			if made := deaf.made.Load(); made != c.makes+1 {
				t.Fatalf("the read made %d consumers, want its own %d and the deaf one's remake",
					made, c.makes)
			}
			if left := consumers(t, client); left != 0 {
				t.Fatalf("the read left %d consumers behind, the deaf one among them", left)
			}
		})
	}
}

// A READ REMAKES A CONSUMER THAT NEVER GETS ITS PULL [natsobj.FirstPullRemakes]
// TIMES, THEN WAITS. A consumer still without its pull after that is behind a
// link holding the pull itself back rather than a cluster that dropped it, and
// remaking it on every second would only add consumers for the cluster to
// create; its heartbeats and the read's own deadline are still over it.
func TestAReadRemakesAnUnpulledConsumerOnlySoOften(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	if err := b.Put(t.Context(), "unheard", bytes.NewReader([]byte("never pulled")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	deaf := &deafStream{}
	deaf.deaf.Store(1 << 20)
	natsobj.SwapStream(b, func(s jetstream.Stream) jetstream.Stream { deaf.Stream = s; return deaf })
	wait := time.Duration(natsobj.FirstPullRemakes+3) * natsobj.QuietWait
	ctx, cancel := context.WithTimeout(t.Context(), wait)
	defer cancel()
	if _, err := b.Get(ctx, "unheard", 0, -1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a get no consumer ever heard the pull of = %v, want its deadline", err)
	}
	if made, want := deaf.made.Load(), int32(1+natsobj.FirstPullRemakes); made != want {
		t.Fatalf("the get made %d consumers in %v, want %d: the first and %d remakes",
			made, wait, want, natsobj.FirstPullRemakes)
	}
	if left := consumers(t, client); left != 0 {
		t.Fatalf("the get left %d consumers behind", left)
	}
}

// A READ WHOSE CLIENT SKIPPED A PULL GOES ON A SECOND LATER, not two
// heartbeats later.
//
// The client decides whether to send its next pull only as a wait begins, and
// now and then decides not to ([natsobj.QuietWait]): its consumer has
// delivered the hand, holds no pull, and has nothing to say, and nothing
// tells the reader — which heard only heartbeats, and replaced the consumer
// once two went missing. A [skippingStream] stages exactly that: after the
// first hand the reader's next wait is spent hearing nothing, and only a wait
// begun after it reads on, as only a wait begun again makes the client send
// the pull. The object is two hands, so the skip falls in the middle of it.
func TestAReadWhoseClientSkippedAPullGoesOn(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	data := bytes.Repeat([]byte("skipped once "), 2*natsobj.ReadAhead*natsobj.MessageBytes/13)
	if err := b.Put(t.Context(), "asked", bytes.NewReader(data), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	skip := &skippingStream{after: natsobj.ReadAhead}
	natsobj.SwapStream(b, func(s jetstream.Stream) jetstream.Stream { skip.Stream = s; return skip })
	began := time.Now()
	if err := readsBack(t, b, 0, data); err != nil {
		t.Fatalf("a read whose client skipped a pull: %v", err)
	}
	if took, bound := time.Since(began), natsobj.QuietWait+5*time.Second; took > bound {
		t.Fatalf("the read took %v, past the %v waiting again costs: it waited out the stall "+
			"its skipped pull left it in", took, bound)
	}
	if skipped := skip.skipped.Load(); skipped != 1 {
		t.Fatalf("%d waits were spent hearing nothing, want the one this case is for", skipped)
	}
	if left := consumers(t, client); left != 0 {
		t.Fatalf("the read left %d consumers behind", left)
	}
}

// A READ ON A REPLICA BEHIND THE STREAM'S LEADER READS WHAT THE LEADER HOLDS.
//
// The broker makes a read's consumer on whichever replica it places it, one
// still catching up after a restart included, and that consumer counts what
// follows from what its replica has stored: it says nothing follows where the
// leader holds more. A [laggingStream] stages exactly that, the replica
// holding none of the object or only its first piece. Taken for the end, the
// get read back none or part of the object — DAMAGED to the store, a definite
// answer, where the object was whole — and a range past what the replica held
// was read as past the object's end. Seen on a three-member fleet: a cache
// syncing from a member just back from a restart took a pushed pack for
// damaged, `ended at 0 of the 1460 bytes`.
func TestAReadOnAReplicaBehindTheLeaderReadsWhatTheLeaderHolds(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("caught up "), 3*natsobj.MessageBytes/10)
	off := int64(2*natsobj.MessageBytes + 5)
	for _, c := range []struct {
		name string
		// holds is how many of the object's pieces the replica behind has
		// stored, and makes how many consumers the read makes: the one
		// made there and its replacement, and a ranged get's own after
		// its walk.
		holds int
		makes int32
		off   int64
	}{
		{"get_on_a_replica_holding_none", 0, 2, 0},
		{"get_on_a_replica_holding_one_piece", 1, 2, 0},
		{"range_on_a_replica_holding_none", 0, 3, off},
		{"range_on_a_replica_holding_one_piece", 1, 3, off},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			client := memberClient(t)
			b := open(t, client)
			before := lastSeq(t, client)
			if err := b.Put(t.Context(), "asked", bytes.NewReader(data), objstore.PutMeta{}); err != nil {
				t.Fatal(err)
			}
			lag := &laggingStream{holds: before + uint64(c.holds)}
			lag.lagging.Store(1)
			natsobj.SwapStream(b, func(s jetstream.Stream) jetstream.Stream { lag.Stream = s; return lag })
			if err := readsBack(t, b, c.off, data[c.off:]); err != nil {
				t.Fatalf("a read whose consumer was made on a replica behind the leader: %v", err)
			}
			if made := lag.made.Load(); made != c.makes {
				t.Fatalf("the read made %d consumers, want %d: the one behind the leader and "+
					"what read on past it", made, c.makes)
			}
			if left := consumers(t, client); left != 0 {
				t.Fatalf("the read left %d consumers behind", left)
			}
		})
	}
}

// A LISTING ON A REPLICA BEHIND THE STREAM'S LEADER NAMES WHAT THE LEADER HELD,
// or fails — and a pass over the unfinished uploads fails with it, rather than
// hand the collector the pieces of an upload the listing missed.
//
// The listing's consumer is made on whichever replica the broker places it,
// and one still catching up has stored the metadata of the uploads before
// "late" but not of "late" itself. Made there with nothing at all, it is made
// again; made there part way through, it cannot go on from a consumer made
// again — that would deliver every message past its end, superseded ones
// among them — and fails, to be asked again. Taken for the end, the listing
// named only "early", and [natsobj.Backend.Pending] answered "late"'s pieces
// as an upload nobody finished: the collector abandons one of those a day
// after it began, which purges the pieces of a whole object a replica a day
// behind had not yet caught up on.
func TestAListingOnAReplicaBehindTheLeaderNamesWhatTheLeaderHeld(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// holds is how many of the bucket's messages the replica behind
		// has stored: none, or the first upload's piece and metadata.
		holds uint64
		// fails says the listing must fail; otherwise it names both
		// names on a consumer made again.
		fails bool
	}{
		{"on_a_replica_holding_none", 0, false},
		{"on_a_replica_holding_part", 2, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			client := memberClient(t)
			b := open(t, client)
			before := lastSeq(t, client)
			for _, name := range []string{"early", "late"} {
				if err := b.Put(t.Context(), name, strings.NewReader(name), objstore.PutMeta{}); err != nil {
					t.Fatal(err)
				}
			}
			if got := lastSeq(t, client) - before; got != 4 {
				t.Fatalf("two one-piece uploads took %d messages, want a piece and a metadata each", got)
			}
			lag := &laggingStream{holds: before + c.holds}
			natsobj.SwapStream(b, func(s jetstream.Stream) jetstream.Stream { lag.Stream = s; return lag })

			lag.lagging.Store(1)
			var names []string
			err := b.List(t.Context(), func(info objstore.Info) error {
				names = append(names, info.Name)
				return nil
			})
			switch {
			case c.fails && err == nil:
				t.Fatalf("a listing that ended short of the leader passed, naming %v", names)
			case c.fails && !strings.Contains(err.Error(), "behind the stream's leader"):
				t.Fatalf("a listing that ended short of the leader failed without saying why: %v", err)
			case !c.fails && err != nil:
				t.Fatalf("a listing whose first consumer held nothing: %v", err)
			case !c.fails && !slices.Equal(names, []string{"early", "late"}):
				t.Fatalf("a listing whose first consumer held nothing named %v, want both", names)
			}

			lag.lagging.Store(1)
			var pending []objstore.Pending
			err = b.Pending(t.Context(), func(p objstore.Pending) error {
				pending = append(pending, p)
				return nil
			})
			switch {
			case len(pending) != 0:
				t.Fatalf("Pending over a listing behind the leader answered %v: finished "+
					"uploads, for the collector to abandon", pending)
			case c.fails && err == nil:
				t.Fatal("Pending over a listing that ended short of the leader passed")
			case !c.fails && err != nil:
				t.Fatalf("Pending over a listing whose first consumer held nothing: %v", err)
			}
			if left := consumers(t, client); left != 0 {
				t.Fatalf("the listings left %d consumers behind", left)
			}
		})
	}
}

// A READ WHOSE CONSUMER THE BROKER KEEPS PLACING BEHIND THE LEADER MAKES IT
// AGAIN A QUIET SECOND APART: once at once, since a consumer is placed afresh,
// and then [natsobj.QuietWait] apart while the replica catches up — never in
// a loop that asks the cluster for a consumer as fast as it can answer.
func TestAReadThatStaysBehindTheLeaderAsksAgainOnlySoOften(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	if err := b.Put(t.Context(), "asked", strings.NewReader("still behind"), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	lag := &laggingStream{}
	lag.lagging.Store(1 << 20)
	natsobj.SwapStream(b, func(s jetstream.Stream) jetstream.Stream { lag.Stream = s; return lag })
	const pauses = 3
	ctx, cancel := context.WithTimeout(t.Context(), pauses*natsobj.QuietWait+natsobj.QuietWait/2)
	defer cancel()
	if _, err := b.Get(ctx, "asked", 0, -1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a get whose every consumer is behind the leader = %v, want its deadline", err)
	}
	if made, most := lag.made.Load(), int32(2+pauses); made < 3 || made > most {
		t.Fatalf("the get made %d consumers in %d quiet seconds, want the first, one at once "+
			"and one a quiet second apart: %d at most", made, pauses, most)
	}
	if left := consumers(t, client); left != 0 {
		t.Fatalf("the get left %d consumers behind", left)
	}
}

// lastSeq is the newest sequence the bucket's stream has stored.
func lastSeq(t *testing.T, client jetstream.JetStream) uint64 {
	t.Helper()
	stream, err := client.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return info.State.LastSeq
}

// laggingStream stands between a backend and its bucket's stream, and makes
// the next lagging ordered consumers it is asked for consumers on a REPLICA
// BEHIND THE LEADER, one that has stored the stream through holds and no
// further. Each is made on the broker as asked, so it is the broker's own,
// but what it says stops at holds: one that would begin past holds has
// nothing to deliver, the message at holds says nothing follows it, and a
// wait after that is handed to a consumer over a subject nothing is on — as a
// replica catching up tells a consumer made before nothing of what it stores
// after. holds is a sequence the consumer delivers, or zero for none.
type laggingStream struct {
	jetstream.Stream
	holds uint64
	// lagging is how many more consumers to make behind the leader, and
	// made how many ordered consumers the stream has been asked for.
	lagging, made atomic.Int32
}

func (s *laggingStream) OrderedConsumer(ctx context.Context, cfg jetstream.OrderedConsumerConfig) (jetstream.Consumer, error) {
	s.made.Add(1)
	cons, err := s.Stream.OrderedConsumer(ctx, cfg)
	if err != nil || s.lagging.Add(-1) < 0 {
		return cons, err
	}
	none := s.holds == 0 ||
		(cfg.DeliverPolicy == jetstream.DeliverByStartSequencePolicy && cfg.OptStartSeq > s.holds)
	return &laggingConsumer{Consumer: cons, s: s, none: none}, nil
}

// laggingConsumer is a consumer on a replica that has stored s's stream
// through s.holds.
type laggingConsumer struct {
	jetstream.Consumer
	s    *laggingStream
	none bool
}

func (c *laggingConsumer) CachedInfo() *jetstream.ConsumerInfo {
	info := c.Consumer.CachedInfo()
	if info == nil || !c.none {
		return info
	}
	behind := *info
	behind.NumPending = 0
	return &behind
}

func (c *laggingConsumer) Messages(opts ...jetstream.PullMessagesOpt) (jetstream.MessagesContext, error) {
	msgs, err := c.Consumer.Messages(opts...)
	if err != nil {
		return nil, err
	}
	silent, err := c.s.Stream.OrderedConsumer(context.Background(), jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{"$O." + natsobj.Bucket + ".C.silent"}})
	if err != nil {
		msgs.Stop()
		return nil, err
	}
	quiet, err := silent.Messages(opts...)
	if err != nil {
		msgs.Stop()
		return nil, err
	}
	return &laggingMessages{MessagesContext: msgs, holds: c.s.holds,
		quiet: &silentMessages{MessagesContext: quiet, silent: silent, stream: c.s.Stream}}, nil
}

// laggingMessages hands a reader its consumer's messages through holds, the
// one at holds saying nothing follows, and every wait after it to quiet.
type laggingMessages struct {
	jetstream.MessagesContext
	holds uint64
	quiet *silentMessages
	past  bool
}

func (m *laggingMessages) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	if m.past {
		return m.quiet.Next(opts...)
	}
	msg, err := m.MessagesContext.Next(opts...)
	if err != nil {
		return msg, err
	}
	meta, err := msg.Metadata()
	if err != nil {
		return nil, err
	}
	switch seq := meta.Sequence.Stream; {
	case seq > m.holds:
		m.past = true
		return m.quiet.Next(opts...)
	case seq == m.holds:
		m.past = true
		return lastHeld{Msg: msg}, nil
	}
	return msg, nil
}

func (m *laggingMessages) Stop() {
	m.MessagesContext.Stop()
	m.quiet.Stop()
}

// lastHeld is the message a replica behind the leader has stored last, which
// says nothing follows it.
type lastHeld struct{ jetstream.Msg }

func (m lastHeld) Metadata() (*jetstream.MsgMetadata, error) {
	meta, err := m.Msg.Metadata()
	if err != nil {
		return nil, err
	}
	last := *meta
	last.NumPending = 0
	return &last, nil
}

// skippingStream stands between a backend and its bucket's stream, and makes
// a reader of every ordered consumer it is asked for SPEND ONE WAIT ON
// NOTHING once it has taken after messages: that wait is handed to a second
// consumer over a subject nothing is on, so it hears heartbeats and nothing
// else until it ends, and only a wait begun after it reads on — a client that
// skipped its next pull, exactly, which sends it as the next wait begins.
type skippingStream struct {
	jetstream.Stream
	after int
	// skipped counts the waits spent hearing nothing.
	skipped atomic.Int32
}

func (s *skippingStream) OrderedConsumer(ctx context.Context, cfg jetstream.OrderedConsumerConfig) (jetstream.Consumer, error) {
	cons, err := s.Stream.OrderedConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &skippingConsumer{Consumer: cons, s: s}, nil
}

// skippingConsumer is a consumer whose reader spends one wait on nothing.
type skippingConsumer struct {
	jetstream.Consumer
	s *skippingStream
}

func (c *skippingConsumer) Messages(opts ...jetstream.PullMessagesOpt) (jetstream.MessagesContext, error) {
	msgs, err := c.Consumer.Messages(opts...)
	if err != nil {
		return nil, err
	}
	silent, err := c.s.Stream.OrderedConsumer(context.Background(), jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{"$O." + natsobj.Bucket + ".C.silent"}})
	if err != nil {
		msgs.Stop()
		return nil, err
	}
	quiet, err := silent.Messages(opts...)
	if err != nil {
		msgs.Stop()
		return nil, err
	}
	return &skippingMessages{MessagesContext: msgs, quiet: &silentMessages{MessagesContext: quiet,
		silent: silent, stream: c.s.Stream}, s: c.s}, nil
}

// skippingMessages hands a reader its consumer's messages, and its wait after
// the s.after-th one to quiet.
type skippingMessages struct {
	jetstream.MessagesContext
	quiet *silentMessages
	s     *skippingStream
	taken int
	spent bool
}

func (m *skippingMessages) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	if m.taken == m.s.after && !m.spent {
		m.spent = true
		m.s.skipped.Add(1)
		return m.quiet.Next(opts...)
	}
	msg, err := m.MessagesContext.Next(opts...)
	if err == nil {
		m.taken++
	}
	return msg, err
}

func (m *skippingMessages) Stop() {
	m.MessagesContext.Stop()
	m.quiet.Stop()
}

// readsBack reads b's object "asked" from off, wanting want.
func readsBack(t *testing.T, b *natsobj.Backend, off int64, want []byte) error {
	r, err := b.Get(deadline(t), "asked", off, -1)
	if err != nil {
		return err
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("read back %d bytes from %d, want %d exactly", len(got), off, len(want))
	}
	return nil
}

// deafStream stands between a backend and its bucket's stream, and makes the
// next deaf ordered consumers it is asked for DEAF: each is made on the broker
// as asked — so a read asking it about itself has the broker's own answer —
// but the read is handed the messages of a second consumer over a subject
// nothing is on, so its pull reaches somebody else and the consumer it asked
// for never has one. A pull a cluster dropped, exactly: the consumer exists,
// holds no pull and has delivered nothing, and the read hears heartbeats.
type deafStream struct {
	jetstream.Stream
	// deaf is how many more consumers to make deaf, and made how many
	// ordered consumers the stream has been asked for.
	deaf, made atomic.Int32

	mu sync.Mutex
	// names is the broker's name for every consumer made, in order.
	names []string
}

func (s *deafStream) OrderedConsumer(ctx context.Context, cfg jetstream.OrderedConsumerConfig) (jetstream.Consumer, error) {
	s.made.Add(1)
	cons, err := s.Stream.OrderedConsumer(ctx, cfg)
	if err == nil {
		s.mu.Lock()
		s.names = append(s.names, cons.CachedInfo().Name)
		s.mu.Unlock()
	}
	if err != nil || s.deaf.Add(-1) < 0 {
		return cons, err
	}
	silent, err := s.Stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{"$O." + natsobj.Bucket + ".C.silent"}})
	if err != nil {
		return nil, err
	}
	return &deafConsumer{Consumer: cons, silent: silent, stream: s.Stream}, nil
}

// deafConsumer is a consumer whose reader is handed silent's messages.
type deafConsumer struct {
	jetstream.Consumer
	silent jetstream.Consumer
	stream jetstream.Stream
}

func (c *deafConsumer) Messages(opts ...jetstream.PullMessagesOpt) (jetstream.MessagesContext, error) {
	msgs, err := c.silent.Messages(opts...)
	if err != nil {
		return nil, err
	}
	return &silentMessages{MessagesContext: msgs, silent: c.silent, stream: c.stream}, nil
}

// silentMessages are a silent consumer's messages, whose consumer is deleted
// with them, so a case can count the consumers its read left behind.
type silentMessages struct {
	jetstream.MessagesContext
	silent jetstream.Consumer
	stream jetstream.Stream
}

func (m *silentMessages) Stop() {
	m.MessagesContext.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.stream.DeleteConsumer(ctx, m.silent.CachedInfo().Name)
}

// dropConsumers deletes every consumer on the bucket's stream — the get's or
// the listing's own, the only one there is — and names what it deleted.
func dropConsumers(t *testing.T, client jetstream.JetStream) []string {
	t.Helper()
	names := consumerNames(t, client)
	if len(names) != 1 {
		t.Fatalf("the stream holds consumers %v, want the one being read", names)
	}
	stream, err := client.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := stream.DeleteConsumer(t.Context(), name); err != nil {
			t.Fatalf("delete the consumer under the read: %v", err)
		}
	}
	return names
}

// consumerNames names the consumers on the bucket's stream.
func consumerNames(t *testing.T, client jetstream.JetStream) []string {
	t.Helper()
	stream, err := client.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	lister := stream.ConsumerNames(t.Context())
	var names []string
	for name := range lister.Name() {
		names = append(names, name)
	}
	if err := lister.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// replacedBy fails the case unless the stream now holds a consumer that is not
// one of deleted — the replacement a read's remainder came from.
func replacedBy(t *testing.T, client jetstream.JetStream, deleted []string) {
	t.Helper()
	if deleted == nil {
		t.Fatal("the read ended before its consumer was deleted, so nothing was staged")
	}
	now := consumerNames(t, client)
	for _, name := range now {
		if !slices.Contains(deleted, name) {
			return
		}
	}
	t.Fatalf("the stream holds %v after %v was deleted under the read: the rest came "+
		"from no replacement", now, deleted)
}

// consumers is how many consumers the bucket's stream has.
func consumers(t *testing.T, client jetstream.JetStream) int {
	t.Helper()
	stream, err := client.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Consumers
}

// deadline is a context fit for a read.
func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// messages is how many messages the bucket's stream holds.
func messages(t *testing.T, client jetstream.JetStream) uint64 {
	t.Helper()
	stream, err := client.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Msgs
}

// A NODE WITH NO JETSTREAM IS REFUSED BY NAME rather than handed a backend
// that fails on the first upload.
func TestNoJetStreamIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := natsobj.Open(t.Context(), nil, natsobj.Config{}); err == nil {
		t.Fatal("a backend was opened over no JetStream")
	}
}

// bucketJS records every request that writes a stream's configuration and the
// time every read was given to answer in, and can hide the bucket from a
// lookup, leave a lookup unanswered or refuse the first creates; everything
// else is the broker underneath.
type bucketJS struct {
	jetstream.JetStream

	mu sync.Mutex
	// writes is every configuration write sent, in order.
	writes []string
	// hidden is how many lookups are told "not found" before the truth.
	hidden int
	// silent leaves every lookup unanswered, and asks counts them.
	silent bool
	asks   int
	// refusals is how many creates are answered with refusal — before
	// the broker is asked at all — and then the broker answers.
	refusals int
	refusal  error
	// drop leaves every other request of each read unanswered — the first,
	// the third, … — as a member that is not leading the object drops a
	// read without a word.
	drop bool
	// left is what each read's context gave it to answer in, by the read
	// — or a negative duration for one handed no deadline at all — and at
	// when each was sent.
	left map[string][]time.Duration
	at   map[string][]time.Time
}

func (b *bucketJS) wrote(verb string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes = append(b.writes, verb)
}

func (b *bucketJS) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.writes)
}

// read records what ctx gave the read named call to answer in and when it was
// sent, and reports whether it is one to leave unanswered.
func (b *bucketJS) read(ctx context.Context, call string) (drop bool) {
	left := time.Duration(-1)
	if deadline, ok := ctx.Deadline(); ok {
		left = time.Until(deadline)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.left == nil {
		b.left, b.at = map[string][]time.Duration{}, map[string][]time.Time{}
	}
	b.left[call] = append(b.left[call], left)
	b.at[call] = append(b.at[call], time.Now())
	return b.drop && len(b.left[call])%2 == 1
}

// lefts is every read's recorded time to answer, by the read, and when each
// request of it was sent.
func (b *bucketJS) lefts() (map[string][]time.Duration, map[string][]time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	left := make(map[string][]time.Duration, len(b.left))
	at := make(map[string][]time.Time, len(b.at))
	for call, l := range b.left {
		left[call], at[call] = slices.Clone(l), slices.Clone(b.at[call])
	}
	return left, at
}

func (b *bucketJS) ObjectStore(ctx context.Context, bucket string) (jetstream.ObjectStore, error) {
	if b.read(ctx, "bucket "+bucket) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	b.mu.Lock()
	silent, hide := b.silent, b.hidden > 0
	if silent {
		b.asks++
	} else if hide {
		b.hidden--
	}
	b.mu.Unlock()
	switch {
	case silent:
		return nil, nats.ErrTimeout
	case hide:
		return nil, jetstream.ErrBucketNotFound
	}
	return b.JetStream.ObjectStore(ctx, bucket)
}

func (b *bucketJS) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	if b.read(ctx, "stream "+name) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.JetStream.Stream(ctx, name)
}

func (b *bucketJS) CreateObjectStore(ctx context.Context, cfg jetstream.ObjectStoreConfig) (jetstream.ObjectStore, error) {
	b.wrote("create")
	b.mu.Lock()
	refuse := b.refusals > 0
	if refuse {
		b.refusals--
	}
	b.mu.Unlock()
	if refuse {
		return nil, b.refusal
	}
	return b.JetStream.CreateObjectStore(ctx, cfg)
}

func (b *bucketJS) UpdateObjectStore(ctx context.Context, cfg jetstream.ObjectStoreConfig) (jetstream.ObjectStore, error) {
	b.wrote("update")
	return b.JetStream.UpdateObjectStore(ctx, cfg)
}

func (b *bucketJS) CreateOrUpdateObjectStore(ctx context.Context, cfg jetstream.ObjectStoreConfig) (jetstream.ObjectStore, error) {
	b.wrote("create-or-update")
	return b.JetStream.CreateOrUpdateObjectStore(ctx, cfg)
}

func (b *bucketJS) CreateStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	b.wrote("create stream")
	return b.JetStream.CreateStream(ctx, cfg)
}

func (b *bucketJS) UpdateStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	b.wrote("update stream")
	return b.JetStream.UpdateStream(ctx, cfg)
}

func (b *bucketJS) CreateOrUpdateStream(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	b.wrote("create-or-update stream")
	return b.JetStream.CreateOrUpdateStream(ctx, cfg)
}

// OPENING A BUCKET THAT EXISTS WRITES NOTHING.
//
// Every data node opens the bucket at boot, at the same moment, and the update
// the library's create-or-update sends first is never answered when it reaches
// a stream another node has just created: only a group's leader answers an
// update, and that group has not elected one yet. A fleet booting together
// spent the whole fifteen-second ask term on it, on every node. A bucket that
// is there is looked up and bound, and nothing is written to it.
func TestOpeningABucketThatExistsWritesNothing(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	open(t, c)

	w := &bucketJS{JetStream: c}
	if _, err := natsobj.Open(t.Context(), w, natsobj.Config{MaxBytes: testCeiling, Replicas: 1}); err != nil {
		t.Fatalf("open an existing bucket: %v", err)
	}
	if sent := w.sent(); len(sent) != 0 {
		t.Errorf("opening a bucket that exists sent %v; want nothing written — "+
			"an update of a bucket a peer has just made is never answered", sent)
	}
}

// A NODE THAT LOSES THE CREATE RACE BINDS THE BUCKET ITS PEER MADE, and writes
// nothing over it.
//
// Staged as the race leaves it: the lookup is told the bucket is not there —
// the peer's create had not landed yet — and the peer's bucket carries a
// configuration of its own, as an older build's would, so this node's create
// is told the name is taken. The bucket is read back rather than rewritten,
// and it is the peer's: what was stored through it reads back here.
func TestANodeThatLosesTheCreateRaceBindsItsPeersBucket(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	peer, err := c.CreateObjectStore(t.Context(), jetstream.ObjectStoreConfig{
		Bucket: natsobj.Bucket, Description: "a peer's build",
		Storage: jetstream.FileStorage, Replicas: 1, MaxBytes: 2 * testCeiling})
	if err != nil {
		t.Fatalf("the peer's create: %v", err)
	}
	if _, err := peer.PutBytes(t.Context(), "the peer's", []byte("kept")); err != nil {
		t.Fatalf("the peer's put: %v", err)
	}

	w := &bucketJS{JetStream: c, hidden: 1}
	b, err := natsobj.Open(t.Context(), w, natsobj.Config{MaxBytes: testCeiling, Replicas: 1})
	if err != nil {
		t.Fatalf("a node that lost the create race failed to open: %v", err)
	}
	if sent := w.sent(); !slices.Equal(sent, []string{"create"}) {
		t.Errorf("the losing node sent %v; want one create and nothing that "+
			"rewrites the bucket its peer made", sent)
	}
	info, err := c.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.CachedInfo().Config.Description; got != "a peer's build" {
		t.Errorf("the bucket's description is %q, want the peer's — the losing "+
			"node rewrote a bucket it did not make", got)
	}
	rc, err := b.Get(deadline(t), "the peer's", 0, -1)
	if err != nil {
		t.Fatalf("read the peer's object through the losing node: %v", err)
	}
	defer rc.Close()
	if got, err := io.ReadAll(rc); err != nil || string(got) != "kept" {
		t.Errorf("the peer's object read back %q (%v), want %q", got, err, "kept")
	}
}

// A LOOKUP NOBODY ANSWERS FALLS THROUGH TO THE CREATE, which decides what the
// lookup could not: absent and it is made. Run at a lookup ceiling of a fifth
// of a second, because the branch is reached only by spending the whole of it.
func TestALookupNobodyAnswersFallsThroughToTheCreate(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	timing := jsprovision.Clustered(false).Timing()
	timing.Lookup, timing.ReAsk = 200*time.Millisecond, 20*time.Millisecond

	w := &bucketJS{JetStream: c, silent: true}
	if _, err := natsobj.OpenAt(t.Context(), w, natsobj.Config{MaxBytes: testCeiling, Replicas: 1}, timing); err != nil {
		t.Fatalf("a bucket whose lookup went unanswered failed to open: %v — a "+
			"broker that did not reply is not one that said the bucket is absent "+
			"or present", err)
	}
	w.mu.Lock()
	asks := w.asks
	w.mu.Unlock()
	if asks < 2 {
		t.Errorf("the lookup was sent %d time(s) before the create; the ceiling "+
			"holds several, and one means it was never asked again", asks)
	}
	if sent := w.sent(); !slices.Equal(sent, []string{"create"}) {
		t.Errorf("after an unanswered lookup the node sent %v, want one create", sent)
	}
	if _, err := c.Stream(t.Context(), natsobj.Stream); err != nil {
		t.Errorf("the bucket is not there after the open: %v", err)
	}
}

// A BUCKET CREATE THE CLUSTER CANNOT ANSWER YET IS ASKED AGAIN, whichever of
// the two ways it goes unanswered — through [jsprovision.Timing.Place], as
// every replicated create at boot is.
//
// The lookup IS answered here — the bucket is not there — so the create is the
// one request left to decide, and on a fleet booting together it is sent into
// a metadata group that is still forming. That group either refuses to place
// it ("no suitable peers", until enough members have joined) or drops it
// unanswered (no leader yet), and both clear by asking again. Sent once, the
// first is final to [jsprovision.Refused] and the second is read back into a
// bucket nobody made: either way a node fails its boot over a fleet that is
// merely starting. The lookup in front of the create absorbs neither, since it
// was answered; [TestOpeningOnALeafWaitsOutItsLink] holds the case where it
// is the lookup that goes unanswered.
//
// At a clustered timing with its two cadences scaled to milliseconds, because
// the production ones ask again after a quarter of a second and a second.
func TestABucketCreateTheClusterCannotAnswerYetIsAskedAgain(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]error{
		"not placed yet": &jetstream.APIError{Code: 400, ErrorCode: 10005,
			Description: "no suitable peers for placement"},
		"not answered": nats.ErrNoResponders,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := memberClient(t)
			timing := jsprovision.Clustered(true).Timing()
			timing.ReAsk, timing.PlacementRetry = 20*time.Millisecond, 5*time.Millisecond

			const refused = 2
			w := &bucketJS{JetStream: c, refusals: refused, refusal: refusal}
			if _, err := natsobj.OpenAt(t.Context(), w, natsobj.Config{MaxBytes: testCeiling, Replicas: 1, Clustered: true}, timing); err != nil {
				t.Fatalf("the bucket did not open once the create was answered: %v — "+
					"a create the forming cluster had not answered yet was taken "+
					"as the answer", err)
			}
			want := slices.Repeat([]string{"create"}, refused+1)
			if sent := w.sent(); !slices.Equal(sent, want) {
				t.Errorf("the node sent %v, want %v: each create the cluster did not "+
					"answer asked again, and nothing else written", sent, want)
			}
			if _, err := c.Stream(t.Context(), natsobj.Stream); err != nil {
				t.Errorf("the bucket is not there after the open: %v", err)
			}
		})
	}
}

// A BUCKET THE ACCOUNT HAS NO LIMIT FOR NAMES THE CLASS, as every other create
// does.
//
// `no JetStream default or applicable tiered limit present` (10120) is the
// broker answering that the account's limits are TIERED and carry none for the
// replica class `stream.replicas` puts this node in — decided before a byte is
// compared, and this bucket reserves none anyway, so there is nothing here to
// make smaller. It is one of [jsprovision.Refused]'s, so it was already final
// rather than read back. But it came back as the broker's bare text, while the
// stream, the coordination buckets and both consumers attach
// [jsprovision.NoApplicableLimitDetail], which names the class and the field
// that picks it: an operator meeting it at this one create had nothing to move.
func TestABucketWithNoApplicableLimitNamesTheClass(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	refusal := &jetstream.APIError{ErrorCode: 10120, Code: 400,
		Description: "no JetStream default or applicable tiered limit present"}
	w := &bucketJS{JetStream: c, refusals: 1, refusal: refusal}

	// THREE COPIES, because the class is what the clause has to name; the
	// refusal arrives before the member is asked, so its having no peers
	// never comes into it.
	b, err := natsobj.Open(t.Context(), w, natsobj.Config{MaxBytes: testCeiling, Replicas: 3})
	if err == nil {
		t.Fatal("a bucket the account carries no limit for was reported as opened")
	}
	if b != nil {
		t.Error("a backend was returned beside the refusal, and nothing was " +
			"placed for it to bind")
	}
	if !errors.Is(err, refusal) {
		t.Errorf("the refusal is not the broker's own:\n%v", err)
	}
	for _, needle := range []string{
		"R3",              // the class the account has no limit for
		"stream.replicas", // the field that decides which class is wanted
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("the refusal does not mention %q, so an operator gets the "+
				"broker's bare text and nothing to move:\n%v", needle, err)
		}
	}
	if strings.Contains(err.Error(), "reading it back") {
		t.Errorf("the refusal was read back, so an account with no applicable "+
			"limit is reported as a bucket that is not there:\n%v", err)
	}
	// ASKED ONCE: a limit table is not changed by a member arriving, so the
	// placement retry has nothing to wait for.
	if sent := w.sent(); !slices.Equal(sent, []string{"create"}) {
		t.Errorf("the node sent %v, want one create", sent)
	}
}

// A CREATE THAT FAILED AND A READ-BACK THAT FOUND NOTHING ARE BOTH IN THE
// ERROR, as they are at every other create site: the create's says what went
// wrong, and the read-back's says the bucket really is absent — each still
// recognisable by [errors.Is], so neither is reduced to text a caller can only
// print.
//
// Staged as a create answered "name in use" — a peer's win, which is read back
// rather than final — whose read-back never finds the bucket, at a read-back
// window of a tenth of a second.
func TestAFailedCreatesReadBackKeepsBothErrors(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	inUse := &jetstream.APIError{ErrorCode: 10058, Code: 400,
		Description: "stream name already in use with a different configuration"}
	w := &bucketJS{JetStream: c, hidden: 1 << 20, refusals: 1, refusal: inUse}
	timing := jsprovision.Clustered(false).Timing()
	timing.ReadBack = 100 * time.Millisecond

	_, err := natsobj.OpenAt(t.Context(), w, natsobj.Config{MaxBytes: testCeiling, Replicas: 1}, timing)
	if err == nil {
		t.Fatal("a bucket the read-back never found was reported as opened")
	}
	if !errors.Is(err, inUse) {
		t.Errorf("the create's own error is not in the report:\n%v", err)
	}
	if !errors.Is(err, jetstream.ErrBucketNotFound) {
		t.Errorf("the read-back's not-found is in the report only as text:\n%v", err)
	}
}

// EVERY READ OPEN MAKES IS ASKED AGAIN AT THE READ TERM, never at a write's.
//
// The bucket's lookup, the read-back after a create whose name was taken, and
// the stream behind the bucket are all metadata READS, which the broker
// answers when it processes them or never ([jsprovision.ReadTerm]). On a fleet
// booting together a bucket another node has just asked for is in flight, and
// every member but the one preferred to lead it drops a read of it without a
// word: a read held for a write's term waits on a reply that does not exist,
// and that was measured costing a fleet's boot sixteen idle seconds a lookup.
// So a request nobody answers is presumed lost at the read term and the next
// is sent a re-ask after it, the first still outstanding beside it; asked as
// a write, the next would follow a write's term later — fifteen seconds
// clustered and thirty solo.
//
// At each topology's own write term and a read term short enough to measure
// against it, since what is held is which of its cadences each call is asked
// again at. Two opens: one that loses the create race as
// [TestANodeThatLosesTheCreateRaceBindsItsPeersBucket] stages it — the lookup
// told the bucket is not there, and the create told its name is taken by a
// peer's — so the read-back runs, and one that finds the bucket; every read's
// first request dropped, and its second answered.
func TestEveryReadOpenMakesIsAskedAgainAtTheReadTerm(t *testing.T) {
	t.Parallel()
	for _, clustered := range []bool{false, true} {
		t.Run(fmt.Sprintf("clustered=%v", clustered), func(t *testing.T) {
			t.Parallel()
			c := memberClient(t)
			if _, err := c.CreateObjectStore(t.Context(), jetstream.ObjectStoreConfig{
				Bucket: natsobj.Bucket, Description: "a peer's build",
				Storage: jetstream.FileStorage, Replicas: 1, MaxBytes: testCeiling}); err != nil {
				t.Fatalf("the peer's create: %v", err)
			}
			timing := jsprovision.Clustered(clustered).Timing()
			timing.ReadTerm, timing.ReAsk = 100*time.Millisecond, 50*time.Millisecond
			w := &bucketJS{JetStream: c, hidden: 1, drop: true}
			cfg := natsobj.Config{MaxBytes: testCeiling, Replicas: 1, Clustered: clustered}
			for _, open := range []string{"losing the create race", "finding the bucket"} {
				if _, err := natsobj.OpenAt(t.Context(), w, cfg, timing); err != nil {
					t.Fatalf("open the bucket, %s: %v", open, err)
				}
			}

			lefts, at := w.lefts()
			// THE LOOKUP OF EACH OPEN, AND THE RACE'S READ-BACK: three
			// reads of the bucket, two requests each. Fewer, and one of
			// the three was not measured at all.
			if n := len(lefts["bucket "+natsobj.Bucket]); n < 6 {
				t.Errorf("the bucket was asked %d time(s), want two for each of "+
					"each open's lookup and the lost race's read-back", n)
			}
			if n := len(lefts["stream "+natsobj.Stream]); n < 4 {
				t.Errorf("the stream was asked %d time(s), want two for each "+
					"open's bind", n)
			}
			ceiling := max(timing.Lookup, timing.ReadBack)
			for call, ls := range lefts {
				for _, left := range ls {
					switch {
					case left < 0:
						t.Errorf("%s was given no deadline at all — a dropped "+
							"read with none waits on a reply nobody will send "+
							"until the boot gives up", call)
					case left > ceiling:
						t.Errorf("%s was given %v to answer, past the %v the "+
							"lookup and the read-back are bounded by", call, left,
							ceiling)
					}
				}
			}
			// WELL INSIDE A WRITE'S TERM, with a read term of slack for a
			// loaded runner: what tells the cadences apart is the hundredfold
			// between them.
			limit := 2*timing.ReadTerm + timing.ReAsk
			for call, sent := range at {
				for i := 0; i+1 < len(sent); i += 2 {
					if gap := sent[i+1].Sub(sent[i]); gap > limit {
						t.Errorf("%s was asked again %v after a request nobody "+
							"answered, want within %v — a dropped read held for a "+
							"write's %v term waits on a reply nobody will send",
							call, gap, limit, timing.AskTerm)
					}
				}
			}
		})
	}
}

// A BUCKET REPLICATED BELOW THIS NODE IS REFUSED, BY THE SETTING, as the queue
// refuses such a stream: an upload acknowledged there would prove fewer copies
// than stream.replicas promises, and nothing here rewrites a bucket that
// exists to make it match.
func TestABucketReplicatedBelowThisNodeIsRefused(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	open(t, c)

	_, err := natsobj.Open(t.Context(), c, natsobj.Config{MaxBytes: testCeiling, Replicas: 3})
	if err == nil || !strings.Contains(err.Error(), "stream.replicas") {
		t.Fatalf("a node configured for three copies opened a bucket kept at "+
			"one: %v — want a refusal naming stream.replicas", err)
	}
}

func open(t testing.TB, client jetstream.JetStream) *natsobj.Backend {
	t.Helper()
	return openWith(t, client, natsobj.Config{MaxBytes: testCeiling, Replicas: 1})
}

// openWith is [open] with the configuration cfg.
func openWith(t testing.TB, client jetstream.JetStream, cfg natsobj.Config) *natsobj.Backend {
	t.Helper()
	b, err := natsobj.Open(t.Context(), client, cfg)
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	clients.Store(objstore.Backend(b), client)
	t.Cleanup(func() { clients.Delete(objstore.Backend(b)) })
	return b
}

func memberClient(t testing.TB) jetstream.JetStream { return memberClientAt(t, js.Config{}) }

// memberClientAt is a client of a fresh member started from base, whose
// server name, store directory and declared store size
// ([brokertest.StoreMaxBytes]) are its own — the rest of base, the file
// store's sync among it, is the caller's.
func memberClientAt(t testing.TB, base js.Config) jetstream.JetStream {
	t.Helper()
	base.ServerName, base.StoreDir = "member", t.TempDir()
	base.StoreMaxBytes = brokertest.StoreMaxBytes
	srv, err := js.StartServer(t.Context(), base)
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	return client(t, srv)
}

// leafClient is a client of a leaf of a member, once the leaf's link is up.
//
// WAITED FOR, as the engine waits for it before it opens anything on a leaf
// ([js.Server.Client]): every case through a leaf opened its bucket while the
// link was still forming, and each paid a second of [jsprovision.ReAsk] for a
// request nobody could answer yet — a second a case, where the member's own
// cases take a tenth of one. Opening on a link that is NOT up is a case of its
// own ([TestOpeningOnALeafWaitsOutItsLink]).
//
// The member binds [js.AnyPort] and names it afterwards, so nothing else can
// take the port between its choosing and its binding.
func leafClient(t testing.TB) jetstream.JetStream { return leafClientAt(t, js.Config{}) }

// leafClientAt is [leafClient] with its member started from base, as
// [memberClientAt] starts one.
func leafClientAt(t testing.TB, base js.Config) jetstream.JetStream {
	t.Helper()
	base.ServerName, base.StoreDir = "member", t.TempDir()
	base.StoreMaxBytes = brokertest.StoreMaxBytes
	base.LeafHost, base.LeafPort = "127.0.0.1", js.AnyPort
	base.FleetAuth = linkFleetAuth()
	member, err := js.StartServer(t.Context(), base)
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(member.Shutdown)
	leaf := startLeaf(t, member.LeafPort())
	c := client(t, leaf)
	awaitLink(t, c)
	return c
}

// linkFleetAuth is the keyring this package's members serving leaves and the
// leaves joined to them hold: a leaf link is admitted only on a key its member
// holds. A literal placeholder, never a key anybody deploys.
var linkFleetAuth = sync.OnceValue(func() js.FleetAuth {
	auth, err := js.FleetAuthFrom(runtoken.OneKey("natsobj-link",
		"the object store's leaf cases' placeholder key material"))
	if err != nil {
		panic(err)
	}
	return auth
})

// startLeaf starts a leaf that dials a member's leaf listener on port.
func startLeaf(t testing.TB, port int) *js.Server {
	t.Helper()
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf",
		LeafURLs:  []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", port)},
		FleetAuth: linkFleetAuth()})
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	return leaf
}

// awaitLink waits until the members' JetStream answers across the leaf's
// link: a leaf runs none of its own, so an answer is the link.
func awaitLink(t testing.TB, c jetstream.JetStream) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		_, err := c.AccountInfo(ctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the leaf's link to its member never answered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A BUCKET OPENED ON A LEAF WHOSE LINK IS NOT UP YET OPENS ONCE IT IS.
//
// A leaf's JetStream is across its link, and until the link forms nobody
// answers: every request comes back "no responders" at once. The first one is
// the bucket's LOOKUP, a read asked again for as long as nobody answers
// ([jsprovision.ReadWith], under [jsprovision.LookupBudget]) — so on a link
// that forms inside that ceiling it is the lookup that waits it out, and the
// create is sent once the link is up. A create that goes unanswered itself is
// asked again as well, through [jsprovision.Timing.Place], and
// [TestABucketCreateTheClusterCannotAnswerYetIsAskedAgain] holds that half.
// What this case holds is the whole of it from the caller's side: the leaf is
// started with no member at all, Open is left asking, and only then does the
// member come up.
//
// THE LEAF DIALS A GATE THIS CASE HOLDS ([holdGate]), never the port its member
// will bind: the address has to be named before the member exists, so it
// cannot be the member's own [js.AnyPort], and a port reserved for the member
// to bind later is released first, for anything else on the machine to take.
func TestOpeningOnALeafWaitsOutItsLink(t *testing.T) {
	t.Parallel()
	gate := holdGate(t)
	c := client(t, startLeaf(t, gate.port()))
	type result struct {
		b   *natsobj.Backend
		err error
	}
	opened := make(chan result, 1)
	go func() {
		b, err := natsobj.Open(t.Context(), c, natsobj.Config{MaxBytes: testCeiling, Replicas: 1})
		opened <- result{b, err}
	}()
	select {
	case r := <-opened:
		t.Fatalf("the bucket opened (%v) with no member for the leaf to reach", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: t.TempDir(),
		StoreMaxBytes: brokertest.StoreMaxBytes, FleetAuth: linkFleetAuth()})
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(member.Shutdown)
	gate.open(fmt.Sprintf("127.0.0.1:%d", member.LeafPort()))
	var r result
	select {
	case r = <-opened:
	case <-time.After(time.Minute):
		t.Fatal("the bucket never opened once the member was up")
	}
	if r.err != nil {
		t.Fatalf("opening on a leaf whose link came up late: %v", r.err)
	}
	// AND IT IS THE BUCKET: an object goes up and comes back across the link.
	if err := r.b.Put(t.Context(), "late", bytes.NewReader([]byte("linked")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	rc, err := r.b.Get(deadline(t), "late", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if got, err := io.ReadAll(rc); err != nil || string(got) != "linked" {
		t.Fatalf("read back %q, %v", got, err)
	}
}

func client(t testing.TB, srv *js.Server) jetstream.JetStream {
	t.Helper()
	nc, err := srv.Conn()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	c, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("a JetStream client: %v", err)
	}
	return c
}

// gate is a loopback address a case holds for its whole life, standing in for
// a member that is not up yet: until [gate.open], every connection to it is
// closed as it arrives — which a leaf reads as it reads a refused dial, no
// member to reach, and dials again — and after it each one is piped to the
// member's own leaf listener, at the rate [gate.slow] set towards the leaf.
type gate struct {
	ln     net.Listener
	target atomic.Pointer[string]
	// rate is how many bytes a second a connection opened from now on
	// carries from the member to the leaf; zero is as fast as loopback.
	rate atomic.Int64

	mu    sync.Mutex
	shut  bool
	conns []net.Conn
}

// holdGate binds a gate on a port the OS picks and keeps it until the case
// ends, when it closes the listener and every connection it carried.
func holdGate(t *testing.T) *gate {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the gate: %v", err)
	}
	g := &gate{ln: ln}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			target := g.target.Load()
			if target == nil || !g.track(in) {
				_ = in.Close()
				continue
			}
			wg.Go(func() { g.pipe(in, *target) })
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		g.mu.Lock()
		g.shut = true
		conns := g.conns
		g.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		wg.Wait()
	})
	return g
}

func (g *gate) port() int { return g.ln.Addr().(*net.TCPAddr).Port }

// open pipes every connection from now on to target.
func (g *gate) open(target string) { g.target.Store(&target) }

// slow holds every connection opened from now on to rate bytes a second from
// the member to the leaf — the direction a read's messages cross.
func (g *gate) slow(rate int64) { g.rate.Store(rate) }

// track records c for the case's end to close, and refuses it once that end
// has begun, so no connection outlives the gate.
func (g *gate) track(c net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut {
		return false
	}
	g.conns = append(g.conns, c)
	return true
}

// pipe copies in to a fresh connection to target and back, until either side
// ends, then closes both.
func (g *gate) pipe(in net.Conn, target string) {
	out, err := net.Dial("tcp", target)
	if err != nil {
		_ = in.Close()
		return
	}
	if !g.track(out) {
		_ = in.Close()
		_ = out.Close()
		return
	}
	rate := g.rate.Load()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(out, in); done <- struct{}{} }()
	go func() { throttle(in, out, rate); done <- struct{}{} }()
	<-done
	_ = in.Close()
	_ = out.Close()
	<-done
}

// throttle copies src to dst until either ends, at no more than rate bytes a
// second — or as fast as it can where rate is zero.
//
// A PIECE IS WRITTEN WHEN A LINK AT RATE WOULD HAVE FINISHED CARRYING IT, each
// one's time counted from when the one before it finished or from when it
// arrived, whichever is later: a link that was idle has carried nothing ahead,
// so it never lets a burst through at once.
func throttle(dst io.Writer, src io.Reader, rate int64) {
	if rate == 0 {
		_, _ = io.Copy(dst, src)
		return
	}
	buf := make([]byte, 4<<10)
	carried := time.Now()
	for {
		n, err := src.Read(buf)
		if n > 0 {
			carried = later(carried, time.Now()).Add(time.Duration(n) * time.Second / time.Duration(rate))
			time.Sleep(time.Until(carried))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// later is whichever of a and b is later.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// testCeiling is the reservation every case here opens the bucket with:
// eight mebibytes, TWICE the largest object any case stores — the 4 MiB read
// across a lost consumer ([TestAReadWhoseConsumerIsLostFinishesOnItsReplacement])
// — which leaves the object's metadata and its pieces' message overhead room
// with a whole second copy to spare. No case holds more at once: the 4 MiB puts
// put_test.go stalls on a frozen link land a window of pieces at most, and are
// purged when they fail, and the contract suite's largest object is two pieces
// and a tail (some 256 KiB, a thirty-second of the ceiling). The margin asks
// for nothing more: a case needs only its own bytes to fit, and a case about
// the ceiling opens a bucket of its own size
// ([TestAFullBucketLeavesTheCoordinationRemainder]). Every member it is
// opened on declares its own limit ([brokertest.StoreMaxBytes]) rather than
// read one off the runner's free disk, so whether it is granted does not
// depend on the machine. The engine never
// derives one this small (store.objects.nats_max_bytes takes a gibibyte at
// least); the package itself takes any positive reservation.
const testCeiling = 8 << 20

// A BUCKET IS CREATED HOLDING ITS RESERVATION, and the ceiling Usage reports
// is the one the broker enforces.
func TestABucketIsCreatedHoldingItsReservation(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	b, err := natsobj.Open(t.Context(), c, natsobj.Config{MaxBytes: 3 * testCeiling, Replicas: 1})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	stream, err := c.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := stream.CachedInfo().Config.MaxBytes; got != 3*testCeiling {
		t.Fatalf("the bucket was created with a ceiling of %d, want the %d reservation", got, 3*testCeiling)
	}
	usage, err := b.Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if usage.MaxBytes != 3*testCeiling || usage.Bytes != 0 {
		t.Fatalf("an empty bucket reports %+v, want nothing stored against its reservation", usage)
	}
}

// NO RESERVATION IS REFUSED, before the broker is asked anything: a node that
// created the bucket unbounded would hand it the remainder the fleet's
// coordination records need.
func TestABucketWithNoReservationIsNeverCreated(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	w := &bucketJS{JetStream: c}
	if _, err := natsobj.Open(t.Context(), w, natsobj.Config{Replicas: 1}); err == nil {
		t.Fatal("a node with no reservation opened the bucket")
	}
	if sent := w.sent(); len(sent) != 0 {
		t.Errorf("a refused open still sent %v", sent)
	}
}

// A BUCKET THAT HOLDS NO CEILING IS REFUSED to a node whose figure is derived,
// naming both gestures that bound it, and nothing here rewrites its
// configuration to fix it.
func TestABucketThatHoldsNoCeilingIsRefused(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	if _, err := c.CreateObjectStore(t.Context(), jetstream.ObjectStoreConfig{
		Bucket: natsobj.Bucket, Storage: jetstream.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("make an unbounded bucket: %v", err)
	}
	w := &bucketJS{JetStream: c}
	_, err := natsobj.Open(t.Context(), w, natsobj.Config{MaxBytes: testCeiling, Replicas: 1})
	if err == nil {
		t.Fatal("a bucket holding no ceiling was opened")
	}
	for _, want := range []string{"store.objects.nats_max_bytes", "nats stream update " + natsobj.Stream, fmt.Sprintf("--max-bytes=%d", testCeiling)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if sent := w.sent(); len(sent) != 0 {
		t.Errorf("the refusal wrote %v to a bucket this node is not the writer of", sent)
	}
}

// A BUCKET THAT HOLDS NO CEILING TAKES THE ONE AN OPERATOR WROTE, at boot: the
// bucket every build before the reservation created, on an embedded broker
// nothing outside the engine can update. Only the ceiling changes, and what
// the bucket held before reads back through it.
func TestABucketThatHoldsNoCeilingTakesAnExplicitReservation(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	unbounded, err := c.CreateObjectStore(t.Context(), jetstream.ObjectStoreConfig{
		Bucket: natsobj.Bucket, Description: "an older build's", Storage: jetstream.FileStorage, Replicas: 1})
	if err != nil {
		t.Fatalf("make an unbounded bucket: %v", err)
	}
	if _, err := unbounded.PutBytes(t.Context(), "kept", []byte("before the bound")); err != nil {
		t.Fatalf("store through the unbounded bucket: %v", err)
	}
	w := &bucketJS{JetStream: c}
	b, err := natsobj.Open(t.Context(), w, natsobj.Config{MaxBytes: testCeiling, Explicit: true, Replicas: 1})
	if err != nil {
		t.Fatalf("an explicit reservation did not bound the bucket: %v", err)
	}
	if sent := w.sent(); !slices.Equal(sent, []string{"update stream"}) {
		t.Errorf("bounding the bucket sent %v; want the one stream update", sent)
	}
	usage, err := b.Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if usage.MaxBytes != testCeiling {
		t.Fatalf("the bucket holds a ceiling of %d, want the %d the operator wrote", usage.MaxBytes, testCeiling)
	}
	info, err := c.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.CachedInfo().Config.Description; got != "an older build's" {
		t.Errorf("the bucket's description is %q: bounding it rewrote more than its ceiling", got)
	}
	rc, err := b.Get(deadline(t), "kept", 0, -1)
	if err != nil {
		t.Fatalf("an object stored before the bound is gone: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(got) != "before the bound" {
		t.Fatalf("read back %q, %v; want what was stored before the bound", got, err)
	}
}

// A BUCKET THAT EXISTS KEEPS THE CEILING IT HOLDS, whatever this node was
// configured with: the broker granted it once, and only the capacity gesture
// changes it.
func TestABucketThatExistsKeepsItsCeiling(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	if _, err := natsobj.Open(t.Context(), c, natsobj.Config{MaxBytes: 2 * testCeiling, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	b, err := natsobj.Open(t.Context(), c, natsobj.Config{MaxBytes: 5 * testCeiling, Explicit: true, Replicas: 1})
	if err != nil {
		t.Fatalf("a bucket at another ceiling was refused: %v", err)
	}
	usage, err := b.Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if usage.MaxBytes != 2*testCeiling {
		t.Fatalf("the bucket reports a ceiling of %d, want the %d it was created with", usage.MaxBytes, 2*testCeiling)
	}
}

// A FULL BUCKET LEAVES THE COORDINATION REMAINDER: a put past its reservation
// is refused as ErrFull and stores nothing, and a coordination bucket on the
// same broker still takes writes: the outage a ceiling-less bucket filling
// the broker would have been.
func TestAFullBucketLeavesTheCoordinationRemainder(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	const reservation = 4 * natsobj.MessageBytes
	b, err := natsobj.Open(t.Context(), c, natsobj.Config{MaxBytes: reservation, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	store, err := objstore.NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{7}, natsobj.MessageBytes)
	var refused error
	for range 8 {
		if _, err := store.Put(t.Context(), bytes.NewReader(body), int64(len(body)), objstore.PutMeta{}); err != nil {
			refused = err
			break
		}
	}
	full, ok := errors.AsType[*objstore.Full](refused)
	if !ok || !errors.Is(refused, objstore.ErrFull) {
		t.Fatalf("a put past the reservation answered %v, want an objstore.Full over ErrFull", refused)
	}
	if full.Remedy != natsobj.FullRemedy {
		t.Errorf("a full bucket named the room %q, want the bucket's own, %q", full.Remedy, natsobj.FullRemedy)
	}
	usage, err := b.Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if usage.MaxBytes != reservation || usage.Bytes > reservation {
		t.Fatalf("the bucket reports %+v against a %d-byte reservation", usage, reservation)
	}
	if err := b.Pending(t.Context(), func(p objstore.Pending) error {
		return fmt.Errorf("the refused put left an unfinished upload %+v", p)
	}); err != nil {
		t.Fatal(err)
	}
	kv, err := c.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "leases"})
	if err != nil {
		t.Fatalf("a coordination bucket could not be made beside a full object bucket: %v", err)
	}
	if _, err := kv.Put(t.Context(), "seat", bytes.Repeat([]byte{1}, 4096)); err != nil {
		t.Fatalf("a coordination write was refused beside a full object bucket: %v", err)
	}
}

// A SEALED BUCKET IS READ AND NEVER WRITTEN: the fence a move to another
// store closes behind itself refuses every put, keeps every object readable,
// and sealing it twice is no error.
func TestASealedBucketRefusesPutsAndKeepsItsObjects(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	b := open(t, c)
	store, err := objstore.NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("kept after the move")
	o, err := store.Put(t.Context(), bytes.NewReader(body), int64(len(body)), objstore.PutMeta{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.Seal(t.Context()); err != nil {
			t.Fatalf("seal: %v", err)
		}
	}
	if _, err := store.Put(t.Context(), bytes.NewReader(body), int64(len(body)), objstore.PutMeta{}); err == nil {
		t.Fatal("a sealed bucket stored a put")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	rc, err := store.Open(ctx, o)
	if err != nil {
		t.Fatalf("a sealed bucket's object does not open: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("a sealed bucket's object reads %q, %v", got, err)
	}
}
