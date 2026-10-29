package host_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
)

// TestTheHostPassesTheSuite runs the code host's suite over one data node with
// each object store backend: the in-memory twin, and S3 through an in-process
// bucket speaking its wire — the backend whose puts go in parts and whose
// reads go in ranges.
func TestTheHostPassesTheSuite(t *testing.T) {
	t.Run("memobj", func(t *testing.T) {
		codetest.Run(t, codetest.Solo(codetest.SoloOptions{}, func(*testing.T) objstore.Backend {
			return memobj.New()
		}))
	})
	t.Run("s3fake", func(t *testing.T) {
		codetest.Run(t, codetest.Solo(codetest.SoloOptions{}, s3Backend))
	})
}

// THE GIT FLOORS PASS THE SUITE: every case one data node can run, with every
// pool on the node floor's own build (gittest.NodeFloor: 2.55, the oldest git
// a git node runs) and every person's git on the client floor's
// (gittest.ClientFloor: 2.40, the oldest a client of the code host may run) —
// the worst pairing the two floors admit, where the rest of the suite runs the
// node pin on both sides. First a probe of the fleet: its git node's pool is of
// the node floor's series and a person's `git version` of the client floor's,
// so an arm whose helpers quietly fell back to the pin cannot pass.
//
// Under every make target the two gits are REQUIRED
// (CREWLET_REQUIRE_GIT_FLOOR=1), so a machine that did not build them fails
// here naming `make git`; a bare `go test` of this package skips the arm and
// runs everything else.
//
// IT RUNS IN PARALLEL with this package's other parallel tests, because it is
// one more whole pass of the suite — 68–76 s under -race on four vCPUs (68.1
// and 69.1 s in two runs here, 76.3 s in a review's; the pin's own memobj pass
// in TestTheHostPassesTheSuite took 63.8–79.2 s in the same runs) — and run
// serially it was that much added to the serial prefix that bounds the
// package's wall time: measured, the package took 418.3 s with the arm serial
// (361.2 s of it serial) and 352.7 s with it parallel (281.9 s); and ten runs
// of the package's parallel tests, this arm among them, passed every case —
// F9's busy slots and F21's crash holds included — under -race, the arm taking
// 65.5–90.0 s while vet, lint and two other packages' suites ran beside some
// of them. It may run in parallel: everything it starts is its own (a fleet,
// its directories, ports and fault script per case), and nothing it runs
// changes the process's environment or working directory, which a parallel
// test may not (t.Setenv and t.Chdir panic in one), while the package's serial
// tests that do have all ended before a parallel one resumes.
//
// Mutations: make gittest.ClientFloor answer gittest.Require(t) — the probe
// fails; unset CREWLET_TEST_GIT_NODE_FLOOR under make test — the arm fails,
// never skips.
func TestTheGitFloorsPassTheSuite(t *testing.T) {
	t.Parallel()
	floors := codetest.SoloOptions{GitPath: gittest.NodeFloor(t), ClientGit: gittest.ClientFloor(t)}
	t.Run("probe", func(t *testing.T) {
		f := codetest.SoloNode(t, floors)
		if got := f.Nodes()[0].Pool().Version(); !sameSeries(got, gitproc.NodeMinVersion) {
			t.Fatalf("the git node's pool runs git %s, want the node floor's series, %d.%d", got,
				gitproc.NodeMinVersion.Major, gitproc.NodeMinVersion.Minor)
		}
		out := codetest.NewPerson(t, f, "ceo").Git().Run(t, "", "version")
		got, err := gitproc.ParseVersion(out)
		if err != nil {
			t.Fatalf("a person's git version: %v", err)
		}
		if !sameSeries(got, gitproc.ClientMinVersion) {
			t.Fatalf("a person's git is %s, want the client floor's series, %d.%d", got,
				gitproc.ClientMinVersion.Major, gitproc.ClientMinVersion.Minor)
		}
	})
	t.Run("memobj", func(t *testing.T) {
		codetest.Run(t, codetest.Solo(floors, func(*testing.T) objstore.Backend {
			return memobj.New()
		}))
	})
	// THE BOX HALF: a coding box's git is a client's, held to the client
	// floor (internal/sandbox's GitVersionStep), and the steps that give a
	// box its repositories run on the floor's own build against a git node
	// on the node floor's.
	t.Run("box recipes", func(t *testing.T) {
		boxRecipes(t, floors, floors.ClientGit)
	})
}

// sameSeries reports whether v is of floor's major.minor, at or above it.
func sameSeries(v, floor gitproc.Version) bool {
	return v.Major == floor.Major && v.Minor == floor.Minor && v.AtLeast(floor)
}

// s3Backend is a fresh in-process S3 bucket, through the production backend.
func s3Backend(t *testing.T) objstore.Backend {
	t.Helper()
	return s3Over(t, s3fake.Start(t, "files"))
}

// s3Over is the in-process bucket srv serves, through the production backend
// — after whatever a benchmark set on srv (a first byte, a connection's
// bandwidth).
func s3Over(t testing.TB, srv *s3fake.Server) objstore.Backend {
	t.Helper()
	b, err := s3obj.Open(t.Context(), s3obj.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "files", Prefix: "acme/",
		PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	})
	if err != nil {
		t.Fatalf("opening the S3 fake: %v", err)
	}
	return b
}
