package authz_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A CHECK IS THE WRITE, DECIDED AT THE READ'S PROOF, AND THE ONLY WRITE A READ
// GRANT SENDS (ADR-0055).
//
// A route declaring [authz.Policy.Checked] admits `dry_run=true` as its own
// verb decided by every rule but the step-up, for the person and for their
// assistant alike: an administrator whose proof is a day old checks a change
// and is still asked the step-up to make it. A read grant sends a write's
// method only as such a check: the flag on a route that declares nothing, a
// misspelt flag, or no flag at all is refused before the handler runs.
// Mutation: drop the declaration test from the guard and the undeclared route
// runs its handler for a read grant; keep the step-up for a check and the
// stale administrator's check is refused.
func TestACheckIsTheWriteDecidedAtTheReadsProof(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	router := authz.NewRouter(mux, authz.ContextGuard(authz.NoChart{}))
	ran := map[string]int{}
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			ran[name]++
			w.WriteHeader(http.StatusOK)
		})
	}
	write := authz.Policy{Action: authz.ActionConfigWrite}
	if err := router.Handle("PUT /checked", write.Checked().Summarised(),
		handler("checked")); err != nil {
		t.Fatal(err)
	}
	if err := router.Handle("PUT /unchecked", write, handler("unchecked")); err != nil {
		t.Fatal(err)
	}

	stale := iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(-48 * time.Hour)}
	assisted := func(scope iam.AssistScope) iam.Principal {
		p := stale
		p.Via, p.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())), scope
		return p
	}
	send := func(p iam.Principal, target string) int {
		r := httptest.NewRequest(http.MethodPut, target, nil)
		r = r.WithContext(iam.WithPrincipal(r.Context(), p))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		name   string
		p      iam.Principal
		target string
		admit  bool
	}{
		{"the person's check", stale, "/checked?dry_run=true", true},
		{"the person's write, its proof a day old", stale, "/checked", false},
		{"the read grant's check", assisted(iam.AssistScopeRead), "/checked?dry_run=true", true},
		{"the read grant's write", assisted(iam.AssistScopeRead), "/checked", false},
		{"the read grant's flag on a route that declares no check",
			assisted(iam.AssistScopeRead), "/unchecked?dry_run=true", false},
		{"the read grant's misspelt flag", assisted(iam.AssistScopeRead),
			"/checked?dry_run=yes", false},
		{"the read grant's flag given twice", assisted(iam.AssistScopeRead),
			"/checked?dry_run=true&dry_run=true", false},
	} {
		before := ran["checked"] + ran["unchecked"]
		code := send(c.p, c.target)
		admitted := ran["checked"]+ran["unchecked"] > before
		if admitted != c.admit {
			t.Errorf("%s: answered %d, the handler ran %v, want it to run %v", c.name, code,
				admitted, c.admit)
		}
	}

	// ONLY A WRITE DECLARES A CHECK OR A SUMMARY.
	for _, p := range []authz.Policy{{Action: authz.ActionConfigRead}} {
		if err := router.Handle("GET /read", p.Checked(), handler("read")); err == nil {
			t.Error("a read was mounted declaring a check")
		}
		if err := router.Handle("GET /read2", p.Summarised(), handler("read")); err == nil {
			t.Error("a read was mounted declaring a summary")
		}
	}

	// ONLY A PUT DECLARES IT REPLACES what its address holds.
	replace := authz.Policy{Action: authz.ActionConfigWrite}
	if err := router.Handle("PATCH /merged", replace.Replacing(), handler("merged")); err == nil {
		t.Error("a PATCH was mounted declaring it replaces what its address holds")
	}
	if err := router.Handle("PUT /replaced", replace.Replacing(), handler("replaced")); err != nil {
		t.Errorf("a PUT declaring it replaces was refused: %v", err)
	}
}
