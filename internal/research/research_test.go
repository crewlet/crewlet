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
		http.Redirect(w, r, "https://collector.example.org/?d=zone", http.StatusFound)
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

var hosts = []string{"example.com"}

// A page is read as its title and text, without scripts or navigation, and
// sends no credential; a second read within the cache's life is not fetched.
func TestAPageIsReadAsItsText(t *testing.T) {
	t.Parallel()
	r, hits, now := site(t)
	p, err := r.Read(t.Context(), hosts, "https://example.com/rules#top", 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Origin rules" || p.Host != "example.com" || p.URL != "https://example.com/rules" ||
		!strings.Contains(p.Text, "Host header overrides need the Enterprise plan.") ||
		strings.Contains(p.Text, "steal") || strings.Contains(p.Text, "Menu") || p.Next != 0 {
		t.Errorf("the page read as %+v", p)
	}
	if _, err := r.Read(t.Context(), hosts, "https://example.com/rules", 0); err != nil || hits.Load() != 1 {
		t.Errorf("a second read fetched again (%d fetches, %v)", hits.Load(), err)
	}
	*now = now.Add(research.CacheTTL)
	if _, err := r.Read(t.Context(), hosts, "https://example.com/rules", 0); err != nil || hits.Load() != 2 {
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
		p, err := r.Read(t.Context(), hosts, "https://example.com/long", from)
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

// Only https on a listed host is read, a redirect off the list is refused, and
// a page that is not text is refused.
func TestOnlyAListedHostsPagesAreRead(t *testing.T) {
	t.Parallel()
	r, _, _ := site(t)
	for _, raw := range []string{"http://example.com/rules", "https://collector.example.org/",
		"https://user:pw@example.com/rules", "https://example.com:8443/rules",
		"https://example.com/away"} {
		if _, err := r.Read(t.Context(), hosts, raw, 0); !errors.Is(err, research.ErrNotListed) {
			t.Errorf("%s read with %v, want it refused as off the list", raw, err)
		}
	}
	if _, err := r.Read(t.Context(), hosts, "https://example.com/image", 0); !errors.Is(err,
		research.ErrNotPage) {
		t.Errorf("an image read with %v, want it refused as no page of text", err)
	}
}
