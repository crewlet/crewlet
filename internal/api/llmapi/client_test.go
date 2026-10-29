package llmapi_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/llmapi"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE MODELS SCREEN WAITS AS LONG AS THE ENGINE WORKS. A test, a credential's
// test and a retry each run a CLI, and the engine bounds each by its own
// budget, well past the dashboard's ordinary request deadline. The page waits
// on each by the engine's number: one below it abandons a test the engine is
// still running and reports a failure that is not one, and one far above it
// leaves a person waiting on an engine that already gave up.
func TestTheModelsScreenWaitsAsLongAsTheEngineWorks(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name string
		want time.Duration
	}{
		{"MODEL_CHECK_BUDGET_SECONDS", llmapi.CheckBudget},
		{"MODEL_CREDENTIAL_BUDGET_SECONDS", llmapi.CredentialBudget},
		{"MODEL_RETRY_BUDGET_SECONDS", llmapi.RetryBudget},
	} {
		raw, err := clientsource.Scalar(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("contract/models.ts %s is %q, which is not a whole number of seconds", tc.name, raw)
		}
		if want := int(tc.want / time.Second); got != want {
			t.Errorf("contract/models.ts %s is %d, the engine's budget is %d seconds", tc.name, got, want)
		}
	}
}
