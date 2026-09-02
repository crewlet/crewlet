package slack

import (
	"testing"

	"github.com/crewlet/crewlet/internal/httpx"
)

// TestTransportRidesTheSharedTransport guards the fallback branch production
// always takes: internal/engine builds the transport without a TransportOptions.HTTP,
// so every seat's Slack call goes through the client built here. It was built
// with a nil Transport — http.DefaultTransport, two idle connections per host
// across the whole process — while NewClient beside it already took the
// shared one, so the two halves of one vendor disagreed.
func TestTransportRidesTheSharedTransport(t *testing.T) {
	t.Parallel()
	tr, err := NewTransport(TransportOptions{
		Config: Config{Seats: []SeatConfig{{
			Handle: "ceo", Token: "xoxb-not-a-real-token", Channel: "general",
		}}},
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if tr.http.Transport != httpx.Transport() {
		t.Errorf("transport client = %T, want the one httpx shares", tr.http.Transport)
	}
	if tr.http.Timeout != ClientTimeout {
		t.Errorf("transport timeout = %v, want %v", tr.http.Timeout, ClientTimeout)
	}
}
