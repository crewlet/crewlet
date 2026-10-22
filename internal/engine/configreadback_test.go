package engine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/iam"
)

// configSurface is the /config surface of the node a [plane] runs: its own
// store, its own coordination plane and its own keyring, exactly as `crewlet
// run` mounts it beside the engine.
type configSurface struct {
	mux *http.ServeMux
}

func newConfigSurface(t *testing.T, p *plane) *configSurface {
	t.Helper()
	svc, err := configapi.New(configapi.Options{
		Store: p.store, Plane: p.fleet, Cipher: p.engine.Cipher(),
		Queue: p.engine.Backends().Queue,
		// No human seat is removed by anything here, so nothing is asked.
		Holders: func(context.Context, []string, time.Time) (map[string]configapi.SeatHolder, error) {
			return map[string]configapi.SeatHolder{}, nil
		},
	})
	if err != nil {
		t.Fatalf("configapi.New: %v", err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	return &configSurface{mux: mux}
}

// do sends one request as an administrator's Tier A token, as the guard
// resolves one: every grant, proved by presenting it.
func (s *configSurface) do(ctx context.Context, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	ctx = iam.WithPrincipal(ctx, iam.Principal{
		ID: uuid.NewSHA1(auth.TokenNamespace, []byte("ops")), Login: iam.TokenLogin("ops"),
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour),
	})
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res := httptest.NewRecorder()
	s.mux.ServeHTTP(res, req)
	return res
}

// revisionOf is the revision a /config answer names: its ETag, quoted.
func revisionOf(res *httptest.ResponseRecorder) string {
	return strings.Trim(res.Header().Get("ETag"), `"`)
}

// A WRITE'S 2XX IS WHAT THE NEXT READ ON THE SAME NODE ANSWERS, even when it
// lands while this node is still applying the write before it.
//
// The sequence crewlet-operator's controller suite caught: a PUT activates A,
// this node's reconciler starts applying A, and a second write activates B
// and answers 201 while that apply is still running. The apply then finished
// and made A this node's active revision again, so the read the second
// writer made next answered A — the revision its own write had just
// replaced — until the next tick applied B.
//
// The second write is made from inside the apply of the first (the engine's
// company-published hook, which runs after the epoch swap and before the
// apply concludes), which is the only place the race can be made to happen
// every time rather than once in a few hundred runs.
func TestAWritesAnswerIsWhatTheNextReadOnItsNodeAnswers(t *testing.T) {
	t.Parallel()
	for _, second := range []struct {
		name               string
		method, path, body string
	}{
		// What the flake was: the credential-rotation gesture, which
		// stores the active document again as a new revision.
		{name: "a reload", method: http.MethodPost, path: "/config/reload"},
		{name: "a replacement", method: http.MethodPut, path: "/config", body: grownCompanyDoc},
	} {
		t.Run(second.name, func(t *testing.T) {
			t.Parallel()
			p := newPlane(t)
			config := newConfigSurface(t, p)
			summary := map[string]string{"X-Summary": "a change", "Content-Type": "application/yaml"}

			first := config.do(t.Context(), http.MethodPut, "/config", companyDoc, summary)
			if first.Code != http.StatusCreated {
				t.Fatalf("PUT /config = %d: %s", first.Code, first.Body)
			}
			replaced := decodeRevisionID(t, first)

			var (
				once    sync.Once
				written *httptest.ResponseRecorder
			)
			p.engine.SetOnCompanyPublished(func(ctx context.Context) {
				once.Do(func() {
					header := map[string]string{"X-Summary": "a later change"}
					if second.body != "" {
						header = map[string]string{"X-Summary": "a later change",
							"Content-Type": "application/yaml", "If-Match": `"` + replaced + `"`}
					}
					written = config.do(ctx, second.method, second.path, second.body, header)
				})
			})
			if err := p.recon.Tick(t.Context()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if written == nil {
				t.Fatal("the apply published no company, so nothing was written inside it")
			}
			if written.Code != http.StatusCreated {
				t.Fatalf("%s %s inside the apply = %d: %s", second.method, second.path,
					written.Code, written.Body)
			}
			wrote := decodeRevisionID(t, written)

			read := config.do(t.Context(), http.MethodGet, "/config", "", nil)
			if read.Code != http.StatusOK {
				t.Fatalf("GET /config = %d: %s", read.Code, read.Body)
			}
			if got := revisionOf(read); got != wrote {
				t.Errorf("GET /config answered revision %s after %s %s answered 201 for %s: "+
					"want the write's revision, not %s, the one it replaced", got,
					second.method, second.path, wrote, replaced)
			}
			// AND THE NODE'S OWN COPY, which it boots on and offers the
			// fleet at its next start: walked back to the replaced
			// revision, a restart would boot on a company the fleet has
			// moved past.
			held, found, err := p.store.Configs().Active(t.Context())
			if err != nil || !found {
				t.Fatalf("this node holds no active revision (found=%v err=%v)", found, err)
			}
			if held.ID != wrote {
				t.Errorf("this node's own active revision is %s after the apply concluded, "+
					"want %s, the write that landed while it ran", held.ID, wrote)
			}
		})
	}
}

// decodeRevisionID is the revision a write's 201 says it activated.
func decodeRevisionID(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil || body.RevisionID == "" {
		t.Fatalf("a write answered without its revision (%v): %s", err, res.Body)
	}
	return body.RevisionID
}
