package host_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
)

// TestTheHostPassesTheSuite runs Gitlet's suite over one data node with
// each object store backend: the in-memory twin, and S3 through an in-process
// bucket speaking its wire (puts in parts, reads in ranges).
//
// BOTH PASSES RUN IN PARALLEL, with each other and the package's other
// parallel tests, for TestTheGitFloorsPassTheSuite's reason: each is a whole
// pass of the suite, and run serially they were most of the serial prefix
// that bounds the package's wall time (once past CI's 30-minute slot).
func TestTheHostPassesTheSuite(t *testing.T) {
	t.Parallel()
	t.Run("memobj", func(t *testing.T) {
		t.Parallel()
		gitlettest.Run(t, gitlettest.Solo(gitlettest.SoloOptions{}, func(*testing.T) objstore.Backend {
			return memobj.New()
		}))
	})
	t.Run("s3fake", func(t *testing.T) {
		t.Parallel()
		gitlettest.Run(t, gitlettest.Solo(gitlettest.SoloOptions{}, s3Backend))
	})
}

// THE GIT FLOORS PASS THE SUITE: every case one data node can run, its pools on
// the node floor's git (gittest.NodeFloor) and every person's on the client
// floor's (gittest.ClientFloor), the worst pairing the floors admit; a probe
// checks both series first, so an arm fallen back to the pin cannot pass.
// Under the make targets both gits are REQUIRED (internal/prereqtest), so a
// machine without them fails naming `make git`; a bare `go test` skips the arm.
// It runs in parallel, one more whole pass that would lengthen the serial
// prefix bounding the package's wall time; it may, as everything it starts is
// its own and it changes neither the process's environment nor working dir.
//
// Mutations: gittest.ClientFloor answering gittest.Require(t) fails the probe;
// unset CREWLET_TEST_GIT_NODE_FLOOR under make test fails the arm, no skip.
func TestTheGitFloorsPassTheSuite(t *testing.T) {
	t.Parallel()
	floors := gitlettest.SoloOptions{GitPath: gittest.NodeFloor(t), ClientGit: gittest.ClientFloor(t)}
	t.Run("probe", func(t *testing.T) {
		f := gitlettest.SoloNode(t, floors)
		if got := f.Nodes()[0].Pool().Version(); !sameSeries(got, gitproc.NodeMinVersion) {
			t.Fatalf("the Gitlet node's pool runs git %s, want the node floor's series, %d.%d", got,
				gitproc.NodeMinVersion.Major, gitproc.NodeMinVersion.Minor)
		}
		out := gitlettest.NewPerson(t, f, "ceo").Git().Run(t, "", "version")
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
		gitlettest.Run(t, gitlettest.Solo(floors, func(*testing.T) objstore.Backend {
			return memobj.New()
		}))
	})
	// THE BOX HALF: a coding box's git is a client's, held to the client
	// floor (internal/sandbox's GitVersionStep), so a box's repository steps
	// run on that build against a Gitlet node on the node floor's.
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

// s3Over is the in-process bucket srv serves, through the production backend,
// with whatever a benchmark set on srv (a first byte, a bandwidth).
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
