package jetstream

import (
	"context"
	"net"
	"testing"
)

// A PROBE THAT COULD NOT ANSWER IS NOT AN ANSWER, and telling the two apart is
// what keeps the caller's message honest.
//
// This collapsed every listen failure into "not free", so a cancelled start,
// an address this host does not have and a privileged port all reported
// themselves as a port somebody else was holding — three different remedies
// behind one sentence, and two of them send the reader hunting for a process
// that does not exist.
func TestAPortProbeSaysWhetherItCouldAnswerAtAll(t *testing.T) {
	t.Parallel()

	// A FREE PORT answers free, with no error.
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	//nolint:errcheck // Listen on a TCP address always yields *TCPAddr.
	port := l.Addr().(*net.TCPAddr).Port

	// HELD: in use, and that IS the answer this exists to give — free is
	// false and err is nil, because the probe ran and the port is taken.
	switch free, err := PortAvailable(t.Context(), "127.0.0.1", port); {
	case err != nil:
		t.Errorf("a held port reported a probe failure: %v", err)
	case free:
		t.Error("a held port reported itself free")
	}

	if err := l.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	switch free, err := PortAvailable(t.Context(), "127.0.0.1", port); {
	case err != nil:
		t.Errorf("a released port reported a probe failure: %v", err)
	case !free:
		t.Error("a released port reported itself held")
	}

	// AN ADDRESS THIS HOST DOES NOT HAVE is a configuration error of a
	// different kind, and it comes back AS one rather than as "in use".
	// 192.0.2.0/24 is TEST-NET-1 (RFC 5737) and is never a local address.
	free, err := PortAvailable(t.Context(), "192.0.2.1", port)
	if free {
		t.Error("an address this host does not have reported itself bindable")
	}
	if err == nil {
		t.Error("an unbindable address was reported as a port in use, which " +
			"sends the reader looking for a process that does not exist")
	}

	// A CANCELLED CALLER gets the cancellation back, not a verdict about
	// the port: nothing was learned about it.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if free, err := PortAvailable(ctx, "127.0.0.1", port); free || err == nil {
		t.Errorf("a cancelled probe answered (free=%v, err=%v) rather than "+
			"reporting that it never ran", free, err)
	}
}
