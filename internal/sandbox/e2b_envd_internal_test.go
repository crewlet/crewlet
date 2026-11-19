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

// A BOX'S FILE CUT OFF PART WAY IS THE PROVIDER'S FAILURE, on every open that
// streams it: the read fails with [WhyProviderUnavailable], as a whole read's
// does, and a refused open is classed by its status.
func TestAStreamCutOffPartWayIsTheProvidersFailure(t *testing.T) {
	t.Parallel()
	const path = "/home/user/.crewlet-check/output"
	for _, tc := range []struct {
		name   string
		answer func(*http.Request) *http.Response
		open   func(c *envdClient) (io.ReadCloser, error)
	}{
		{"a stream", answered(http.StatusOK, -1, ""), func(c *envdClient) (io.ReadCloser, error) {
			return c.openFile(t.Context(), path)
		}},
		{"a ranged end", answered(http.StatusPartialContent, 64, "bytes 36-99/100"),
			func(c *envdClient) (io.ReadCloser, error) {
				r, _, err := c.openTail(t.Context(), path, 64)
				return r, err
			}},
		{"an end sent whole with its length", answered(http.StatusOK, 100, ""),
			func(c *envdClient) (io.ReadCloser, error) {
				r, _, err := c.openTail(t.Context(), path, 64)
				return r, err
			}},
		{"an end sent whole without a length", answered(http.StatusOK, -1, ""),
			func(c *envdClient) (io.ReadCloser, error) {
				r, _, err := c.openTail(t.Context(), path, 64)
				return r, err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &envdClient{host: "http://box.example.com", fileIdle: time.Minute,
				http: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
					return tc.answer(r), nil
				})}}
			r, err := tc.open(c)
			if err == nil {
				_, err = io.ReadAll(r)
				_ = r.Close()
			}
			if !isClass(err, WhyProviderUnavailable) {
				t.Errorf("a body cut off after 10 bytes = %v; want it classed as the provider's failure", err)
			}
		})
	}
	refused := &envdClient{host: "http://box.example.com", fileIdle: time.Minute,
		http: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Request: r,
				Body: io.NopCloser(strings.NewReader("down"))}, nil
		})}}
	if _, _, err := refused.openTail(t.Context(), path, 64); !isClass(err, WhyProviderUnavailable) {
		t.Errorf("a 503 on the end's open = %v; want it classed as the provider's failure", err)
	}
}

// answered is a response of status whose body yields 10 bytes and then fails,
// with length as its Content-Length (-1 for none) and contentRange set when
// not empty.
func answered(status int, length int64, contentRange string) func(*http.Request) *http.Response {
	return func(r *http.Request) *http.Response {
		resp := &http.Response{StatusCode: status, ContentLength: length, Header: http.Header{},
			Body: io.NopCloser(io.MultiReader(strings.NewReader("0123456789"), failing{})), Request: r}
		if contentRange != "" {
			resp.Header.Set("Content-Range", contentRange)
		}
		return resp
	}
}

// failing is a body whose connection dropped.
type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func isClass(err error, want Why) bool {
	why, ok := WhyOf(err)
	return ok && why == want
}

// roundTrip is an http.RoundTripper made of a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
