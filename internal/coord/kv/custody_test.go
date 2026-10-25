package kv

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
)

// A KEEPER RECORD NOTHING CAN READ IS UNDECIDABLE — NEVER A KEEPER, AND NEVER
// THE STORE UNAVAILABLE.
//
// The contract suite cannot plant one, for the reason it cannot plant a
// corrupt secret: every claim names its node or is refused, so only a value
// that reached the bucket some other way is one — written by hand or by a
// process that is not this engine, by a later build that reshaped the record,
// or corrupted. Read as the store unavailable, the answer stopped the keeper's
// pass at that batch on every pass and every boot, leaving every batch written
// after it unsettled for the month the record lasts, since a pass lists the
// oldest first. So each shape a record can fail to name a keeper in is
// planted here, straight into the bucket, and must answer the one definite
// error that lets the pass go on to the next batch — headers included, which
// the leader read decodes before the value and the create reads back to tell
// a marker from a value.
//
// UNDECIDABLE MEANS THE KEEPER cannot be read, and nothing less: a record
// whose `node` reads and whose diagnostic instant does not — retyped by a
// later build, written another way by hand — names its keeper, and is answered
// with it. And the error names the bucket and the key the record is under,
// since removing it is the one remedy short of the month and the error is
// what the caller logs.
//
// Mutation: answer a value that does not decode with a plain error in
// ClaimCustody, and the garbled shapes fail; drop its errUndecodableHeaders
// arms, and the raw message answers as the store unavailable; decode the
// whole custodyRecord again, and the records with an unreadable instant are
// answered undecidable; drop the bucket from the error, and every undecidable
// shape fails.
func TestAKeeperRecordNothingCanReadIsUndecidable(t *testing.T) {
	t.Parallel()
	ns, nc := listeningServer(t)
	store := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	ctx := t.Context()

	undecidable := func(batch string) {
		t.Helper()
		keeper, err := store.ClaimCustody(ctx, batch, "data-a")
		switch {
		case keeper != "":
			t.Errorf("%s: answered the keeper %q from a record that names nobody "+
				"readable — one copy would be kept or deleted on a guess", batch, keeper)
		case !errors.Is(err, coord.ErrCustodyUndecidable):
			t.Errorf("%s: answered %v, want coord.ErrCustodyUndecidable", batch, err)
		case errors.Is(err, coord.ErrUnavailable):
			t.Errorf("%s: answered %v, which is also unavailable — a keeper waits "+
				"on that, and stops every batch behind this one", batch, err)
		case !strings.Contains(err.Error(), "key "+encodeKey(batch)+" in bucket "+store.custody.Bucket()):
			t.Errorf("%s: answered %v, which does not say where the record is — "+
				"bucket %s, key %s — so nobody reading the log can remove it",
				batch, err, store.custody.Bucket(), encodeKey(batch))
		}
	}

	for _, c := range []struct{ batch, value string }{
		{"garbled", `{not json`},
		{"empty", ``},
		{"no-node", `{"node":""}`},
		{"another-shape", `{"keeper":"data-z"}`},
	} {
		if _, err := store.custody.Put(ctx, encodeKey(c.batch), []byte(c.value)); err != nil {
			t.Fatalf("plant %s: %v", c.batch, err)
		}
		undecidable(c.batch)
	}

	// Headers nothing can decode: a message the client library cannot
	// write, published raw onto the key's subject.
	publishRaw(t, ns, bucketSubjects(store.custody)+encodeKey("raw-headers"),
		"NATS/1.0\r\nthis line names no header\r\n\r\n", `{"node":"data-z"}`)
	undecidable("raw-headers")

	// A record whose keeper reads is answered with it, whatever else it
	// carries: only what decides is decoded.
	for _, c := range []struct{ batch, value string }{
		{"instant-retyped", `{"node":"data-z","at":1700000000}`},
		{"instant-reformatted", `{"node":"data-z","at":"not-a-time"}`},
		{"field-added", `{"node":"data-z","at":"2026-10-21T00:00:00Z","by":"a later build"}`},
	} {
		if _, err := store.custody.Put(ctx, encodeKey(c.batch), []byte(c.value)); err != nil {
			t.Fatalf("plant %s: %v", c.batch, err)
		}
		if keeper, err := store.ClaimCustody(ctx, c.batch, "data-a"); err != nil || keeper != "data-z" {
			t.Errorf("%s: a record naming data-z = (%q, %v), want data-z — its keeper "+
				"is readable, and only it decides", c.batch, keeper, err)
		}
	}

	// The bucket itself answers: a batch nobody planted is claimed and told
	// to the next claimant, so what failed above was each record.
	for _, node := range []string{"data-a", "data-b"} {
		if keeper, err := store.ClaimCustody(ctx, "healthy", node); err != nil || keeper != "data-a" {
			t.Fatalf("claim of a healthy batch by %s = (%q, %v), want data-a", node, keeper, err)
		}
	}

	// And the remedy the contract names: the record removed, a claim
	// creates it afresh.
	if err := store.custody.Delete(ctx, encodeKey("garbled")); err != nil {
		t.Fatalf("remove the garbled record: %v", err)
	}
	if keeper, err := store.ClaimCustody(ctx, "garbled", "data-b"); err != nil || keeper != "data-b" {
		t.Errorf("a claim after the record was removed = (%q, %v), want data-b", keeper, err)
	}
}

// listeningServer is [embeddedServer] reachable over loopback TCP as well,
// for a case that has to write the wire protocol itself.
func listeningServer(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName: "coordkv-raw-test",
		JetStream:  true,
		Host:       "127.0.0.1",
		Port:       -1,
		StoreDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("configure the server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatal("the server did not become ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return ns, nc
}

// publishRaw publishes one message with exactly these header bytes, which the
// client library refuses to encode, and waits for the stream to store it.
func publishRaw(t *testing.T, ns *server.Server, subject, header, payload string) {
	t.Helper()
	conn, err := net.Dial("tcp", ns.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	read := bufio.NewReader(conn)
	if info, err := read.ReadString('\n'); err != nil || !strings.HasPrefix(info, "INFO ") {
		t.Fatalf("the server's greeting = (%q, %v)", info, err)
	}
	const inbox = "_INBOX.raw"
	fmt.Fprintf(conn, "CONNECT {\"verbose\":false,\"pedantic\":false,\"headers\":true,\"protocol\":1}\r\n")
	fmt.Fprintf(conn, "SUB %s 1\r\n", inbox)
	fmt.Fprintf(conn, "HPUB %s %s %d %d\r\n%s%s\r\n",
		subject, inbox, len(header), len(header)+len(payload), header, payload)
	// The stream's acknowledgement is what says the message is stored.
	for {
		line, err := read.ReadString('\n')
		if err != nil {
			t.Fatalf("awaiting the stream's acknowledgement: %v", err)
		}
		switch {
		case strings.HasPrefix(line, "PING"):
			fmt.Fprintf(conn, "PONG\r\n")
		case strings.HasPrefix(line, "-ERR"):
			t.Fatalf("the server refused the raw publish: %s", line)
		case strings.HasPrefix(line, "MSG "):
			ack, err := read.ReadString('\n')
			if err != nil {
				t.Fatalf("read the acknowledgement: %v", err)
			}
			if strings.Contains(ack, `"error"`) || !strings.Contains(ack, `"seq"`) {
				t.Fatalf("the stream did not store the raw message: %s", ack)
			}
			return
		}
	}
}
