package embeddings_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// widthRecorder is an OpenAI-compatible endpoint that answers every input with
// a vector of its own width and records, for every request, whether it carried
// `dimensions` and with what.
type widthRecorder struct {
	url   string
	width int

	mu    sync.Mutex
	asked []*int
}

func newWidthRecorder(t *testing.T, width int) *widthRecorder {
	t.Helper()
	w := &widthRecorder{width: width}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			Input      json.RawMessage `json:"input"`
			Dimensions *int            `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		inputs := 1
		var many []string
		if json.Unmarshal(req.Input, &many) == nil {
			inputs = len(many)
		}
		w.mu.Lock()
		w.asked = append(w.asked, req.Dimensions)
		w.mu.Unlock()
		vector := strings.TrimSuffix(strings.Repeat("0.5,", w.width), ",")
		items := make([]string, inputs)
		for i := range items {
			items[i] = fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":[%s]}`, i, vector)
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(`{"object":"list","data":[` + strings.Join(items, ",") + `],"model":"m"}`))
	}))
	t.Cleanup(srv.Close)
	w.url = srv.URL
	return w
}

// requests is what each request asked for: nil where it carried no
// `dimensions`.
func (w *widthRecorder) requests() []*int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*int(nil), w.asked...)
}

// THE WIDTH IS ASKED FOR UNLESS THE CONFIGURATION SAYS THE ENDPOINT TAKES NONE,
// and checked either way. Every request — a single Embed and each request of a
// batch — carries `dimensions` at the configured width by default; with
// OmitDimensions none does, and the vector that comes back is still held to
// the configured width.
func TestTheWidthIsAskedForUnlessTheEndpointTakesNone(t *testing.T) {
	t.Parallel()
	for _, omit := range []bool{false, true} {
		t.Run(fmt.Sprintf("omit=%v", omit), func(t *testing.T) {
			t.Parallel()
			w := newWidthRecorder(t, 8)
			p, err := embeddings.New(embeddings.Config{
				Model: "m", Dimensions: 8, APIKey: "sk-test", BaseURL: w.url,
				Limits: small, OmitDimensions: omit,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := p.Embed(t.Context(), "one"); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			// Four inputs against three a request: two requests.
			if _, err := p.EmbedBatch(t.Context(), []string{"a", "b", "c", "d"}); err != nil {
				t.Fatalf("EmbedBatch: %v", err)
			}
			asked := w.requests()
			if len(asked) != 3 {
				t.Fatalf("%d requests, want 3 (one Embed, a batch of two)", len(asked))
			}
			for i, dims := range asked {
				switch {
				case omit && dims != nil:
					t.Errorf("request %d asked for %d dimensions of an endpoint that takes none", i, *dims)
				case !omit && (dims == nil || *dims != 8):
					t.Errorf("request %d asked for %v dimensions, want the configured 8", i, dims)
				}
			}
		})
	}

	// AND THE CHECK HOLDS WITHOUT THE ASK: an endpoint answering at another
	// width is refused as a configuration fault, not stored.
	w := newWidthRecorder(t, 4)
	p, err := embeddings.New(embeddings.Config{
		Model: "m", Dimensions: 8, APIKey: "sk-test", BaseURL: w.url,
		Limits: small, OmitDimensions: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Embed(t.Context(), "one"); err == nil {
		t.Fatal("a 4-wide answer to an 8-wide embedder was accepted")
	}
}
