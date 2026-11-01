package engine_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// nobody is a credential resolver that resolves nobody: what this case asks of
// the origin is its wiring, never a decision.
type nobody struct{}

func (nobody) Principal(context.Context, string) (iam.Principal, iam.Resolution) {
	return iam.Principal{}, iam.Anonymous
}

// THE ARTIFACT ORIGIN IS BUILT OVER THIS ENGINE: the handler serves the binary's
// embedded runtime from the `artifacts/` directory of the tree it is handed,
// answers everything the API serves as an empty 404, and this data node's own
// artifacts copy comes to answer for the authorizations it decides. The
// origin's own behaviour is internal/artifactserve's to hold; this holds that a
// node can build it at all, from its own Tier A.
func TestTheArtifactOriginIsBuiltOverTheEngine(t *testing.T) {
	t.Parallel()
	boot := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
		b.API.ExternalURL = "http://127.0.0.1:8000"
		b.API.Artifacts = config.APIArtifacts{Host: "127.0.0.1", Port: 8001,
			ExternalURL: "http://127.0.0.1:8001"}
	})
	e := newEngine(t, engine.Options{Bootstrap: boot})
	tree := fstest.MapFS{"artifacts/runtime/1.0.0/runtime.js": {Data: []byte("window.crewlet = {};")}}
	h, stop, err := engine.ArtifactOrigin(e, boot, nobody{}, tree)
	if err != nil {
		t.Fatalf("ArtifactOrigin: %v", err)
	}
	t.Cleanup(stop)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://127.0.0.1:8001/_crewlet/runtime/1.0.0/runtime.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "window.crewlet = {};" {
		t.Errorf("the embedded runtime answered %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://127.0.0.1:8001/health", nil))
	if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
		t.Errorf("/health on the artifact origin answered %d with %q", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(30 * time.Second)
	for !e.ArtifactsCopyServes(t.Context()) {
		if time.Now().After(deadline) {
			t.Fatal("this data node's artifacts copy never came to answer for its own reads")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, _, err := engine.ArtifactServing(nil); err == nil {
		t.Error("the origin was built with no Tier A")
	}
	unset := *boot
	unset.API.Artifacts.ExternalURL = ""
	if _, _, err := engine.ArtifactOrigin(e, &unset, nobody{}, tree); err == nil {
		t.Error("the origin was built with no external URL to name its frames by")
	}
}
