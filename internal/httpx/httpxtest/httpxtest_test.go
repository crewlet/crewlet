package httpxtest

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// THE DOUBLE'S POOL IS THE SERVER'S, which is the whole reason [Rewrite]
// takes a server rather than a URL.
//
// The assertion is made the way the failure happens: something else in the
// binary calls CloseIdleConnections on the process-global transport — which
// is the last thing every httptest.Server.Close does — and the connection
// this double holds must survive it. Sitting on http.DefaultTransport the
// second call has to re-dial and the connection count reads 2; that
// difference is the bug this package exists for, so a revert to the shared
// pool fails HERE rather than as a rare flake in somebody else's package.
func TestAnotherClosersIdleSweepDoesNotReachThisPool(t *testing.T) {
	t.Parallel()
	var conns atomic.Int64
	// 204: the bodyless answer the read loop returns to the idle pool
	// BEFORE the caller has its response, and therefore the shape the lost
	// CI run failed on.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: Rewrite(t, srv)}
	call := func() {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete,
			"https://api.example.invalid/sandboxes/sbx1", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}

	call()
	// EXACTLY WHAT A NEIGHBOUR'S t.Cleanup DOES: httptest.Server.Close ends
	// by calling this on the process-global transport, for every server in
	// the binary rather than its own.
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	call()

	if got := conns.Load(); got != 1 {
		t.Fatalf("the second call opened a new connection (%d in total), so this "+
			"double is sitting on the pool every other test's cleanup closes", got)
	}
}

// THE CALLER'S REQUEST KEEPS THE ADDRESS IT BUILT, and only the copy that
// goes out is rewritten.
//
// A RoundTripper may not modify the request it is handed, and the address the
// SUBJECT derived is what several callers of this package assert on — a
// double that rewrote in place would overwrite the thing under test.
func TestTheCallersRequestKeepsTheAddressItBuilt(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "" {
			t.Error("the server was reached with no Host header")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	rw := Rewrite(t, srv)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"https://49983-sbx1-cl1.test.invalid/files", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res, err := (&http.Client{Transport: rw}).Do(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	_ = res.Body.Close()

	if req.URL.Host != "49983-sbx1-cl1.test.invalid" {
		t.Errorf("the caller's request was rewritten to %q", req.URL.Host)
	}
	if !rw.SawHost("https://49983-sbx1-cl1.test.invalid") {
		t.Errorf("hosts = %v, want the address the caller built", rw.Hosts())
	}
}

// AND A POOL WITH NO SERVER BEHIND IT IS STILL THIS TEST'S OWN.
//
// [Pool] is for the callers with nothing to borrow from — a test against a
// real listener the engine opened. The property that matters is the same one:
// a neighbour's sweep of the process-global pool must not reach it.
func TestAPoolWithNoServerIsStillPrivate(t *testing.T) {
	t.Parallel()
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	client := Pool(t)
	get := func() {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, srv.URL, nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}

	get()
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	get()

	if got := conns.Load(); got != 1 {
		t.Fatalf("the second call re-dialled (%d connections), so Pool handed back "+
			"the shared transport rather than one of its own", got)
	}
}
