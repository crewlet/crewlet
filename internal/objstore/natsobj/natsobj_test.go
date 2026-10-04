package natsobj_test

import (
	"fmt"
	"net"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// THE BROKER'S OWN OBJECT STORE PASSES THE SUITE, on a member's own client.
func TestContract(t *testing.T) {
	t.Parallel()
	objstoretest.Run(t, func(t *testing.T) objstore.Backend {
		return open(t, memberClient(t))
	}, objstoretest.Options{})
}

// AND THROUGH A LEAF: a node without `data` runs no JetStream and reaches the
// members' across its leaf link, and its uploads and downloads go through
// exactly that.
func TestContractThroughALeaf(t *testing.T) {
	t.Parallel()
	objstoretest.Run(t, func(t *testing.T) objstore.Backend {
		return open(t, leafClient(t))
	}, objstoretest.Options{})
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
