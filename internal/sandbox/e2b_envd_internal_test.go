package sandbox

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	c := newEnvdClient("box.example.com", "", &http.Client{
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
		if got := newEnvdClient("box.example.com", "", tc.from).http.Transport; got != httpx.Transport() {
			t.Errorf("%s: transport = %T, want the one httpx shares", tc.name, got)
		}
	}
}

// A FILE TRANSFER IS BOUNDED BY SILENCE. An envd that takes the connection and
// never answers held a /files read for as long as the caller's context lived,
// and the completion poll's context lives as long as the process — so one
// wedged box stalled the poll of every other box behind it. The transfer is
// abandoned once no byte has moved for the bound, and says that is why, rather
// than reporting a "context canceled" the caller did not cause.
func TestASilentFileTransferIsAbandonedAndSaysWhy(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	c := newEnvdClient(server.URL, "", server.Client())
	c.fileIdle = 50 * time.Millisecond

	for name, transfer := range map[string]func() error{
		"whole": func() error { _, err := c.readFile(t.Context(), "/home/user/.crewlet/done"); return err },
		"tail": func() error {
			_, err := c.readTail(t.Context(), "/home/user/.crewlet/result.json", 64)
			return err
		},
		"open": func() error { _, err := c.openFile(t.Context(), "/home/user/.crewlet/result.json"); return err },
		"write": func() error {
			return c.writeFile(t.Context(), "/home/user/.crewlet/brief.md", []byte("x"))
		},
	} {
		done := make(chan error, 1)
		go func() { done <- transfer() }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "moved no byte") {
				t.Errorf("%s: a silent envd answered %v; want the transfer abandoned for its silence", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: a silent envd held the transfer past ten seconds against a 50ms bound", name)
		}
	}
}

// A FILE WRITE CAN BE SENT TWICE, and says how long it is. The idle bound's
// progress wrapper hides the buffer a request would otherwise take both from:
// without the length the body goes out chunked, and without GetBody the
// transport cannot retry a write on a fresh connection when the server closed
// the first before reading it (an HTTP/2 GOAWAY) — which the plain buffer the
// write used to send could.
func TestAFileWriteCarriesItsLengthAndCanBeSentAgain(t *testing.T) {
	t.Parallel()
	var sent, again []byte
	var length int64
	c := &envdClient{host: "http://box.example.com", fileIdle: time.Minute,
		http: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			length = r.ContentLength
			sent, _ = io.ReadAll(r.Body)
			if r.GetBody == nil {
				return nil, errors.New("the request cannot be sent again")
			}
			body, err := r.GetBody()
			if err != nil {
				return nil, err
			}
			again, _ = io.ReadAll(body)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")),
				Request: r}, nil
		})}}
	if err := c.writeFile(t.Context(), "/home/user/.crewlet/brief.md", []byte("fix the flake")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if length != int64(len(sent)) || length == 0 {
		t.Errorf("Content-Length %d for a %d-byte body; want the body's own length", length, len(sent))
	}
	if !bytes.Equal(sent, again) || !bytes.Contains(sent, []byte("fix the flake")) {
		t.Errorf("the body sent again differs from the one sent: %q vs %q", again, sent)
	}
}

// roundTrip is an http.RoundTripper made of a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
