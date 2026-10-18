package artifactupload

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// contractName is one runtime contract's name: an exact semantic version.
var contractName = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// LatestContract asks for the newest contract the build serves, resolved to
// that exact version at publish.
const LatestContract = "latest"

// RuntimeSet is the runtime contracts one build serves.
type RuntimeSet struct {
	contracts []string
}

// RuntimesIn is the contracts a runtime tree holds: one directory per exact
// version (`1.0.0/`), as the build embeds them and the artifact origin serves
// them. A tree with no such directory, or none at all, is a build serving no
// runtime, which is a set rather than an error: a page that declares no
// capability needs none.
func RuntimesIn(tree fs.FS) (RuntimeSet, error) {
	entries, err := fs.ReadDir(tree, ".")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return RuntimeSet{}, nil
	case err != nil:
		return RuntimeSet{}, fmt.Errorf("artifactupload: read the runtime tree: %w", err)
	}
	var set RuntimeSet
	for _, e := range entries {
		if e.IsDir() && contractName.MatchString(e.Name()) {
			set.contracts = append(set.contracts, e.Name())
		}
	}
	slices.SortFunc(set.contracts, compareContracts)
	return set, nil
}

// Latest is the newest contract, empty for none.
func (r RuntimeSet) Latest() string {
	if len(r.contracts) == 0 {
		return ""
	}
	return r.contracts[len(r.contracts)-1]
}

// Serves reports whether contract is one of the set.
func (r RuntimeSet) Serves(contract string) bool { return slices.Contains(r.contracts, contract) }

// Contracts is every contract served, oldest first.
func (r RuntimeSet) Contracts() []string { return slices.Clone(r.contracts) }

// compareContracts orders two exact versions numerically.
func compareContracts(a, b string) int {
	pa, pb := contractName.FindStringSubmatch(a), contractName.FindStringSubmatch(b)
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x - y
		}
	}
	return 0
}

// resolveContract is the exact contract a version of kind pins, from what the
// publish asked for and what the artifact already pins (empty for a create):
//
//   - an empty request keeps the current pin, and a create takes the newest;
//   - `latest` is the newest this build serves, resolved now and stored exact;
//   - an exact version must be one this build serves.
//
// Only a framed kind runs the runtime; a native kind is drawn by the dashboard,
// pins none, and refuses one. A version declaring a capability needs a runtime
// to reach it through.
func (s *Service) resolveContract(kind artifacts.Kind, asked, current string, caps []artifacts.Capability) (string, error) {
	if !kind.Framed() {
		// A NATIVE KIND HAS NO PAGE to grant a capability to or run a
		// runtime in, so either is refused naming the kind, never stored as
		// a declaration nothing honours.
		switch {
		case asked != "":
			return "", fmt.Errorf("%w: a %s artifact runs no page runtime; publish it with no "+
				"contract", ErrContractUnknown, kind)
		case len(caps) > 0:
			return "", fmt.Errorf("%w: a %s artifact is drawn by the dashboard and declares no "+
				"capabilities, and the version declares %s", ErrInvalid, kind, capabilityList(caps))
		}
		return "", nil
	}
	contract := asked
	switch asked {
	case "":
		contract = current
		if contract == "" {
			contract = s.runtimes.Latest()
		}
	case LatestContract:
		contract = s.runtimes.Latest()
	}
	if contract != "" && !s.runtimes.Serves(contract) {
		return "", fmt.Errorf("%w: %q is not a runtime contract this build serves (latest "+
			"is %q)", ErrContractUnknown, contract, s.runtimes.Latest())
	}
	if contract == "" && len(caps) > 0 {
		return "", fmt.Errorf("%w: the version declares %s and this build serves no page "+
			"runtime to reach them through", ErrContractUnknown, capabilityList(caps))
	}
	return contract, nil
}

// checkCapabilities refuses a capability this build does not serve.
func checkCapabilities(caps []artifacts.Capability) error {
	for _, c := range caps {
		if !c.Valid() {
			return fmt.Errorf("%w: %q (this build serves %s)", ErrCapabilityUnknown, c,
				capabilityList(artifacts.Capabilities))
		}
	}
	return nil
}

func capabilityList(caps []artifacts.Capability) string {
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = string(c)
	}
	return strings.Join(names, ", ")
}
