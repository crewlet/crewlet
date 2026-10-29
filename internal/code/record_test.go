package code_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NEWER BUILD'S RECORD ROUND-TRIPS LOSSLESSLY: a field this build has no name
// for is carried back on encode, so a node that republishes a record it only
// half understood never strips the half it did not — and the envelope of a
// record at a version this build cannot read still decodes, since that is
// what files it.
func TestANewerRecordRoundTripsLosslessly(t *testing.T) {
	t.Parallel()
	body, err := encodeSuiteRecord(string(code.KindRepo), "repo-a", "op-1", code.RecordVersion)
	if err != nil {
		t.Fatal(err)
	}
	newer := strings.Replace(string(body), `{`, `{"novel":{"a":1},`, 1)
	rec, err := code.Decode([]byte(newer))
	if err != nil {
		t.Fatalf("decode a record carrying a field this build never wrote: %v", err)
	}
	again, err := code.Encode(rec)
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
	again, err = code.Encode(rec)
	if err != nil {
		t.Fatalf("encode a record carrying a known key: %v", err)
	}
	if !strings.Contains(string(again), `"actor":"the known actor"`) ||
		strings.Contains(string(again), "a carried actor") {
		t.Fatalf("a carried key won over the field this build set: %s", again)
	}

	future, err := encodeSuiteRecord(string(code.KindRepo), "repo-a", "op-2", code.RecordVersion+3)
	if err != nil {
		t.Fatal(err)
	}
	env, err := code.DecodeEnvelope(future)
	if err != nil || env.V != code.RecordVersion+3 || env.Subject != code.RepoSubject("repo-a") {
		t.Fatalf("a future record's envelope = %+v, %v", env, err)
	}
	if _, err := code.Decode(future); err == nil {
		t.Fatal("a record at a version this build does not read decoded whole")
	} else {
		var future *code.ErrFutureVersion
		if !errors.As(err, &future) {
			t.Fatalf("a future record's decode = %v, want *ErrFutureVersion", err)
		}
		// THE SUBJECT IS WHAT THE DEFERRAL IS FILED UNDER, so it travels on
		// the error rather than being re-derived by the caller.
		if future.Subject != code.RepoSubject("repo-a") || future.Got != code.RecordVersion+3 ||
			future.Want != code.RecordVersion {
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
	if _, err := code.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind, OpID: "op-1"}); err == nil {
		t.Error("a barrier carrying an operation id encoded")
	}
	if _, err := code.EncodeBarrier(statelog.Envelope{Kind: string(code.KindRepo)}); err == nil {
		t.Error("a repository envelope encoded as a barrier")
	}
	body, err := code.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind, Gen: 2})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	env, err := code.DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode the barrier: %v", err)
	}
	if env.OpID != "" || env.V != 1 || env.Gen != 2 || env.Subject != code.BarrierSubject() ||
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
	scratch := code.RepoSettings{ManagedBy: code.ManagedTools, Kind: code.RepoScratch,
		Owner: code.Owner{Kind: code.OwnerSeat, Ref: "dev"}, Visibility: code.VisibilityCompany,
		Search: code.SearchGrep, DefaultBranch: "main"}
	if err := scratch.Validate(); err != nil {
		t.Fatalf("a scratch repository's settings are refused: %v", err)
	}
	for name, mutate := range map[string]func(*code.RepoSettings){
		"an unknown manager": func(s *code.RepoSettings) { s.ManagedBy = "drift" },
		// WHAT A REPOSITORY IS FOR (RepoKind), held to who manages it and to
		// what it is: no kind at all; a declaration of anything but a project;
		// a tool's repository stamped with a configuration's activation; a
		// scratch repository owned by anything but its seat, or protected.
		"no kind":            func(s *code.RepoSettings) { s.Kind = "" },
		"an unknown kind":    func(s *code.RepoSettings) { s.Kind = "sandbox" },
		"a declared scratch": func(s *code.RepoSettings) { s.Kind = code.RepoScratch },
		"a tool's repository from an activation": func(s *code.RepoSettings) {
			s.ManagedBy, s.ConfigEpoch = code.ManagedTools, 1
		},
		"a scratch repository a unit owns": func(s *code.RepoSettings) {
			s.ManagedBy, s.Kind, s.Protections = code.ManagedTools, code.RepoScratch, nil
		},
		"a protected scratch repository": func(s *code.RepoSettings) {
			s.ManagedBy, s.Kind, s.Owner = code.ManagedTools, code.RepoScratch,
				code.Owner{Kind: code.OwnerSeat, Ref: "dev"}
		},
		"an unknown owner kind":    func(s *code.RepoSettings) { s.Owner.Kind = "team" },
		"no owner":                 func(s *code.RepoSettings) { s.Owner.Ref = " " },
		"no visibility":            func(s *code.RepoSettings) { s.Visibility = "" },
		"an unknown visibility":    func(s *code.RepoSettings) { s.Visibility = "team" },
		"no search mode":           func(s *code.RepoSettings) { s.Search = "" },
		"an unknown search mode":   func(s *code.RepoSettings) { s.Search = "fuzzy" },
		"a default branch no ref":  func(s *code.RepoSettings) { s.DefaultBranch = "a..b" },
		"a description too long":   func(s *code.RepoSettings) { s.Description = strings.Repeat("x", code.MaxDescriptionBytes+1) },
		"a description not UTF-8":  func(s *code.RepoSettings) { s.Description = "\xff" },
		"a pattern no ref matches": func(s *code.RepoSettings) { s.Protections[0].Pattern = "heads/main" },
		"an unknown direct push":   func(s *code.RepoSettings) { s.Protections[0].DirectPush = "bots" },
		"a tag allowed to move": func(s *code.RepoSettings) {
			s.Protections = []code.Protection{{Pattern: "refs/tags/*", AllowForce: true, DirectPush: code.DirectPersons}}
		},
		"rules out of order": func(s *code.RepoSettings) {
			s.Protections = []code.Protection{
				{Pattern: "refs/heads/z", DirectPush: code.DirectPersons},
				{Pattern: "refs/heads/a", DirectPush: code.DirectPersons},
			}
		},
		"a rule twice": func(s *code.RepoSettings) {
			s.Protections = append(s.Protections, s.Protections[0])
		},
		"too many rules": func(s *code.RepoSettings) {
			s.Protections = nil
			for i := range code.MaxProtectionsPerRepo + 1 {
				s.Protections = append(s.Protections, code.Protection{
					Pattern: "refs/heads/r" + strings.Repeat("x", i), DirectPush: code.DirectPersons})
			}
		},
	} {
		s := suiteSettings()
		mutate(&s)
		if err := s.Validate(); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}
