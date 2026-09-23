package authapi_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY 503 THIS SURFACE ANSWERS CARRIES A RETRY-AFTER, BECAUSE NOTHING HERE
// SPELLS ONE ITSELF.
//
// A 503 with no Retry-After is indistinguishable to a client from a node that
// is down for good, and the sign-in surface is where a client most needs to
// tell them apart: a browser that cannot, gives up on the one page that would
// have let the person back in. Ten sites answered a bare 503. The rule is held
// structurally — no file in this package names the status, so the one writer
// that pairs it with the header, httpjson.Unavailable, is the only way to
// answer one — because a behavioural case per site would have to provoke ten
// different failures and would still say nothing about the eleventh.
func TestEveryUnavailableAnswerHereCarriesARetryAfter(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
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
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "http" &&
					n.Sel.Name == "StatusServiceUnavailable" {
					t.Errorf("%s spells a 503 itself; answer it with "+
						"httpjson.Unavailable, which carries the Retry-After",
						fset.Position(n.Pos()))
				}
			case *ast.BasicLit:
				if n.Kind == token.INT && n.Value == "503" {
					t.Errorf("%s spells a 503 as a literal; answer it with "+
						"httpjson.Unavailable", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	// THE CONTROL: a glob that matched nothing would pass every rule.
	if checked < 5 {
		t.Fatalf("checked %d source files, which is not this package", checked)
	}
}

// failingRevoke is a writer whose revocation cannot be recorded.
type failingRevoke struct{ stubWriter }

func (failingRevoke) Revoke(context.Context, string, string, string) (statelog.Result, error) {
	return statelog.Result{}, errors.New("the broker is unreachable")
}

// AND ONE SITE, END TO END: signing out everywhere on a node that cannot
// record it is the unknown arm, with the hint a client retries on — not a bare
// 503 a browser reads as the engine being gone.
func TestASignOutEverywhereThatCannotLandSaysWhenToRetry(t *testing.T) {
	t.Parallel()
	svc := buildWith(t, bootstrapFor(t), nil, func(o *authapi.Options) {
		o.Writer = failingRevoke{}
	})
	mux := http.NewServeMux()
	svc.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout/all", nil)
	req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
		ID:    uuid.MustParse("0192f00d-0000-7000-8000-00000000000a"),
		Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" {
		t.Errorf("answered %d with Retry-After %q, want 503 carrying 2",
			rec.Code, rec.Header().Get("Retry-After"))
	}
}

// A SIGN-IN THAT CANNOT START BECAUSE OF THIS NODE'S OWN CONFIGURATION IS A
// FAULT, NOT AN OUTAGE.
//
// Starting a provider round trip fails on a provider block that does not
// validate, a keyring this node lacks, or its own randomness — none of which
// clears by waiting — and it answered 503, which told every client to retry in
// two seconds for ever. It is a 500; what does clear by waiting, the
// provider's discovery being unreachable, stays a 503 with its hint.
func TestAProviderSignInThatCannotStartIsAFault(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	// NO CLIENT SECRET, which the provider block refuses before it seals
	// anything.
	svc := build(t, b, oidc.NewProvider(oidc.Config{
		Issuer: idp.URL, ClientID: idpClientID,
		RedirectURI: b.API.ExternalBase() + auth.PathAuthOIDCCallback,
	}, idp.Client(), func() time.Time { return clock }))
	mux := http.NewServeMux()
	svc.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, auth.PathAuthOIDCStart, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("answered %d (%s), want 500", rec.Code, rec.Body)
	}
}
