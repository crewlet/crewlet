package gitlet

import (
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECONCILE'S DECISIONS IN FLIGHT FIT ONE APPLY TRANSACTION. The records a
// reconcile holds in flight append together, and the applier commits them as
// one batch only while their rows fit statelog.ApplyTxRowBudget — so
// reconcileInFlight is held to the widest record a reconcile writes, measured
// here as the applier counts it: a create of a repository holding its bound of
// rules and of path owners, a settings change replacing every one of each, and
// a restore doing the same, of a repository holding an open change, each
// under another owner. The settings change is what reconcileRecordRows states,
// exactly, its change's count moved between two tally rows and the repository
// between two repository tally rows; the restore one change tally row fewer,
// since a deleted repository's changes are in no tally; and reconcileInFlight
// is the most of them one transaction holds.
//
// Mutations: thirty-two in flight, the width before the path owners' rows —
// 8 224 rows, red; leave the path owners out of reconcileRecordRows — the
// measured records touch more than it states, red; leave the change or the
// repository tally rows out of it, and the settings change touches more than
// it states, red.
func TestAReconcileInFlightFitsOneApplyTransaction(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	// full are settings at every row-bearing bound, their rules and owners
	// named after tag, so settings of another tag replace every one of each.
	full := func(tag string, epoch int64) RepoSettings {
		s := settingsFor("")
		s.ConfigEpoch, s.Owner = epoch, Owner{Kind: OwnerUnit, Ref: "unit-" + tag}
		s.Protections = nil
		for i := range MaxProtectionsPerRepo {
			s.Protections = append(s.Protections, Protection{PushRule: PushRule{
				Pattern: fmt.Sprintf("refs/heads/%s/%02d", tag, i), DirectPush: DirectPersons}})
		}
		for i := range MaxPathOwners {
			s.PathOwners = append(s.PathOwners, PathOwner{
				Owner: Owner{Kind: OwnerUnit, Ref: fmt.Sprintf("%s-%02d", tag, i)}, Paths: []string{"web/**"}})
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("the settings at every bound are refused: %v", err)
		}
		return s
	}
	const id = "repo-a"
	created := e.applyCounting(rec(t, NameSubject("web"), OpCreate, Scope{NamePath(NameToken("web")), RepoPath(id)},
		CreateRepo{RepoID: id, Name: "web", Settings: full("a", 1)}))
	// AN OPEN CHANGE, which every owner's tally counts while the repository is in use.
	e.apply(pushOf(t, id, pushEntry("p1", move("refs/heads/main", ZeroOID, oid("main")),
		move("refs/heads/feature", ZeroOID, oid("feature")))),
		rec(t, RepoSubject(id), OpChangeOpen, Scope{RepoPath(id)}, ChangeOpen{RepoID: id,
			ChangeID: "0192f6c1-0000-7000-8000-000000000001", SourceRef: "refs/heads/feature",
			TargetRef: "refs/heads/main", Head: oid("feature"), Title: "Retry the upload",
			Owner: Principal{Kind: PrincipalAgent, Seat: "dev"}}))
	settled := e.applyCounting(rec(t, RepoSubject(id), OpSettings, Scope{RepoPath(id)},
		SettingsRecord{RepoID: id, Settings: full("b", 1)}))
	e.apply(rec(t, RepoSubject(id), OpDelete, Scope{RepoPath(id)}, LifecycleRecord{RepoID: id, ConfigEpoch: 1}))
	back := full("c", 2)
	restored := e.applyCounting(rec(t, RepoSubject(id), OpRestore, Scope{RepoPath(id)},
		LifecycleRecord{RepoID: id, ConfigEpoch: 2, Settings: &back}))
	widest := max(created, settled, restored)
	if settled != reconcileRecordRows || restored != reconcileRecordRows-tallyRows ||
		created > reconcileRecordRows {
		t.Errorf("a create touches %d rows, a settings change %d and a restore %d; reconcileRecordRows states "+
			"%d as the most, which the settings change reaches with its change and repository tally rows and "+
			"the restore, one change tally row short of it, does not pass", created, settled, restored,
			reconcileRecordRows)
	}
	if reconcileInFlight*widest > statelog.ApplyTxRowBudget {
		t.Errorf("%d decisions in flight (reconcileInFlight) of the widest record a reconcile writes — %d rows "+
			"(a create %d, a settings change %d, a restore %d) — are %d rows, past the %d one apply transaction "+
			"commits (statelog.ApplyTxRowBudget)", reconcileInFlight, widest, created, settled, restored,
			reconcileInFlight*widest, statelog.ApplyTxRowBudget)
	}
	if (reconcileInFlight+1)*widest <= statelog.ApplyTxRowBudget {
		t.Errorf("%d decisions in flight (reconcileInFlight) of %d rows each leave room for one more in the %d "+
			"rows one apply transaction commits: hiding a decision's round trip wants as many as fit",
			reconcileInFlight, widest, statelog.ApplyTxRowBudget)
	}
}
