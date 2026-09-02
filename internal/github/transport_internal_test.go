package github

import (
	"testing"

	"github.com/crewlet/crewlet/internal/httpx"
)

// TestClientRidesTheSharedTransport is the guard for the fallback branch that
// production always takes: no caller sets ClientOptions.HTTP, so the client
// every seat talks to GitHub through is the one built here. It was built with
// a nil Transport, which is http.DefaultTransport and its two idle
// connections per host, process-wide — the exact condition internal/httpx
// exists to remove, silently unfixed because a nil Transport reads as a
// default rather than as an omission.
func TestClientRidesTheSharedTransport(t *testing.T) {
	t.Parallel()
	c, err := NewClient(ClientOptions{Token: "ghp-not-a-real-token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.http.Transport != httpx.Transport() {
		t.Errorf("client transport = %T, want the one httpx shares", c.http.Transport)
	}
	if c.http.Timeout != ClientTimeout {
		t.Errorf("client timeout = %v, want %v", c.http.Timeout, ClientTimeout)
	}
}
