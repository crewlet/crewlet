package authz

import "errors"

// ErrNoChart reports a decision that needed the company hierarchy on a
// surface that has none.
//
// A SENTINEL because the two callers do opposite things with it: an HTTP
// surface answers 503 and says to try again, and a command-line tool says
// which flag would give it a company. Both are wrong if this reads as a
// refusal — see [Chart].
var ErrNoChart = errors.New("authz: this surface holds no company chart, so " +
	"a lead relation cannot be decided")

// ErrNoPattern reports a route mounted with no pattern at all.
var ErrNoPattern = errors.New("authz: a guarded route needs a pattern")

// PolicyError is a route table mistake, named by the pattern that has it.
//
// ITS OWN TYPE because a route table collects them: a caller mounting forty
// routes wants every mistake named at once rather than the first one, and a
// sentinel carries no pattern to report.
type PolicyError struct {
	Pattern string
	Problem string
}

// Error renders the mistake as a sentence naming the pattern to fix.
func (e *PolicyError) Error() string {
	return "authz: the route " + e.Pattern + " " + e.Problem
}

// newPolicyError is the constructor the router uses.
func newPolicyError(pattern, problem string) error {
	return &PolicyError{Pattern: pattern, Problem: problem}
}
