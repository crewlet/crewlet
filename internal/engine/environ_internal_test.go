package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// THE SECRET STORE STANDS IN FRONT OF THE ENVIRONMENT THE ENGINE WAS HANDED.
//
// Every apply rebuilds the resolver over the store's snapshot, and the chain it
// built ended in the PROCESS environment whatever the engine had been handed —
// so a node's own environment answered until the first apply and the process's
// after it. The store still wins over a stale value, the handed environment
// still answers what the store does not hold, and nothing reaches past it.
func TestTheSecretStoreFrontsTheHandedEnvironment(t *testing.T) {
	t.Parallel()
	e, sv := engineWithSecrets(t)
	e.environ = config.MapSource{
		"PLAIN_URL":  "https://tracker.example.com",
		"SOME_TOKEN": "the-stale-one-from-dot-env",
	}
	if err := sv.Set(t.Context(), "SOME_TOKEN", "the-rotated-one",
		"operator", "cli", time.Now().UTC()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !e.refreshSecrets(t.Context()) {
		t.Fatal("the premise: the store's snapshot was installed")
	}
	for ref, want := range map[string]string{
		"${SOME_TOKEN}": "the-rotated-one",
		"${PLAIN_URL}":  "https://tracker.example.com",
		"${PATH}":       "",
	} {
		if got := e.Resolve(ref); got != want {
			t.Errorf("%s = %q, want %q", ref, got, want)
		}
	}
}
