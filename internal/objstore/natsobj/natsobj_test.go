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

// THE BROKER'S OWN OBJECT STORE PASSES THE SUITE, on a member's own client.
func TestContract(t *testing.T) {
	t.Parallel()
	objstoretest.Run(t, func(t *testing.T) objstore.Backend {
		return open(t, memberClient(t))
	}, suite)
}

// AND THROUGH A LEAF: a node without `data` runs no JetStream and reaches the
// members' across its leaf link, and its uploads and downloads go through
// exactly that.
func TestContractThroughALeaf(t *testing.T) {
	t.Parallel()
	objstoretest.Run(t, func(t *testing.T) objstore.Backend {
		return open(t, leafClient(t))
	}, suite)
}

// EVERY MESSAGE OF A PUT BUT ITS LAST IS FULL, however few bytes at a time the
// reader yields. The library sends one message per read it makes, so a body
// arriving a byte at a time would otherwise be stored as a message per byte.
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
// byte for byte.
//
// A get is an ordered consumer of the object's messages, and the broker can
// take that consumer away under a reader — reaped while the reader was slow,
// dropped in a leader change. The client then makes a new one from the last
// message the reader took, which is the path every one of those failures goes
// down, and the one this case stages: the consumer is deleted a few messages
// into a 4 MiB read, past what the read held in hand, so the rest can only
// come from a replacement. This replaced a read paced over twelve seconds to
// outlast two of the consumer's heartbeats; the vendored client arms none
// between reads, so that read tripped nothing and never asserted that a
// replacement was made.
func TestAReadWhoseConsumerIsLostFinishesOnItsReplacement(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	// THE REPLACEMENT WAITS FOR THE OUTSTANDING PULL TO EXPIRE, so the
	// expiry is the client's floor of a second rather than the production
	// ten: what is held is that the rest of the read comes from a new
	// consumer, not how long that takes ([TestTheTimeALostReaderCostsIsBounded]).
	natsobj.ShortenPulls(b, time.Second)
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
	var deleted []string
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
			deleted = dropConsumers(t, client)
		}
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("a read across a lost consumer answered %d bytes, want %d exactly", got.Len(), len(data))
	}
	replacedBy(t, client, deleted)
}

// A LISTING ENDS EVEN WHEN THE NEWEST METADATA CANNOT BE READ. The library's
// own watch learns it has seen everything only from a message it decoded, so
// one that ended on garbage never finished — and the collector, whose context
// has no deadline, waited on it for ever.
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
	var names []string
	if err := b.List(ctx, func(info objstore.Info) error {
		names = append(names, info.Name)
		return nil
	}); err != nil {
		t.Fatalf("a listing ending on unreadable metadata: %v", err)
	}
	if len(names) != 1 || names[0] != "readable" {
		t.Fatalf("listed %q, want the one readable object", names)
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
// visiting every name exactly once.
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
	natsobj.ShortenPulls(b, time.Second) // for the read case's reason
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
	var deleted, last []string
	if err := b.List(t.Context(), func(info objstore.Info) error {
		seen[info.Name]++
		switch len(seen) {
		case 10:
			deleted = dropConsumers(t, client)
		case names:
			last = consumerNames(t, client)
		}
		return nil
	}); err != nil {
		t.Fatalf("a listing whose consumer was deleted: %v", err)
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

// THE TIME A LOST READER COSTS IS BOUNDED, and by a number that keeps the
// client's own heartbeat.
//
// A get or a listing whose consumer the broker loses is silent until its
// outstanding pull expires — that is when the client makes the replacement the
// two cases above stage — so the expiry IS the stall, and the client's default
// made it thirty seconds. Below ten, the client halves its five-second
// heartbeat for an ordered consumer, and missed heartbeats replace the
// consumer too, with a metadata request each, on a broker already too slow to
// send them. And it stays well inside the minute a read that hears nothing is
// ended at.
func TestTheTimeALostReaderCostsIsBounded(t *testing.T) {
	t.Parallel()
	if natsobj.PullExpiry < 10*time.Second {
		t.Errorf("a pull expires after %v; below ten seconds the client halves the "+
			"heartbeat its ordered consumers are replaced on", natsobj.PullExpiry)
	}
	if natsobj.PullExpiry > objstore.ReadStall/4 {
		t.Errorf("a lost reader stays silent for %v, too close to the %v a read is "+
			"ended at for hearing nothing", natsobj.PullExpiry, objstore.ReadStall)
	}
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

// bucketJS records every request that writes a stream's configuration, and
// can hide the bucket from a lookup or leave a lookup unanswered; everything
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

func (b *bucketJS) ObjectStore(ctx context.Context, bucket string) (jetstream.ObjectStore, error) {
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

func (b *bucketJS) CreateObjectStore(ctx context.Context, cfg jetstream.ObjectStoreConfig) (jetstream.ObjectStore, error) {
	b.wrote("create")
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
	if _, err := natsobj.Open(t.Context(), w, natsobj.Config{Replicas: 1}); err != nil {
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
		Storage: jetstream.FileStorage, Replicas: 1})
	if err != nil {
		t.Fatalf("the peer's create: %v", err)
	}
	if _, err := peer.PutBytes(t.Context(), "the peer's", []byte("kept")); err != nil {
		t.Fatalf("the peer's put: %v", err)
	}

	w := &bucketJS{JetStream: c, hidden: 1}
	b, err := natsobj.Open(t.Context(), w, natsobj.Config{Replicas: 1})
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
	if _, err := natsobj.OpenAt(t.Context(), w, natsobj.Config{Replicas: 1}, timing); err != nil {
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

// A BUCKET REPLICATED BELOW THIS NODE IS REFUSED, BY THE SETTING, as the queue
// refuses such a stream: an upload acknowledged there would prove fewer copies
// than stream.replicas promises, and nothing here rewrites a bucket that
// exists to make it match.
func TestABucketReplicatedBelowThisNodeIsRefused(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	open(t, c)

	_, err := natsobj.Open(t.Context(), c, natsobj.Config{Replicas: 3})
	if err == nil || !strings.Contains(err.Error(), "stream.replicas") {
		t.Fatalf("a node configured for three copies opened a bucket kept at "+
			"one: %v — want a refusal naming stream.replicas", err)
	}
}

func open(t *testing.T, client jetstream.JetStream) *natsobj.Backend {
	t.Helper()
	b, err := natsobj.Open(t.Context(), client, natsobj.Config{Replicas: 1})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	clients.Store(objstore.Backend(b), client)
	t.Cleanup(func() { clients.Delete(objstore.Backend(b)) })
	return b
}

func memberClient(t *testing.T) jetstream.JetStream {
	t.Helper()
	srv, err := js.StartServer(t.Context(), js.Config{ServerName: "member", StoreDir: t.TempDir()})
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
func leafClient(t *testing.T) jetstream.JetStream {
	t.Helper()
	port := unusedPort(t)
	member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: port, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(member.Shutdown)
	leaf := startLeaf(t, port)
	c := client(t, leaf)
	awaitLink(t, c)
	return c
}

// startLeaf starts a leaf that dials a member's leaf listener on port.
func startLeaf(t *testing.T, port int) *js.Server {
	t.Helper()
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf",
		LeafURLs: []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", port)}})
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	return leaf
}

// awaitLink waits until the members' JetStream answers across the leaf's
// link: a leaf runs none of its own, so an answer is the link.
func awaitLink(t *testing.T, c jetstream.JetStream) {
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
// answers: the create comes back "no responders" at once. Open asks again
// rather than failing ([jsprovision.Place]), and that is the whole of what
// this case holds — the leaf is started with no member at all, Open is left
// asking, and only then does the member come up.
func TestOpeningOnALeafWaitsOutItsLink(t *testing.T) {
	t.Parallel()
	port := unusedPort(t)
	c := client(t, startLeaf(t, port))
	type result struct {
		b   *natsobj.Backend
		err error
	}
	opened := make(chan result, 1)
	go func() {
		b, err := natsobj.Open(t.Context(), c, natsobj.Config{Replicas: 1})
		opened <- result{b, err}
	}()
	select {
	case r := <-opened:
		t.Fatalf("the bucket opened (%v) with no member for the leaf to reach", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: port, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(member.Shutdown)
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

func client(t *testing.T, srv *js.Server) jetstream.JetStream {
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

func unusedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
