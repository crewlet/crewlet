package executor

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/envref"
)

// toolEnvOmitted is the variables a run's harness keeps out of the environment
// of the commands its model runs: the names it signs in by
// ([sandbox.RunRequest.SignInEnv]) and own, the runner's wiring the harness
// alone reads. Sorted, each once. A name outside the shell's identifier grammar
// is refused, since neither harness's mechanism could remove it.
func toolEnvOmitted(signIn []string, own ...string) ([]string, error) {
	names := make([]string, 0, len(signIn)+len(own))
	for _, name := range slices.Concat(signIn, own) {
		if !envref.ValidName(name) {
			return nil, fmt.Errorf("executor: the run's sign-in variable %q is not a variable name, so it "+
				"cannot be kept out of its tools' environment", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}
