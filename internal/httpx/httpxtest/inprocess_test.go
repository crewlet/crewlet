package httpxtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// get asks client for url on ctx, failing the test on a request it cannot
// build.
func get(ctx context.Context, t *testing.T, client *http.Client, method, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return client.Do(req)
}

// AN ANSWER ARRIVES AS IT IS WRITTEN, on the clock it runs on. The response
// is handed back at the handler's first write — not at its WriteHeader, which
// a server sends nothing for — and each later write reaches the reader at the
// instant it is made, so a test of a stream's pacing reads exactly the gaps
// its handler slept.
func TestAnInProcessAnswerArrivesAsItIsWritten(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		client := InProcess(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusCreated)
			time.Sleep(time.Second)
			_, _ = io.WriteString(w, "a")
			time.Sleep(time.Minute)
			_, _ = io.WriteString(w, "b")
		}))
		start := time.Now()
		res, err := get(t.Context(), t, client, http.MethodGet, "http://api.example.invalid/x")
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		defer func() { _ = res.Body.Close() }()
		if at := time.Since(start); at != time.Second {
			t.Fatalf("the response arrived after %v, want at the first write (1s)", at)
		}
		if res.StatusCode != http.StatusCreated || res.Status != "201 Created" {
			t.Fatalf("status %q, want the one the handler wrote", res.Status)
		}
		buf := make([]byte, 8)
		if n, err := res.Body.Read(buf); err != nil || string(buf[:n]) != "a" {
			t.Fatalf("first read %q, %v; want the first write", buf[:n], err)
		}
		if n, err := res.Body.Read(buf); err != nil || string(buf[:n]) != "b" {
			t.Fatalf("second read %q, %v; want the second write", buf[:n], err)
		}
		if at := time.Since(start); at != time.Second+time.Minute {
			t.Fatalf("the second write arrived after %v, want at 1m1s", at)
		}
		if n, err := res.Body.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("read past the end: %d, %v; want EOF once the handler returned", n, err)
		}
	})
}

// THE CALLER'S CONTEXT ENDS THE CALL WHEREVER IT IS, with its cause, as the
// transport reports one: before the answer the round trip returns it, part
// way through the body's next read does, and either way the handler's next
// write fails as it would on a dropped connection, so the handler returns and
// nothing is left running.
func TestAnInProcessCallEndsWithItsCallersContext(t *testing.T) {
	t.Parallel()
	cause := errors.New("the caller gave up")
	for _, tc := range []struct {
		name   string
		before bool // the context ends before anything is written
	}{
		{"before the answer", true},
		{"part-way through", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				wrote := make(chan error, 1)
				client := InProcess(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if !tc.before {
						_, _ = io.WriteString(w, "a")
					}
					time.Sleep(time.Hour)
					_, err := io.WriteString(w, "late")
					wrote <- err
				}))
				ctx, cancel := context.WithCancelCause(t.Context())
				time.AfterFunc(time.Second, func() { cancel(cause) })

				res, err := get(ctx, t, client, http.MethodGet, "http://api.example.invalid/x")
				if tc.before {
					if !errors.Is(err, cause) {
						t.Fatalf("round trip: %v, want the context's cause", err)
					}
				} else {
					if err != nil {
						t.Fatalf("round trip: %v", err)
					}
					got, err := io.ReadAll(res.Body)
					if string(got) != "a" || !errors.Is(err, cause) {
						t.Fatalf("read %q, %v; want what was written, then the context's cause", got, err)
					}
					_ = res.Body.Close()
				}
				if err := <-wrote; err == nil {
					t.Fatal("the handler's write after its caller left succeeded")
				}
			})
		})
	}
}

// THE CASE DOES NOT END WHILE A HANDLER IT STARTED IS RUNNING. A caller that
// gave up leaves its handler asleep behind it; the client's cleanup runs that
// handler to its end on the bubble's clock, where a bubble whose case had
// returned would call it deadlocked and fail the case instead.
func TestAnInProcessHandlerIsRunToItsEnd(t *testing.T) {
	t.Parallel()
	var ended atomic.Bool
	synctest.Test(t, func(t *testing.T) {
		client := InProcess(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			time.Sleep(time.Hour)
			ended.Store(true)
		}))
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err := get(ctx, t, client, http.MethodGet, "http://api.example.invalid/x")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("round trip: %v, want the caller's deadline", err)
		}
	})
	if !ended.Load() {
		t.Fatal("the case ended with the handler it started still running")
	}
}

// AN ANSWER IS DATED AND TYPED AS A SERVER'S IS: the Date is the clock the
// handler runs on — a bubble's, inside one — and a Content-Type nobody set is
// sniffed from the first write. A header the handler did set is kept.
func TestAnInProcessAnswerIsDatedAndTyped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		client := InProcess(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/typed" {
				w.Header().Set("Content-Type", "application/json")
			}
			_, _ = io.WriteString(w, "<html><body>hi</body></html>")
		}))
		want := time.Now().UTC().Format(http.TimeFormat)
		for path, typ := range map[string]string{
			"/sniffed": "text/html; charset=utf-8",
			"/typed":   "application/json",
		} {
			res, err := get(t.Context(), t, client, http.MethodGet, "http://api.example.invalid"+path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if got := res.Header.Get("Date"); got != want {
				t.Errorf("%s: Date %q, want the bubble's clock %q", path, got, want)
			}
			if got := res.Header.Get("Content-Type"); got != typ {
				t.Errorf("%s: Content-Type %q, want %q", path, got, typ)
			}
		}
	})
}

// A RESPONSE THAT CARRIES NO BODY HAS NONE: a HEAD is answered with its
// headers and an empty body whatever the handler wrote, and a write to a 204
// is refused as the server refuses it. A handler that writes nothing at all
// answers 200.
func TestAnInProcessBodylessAnswerHasNoBody(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		refused := make(chan error, 1)
		client := InProcess(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/head":
				w.Header().Set("Content-Length", "5")
				_, _ = io.WriteString(w, "hello")
			case "/none":
				w.WriteHeader(http.StatusNoContent)
				_, err := io.WriteString(w, "x")
				refused <- err
			case "/silent":
			}
		}))
		for _, c := range []struct {
			method, path string
			status       int
			length       int64
		}{
			{http.MethodHead, "/head", http.StatusOK, 5},
			{http.MethodGet, "/none", http.StatusNoContent, 0},
			{http.MethodGet, "/silent", http.StatusOK, -1},
		} {
			res, err := get(t.Context(), t, client, c.method, "http://api.example.invalid"+c.path)
			if err != nil {
				t.Fatalf("%s %s: %v", c.method, c.path, err)
			}
			got, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if res.StatusCode != c.status || res.ContentLength != c.length || len(got) != 0 {
				t.Errorf("%s %s: %d, length %d, body %q; want %d, length %d, no body",
					c.method, c.path, res.StatusCode, res.ContentLength, got, c.status, c.length)
			}
		}
		if err := <-refused; !errors.Is(err, http.ErrBodyNotAllowed) {
			t.Errorf("a write to a 204: %v, want http.ErrBodyNotAllowed", err)
		}
	})
}

// THE HANDLER IS HANDED WHAT A SERVER HANDS IT, and the caller keeps its own
// request: a body however the caller built it, the host, the request URI and
// a remote address — on a copy, since a RoundTripper may not edit the request
// it is handed.
func TestAnInProcessHandlerIsHandedAServersRequest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		client := InProcess(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil || r.Host != "api.example.invalid" || r.RequestURI != "/x?y=1" ||
				r.RemoteAddr == "" {
				t.Errorf("handler saw body %q (%v), host %q, URI %q, remote %q",
					body, err, r.Host, r.RequestURI, r.RemoteAddr)
			}
			_, _ = w.Write(body)
		}))
		for _, payload := range []string{"", "ping"} {
			var reader io.Reader
			if payload != "" {
				reader = strings.NewReader(payload)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				"http://api.example.invalid/x?y=1", reader)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			res, err := client.Do(req)
			if err != nil {
				t.Fatalf("round trip: %v", err)
			}
			got, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if string(got) != payload {
				t.Errorf("echoed %q, want %q", got, payload)
			}
			if req.RequestURI != "" || req.RemoteAddr != "" {
				t.Errorf("the caller's own request was edited: URI %q, remote %q",
					req.RequestURI, req.RemoteAddr)
			}
		}
	})
}
