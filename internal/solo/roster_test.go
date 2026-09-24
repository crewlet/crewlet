package solo

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

const (
	// marker is this package. A test import of it declares "run me alone".
	marker = "github.com/crewlet/crewlet/internal/solo"

	// harness is the only way in this tree to stand up a multi-member NATS
	// cluster in-process. Its ENTIRE exported surface is four such
	// constructors — StartCluster, StartPartitionableCluster, StartRelays and
	// StartDirectMesh — so "imports the harness" is an exact statement of
	// "forms a quorum", not an approximation of one. That is what lets this
	// guard be a set equality rather than a judgement call, and it is worth
	// re-checking if a single-server helper is ever added there: the day one
	// is, importing the harness stops implying a cluster and this predicate
	// has to become a call-site scan instead.
	harness = "github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
)

// pkg is the slice of `go list -json` this guard reads.
type pkg struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// list runs `go list -json` over the module, failing rather than skipping.
//
// A guard that skips when it cannot see its subject is the thing this file
// exists to prevent, so a listing that does not run is fatal — never a t.Skip,
// and never `-e`, which would turn a broken package into a record with an
// Error field and a zero exit.
func list(t *testing.T) []pkg {
	t.Helper()

	// The caller's toolchain, for the reason internal/solo/partition states
	// at goCommand: `make GO=/path/to/go` must not discover packages with a
	// different go than it runs the suite with.
	goCmd := os.Getenv("CREWLET_GO")
	if goCmd == "" {
		goCmd = "go"
	}
	cmd := exec.Command(goCmd, "list", "-json=ImportPath,Imports,TestImports,XTestImports", "./...")
	// THE MODULE ROOT, from sourcetree rather than a counted `..`.
	//
	// It was "..", which is internal/, so `go list ./...` enumerated
	// internal/... alone and this guard never saw cmd/crewlet or static.
	// Measured at the time: the guard scanned 117 packages while the partition
	// command, which runs from the root, listed 119. A cluster-forming test
	// package added under cmd/ would have been partitioned solo by the
	// Makefile and checked by nothing — a hole in exactly the guard whose job
	// is that there are no holes. The assertion below is what stops it
	// coming back.
	cmd.Dir = sourcetree.Root(t)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./...: %v (this guard cannot run without the package list)", err)
	}

	var pkgs []pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		switch err := dec.Decode(&p); {
		case err == io.EOF:
			if len(pkgs) == 0 {
				t.Fatal("go list returned no packages; this guard is asserting about nothing")
			}
			return pkgs
		case err != nil:
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
}

// requireModuleRoot fails unless the listing covers the whole module.
//
// The partition the Makefile runs lists from the module root, so a guard
// listing from anywhere else is checking a subset while claiming to check the
// set — which is how "..", one level short, went unnoticed. Asserted on a
// package OUTSIDE internal/ rather than on a count, because a count drifts
// every time somebody adds a package and would be edited rather than believed.
func requireModuleRoot(t *testing.T, pkgs []pkg) {
	t.Helper()
	const outside = "github.com/crewlet/crewlet/cmd/crewlet"
	if !slices.ContainsFunc(pkgs, func(p pkg) bool { return p.ImportPath == outside }) {
		t.Fatalf("the listing does not contain %s, so it is not the whole module — "+
			"this guard is scanning a subtree and would miss a cluster-forming "+
			"package outside internal/", outside)
	}
}

func testImports(p pkg, path string) bool {
	return slices.Contains(p.TestImports, path) || slices.Contains(p.XTestImports, path)
}

// THE ROSTER. Every package that stands up a multi-member broker declares it,
// and every package that declares it stands one up.
//
// This is the guard that makes the marker worth having. Without it the
// declaration is a convention held by memory, which is the state the sign-off
// trailer was in before scripts/check-signoff.sh existed — and the state the
// suite partition was in until this file: internal/node and internal/statelog
// each stood up a THREE-member cluster inside the shared runner for as long as
// the partition was a hand-maintained `grep -v /internal/e2e`, surviving on the
// harness's own four-attempt port-race retry rather than on being partitioned.
// Nothing reported that, because a filter naming one package cannot say which
// packages it should have named.
//
// It fails in BOTH directions deliberately. A missing declaration is the flake
// this partition exists to prevent; a spurious one is a package claiming a
// runner it does not need, which is how a marker decays into a
// flake-suppressant nobody revisits.
func TestEveryClusterFormingPackageRunsAlone(t *testing.T) {
	t.Parallel()

	pkgs := list(t)
	requireModuleRoot(t, pkgs)

	var heavy, declared []string
	for _, p := range pkgs {
		// The harness's OWN suite stands up two three-member partitionable
		// clusters (see its partition_test.go), and no import predicate can
		// see that: a package cannot import itself. It is heavy by being the
		// thing, which is exactly the case a purely computed roster would have
		// missed.
		if testImports(p, harness) || p.ImportPath == harness {
			heavy = append(heavy, p.ImportPath)
		}
		if testImports(p, marker) {
			declared = append(declared, p.ImportPath)
		}
	}

	sort.Strings(heavy)
	sort.Strings(declared)

	if !slices.Equal(heavy, declared) {
		for _, p := range heavy {
			if !slices.Contains(declared, p) {
				t.Errorf("%s stands up a multi-member broker but does not run alone.\n"+
					"Add to that package's TestMain:\n"+
					"\tfunc TestMain(m *testing.M) { os.Exit(solo.Run(m)) }\n"+
					"importing %q. See `go doc ./internal/solo` for why.", p, marker)
			}
		}
		for _, p := range declared {
			if !slices.Contains(heavy, p) {
				t.Errorf("%s asks for a runner to itself but stands up no multi-member broker.\n"+
					"Drop its %q import, or this marker becomes a way to quiet a flake "+
					"rather than a statement about contention.", p, marker)
			}
		}
	}

	t.Logf("scanned %d packages: %d stand up a cluster, %d declare it", len(pkgs), len(heavy), len(declared))
}

// The marker never reaches a shipped binary.
//
// It imports `testing`, which registers flags in whatever binary links it, so
// a non-test import would put the test framework's flag set into `crewlet`.
// The partition command reads TEST imports only for the same reason, and this
// is what stops the two readings from ever diverging.
func TestNothingOutsideATestImportsTheMarker(t *testing.T) {
	t.Parallel()

	pkgs := list(t)
	requireModuleRoot(t, pkgs)

	for _, p := range pkgs {
		if slices.Contains(p.Imports, marker) {
			t.Errorf("%s imports %s from a NON-test file; the marker pulls in `testing`, "+
				"so it belongs in a _test.go file only", p.ImportPath, marker)
		}
	}
}
