package code

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
// a restore doing the same. The settings change and the restore are what
// reconcileRecordRows states, exactly, and reconcileInFlight is the most of
// them one transaction holds.
//
// Mutations: thirty-two in flight, the width before the path owners' rows —
// 8 224 rows, red; leave the path owners out of reconcileRecordRows — the
// measured records touch more than it states, red.
func TestAReconcileInFlightFitsOneApplyTransaction(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	// full are settings at every row-bearing bound, their rules and owners
	// named after tag, so settings of another tag replace every one of each.
	full := func(tag string, epoch int64) RepoSettings {
		s := settingsFor("")
		s.ConfigEpoch = epoch
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
	settled := e.applyCounting(rec(t, RepoSubject(id), OpSettings, Scope{RepoPath(id)},
		SettingsRecord{RepoID: id, Settings: full("b", 1)}))
	e.apply(rec(t, RepoSubject(id), OpDelete, Scope{RepoPath(id)}, LifecycleRecord{RepoID: id, ConfigEpoch: 1}))
	back := full("c", 2)
	restored := e.applyCounting(rec(t, RepoSubject(id), OpRestore, Scope{RepoPath(id)},
		LifecycleRecord{RepoID: id, ConfigEpoch: 2, Settings: &back}))
	widest := max(created, settled, restored)
	if settled != reconcileRecordRows || restored != reconcileRecordRows || created > reconcileRecordRows {
		t.Errorf("a create touches %d rows, a settings change %d and a restore %d; reconcileRecordRows states "+
			"%d as the most, which the settings change and the restore reach", created, settled, restored,
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
