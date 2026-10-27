package artifactdata_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
)

// A SPENT WRITE BUDGET SAYS ITS WAIT AS A PERSON PLANS BY: rounded up to the
// largest unit ("again in 2m" for 1m23.456s), never the Duration's own
// "1m23.456s", and never shorter than the wait is.
func TestASpentBudgetSaysItsWaitAsATime(t *testing.T) {
	err := &artifactdata.RateLimited{Scope: "principal", Cost: 3, RetryAfter: 83456 * time.Millisecond}
	if msg := err.Error(); !strings.Contains(msg, "holds it again in 2m; retry then") {
		t.Fatalf("the refusal says %q, want its wait as \"again in 2m\"", msg)
	}
}
