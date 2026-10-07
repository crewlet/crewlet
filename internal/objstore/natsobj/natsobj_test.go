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

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// suite is what the broker's object store promises the suite: messages of
// [natsobj.MessageBytes], a digest it keeps, and a listing read slowly for
// longer than the library's ordered consumer takes to call itself inactive —
// two of its five-second heartbeats — so a listing that restarted under a
// slow reader would show it.
var suite = objstoretest.Options{
	Piece:       natsobj.MessageBytes,
	Digest:      true,
	SlowListing: 12 * time.Second,
	Unfinished:  unfinished,
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

// A DOWNLOAD READ SLOWLY OUTLASTS THE LIBRARY'S OWN TIMEOUTS: its five-second
// default, which a get under a deadline does not take, and the two heartbeats
// after which its ordered consumer calls itself inactive and starts again.
func TestASlowReadOutlastsTheLibrarysTimeouts(t *testing.T) {
	t.Parallel()
	b := open(t, memberClient(t))
	data := bytes.Repeat([]byte("read slowly "), 4<<20/12)
	if err := b.Put(t.Context(), "slow", bytes.NewReader(data), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	r, err := b.Get(ctx, "slow", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	const reads = 64
	start := time.Now()
	var got bytes.Buffer
	buf := make([]byte, len(data)/reads+1)
	for {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("a slow read failed %v in, at %d bytes: %v", time.Since(start), got.Len(), err)
		}
		time.Sleep(12 * time.Second / reads)
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("a slow read answered %d bytes, want %d", got.Len(), len(data))
	}
	if took := time.Since(start); took < 11*time.Second {
		t.Fatalf("the read took %v; it was meant to outlast two heartbeats", took)
	}
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
	marker := string(objstore.HashOf([]byte("a chunk")))
	if err := b.Put(t.Context(), marker, bytes.NewReader([]byte("chunk")), objstore.PutMeta{}); err != nil {
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

// A DELETE MARKER AN EARLIER BUILD LEFT IS LISTED, AND CLEARED BY AN ORDINARY
// DELETE. The library's own delete — what every earlier build made — keeps
// the marker for ever; the collector can only remove what a listing shows
// it, so the listing shows the name, as an object of no bytes, and the delete
// it makes purges the marker like any other metadata.
func TestAMarkerAnEarlierBuildLeftIsListedAndCleared(t *testing.T) {
	t.Parallel()
	client := memberClient(t)
	b := open(t, client)
	name := string(objstore.HashOf([]byte("a chunk an earlier build stored")))
	if err := b.Put(t.Context(), name, bytes.NewReader([]byte("chunk")), objstore.PutMeta{}); err != nil {
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

func leafClient(t *testing.T) jetstream.JetStream {
	t.Helper()
	port := unusedPort(t)
	member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: port, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(member.Shutdown)
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf",
		LeafURLs: []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", port)}})
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	return client(t, leaf)
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
