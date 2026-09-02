package sandbox

import (
	"net/http"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// TestEnvdClientKeepsTheCallersTransport pins both halves of what
// newEnvdClient derives: the caller's transport survives, and its deadline
// does not. A client that inherited the control plane's timeout would kill
// every command that outran it, which is the whole reason envd gets its own
// client rather than the one it was handed.
func TestEnvdClientKeepsTheCallersTransport(t *testing.T) {
	t.Parallel()
	marker := http.RoundTripper(&http.Transport{})
	c := newEnvdClient("box.example.com", &http.Client{
		Transport: marker, Timeout: 30 * time.Second,
	})
	if c.http.Transport != marker {
		t.Errorf("transport = %T, want the caller's", c.http.Transport)
	}
	if c.http.Timeout != 0 {
		t.Errorf("timeout = %v, want none — the idle timeout is the bound", c.http.Timeout)
	}
}

// TestEnvdClientFallsBackToTheSharedTransport covers the branch no caller
// takes today. A nil Transport is http.DefaultTransport and its two idle
// connections per host, so the fallback has to name the shared one — see
// internal/httpx's package doc.
func TestEnvdClientFallsBackToTheSharedTransport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		from *http.Client
	}{
		{"no client at all", nil},
		{"a client with no transport of its own", &http.Client{Timeout: time.Second}},
	} {
		if got := newEnvdClient("box.example.com", tc.from).http.Transport; got != httpx.Transport() {
			t.Errorf("%s: transport = %T, want the one httpx shares", tc.name, got)
		}
	}
}
