package natsobj_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
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
