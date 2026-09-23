package engine_test

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/oidc"
)

// deactivatedSessions is one provider session whose holder the provider has
// since off-boarded, and the endings the probe asked for.
type deactivatedSessions struct {
	mu    sync.Mutex
	live  []oidc.LiveSession
	ended []string
}

func (s *deactivatedSessions) LiveOIDC(context.Context) ([]oidc.LiveSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]oidc.LiveSession(nil), s.live...), nil
}

func (s *deactivatedSessions) End(_ context.Context, session oidc.LiveSession, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = append(s.ended, session.Lineage)
	return nil
}

func (*deactivatedSessions) Rotated(context.Context, string, string) error { return nil }

// offboardingProvider is an identity provider that answers every refresh with
// `invalid_grant` — the account behind it is gone.
func offboardingProvider(t *testing.T) *oidc.Provider {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc(oidc.MetadataPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/authorize",
			"token_endpoint":                        srv.URL + "/token",
			"jwks_uri":                              srv.URL + "/jwks",
			"scopes_supported":                      []string{"openid", "offline_access"},
			"code_challenge_methods_supported":      []string{"S256"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid_grant", "error_description": "the account is disabled",
		})
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return oidc.NewProvider(oidc.Config{
		Issuer: srv.URL, ClientID: "crewlet", ClientSecret: "not-a-real-secret",
		RedirectURI: "https://crewlet.example.com/auth/oidc/callback",
	}, srv.Client(), nil)
}

// THE PROBE THE NODE BUILDS SAYS WHAT IT ENDS, ON THE NODE'S OWN FEED.
//
// The probe is the only way a central off-boarding reaches this engine, and
// `iam_session_ended` with reason `idp_revoked` is how an administrator
// watching somebody's departure sees it land. The event type, its producer
// (`Prober.WithAudit`) and the producer's own test all existed while the duty
// built its prober without the audit — so every session it ended was recorded
// in `iam_history` and announced nowhere, and nothing failed. What is asserted
// is the node's path: the prober built as the duty builds it, publishing
// through the node's queue under the node's name.
//
// Mutation: drop the WithAudit from Engine.newProber and nothing arrives.
func TestTheNodesDeactivationProbeAnnouncesWhatItEnds(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	heard := &feed{}
	heard.listen(e)
	sessions := &deactivatedSessions{live: []oidc.LiveSession{{
		Lineage: "0192f00d-0000-7000-8000-0000000000c1",
		Person:  "0192f00d-0000-7000-8000-00000000000a",
		Refresh: "a-refresh-token-the-provider-retired",
	}}}
	prober := engine.ProberForTest(e, offboardingProvider(t), sessions)
	_, ended, err := prober.Run(t.Context())
	if err != nil || ended != 1 {
		t.Fatalf("the pass ended %d sessions (%v), want the one the provider "+
			"off-boarded", ended, err)
	}

	kind := (types.IAMSessionEnded{}).EventType()
	waitFor(t, "the probe's ending on the node's feed", func() bool {
		return len(heard.of(kind)) == 1
	})
	ev := heard.of(kind)[0]
	row, ok := events.DataAs[*types.IAMSessionEnded](ev)
	if !ok {
		t.Fatalf("the event carries %T", ev.Data)
	}
	if row.Reason != types.EndIdPRevoked || row.Lineage != sessions.live[0].Lineage ||
		row.Person != sessions.live[0].Person || ev.Source == "" {
		t.Errorf("heard %+v from %q, want the off-boarded session ended as %q "+
			"under this node's name", row, ev.Source, types.EndIdPRevoked)
	}
}

// THE PROBE IS BUILT IN ONE PLACE, AND IT IS THE PLACE THAT GIVES IT ITS AUDIT.
//
// The case above holds Engine.newProber. This holds the duty to it: a second
// `oidc.NewProber` in this package — the shape the duty's construction had
// before — would build a probe that ends sessions silently while the case
// above goes on passing against a constructor nothing calls.
//
// Mutation: build the prober in identityduties.go with oidc.NewProber again
// and this fails naming the file.
func TestTheProbeIsBuiltOnlyWhereItsAuditIs(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var sites []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewProber" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "oidc" {
					sites = append(sites, name+": "+fn.Name.Name)
				}
				return true
			})
		}
	}
	if len(sites) != 1 || sites[0] != "authevents.go: newProber" {
		t.Errorf("oidc.NewProber is called at %v, want only in authevents.go's "+
			"newProber: a probe built anywhere else ends sessions and announces "+
			"none of them", sites)
	}
}
