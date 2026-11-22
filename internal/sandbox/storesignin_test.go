package sandbox

import (
	"maps"
	"testing"
)

// storeRunner is a runner whose harness signs in by names from its own store.
type storeRunner struct {
	Runner
	names []string
}

func (r storeRunner) StoreSignIn(signIn []string) []string {
	var out []string
	for _, n := range signIn {
		for _, m := range r.names {
			if n == m {
				out = append(out, n)
			}
		}
	}
	return out
}

// A BOX IS MADE WITHOUT THE SIGN-IN ITS HARNESS READS FROM ITS OWN STORE: a
// variable of the run's sign-in that the runner says its harness signs in by
// from its credential store is no variable of the box, every other variable
// (a sign-in the harness reads from its environment, a tool's token) stays,
// and a runner saying none leaves the environment as it is.
//
// Mutation: drop no variable, or drop one that is no sign-in.
func TestABoxIsMadeWithoutTheSignInItsHarnessReadsFromItsStore(t *testing.T) {
	t.Parallel()
	env := map[string]string{"ANTHROPIC_API_KEY": "k", "OPENROUTER_API_KEY": "o", "GITLAB_TOKEN": "g"}
	signIn := []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"}
	got := withoutStoreSignIn(env, signIn, storeRunner{names: []string{"ANTHROPIC_API_KEY", "GITLAB_TOKEN"}})
	if want := map[string]string{"OPENROUTER_API_KEY": "o", "GITLAB_TOKEN": "g"}; !maps.Equal(got, want) {
		t.Errorf("the box is made with %v, want %v", got, want)
	}
	if env["ANTHROPIC_API_KEY"] != "k" {
		t.Error("the spec's own environment was changed")
	}
	if got := withoutStoreSignIn(env, signIn, storeRunner{}); !maps.Equal(got, env) {
		t.Errorf("a runner signing in by no store variable made the box with %v", got)
	}
	if got := withoutStoreSignIn(env, signIn, nil); !maps.Equal(got, env) {
		t.Errorf("no runner made the box with %v", got)
	}
}
