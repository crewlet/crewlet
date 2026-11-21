package research_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/research"
)

// site serves a few pages as https://example.com, and counts its requests.
func site(t *testing.T) (*research.Reader, *atomic.Int32, *time.Time) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/rules", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" ||
			r.Header.Get("User-Agent") != research.UserAgent {
			t.Errorf("the fetch sent %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Origin rules</title><script>steal()</script>
			</head><body><nav>Menu</nav><h1>Origin rules</h1><p>Host header overrides need
			the <b>Enterprise</b> plan.</p><p>DNS overrides too.</p></body></html>`))
	})
	mux.HandleFunc("/long", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("a line of text\n", 2000)))
	})
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.com/rules", http.StatusFound)
	})
	mux.HandleFunc("/plain", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/rules", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/loop", http.StatusFound)
	})
	mux.HandleFunc("/linked", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><a href="/rules#x">Rules</a>
			<a href="https://other.example.net/a">A</a><a href="http://plain.example/">P</a>
			<a href="mailto:a@example.com">M</a><a href="/rules">Again</a></body></html>`))
	})
	mux.HandleFunc("/image", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	return research.New(&http.Client{Transport: tr}, func() time.Time { return now }), &hits, &now
}

// A page is read as its title and text, without scripts or navigation, and
// sends no credential; a second read within the cache's life is not fetched.
func TestAPageIsReadAsItsText(t *testing.T) {
	t.Parallel()
	r, hits, now := site(t)
	p, err := r.Read(t.Context(), "https://example.com/rules#top", 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Origin rules" || p.Host != "example.com" || p.URL != "https://example.com/rules" ||
		!strings.Contains(p.Text, "Host header overrides need the Enterprise plan.") ||
		strings.Contains(p.Text, "steal") || strings.Contains(p.Text, "Menu") || p.Next != 0 {
		t.Errorf("the page read as %+v", p)
	}
	if _, err := r.Read(t.Context(), "https://example.com/rules", 0); err != nil || hits.Load() != 1 {
		t.Errorf("a second read fetched again (%d fetches, %v)", hits.Load(), err)
	}
	*now = now.Add(research.CacheTTL)
	if _, err := r.Read(t.Context(), "https://example.com/rules", 0); err != nil || hits.Load() != 2 {
		t.Errorf("a read after the cache's life was not fetched again (%d fetches, %v)", hits.Load(), err)
	}
}

// A long page is read a part at a time, each ending at a line, and the parts
// make the whole.
func TestALongPageIsReadAPartAtATime(t *testing.T) {
	t.Parallel()
	r, _, _ := site(t)
	var whole strings.Builder
	from := 0
	for parts := 0; ; parts++ {
		p, err := r.Read(t.Context(), "https://example.com/long", from)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Text) > research.PartBytes || !strings.HasSuffix(p.Text, "\n") && p.Next != 0 {
			t.Fatalf("a part is %d bytes ending %q", len(p.Text), p.Text[len(p.Text)-5:])
		}
		whole.WriteString(p.Text)
		if p.Next == 0 {
			break
		}
		from = p.Next
	}
	if whole.String() != strings.Repeat("a line of text\n", 2000) {
		t.Error("the parts do not make the whole page")
	}
}

// TestAnyPublicHttpsPageIsRead: any https page on port 443 is read, whatever
// its host, a redirect to another public https page is followed, and a page's
// links are answered as absolute https addresses, each once. An address that
// is not https, carries credentials or another port, a redirect to plain http,
// a chain past the bound and a file that is not text are refused. Mutation:
// let Public take http, or follow a redirect without holding it to Public.
func TestAnyPublicHttpsPageIsRead(t *testing.T) {
	t.Parallel()
	r, _, _ := site(t)
	for _, raw := range []string{"https://docs.example.com/rules", "https://example.com/away"} {
		if p, err := r.Read(t.Context(), raw, 0); err != nil || p.Title != "Origin rules" {
			t.Errorf("%s read as %+v, %v", raw, p, err)
		}
	}
	p, err := r.Read(t.Context(), "https://example.com/linked", 0)
	want := []string{"https://example.com/rules", "https://other.example.net/a"}
	if err != nil || strings.Join(p.Links, " ") != strings.Join(want, " ") {
		t.Errorf("the page's links read as %q, %v, want %q", p.Links, err, want)
	}
	for _, raw := range []string{"http://example.com/rules", "https://user:pw@example.com/rules",
		"https://example.com:8443/rules", "https://example.com/plain", "ftp://example.com/"} {
		if _, err := r.Read(t.Context(), raw, 0); !errors.Is(err, research.ErrNotPublic) {
			t.Errorf("%s read with %v, want it refused as no public https page", raw, err)
		}
	}
	if _, err := r.Read(t.Context(), "https://example.com/loop", 0); err == nil ||
		!strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("a redirect loop read with %v", err)
	}
	if _, err := r.Read(t.Context(), "https://example.com/image", 0); !errors.Is(err,
		research.ErrNotPage) {
		t.Errorf("an image read with %v, want it refused as no page of text", err)
	}
}

// TestAPrivateAddressIsNeverReached: through the engine's guarded client, an
// address on this host or a private network is refused at connect time, so a
// page cannot be used to reach the engine or its neighbours. Mutation: hand
// the reader an unguarded client.
func TestAPrivateAddressIsNeverReached(t *testing.T) {
	t.Parallel()
	r := research.New(httpx.Guarded(research.Timeout), time.Now)
	for _, raw := range []string{"https://127.0.0.1/", "https://10.0.0.1/", "https://169.254.169.254/latest",
		"https://[::1]/"} {
		_, err := r.Read(t.Context(), raw, 0)
		if !errors.Is(err, httpx.ErrNotPublic) {
			t.Errorf("%s read with %v, want it refused as not public", raw, err)
		}
	}
}
