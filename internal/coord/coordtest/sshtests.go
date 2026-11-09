package coordtest

import (
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// ---- SSH tests ----------------------------------------------------------- //

// The two authorities a rotation tests a server with, as fingerprints. The
// second carries a slash, which a key segment escapes.
const (
	sshCurrent = "SHA256:currentAuthorityFingerprint0000000000000000"
	sshNext    = "SHA256:next/AuthorityFingerprint+0000000000000000"
)

func (h *fleetHarness) sshTest(host, authority string, at time.Time, ok bool) coord.SSHTest {
	h.t.Helper()
	s, err := h.f.PutSSHTest(h.ctx, coord.SSHTest{
		Host: host, Authority: authority, At: at, By: "jane-doe", ByKind: iam.ActorHuman, OK: ok,
		Steps: []coord.SSHTestStep{{ID: "reach", OK: true, Detail: "Reached api-1.example.com:22."},
			{ID: "host_key", OK: ok, Detail: "The server presented a pinned host key."}},
	})
	if err != nil {
		h.t.Fatalf("PutSSHTest(%s, %s): %v", host, authority, err)
	}
	return s
}

var sshTestCases = []fleetCase{{
	// ONE RECORD PER SERVER AND AUTHORITY, THE LATER TEST WINS: a rotation
	// reads each server's test with the staged authority apart from its test
	// with the current one, and an earlier test landing second must not
	// show an older answer.
	name: "an SSH test keeps the later test per server and authority, whole",
	fn: func(h *fleetHarness) {
		later := h.sshTest("api-1", sshNext, h.now().Add(time.Minute), true)
		if later.Version == 0 || later.Host != "api-1" || later.Authority != sshNext || len(later.Steps) != 2 ||
			!later.At.Equal(h.now().Add(time.Minute)) {
			h.t.Fatalf("stored %+v", later)
		}
		if kept := h.sshTest("api-1", sshNext, h.now(), false); !kept.OK || kept.Version != later.Version {
			h.t.Fatalf("an earlier test landing second returned %+v, want the later one kept", kept)
		}
		current := h.sshTest("api-1", sshCurrent, h.now(), false)
		if current.Version == later.Version || current.OK {
			h.t.Fatalf("a test with the current authority = %+v, want a record of its own", current)
		}
		read, found, err := h.f.SSHTest(h.ctx, "api-1", sshNext)
		if err != nil || !found || read.Version != later.Version || !read.OK {
			h.t.Fatalf("SSHTest(api-1, next) = %+v (found %v, %v)", read, found, err)
		}
		if _, found, err := h.f.SSHTest(h.ctx, "db-2", sshNext); found || err != nil {
			h.t.Fatalf("a server never tested reads (found %v, %v)", found, err)
		}
		if gone, err := h.f.DeleteSSHTest(h.ctx, "api-1", sshNext, current.Version); err != nil || gone {
			h.t.Fatalf("a remove at another record's version = (%v, %v), want a lost race", gone, err)
		}
		if gone, err := h.f.DeleteSSHTest(h.ctx, "api-1", sshNext, later.Version); err != nil || !gone {
			h.t.Fatalf("a remove at the current version = (%v, %v)", gone, err)
		}
		if _, found, _ := h.f.SSHTest(h.ctx, "api-1", sshNext); found {
			h.t.Fatal("a removed test still reads")
		}
	},
}, {
	// BOUNDED BY ITS WRITER, and a refused write leaves nothing behind.
	name: "an SSH test names its server, authority, instant and author, and stays within its bounds",
	fn: func(h *fleetHarness) {
		step := []coord.SSHTestStep{{ID: "reach", OK: true, Detail: "Reached."}}
		long := []coord.SSHTestStep{{ID: "reach", Detail: strings.Repeat("x", coord.MaxSSHTestDetailBytes+1)}}
		many := make([]coord.SSHTestStep, 9)
		for i := range many {
			many[i] = coord.SSHTestStep{ID: "reach"}
		}
		ok := coord.SSHTest{Host: "api-1", Authority: sshCurrent, At: h.now(), By: "jane-doe", ByKind: iam.ActorHuman, Steps: step}
		for name, mutate := range map[string]func(*coord.SSHTest){
			"no host":           func(s *coord.SSHTest) { s.Host = "" },
			"a dotted host":     func(s *coord.SSHTest) { s.Host = "api.1" },
			"no authority":      func(s *coord.SSHTest) { s.Authority = "" },
			"an authority name": func(s *coord.SSHTest) { s.Authority = "crewlet-ssh-ca-1" },
			"no instant":        func(s *coord.SSHTest) { s.At = time.Time{} },
			"no author":         func(s *coord.SSHTest) { s.By = "" },
			"no author kind":    func(s *coord.SSHTest) { s.ByKind = "" },
			"no step":           func(s *coord.SSHTest) { s.Steps = nil },
			"too many steps":    func(s *coord.SSHTest) { s.Steps = many },
			"a long sentence":   func(s *coord.SSHTest) { s.Steps = long },
		} {
			bad := ok
			mutate(&bad)
			if _, err := h.f.PutSSHTest(h.ctx, bad); err == nil {
				h.t.Errorf("PutSSHTest with %s was accepted", name)
			}
		}
		if all, _ := h.f.ListSSHTests(h.ctx); len(all) != 0 {
			h.t.Errorf("a refused test left %+v behind", all)
		}
	},
}, {
	name: "SSH tests list by server, then authority",
	fn: func(h *fleetHarness) {
		h.sshTest("db-2", sshCurrent, h.now(), true)
		h.sshTest("api-1", sshNext, h.now(), true)
		h.sshTest("api-1", sshCurrent, h.now(), true)
		all, err := h.f.ListSSHTests(h.ctx)
		if err != nil || len(all) != 3 || all[0].Host != "api-1" || all[0].Authority != sshCurrent ||
			all[1].Authority != sshNext || all[2].Host != "db-2" || all[0].Version == 0 {
			h.t.Fatalf("ListSSHTests = %+v, %v; want api-1's two, then db-2's", all, err)
		}
	},
}}
