package gitlet_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NEWER BUILD'S RECORD ROUND-TRIPS LOSSLESSLY: a field this build has no name
// for is carried back on encode, so a node that republishes a record it only
// half understood never strips the half it did not — and the envelope of a
// record at a version this build cannot read still decodes, since that is
// what files it.
func TestANewerRecordRoundTripsLosslessly(t *testing.T) {
	t.Parallel()
	body, err := encodeSuiteRecord(string(gitlet.KindRepo), "repo-a", "op-1", gitlet.RecordVersion)
	if err != nil {
		t.Fatal(err)
	}
	newer := strings.Replace(string(body), `{`, `{"novel":{"a":1},`, 1)
	rec, err := gitlet.Decode([]byte(newer))
	if err != nil {
		t.Fatalf("decode a record carrying a field this build never wrote: %v", err)
	}
	again, err := gitlet.Encode(rec)
	if err != nil {
		t.Fatalf("encode it back: %v", err)
	}
	if !strings.Contains(string(again), `"novel":{"a":1}`) {
		t.Fatalf("the carried field was dropped: %s", again)
	}
	// A CARRIED KEY LOSES TO A KNOWN ONE: a node that set a field this
	// build names must not have it written over by a stale copy it carried.
	rec.Extra["actor"] = []byte(`"a carried actor"`)
	rec.Actor = "the known actor"
	again, err = gitlet.Encode(rec)
	if err != nil {
		t.Fatalf("encode a record carrying a known key: %v", err)
	}
	if !strings.Contains(string(again), `"actor":"the known actor"`) ||
		strings.Contains(string(again), "a carried actor") {
		t.Fatalf("a carried key won over the field this build set: %s", again)
	}

	future, err := encodeSuiteRecord(string(gitlet.KindRepo), "repo-a", "op-2", gitlet.RecordVersion+3)
	if err != nil {
		t.Fatal(err)
	}
	env, err := gitlet.DecodeEnvelope(future)
	if err != nil || env.V != gitlet.RecordVersion+3 || env.Subject != gitlet.RepoSubject("repo-a") {
		t.Fatalf("a future record's envelope = %+v, %v", env, err)
	}
	if _, err := gitlet.Decode(future); err == nil {
		t.Fatal("a record at a version this build does not read decoded whole")
	} else {
		var future *gitlet.ErrFutureVersion
		if !errors.As(err, &future) {
			t.Fatalf("a future record's decode = %v, want *ErrFutureVersion", err)
		}
		// THE SUBJECT IS WHAT THE DEFERRAL IS FILED UNDER, so it travels on
		// the error rather than being re-derived by the caller.
		if future.Subject != gitlet.RepoSubject("repo-a") || future.Got != gitlet.RecordVersion+3 ||
			future.Want != gitlet.RecordVersion {
			t.Fatalf("the future version names %+v", future)
		}
	}
}

// THE BARRIER CARRIES NO OP ID AND NO OTHER KIND: an op id becomes a message
// id, and a repeat inside the duplicate window is answered from the dedupe
// cache with no quorum round trip — a position nothing confirmed, the one
// claim a barrier exists not to make.
func TestTheBarrierCarriesNoOpID(t *testing.T) {
	t.Parallel()
	if _, err := gitlet.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind, OpID: "op-1"}); err == nil {
		t.Error("a barrier carrying an operation id encoded")
	}
	if _, err := gitlet.EncodeBarrier(statelog.Envelope{Kind: string(gitlet.KindRepo)}); err == nil {
		t.Error("a repository envelope encoded as a barrier")
	}
	body, err := gitlet.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind, Gen: 2})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	env, err := gitlet.DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode the barrier: %v", err)
	}
	if env.OpID != "" || env.V != 1 || env.Gen != 2 || env.Subject != gitlet.BarrierSubject() ||
		len(env.Scope) != 1 || env.Scope[0] != statelog.FrameworkScope {
		t.Fatalf("the barrier decodes as %+v", env)
	}
}

// SETTINGS REFUSE WHAT A DECLARATION CANNOT MEAN — the domain's own guard
// behind the configuration validator, since the applier salvages rather than
// refuses; and one declaration is one canonical order, so the rules are
// sorted and unique.
func TestRepoSettingsRefuseWhatADeclarationCannotMean(t *testing.T) {
	t.Parallel()
	good := suiteSettings()
	if err := good.Validate(); err != nil {
		t.Fatalf("the suite's settings are refused: %v", err)
	}
	// AND A SEAT'S SCRATCH REPOSITORY, a tool's, unprotected and its seat's
	// own, is settings a create may carry.
	scratch := gitlet.RepoSettings{ManagedBy: gitlet.ManagedTools, Kind: gitlet.RepoScratch,
		Owner: gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "dev"}, Visibility: gitlet.VisibilityCompany,
		Search: gitlet.SearchGrep, DefaultBranch: "main"}
	if err := scratch.Validate(); err != nil {
		t.Fatalf("a scratch repository's settings are refused: %v", err)
	}
	for name, mutate := range map[string]func(*gitlet.RepoSettings){
		"an unknown manager": func(s *gitlet.RepoSettings) { s.ManagedBy = "drift" },
		// WHAT A REPOSITORY IS FOR (RepoKind), held to who manages it and to
		// what it is: no kind at all; a declaration of anything but a project;
		// a tool's repository stamped with a configuration's activation; a
		// scratch repository owned by anything but its seat, or protected.
		"no kind":            func(s *gitlet.RepoSettings) { s.Kind = "" },
		"an unknown kind":    func(s *gitlet.RepoSettings) { s.Kind = "sandbox" },
		"a declared scratch": func(s *gitlet.RepoSettings) { s.Kind = gitlet.RepoScratch },
		"a tool's repository from an activation": func(s *gitlet.RepoSettings) {
			s.ManagedBy, s.ConfigEpoch = gitlet.ManagedTools, 1
		},
		"a scratch repository a unit owns": func(s *gitlet.RepoSettings) {
			s.ManagedBy, s.Kind, s.Protections = gitlet.ManagedTools, gitlet.RepoScratch, nil
		},
		"a protected scratch repository": func(s *gitlet.RepoSettings) {
			s.ManagedBy, s.Kind, s.Owner = gitlet.ManagedTools, gitlet.RepoScratch,
				gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "dev"}
		},
		"an unknown owner kind":    func(s *gitlet.RepoSettings) { s.Owner.Kind = "team" },
		"no owner":                 func(s *gitlet.RepoSettings) { s.Owner.Ref = " " },
		"no visibility":            func(s *gitlet.RepoSettings) { s.Visibility = "" },
		"an unknown visibility":    func(s *gitlet.RepoSettings) { s.Visibility = "team" },
		"no search mode":           func(s *gitlet.RepoSettings) { s.Search = "" },
		"an unknown search mode":   func(s *gitlet.RepoSettings) { s.Search = "fuzzy" },
		"a default branch no ref":  func(s *gitlet.RepoSettings) { s.DefaultBranch = "a..b" },
		"a description too long":   func(s *gitlet.RepoSettings) { s.Description = strings.Repeat("x", gitlet.MaxDescriptionBytes+1) },
		"a description not UTF-8":  func(s *gitlet.RepoSettings) { s.Description = "\xff" },
		"a pattern no ref matches": func(s *gitlet.RepoSettings) { s.Protections[0].Pattern = "heads/main" },
		"an unknown direct push":   func(s *gitlet.RepoSettings) { s.Protections[0].DirectPush = "bots" },
		"a tag allowed to move": func(s *gitlet.RepoSettings) {
			s.Protections = []gitlet.Protection{{PushRule: gitlet.PushRule{Pattern: "refs/tags/*", AllowForce: true,
				DirectPush: gitlet.DirectPersons}}}
		},
		"rules out of order": func(s *gitlet.RepoSettings) {
			s.Protections = []gitlet.Protection{
				{PushRule: gitlet.PushRule{Pattern: "refs/heads/z", DirectPush: gitlet.DirectPersons}},
				{PushRule: gitlet.PushRule{Pattern: "refs/heads/a", DirectPush: gitlet.DirectPersons}},
			}
		},
		"a rule twice": func(s *gitlet.RepoSettings) {
			s.Protections = append(s.Protections, s.Protections[0])
		},
		"too many rules": func(s *gitlet.RepoSettings) {
			s.Protections = nil
			for i := range gitlet.MaxProtectionsPerRepo + 1 {
				s.Protections = append(s.Protections, gitlet.Protection{PushRule: gitlet.PushRule{
					Pattern: "refs/heads/r" + strings.Repeat("x", i), DirectPush: gitlet.DirectPersons}})
			}
		},
	} {
		s := suiteSettings()
		mutate(&s)
		if err := s.Validate(); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}
