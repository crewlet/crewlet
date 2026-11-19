package sandbox

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	fromByte5 := func(c *envdClient) (io.ReadCloser, error) {
		r, _, err := c.openRange(t.Context(), path, 5, 64)
		return r, err
	}
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
		{"a range", answered(http.StatusPartialContent, 64, "bytes 5-68/100"), fromByte5},
		{"a range sent whole with its length", answered(http.StatusOK, 100, ""), fromByte5},
		{"a range sent whole without a length", answered(http.StatusOK, -1, ""), fromByte5},
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

// A RANGE OF A BOX'S FILE IS ASKED FOR AS ONE, and every way envd can answer it
// is read as the file's bytes from the offset and its whole size: a 206 for
// what was asked, a 416 for an offset at or past the end, a 404 for no file,
// and a 200 from a server that ignored the range, read through in bounded
// memory. An answer that names no whole size, or a range that starts anywhere
// but the offset, is an error rather than bytes from the wrong place, and one
// that runs past the range is cut at n.
// Mutation: send a suffix range, and the 206 rows go red.
func TestARangeOfABoxsFileIsAskedForAndReadAsOne(t *testing.T) {
	t.Parallel()
	const path = "/home/user/.crewlet/events.jsonl"
	content := make([]byte, 1000)
	for i := range content {
		content[i] = byte(i % 251)
	}
	served := func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(content))
	}
	whole := func(length bool) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			if length {
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			}
			_, _ = w.Write(content)
		}
	}
	// overlong is a 206 that runs on past the range asked for, to the end.
	overlong := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 100-999/1000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[100:])
	}
	status := func(code int, contentRange string) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			if contentRange != "" {
				w.Header().Set("Content-Range", contentRange)
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte("envd says no"))
		}
	}
	for _, tc := range []struct {
		name      string
		serve     func(http.ResponseWriter, *http.Request)
		off, n    int64
		asked     string
		want      []byte
		size      int64
		refused   string
		class     Why
		classless bool
	}{
		{name: "a range from the middle", serve: served, off: 100, n: 50, asked: "bytes=100-149",
			want: content[100:150], size: 1000},
		{name: "a range past the end", serve: served, off: 900, n: 500, asked: "bytes=900-1399",
			want: content[900:], size: 1000},
		{name: "a range to the end of any file", serve: served, off: 100, n: math.MaxInt64,
			asked: "bytes=100-", want: content[100:], size: 1000},
		{name: "a range at the size", serve: served, off: 1000, n: 50, asked: "bytes=1000-1049", size: 1000},
		{name: "a range past a file that shrank", serve: served, off: 4000, n: 50, asked: "bytes=4000-4049",
			size: 1000},
		{name: "a 206 past the range asked for", serve: overlong, off: 100, n: 50, asked: "bytes=100-149",
			want: content[100:150], size: 1000},
		{name: "a 206 that names no whole size", serve: status(http.StatusPartialContent, "bytes 100-149/*"),
			off: 100, n: 50, asked: "bytes=100-149", refused: "names no whole size"},
		{name: "a 206 from another offset", serve: status(http.StatusPartialContent, "bytes 0-49/1000"),
			off: 100, n: 50, asked: "bytes=100-149", refused: "starts elsewhere"},
		{name: "a 416 that names no size", serve: status(http.StatusRequestedRangeNotSatisfiable, ""),
			off: 100, n: 50, asked: "bytes=100-149", refused: "names no whole size"},
		{name: "a missing file", serve: status(http.StatusNotFound, ""), off: 100, n: 50,
			asked: "bytes=100-149"},
		{name: "a refusal", serve: status(http.StatusBadRequest, ""), off: 100, n: 50, asked: "bytes=100-149",
			refused: "envd says no", classless: true},
		{name: "an envd that failed", serve: status(http.StatusServiceUnavailable, ""), off: 100, n: 50,
			asked: "bytes=100-149", refused: "envd says no", class: WhyProviderUnavailable},
		{name: "a whole answer with a length", serve: whole(true), off: 100, n: 50, asked: "bytes=100-149",
			want: content[100:150], size: 1000},
		{name: "a whole answer with a length, past its end", serve: whole(true), off: 4000, n: 50,
			asked: "bytes=4000-4049", size: 1000},
		{name: "a whole answer without a length", serve: whole(false), off: 100, n: 50, asked: "bytes=100-149",
			want: content[100:150], size: 1000},
		{name: "a whole answer without a length, past its end", serve: whole(false), off: 4000, n: 50,
			asked: "bytes=4000-4049", size: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var asked []string
			c := &envdClient{host: "http://box.example.com", fileIdle: time.Minute,
				http: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
					asked = append(asked, r.Header.Get("Range"))
					rec := httptest.NewRecorder()
					tc.serve(rec, r)
					return rec.Result(), nil
				})}}
			r, size, err := c.openRange(t.Context(), path, tc.off, tc.n)
			var got []byte
			if err == nil {
				got, err = io.ReadAll(r)
				_ = r.Close()
			}
			if len(asked) != 1 || asked[0] != tc.asked {
				t.Errorf("the download asked for %q; want one range, %q", asked, tc.asked)
			}
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("openRange = %d bytes of %d, %v; want an error saying %q", len(got), size, err,
						tc.refused)
				}
				if why, ok := WhyOf(err); tc.class != "" && why != tc.class || tc.classless && ok {
					t.Errorf("the error is classed %q (%v); want %q", why, ok, tc.class)
				}
				return
			}
			if err != nil || !bytes.Equal(got, tc.want) || size != tc.size {
				t.Errorf("openRange = %d bytes of %d, %v; want %d bytes of %d", len(got), size, err, len(tc.want),
					tc.size)
			}
		})
	}
}

// A WHOLE ANSWER WITHOUT A LENGTH KEEPS AT MOST [MaxFileBytes], the bound on
// anything held whole, and still names the file's whole size: the caller reads
// on from where the bytes ended, and memory never holds the file.
func TestARangeSentWholeWithoutALengthKeepsAtMostTheCap(t *testing.T) {
	t.Parallel()
	const extra = 4096
	c := &envdClient{host: "http://box.example.com", fileIdle: time.Minute,
		http: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			body := io.LimitReader(byteRun(7), MaxFileBytes+extra)
			return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: http.Header{},
				Body: io.NopCloser(body), Request: r}, nil
		})}}
	r, size, err := c.openRange(t.Context(), "/home/user/.crewlet/events.jsonl", 10, math.MaxInt64)
	if err != nil {
		t.Fatalf("openRange: %v", err)
	}
	n, err := io.Copy(io.Discard, r)
	_ = r.Close()
	if err != nil || n != MaxFileBytes || size != MaxFileBytes+extra {
		t.Errorf("openRange streamed %d bytes of %d, %v; want the cap's %d of %d", n, size, err, MaxFileBytes,
			MaxFileBytes+extra)
	}
}

// byteRun is an endless stream of one byte.
type byteRun byte

func (b byteRun) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
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
