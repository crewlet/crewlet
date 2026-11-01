package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// checkoutDesk is where `code_checkout` gives a job a repository: what it was
// asked, for which run and job, and what it answers.
type checkoutDesk struct {
	got      builtin.Checkout
	err      error
	asked    []builtin.RepoRequest
	runs     []string
	launches []string
}

func (d *checkoutDesk) Checkout(_ context.Context, turn *turnctx.Turn, launch string,
	req builtin.RepoRequest) (builtin.Checkout, error) {
	d.asked = append(d.asked, req)
	d.runs = append(d.runs, turn.RunID)
	d.launches = append(d.launches, launch)
	return d.got, d.err
}

// checkoutCall calls `code_checkout` as the job "launch-1" of the run "run-1"
// makes it over its bridge.
func checkoutCall(t *testing.T, desk *checkoutDesk, args map[string]any) tools.Result {
	t.Helper()
	ctx := turnctx.WithJob(t.Context(), "launch-1")
	got, err := builtin.NewCodeCheckout(desk).CallForTurn(ctx, &turnctx.Turn{RunID: "run-1"}, args)
	if err != nil {
		t.Fatalf("code_checkout: %v", err)
	}
	return got
}

// webCheckout is web given to the run to write on its seat branch, with the
// one step that clones it.
func webCheckout(outcome sandbox.GrantOutcome) builtin.Checkout {
	return builtin.Checkout{Outcome: outcome,
		Repo: sandbox.RunRepo{Repo: "web", RepoID: "0190d8f0-0000-7000-8000-00000000000a",
			Base: "0123456789abcdef0123456789abcdef01234567", Branch: "seats/swe/ENG-7",
			Access: sandbox.RepoWrite},
		Steps: []sandbox.SetupStep{{Commands: []string{"git clone --filter=blob:none \"$CREWLET_GIT_URL/web.git\" web"}}}}
}

// A CHECKOUT ASKS FOR ONE REPOSITORY, READ STRICTLY: the desk is asked exactly
// what the call named, for the run and the job that made it, and the run is
// answered the commands that clone what the row granted. Every malformed
// field is refused naming it before the desk is asked, because what a
// repository is given is a grant: a misspelt `acess` dropped would hand the
// run the default access.
//
// Mutations: read the fields with the tolerant argString (a list read as "")
// → the "access that is a list" case reaches the desk, red; drop the
// unknown-key check → the `acess` case reaches the desk, red; answer the
// steps of the request rather than the desk's → the clone command is missing,
// red.
func TestACheckoutAsksForExactlyTheRepositoryItNames(t *testing.T) {
	t.Parallel()
	desk := &checkoutDesk{got: webCheckout(sandbox.GrantAdded)}
	got := checkoutCall(t, desk, map[string]any{"repo": " web ", "access": "write", "paths": []any{"src"},
		"open_change": map[string]any{"target": "main", "draft": true}})
	if got.Failed {
		t.Fatalf("a well-formed checkout was refused: %s", got.Output)
	}
	want := builtin.RepoRequest{Repo: "web", Access: "write", Paths: []string{"src"},
		OpenChange: &builtin.OpenChangeRequest{Target: "main", Draft: true}}
	if len(desk.asked) != 1 || desk.asked[0].Repo != want.Repo || desk.asked[0].Access != want.Access ||
		!slices.Equal(desk.asked[0].Paths, want.Paths) || desk.asked[0].OpenChange == nil ||
		*desk.asked[0].OpenChange != *want.OpenChange {
		t.Fatalf("the desk was asked %+v, want %+v", desk.asked, want)
	}
	if desk.runs[0] != "run-1" || desk.launches[0] != "launch-1" {
		t.Fatalf("the desk was asked for run %q job %q, want run-1 job launch-1", desk.runs[0], desk.launches[0])
	}
	for _, part := range []string{"Repository web is checked out to this run", "seats/swe/ENG-7",
		"git clone --filter=blob:none \"$CREWLET_GIT_URL/web.git\" web"} {
		if !strings.Contains(got.Output, part) {
			t.Errorf("the answer does not say %q:\n%s", part, got.Output)
		}
	}

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no repo", map[string]any{"branch": "x"}, "it needs `repo`"},
		{"an unknown key", map[string]any{"repo": "web", "acess": "write"}, `it was given "acess"`},
		{"an access that is not one", map[string]any{"repo": "web", "access": "admin"}, "`access` is \"admin\""},
		{"an access that is a list", map[string]any{"repo": "web", "access": []any{"read"}},
			"`access` is not a string"},
		{"a branch that is an object", map[string]any{"repo": "web", "branch": map[string]any{"name": "x"}},
			"`branch` is not a string"},
		{"a repo that is an object", map[string]any{"repo": map[string]any{"name": "web"}},
			"`repo` is not a string"},
		{"paths that are not a list", map[string]any{"repo": "web", "paths": map[string]any{"dir": "src"}},
			"`paths` must be a list"},
		{"a path that is a list", map[string]any{"repo": "web", "paths": []any{[]any{"src"}}}, "paths[0] is not"},
		{"a path that is null", map[string]any{"repo": "web", "paths": []any{"src", nil}}, "paths[1] is not"},
		{"a draft that is a number", map[string]any{"repo": "web",
			"open_change": map[string]any{"draft": 1.0}}, "open_change.draft is not true or false"},
		{"a change beside a branch", map[string]any{"repo": "web", "change": "42", "branch": "x"},
			"names `change` beside `branch` or `base`"},
		{"a change beside open_change", map[string]any{"repo": "web", "change": "42",
			"open_change": map[string]any{}}, "names `change` and `open_change`"},
		{"open_change to read", map[string]any{"repo": "web", "access": "read",
			"open_change": map[string]any{}}, "`open_change` with `access: read`"},
		{"open_change that is not an object", map[string]any{"repo": "web", "open_change": true},
			"`open_change` is not an object"},
		{"open_change with an unknown key", map[string]any{"repo": "web",
			"open_change": map[string]any{"targte": "main"}}, `open_change holds "targte"`},
		{"a draft that is not a bool", map[string]any{"repo": "web",
			"open_change": map[string]any{"draft": "yes"}}, "open_change.draft is not true or false"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkoutCall(t, &checkoutDesk{}, c.args)
			if !got.Failed || !strings.Contains(got.Output, c.want) {
				t.Fatalf("a checkout with %s = %q, want refused saying %q", c.name, got.Output, c.want)
			}
			if !errors.Is(got.Cause, tools.RefusalInvalid) {
				t.Errorf("the refusal is classed %v, want invalid", got.Cause)
			}
		})
	}
	if len(desk.asked) != 1 {
		t.Fatalf("the desk was asked %d times, want only the well-formed checkout's", len(desk.asked))
	}
}

// A CHECKOUT IS A CODING RUN'S: a call no job made has no row to grant on,
// and the desk is never asked. What the row did with a checkout is said in
// the run's words: held, conflict, full and not running each refused or
// answered as what they are, and a refusal the desk composed reaches the run
// as it was written.
func TestACheckoutIsAnsweredAsTheRowTookIt(t *testing.T) {
	t.Parallel()
	desk := &checkoutDesk{got: webCheckout(sandbox.GrantAdded)}
	got, err := builtin.NewCodeCheckout(desk).CallForTurn(t.Context(), &turnctx.Turn{RunID: "run-1"},
		map[string]any{"repo": "web"})
	if err != nil || !got.Failed || !errors.Is(got.Cause, tools.RefusalUnavailable) || len(desk.asked) != 0 {
		t.Fatalf("a checkout no job made = %+v, %v (asked %d), want refused unavailable without asking", got,
			err, len(desk.asked))
	}
	if builtin.NewCodeCheckout(nil) != nil {
		t.Fatal("a checkout tool was built with no desk")
	}

	cases := []struct {
		name    string
		got     builtin.Checkout
		failed  bool
		class   tools.Refusal
		mention string
	}{
		{"held", webCheckout(sandbox.GrantHeld), false, "", "(it was already)"},
		{"conflict", webCheckout(sandbox.GrantConflict), true, tools.RefusalConflict,
			"this run has web checked out already, on branch seats/swe/ENG-7"},
		{"another requester", builtin.Checkout{Outcome: sandbox.GrantConflict}, true, tools.RefusalConflict,
			"checked out for another"},
		{"full", builtin.Checkout{Outcome: sandbox.GrantFull}, true, tools.RefusalConflict,
			"the most one run is given"},
		{"not running", builtin.Checkout{Outcome: sandbox.GrantNotRunning}, true, tools.RefusalNotRunning,
			"no longer running"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkoutCall(t, &checkoutDesk{got: c.got}, map[string]any{"repo": "web"})
			if got.Failed != c.failed || !strings.Contains(got.Output, c.mention) {
				t.Fatalf("a checkout the row took as %s = %+v, want failed %v saying %q", c.name, got, c.failed,
					c.mention)
			}
			if c.failed && !errors.Is(got.Cause, c.class) {
				t.Errorf("it is classed %v, want %s", got.Cause, c.class)
			}
		})
	}

	composed := builtin.CheckoutUnavailable("this node does not serve the company's code host now")
	got = checkoutCall(t, &checkoutDesk{err: composed}, map[string]any{"repo": "web"})
	if !got.Failed || got.Output != composed.Result.Output {
		t.Fatalf("a refusal the desk composed reached the run as %q, want %q", got.Output, composed.Result.Output)
	}
}

// A CHECKOUT READS WHAT THE CHECK ADMITS AS THE CHECK READS IT (ADR-0059):
// the check at the surface lets a number or a bool through where a string
// belongs, and one string where a list of strings does, as lossless; the
// checkout reads each of them through argval, as the model spelt it, rather
// than refusing a call the check passed by a rule of its own.
//
// Mutation: read `change` with a `case string` of its own → change 42 is
// refused, red; read `paths` as a list only → "src, lib" is refused, red.
func TestACheckoutReadsWhatTheCheckAdmitsAsTheCheckReadsIt(t *testing.T) {
	t.Parallel()
	desk := &checkoutDesk{got: webCheckout(sandbox.GrantAdded)}
	got := checkoutCall(t, desk, map[string]any{"repo": "web", "change": 42.0, "paths": "src, lib"})
	if got.Failed {
		t.Fatalf("a checkout the check admits was refused: %s", got.Output)
	}
	if len(desk.asked) != 1 || desk.asked[0].Change != "42" ||
		!slices.Equal(desk.asked[0].Paths, []string{"src", "lib"}) {
		t.Fatalf("the desk was asked %+v, want change \"42\" over the paths src and lib", desk.asked)
	}
}
