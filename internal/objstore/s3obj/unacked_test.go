//go:build linux || darwin

package s3obj_test

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore/s3obj"
)

// THE SOCKET'S COUNT IS READ ON THIS PLATFORM — one of the two the engine
// builds for — because a write's watch that could not read it would end every
// write at the floor: four mebibytes written to a peer that reads nothing sit
// in the socket unacknowledged, and once the peer has read them all the count
// falls.
func TestTheSocketCountIsReadOnThisPlatform(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	sender, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	peer, ok := <-accepted
	if !ok {
		t.Fatal("the peer was never accepted")
	}
	defer peer.Close()

	// AS MUCH AS THE SOCKETS TAKE, the peer reading nothing: the write ends
	// at its deadline with the rest unsent.
	if err := sender.SetWriteDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	sent, _ := sender.Write(make([]byte, 4<<20))
	held, known := s3obj.Unacked(sender)
	if !known {
		t.Fatal("the socket's unacknowledged bytes cannot be read on this platform")
	}
	if held <= 0 {
		t.Fatalf("%d bytes sent to a peer reading nothing leave %d unacknowledged; want some", sent, held)
	}
	if _, err := io.ReadFull(peer, make([]byte, sent)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		now, known := s3obj.Unacked(sender)
		if known && now < held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the peer read all %d bytes and %d are still unacknowledged, as before (%d)", sent, now, held)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
