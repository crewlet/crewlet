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

// TestEnvdClientKeepsTheCallersTransport pins what newEnvdClient derives: the
// caller's transport survives and its deadline does not, since the control
// plane's timeout would kill every command that outran it.
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

// TestEnvdClientFallsBackToTheSharedTransport holds the fallback to httpx's
// shared transport: a nil Transport is http.DefaultTransport, with two idle
// connections per host (internal/httpx's package doc).
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

// A FILE TRANSFER IS BOUNDED BY SILENCE: the completion poll's context lives
// as long as the process, so an envd that takes the connection and never
// answers would stall the poll of every other box. The transfer is abandoned
// once no byte has moved for the bound, and says so rather than reporting a
// "context canceled" the caller did not cause.
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
		case <-time.After(10 * time.Second): //fixedwait:liveness
			t.Fatalf("%s: a silent envd held the transfer past ten seconds against a 50ms bound", name)
		}
	}
}

// A FILE WRITE CAN BE SENT TWICE, and states its length, in each of its
// requests (the upload and the move). The idle bound's progress wrapper hides
// the buffer the transport would take both from: without the length a body
// goes out chunked, and without GetBody the transport cannot retry on a fresh
// connection when the server closed the first unread (an HTTP/2 GOAWAY).
func TestAFileWriteCarriesItsLengthAndCanBeSentAgain(t *testing.T) {
	t.Parallel()
	type request struct {
		path        string
		sent, again []byte
		length      int64
	}
	var requests []request
	c := &envdClient{host: "http://box.example.com", fileIdle: time.Minute,
		http: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			got := request{path: r.URL.Path, length: r.ContentLength}
			got.sent, _ = io.ReadAll(r.Body)
			if r.GetBody == nil {
				return nil, errors.New("the request cannot be sent again")
			}
			body, err := r.GetBody()
			if err != nil {
				return nil, err
			}
			got.again, _ = io.ReadAll(body)
			requests = append(requests, got)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")),
				Request: r}, nil
		})}}
	if err := c.writeFile(t.Context(), "/home/user/.crewlet/brief.md", []byte("fix the flake")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	var paths []string
	for _, r := range requests {
		paths = append(paths, r.path)
	}
	if len(requests) != 2 || paths[0] != "/files" || paths[1] != "/filesystem.Filesystem/Move" {
		t.Fatalf("the write made the requests %v; want the upload, then the move", paths)
	}
	for _, r := range requests {
		if r.length != int64(len(r.sent)) || r.length == 0 {
			t.Errorf("%s: Content-Length %d for a %d-byte body; want the body's own length", r.path, r.length,
				len(r.sent))
		}
		if !bytes.Equal(r.sent, r.again) {
			t.Errorf("%s: the body sent again differs from the one sent: %q vs %q", r.path, r.again, r.sent)
		}
	}
	if !bytes.Contains(requests[0].sent, []byte("fix the flake")) {
		t.Errorf("the upload carried %q, not the content", requests[0].sent)
	}
}

// roundTrip is an http.RoundTripper made of a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
