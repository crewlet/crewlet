package iamapi_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A WRITE NOBODY CAN CONFIRM IS NEITHER ANSWERED NOR ANNOUNCED AS LANDED.
//
// The writer answered a bare position, so an `unknown` outcome came back as a
// zero position beside a nil error and every route read it as success: a 200,
// an invitation link, a token value, and the removal, revocation, reset and
// credential events beside them — each said of a write that may not exist.
// Each case makes the route's write answer unknown and holds it to a 503
// carrying the Retry-After and the op id, with nothing announced and nothing
// handed out.
//
// Mutation: treat unknown as landed on any one route and its case answers 200
// (or 201), announces an event, or hands out a link or a token.
func TestAWriteNobodyCanConfirmIsNeitherAnsweredNorAnnounced(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, call, method, target string
		body                       any
	}{
		{"removing a person", "remove", http.MethodDelete,
			"/iam/people/" + bob.String(), nil},
		{"ending somebody's sessions", "revoke", http.MethodDelete,
			"/iam/people/" + bob.String() + "/sessions", nil},
		{"resetting a second factor", "credentials", http.MethodPost,
			"/iam/people/" + bob.String() + "/mfa/reset", nil},
		{"revoking a credential", "credentials", http.MethodDelete,
			"/iam/credentials/c-1?person=" + bob.String(), nil},
		{"minting a token", "mint", http.MethodPost,
			"/iam/credentials?person=" + alice.String(), map[string]any{"label": "ci"}},
		{"inviting somebody", "invite", http.MethodPost, "/iam/invitations",
			map[string]any{"email": "dana@example.com"}},
		{"creating a person", "enrol", http.MethodPost, "/iam/people",
			map[string]any{"login": "dana.sre", "email": "dana@example.com"}},
		{"invalidating every session", "invalidate", http.MethodPost,
			"/iam/invalidate-all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.outcomes = map[string]statelog.Outcome{
				tc.call: statelog.OutcomeUnknown,
			}
			caller := administrator()
			caller.Grants = append(caller.Grants, iam.GrantFleetOperate)
			got := r.as(caller, tc.method, tc.target, tc.body)
			if got.status != http.StatusServiceUnavailable ||
				got.header.Get("Retry-After") != "2" {
				t.Fatalf("answered %d with Retry-After %q (%v), want 503 carrying 2",
					got.status, got.header.Get("Retry-After"), got.body)
			}
			if op, _ := got.body["op_id"].(string); op == "" {
				t.Errorf("body %v names no operation id — the only safe retry "+
					"is under the same one", got.body)
			}
			for _, handed := range []string{"url", "token"} {
				if _, ok := got.body[handed]; ok {
					t.Errorf("handed out a %s for a write nobody can confirm", handed)
				}
			}
			if seen := r.audit.all(); len(seen) != 0 {
				t.Errorf("announced %v about a write whose outcome is unknown", seen)
			}
		})
	}
}

// A WRITE DURABLE AND NOT YET APPLIED HERE IS A 202, NOT A 200.
//
// `200` promises the administrator's next read HERE sees what they wrote;
// a pending record will be applied by every node and this one has not yet.
// The package doc promised the 202 and the handler never gave it. Mutation:
// answer pending as applied and the status is 200.
func TestAWriteDurableButNotAppliedHereIsAccepted(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.outcomes = map[string]statelog.Outcome{"revoke": statelog.OutcomePending}
	got := r.as(administrator(), http.MethodDelete,
		"/iam/people/"+bob.String()+"/sessions", nil)
	if got.status != http.StatusAccepted || got.body["position"] == "" ||
		got.body["outcome"] != string(statelog.OutcomePending) {
		t.Fatalf("answered %d (%v), want 202 with the position to read at",
			got.status, got.body)
	}
	// DURABLE IS LANDED: the ending is announced, because every node will
	// apply it.
	if seen := r.audit.all(); len(seen) != 1 {
		t.Errorf("announced %d events for a durable revocation, want 1", len(seen))
	}
}

// A STEP NOBODY CAN CONFIRM ENDS THE SEQUENCE THERE.
//
// An edit is several records, and each is built on the one before: a seat
// binding whose outcome is unknown may or may not be on the log, so the stage
// and the document after it must not be published over a guess. Mutation:
// carry on past an unknown step and the update is asked for.
func TestAnEditStopsAtAStepNobodyCanConfirm(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.outcomes = map[string]statelog.Outcome{"claim:seat": statelog.OutcomeUnknown}
	stage := iam.StageSuspended
	got := r.as(administrator(), http.MethodPatch, "/iam/people/"+bob.String(),
		map[string]any{"seat": "platform", "stage": stage,
			"grants": []iam.Grant{iam.GrantStateRead}})
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("answered %d (%v), want 503", got.status, got.body)
	}
	for _, call := range r.writer.calls {
		if call == "stage" || call == "update" {
			t.Errorf("published %q after a step nobody could confirm (calls %v)",
				call, r.writer.calls)
		}
	}
}

// AN ESTATE THAT COULD NOT DECIDE IS A 503 THAT SAYS WHEN TO COME BACK, AND
// WHICH OPERATION TO COME BACK WITH.
//
// A refusal the framework raises — this node is behind, below the trim floor,
// holding a record it cannot decode — answered a bare 503 with no Retry-After
// and no op id, which a client cannot tell from a node gone for good, and
// which leaves the retry the docs promise with nothing to retry under.
func TestARefusedWriteSaysWhenAndWhatToRetry(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.err = fmt.Errorf("iamdomain: publish: %w", &statelog.Unavailable{
		Reason: statelog.ReasonBehind, Detail: "this node is behind"})
	req := "/iam/people/" + bob.String() + "/sessions"
	got := r.as(administrator(), http.MethodDelete, req, nil)
	if got.status != http.StatusServiceUnavailable || got.header.Get("Retry-After") != "2" {
		t.Fatalf("answered %d with Retry-After %q", got.status, got.header.Get("Retry-After"))
	}
	if op, _ := got.body["op_id"].(string); !strings.HasPrefix(op, "sessions:revoke:"+bob.String()) {
		t.Errorf("op id %q, want the one this route published under", op)
	}
}

// fakeBootstrap mints a code, or fails with err.
// A BOOTSTRAP CODE THAT CANNOT BE MINTED SAYS WHETHER WAITING WILL HELP.
//
// Every failure answered a bare 503 — no Retry-After, and a status that told
// a client to come back when a file this node could not write never would be
// written. What clears by waiting (an estate that could not be read, a record
// that could not be landed or confirmed) is 503 with the hint; a fault is 500.
func TestABootstrapMintFailureSaysWhetherWaitingHelps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		err   error
		want  int
		retry string
	}{
		{"a record that could not be landed",
			fmt.Errorf("authapi: publish: %w", statelog.ErrUnavailable),
			http.StatusServiceUnavailable, "2"},
		{"a file this node could not write",
			errors.New("authapi: write the bootstrap code: permission denied"),
			http.StatusInternalServerError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, func(o *iamapi.Options) {
				o.Bootstrap = &fakeBootstrap{err: tc.err}
			})
			// AN ESTATE NOBODY CAN ADMINISTER, so the route reaches the mint.
			for id, row := range r.directory.people {
				row.Stage = iam.StageSuspended
				r.directory.people[id] = row
			}
			got := r.as(administrator(), http.MethodPost, "/iam/bootstrap-code", nil)
			if got.status != tc.want || got.header.Get("Retry-After") != tc.retry {
				t.Errorf("answered %d with Retry-After %q (%v), want %d with %q",
					got.status, got.header.Get("Retry-After"), got.body, tc.want, tc.retry)
			}
		})
	}
}

// AN INVITATION WITH NO ADDRESS TO POINT AT IS A FAULT, NOT AN OUTAGE.
//
// `api.external_url` is this node's own configuration, and waiting never
// supplies it; the 503 it answered told every client to retry in two seconds
// for ever.
func TestAnInvitationWithNoExternalURLIsAFault(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *iamapi.Options) { o.ExternalBase = "" })
	got := r.as(administrator(), http.MethodPost, "/iam/invitations",
		map[string]any{"email": "dana@example.com"})
	if got.status != http.StatusInternalServerError || got.body["error"] != "no_external_url" {
		t.Errorf("answered %d (%v), want 500 no_external_url", got.status, got.body)
	}
}

// EVERY 503 THIS SURFACE ANSWERS CARRIES A RETRY-AFTER, BECAUSE NOTHING HERE
// SPELLS ONE ITSELF.
//
// The same gate internal/api/authapi holds itself to: three sites here spelled
// a bare 503 through FailWith, and a behavioural case per site says nothing
// about the next one. No file in this package names the status, so
// httpjson.Unavailable and httpjson.UnavailableWith — which pair it with the
// header — are the only ways to answer one.
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
						"httpjson.Unavailable or UnavailableWith, which carry "+
						"the Retry-After", fset.Position(n.Pos()))
				}
			case *ast.BasicLit:
				if n.Kind == token.INT && n.Value == "503" {
					t.Errorf("%s spells a 503 as a literal", fset.Position(n.Pos()))
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
