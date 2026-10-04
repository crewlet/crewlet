package authapi_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY 503 THIS SURFACE ANSWERS SAYS WHETHER TO COME BACK, BECAUSE NOTHING
// HERE SPELLS ONE ITSELF OR PICKS ITS HINT WITHOUT ITS CAUSE.
//
// A 503 that does not say whether to come back is indistinguishable to a
// client from a node that is down for good, and the sign-in surface is where a
// client most needs to tell them apart: a browser that cannot, gives up on the
// one page that would have let the person back in. Ten sites answered a bare
// 503. The rule is held structurally — no file in this package names the
// status, so the one writer that decides the header with it,
// httpjson.Unavailable, is the only way to answer one: a Retry-After where
// waiting clears the cause, and none, deliberately, where it cannot — because
// a behavioural case per site would have to provoke ten different failures and
// would still say nothing about the eleventh.
//
// AND NO FILE NAMES THE BARE HINT, auth.RetryIdentitySeconds: every site asks
// auth.RetryIdentity with what caused the 503, nil where nothing did. Every
// site took the constant, so a write the state log refused for good — too
// large, a full log, an evicted node — told a client to come back in two
// seconds; the refusal's own rule answers those with no header, which is the
// answer.
func TestEveryUnavailableAnswerHereSaysWhetherToComeBack(t *testing.T) {
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
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "auth" &&
					n.Sel.Name == "RetryIdentitySeconds" {
					t.Errorf("%s takes the identity hint whatever caused the "+
						"503; ask auth.RetryIdentity with the cause, nil where "+
						"there is none", fset.Position(n.Pos()))
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
	svc := buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
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

// refusingRevoke is a writer whose revocation the state log refused.
type refusingRevoke struct {
	stubWriter
	reason statelog.Reason
}

func (w refusingRevoke) Revoke(context.Context, string, string, string) (statelog.Result, error) {
	return statelog.Result{}, fmt.Errorf("iamdomain: publish: %w",
		&statelog.Unavailable{Reason: w.reason, Detail: "what the refusal is about"})
}

// AND A SIGN-OUT EVERYWHERE THE LOG REFUSED FOR GOOD SAYS SO BY CARRYING NO
// HINT. A full log or a record too large answers the same retry the same, and
// the identity hint told a browser to come back in two seconds for ever; the
// refusal's own rule answers it with no Retry-After, as /work does.
// A node that is behind is the control, and keeps the hint.
func TestASignOutEverywhereTheLogRefusedSaysWhetherToRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason statelog.Reason
		retry  string
	}{
		{statelog.ReasonLogFull, ""},
		{statelog.ReasonRecordTooLarge, ""},
		{statelog.ReasonBehind, "2"},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			svc := buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				o.Writer = refusingRevoke{reason: tc.reason}
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
			if rec.Code != http.StatusServiceUnavailable ||
				rec.Header().Get("Retry-After") != tc.retry {
				t.Errorf("a %s refusal answered %d with Retry-After %q, want 503 "+
					"with %q", tc.reason, rec.Code, rec.Header().Get("Retry-After"),
					tc.retry)
			}
		})
	}
}
