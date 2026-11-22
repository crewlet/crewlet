package googleauth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// AN ANSWER IS KEPT WHOLE, OR REFUSED WHOLE: a CSV report comes back as
// Google sent it, with its type, and an answer past the bound a call reads,
// CSV or JSON, is refused as too large rather than handed back cut.
//
// Mutation: drop the bound's check, and the cut CSV reads as the whole report.
func TestAnAnswerThatIsNotJSONIsKeptWholeOrRefusedWhole(t *testing.T) {
	t.Parallel()
	const csv = "DATE,ESTIMATED_EARNINGS\n2026-11-20,1.23\n"
	var huge atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		if huge.Load() {
			_, _ = w.Write([]byte(strings.Repeat("x", 9<<20)))
			return
		}
		_, _ = w.Write([]byte(csv))
	}))
	t.Cleanup(srv.Close)
	tr := googleauth.NewTransport(&http.Client{Transport: httpxtest.Rewrite(t, srv)})
	const address = "https://adsense.googleapis.com/v2/accounts/pub-1/reports:generateCsv"
	var raw googleauth.Raw
	if err := tr.DoRead(t.Context(), http.MethodGet, address, "ya29.t", nil, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw.Data) != csv || raw.ContentType != "text/csv" {
		t.Errorf("the CSV came back as %q of type %q", raw.Data, raw.ContentType)
	}
	huge.Store(true)
	raw = googleauth.Raw{}
	err := tr.DoRead(t.Context(), http.MethodGet, address, "ya29.t", nil, &raw)
	if !errors.Is(err, googleauth.ErrTooLarge) || len(raw.Data) != 0 {
		t.Errorf("an answer past the bound answered %v with %d bytes, want too large and none", err, len(raw.Data))
	}
	var decoded any
	if err := tr.DoRead(t.Context(), http.MethodGet, address, "ya29.t", nil, &decoded); !errors.Is(err, googleauth.ErrTooLarge) {
		t.Errorf("a JSON answer past the bound answered %v, want too large", err)
	}
}
