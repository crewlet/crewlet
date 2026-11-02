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

// ErrUnresolved is the one UNKNOWN a caller acts on rather than reports: the
// object names something nobody has resolved or read yet ([Object.Unresolved]),
// and only the resolved record could admit this caller. So the answer is
// "resolve it, then decide again on the record", and [Decide] returns it for
// a caller holding nothing that admits them outright, in exactly these cases:
//
//   - somebody else's record named by a LOGIN, to a caller who leads
//     somebody — the chart can say whether they lead its holder only of the
//     seat the login resolves to;
//   - a work item to a caller without the Worklet app ([ClassAppOrAsked]):
//     whether an open ask on it names them is on the row;
//   - a row whose container and author nobody has read ([ClassTidy],
//     [ClassContainer], [ClassDestructive]) — which project or space it is
//     filed in, and who wrote it, decide it.
//
// A caller the resolved record could never admit is refused before anything
// is read rather than answered this — a caller who leads nobody on somebody's
// queue, and a caller without a relation row's app — so what the record says
// is never theirs to learn.
//
// AN UNKNOWN RATHER THAN AN ALLOW, for the direction a caller that forgets to
// check it fails in: read as an unknown it is a 503 a lead notices, and read
// as an allow it would admit every lead to every record, which nobody would.
var ErrUnresolved = errors.New("authz: whose record this is decides it, and " +
	"the name has not been resolved to one yet")

// ErrUnsettled reports an artifact role this node could not settle: what the
// chart did answer admits the caller, and an arm it could not answer (a unit
// they may sit in, an agent they may lead) might raise the role further. See
// [Standing.Err]. An UNKNOWN for a surface that needs the role itself, never a
// refusal and never the lower role: stamped into a write, the lower role would
// have the applier refuse what the caller may do.
var ErrUnsettled = errors.New("authz: a relation that could raise this role " +
	"could not be read, so the role is not known exactly")

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
