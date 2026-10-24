package mattermost

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// THE ENGINE'S SOCKET IS ADMITTED WHATEVER ADDRESS IT DIALS AND WHATEVER
// SITE URL PEOPLE BROWSE. The server here applies Mattermost's own origin
// rule ([AdmitsOrigin]) and is reached at 127.0.0.1 while its Site URL names
// a public host — the split every Kubernetes deployment has, where the
// engine dials an in-cluster Service. The engine used to send the Origin of
// the address it dialled, and that rule refused every seat's upgrade with a
// 403 while its REST calls worked.
//
// Mutation: put `"Origin": {BrowserOrigin(c.URL())}` back into
// [socketHeader] and every case is refused.
func TestTheEngineSocketPassesTheOriginCheckOnAnyRoute(t *testing.T) {
	for _, tc := range []struct {
		name, site, allowCorsFrom string
	}{
		{"a public Site URL", "https://chat.example.com", ""},
		{"no Site URL", "", ""},
		{"an AllowCorsFrom naming only the public origin", "https://chat.example.com", "https://chat.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				origins []string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				origins = append(origins, r.Header.Values("Origin")...)
				mu.Unlock()
				if !AdmitsOrigin(r.Header.Get("Origin"), r.Host, tc.site, tc.allowCorsFrom) {
					http.Error(w, "websocket: request origin not allowed by Upgrader.CheckOrigin",
						http.StatusForbidden)
					return
				}
				// The rule above is the server's; the library's own
				// same-origin check would be a second, different one.
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
				if err != nil {
					return
				}
				conn.Close(websocket.StatusNormalClosure, "")
			}))
			t.Cleanup(srv.Close)

			client, err := NewClient(ClientOptions{URL: srv.URL, Token: "tok-swe"})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			sock, err := dialWebsocket(t.Context(), Seat{Handle: "swe"}, client)
			if err != nil {
				t.Fatalf("the engine's socket to %s was refused under Site URL %q: %v",
					srv.URL, tc.site, err)
			}
			sock.Close()
			mu.Lock()
			defer mu.Unlock()
			if len(origins) != 0 {
				t.Errorf("the engine sent Origin %q: an Origin puts its socket under "+
					"the browser rule, which no address but the Site URL passes", origins)
			}
		})
	}
}

// THE DOCTOR'S SEAT PROBE IS THE ENGINE'S HANDSHAKE, so a seat it reports
// healthy is one the engine can connect.
func TestTheDoctorsSeatDialIsTheEnginesHandshake(t *testing.T) {
	headers := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)

	if err := RealDial(t.Context(), WebsocketURL(srv.URL), "", "tok-swe"); err != nil {
		t.Fatalf("RealDial: %v", err)
	}
	header := <-headers
	for name, want := range socketHeader("tok-swe") {
		if got := header.Values(name); len(got) != 1 || got[0] != want[0] {
			t.Errorf("%s = %q, want %q as the engine sends it", name, got, want)
		}
	}
	if got := header.Values("Origin"); len(got) != 0 {
		t.Errorf("the seat probe sent Origin %q, which the engine never does", got)
	}
}
