package codetest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// RepoName is the repository every case declares.
const RepoName = "acme"

// Settings are what every case declares RepoName with: main, protected
// `direct_push: persons`, owned by the unit ENG, and indexed for search — what
// a declaration leaving search unset is recorded with.
func Settings() code.RepoSettings {
	return code.RepoSettings{
		ManagedBy: code.ManagedConfig, Owner: code.Owner{Kind: code.OwnerUnit, Ref: "ENG"},
		Visibility: code.VisibilityCompany, Search: code.SearchIndexed, DefaultBranch: "main",
		Protections: []code.Protection{{PushRule: code.PushRule{Pattern: "refs/heads/main", DirectPush: code.DirectPersons}}},
	}
}

// Case is one case's fleet, its repository and the case's context.
type Case struct {
	T     *testing.T
	Fleet Fleet
	Ctx   context.Context
}

// URL is RepoName's URL through the i-th git node — read again at every call,
// because a restarted node listens on a new one.
func (c *Case) URL(i int) string { return c.Fleet.Nodes()[i].URL() + "/git/" + RepoName }

// Rows is RepoName's rows, read linearizable.
func (c *Case) Rows() Rows {
	c.T.Helper()
	rows, err := c.Fleet.Rows(c.Ctx, RepoName, Linearizable)
	if err != nil {
		c.T.Fatalf("the rows of %s: %v", RepoName, err)
	}
	return rows
}

// MustPush pushes and fails the case unless every ref went through.
func (c *Case) MustPush(p *Person, dir string, refspecs ...string) Pushed {
	c.T.Helper()
	res := p.Push(c.Ctx, dir, c.URL(0), refspecs...)
	if res.Err != nil {
		c.T.Fatalf("git push %v: %s", refspecs, res)
	}
	return res
}

// newCase builds a case's fleet and declares its repository.
func newCase(t *testing.T, newFleet NewFleet, s Shape, settings code.RepoSettings) *Case {
	t.Helper()
	f := newFleet(t, s)
	f.Declare(t, code.Declared{Name: RepoName, Settings: settings})
	return &Case{T: t, Fleet: f, Ctx: t.Context()}
}

// suiteCase is one case of Run.
type suiteCase struct {
	name string
	run  func(t *testing.T, newFleet NewFleet)
}

// Run runs every case any topology passes — the one-data-node topology
// included — each on a fleet of its own, R1 FIRST: the cache's own tests run
// against a fake of the snapshot contract, so the first case through the real
// reader is the one that rebuilds a cache from rows alone, and a drift between
// that fake and the reader fails there, named, before any push case muddies
// it. After every case every cache must hold exactly the rows
// (NothingButSyncWritesACache's check).
func Run(t *testing.T, newFleet NewFleet) {
	t.Helper()
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) { c.run(t, newFleet) })
	}
}

// cases is Run's cases, in the order they run.
func cases() []suiteCase {
	return []suiteCase{
		{"R1_ADeletedCacheRebuildsIdentically", aDeletedCacheRebuildsIdentically},
		{"F2_APushAppliedLateIsReportedPending", aPushAppliedLateIsReportedPending},
		{"F4_TwoPushesToOneRefLandOnce", twoPushesToOneRefLandOnce},
		{"F5_AFailedUploadStoresNothing", aFailedUploadStoresNothing},
		{"F6_AnUnknownOutcomeIsSaidAndConverges", anUnknownOutcomeIsSaidAndConverges},
		{"F7_AClientThatHangsUpLeavesNoScratch", aClientThatHangsUpLeavesNoScratch},
		{"F8_AnEngineThatDiesInTheHookFailsThePush", anEngineThatDiesInTheHookFailsThePush},
		{"F9_BusySlotsAnswer503", busySlotsAnswer503},
		{"F10_ACorruptCacheIsRebuiltUnderAFetch", aCorruptCacheIsRebuiltUnderAFetch},
		{"F11_AMissingPackIs503NamingIt", aMissingPackIs503NamingIt},
		{"F12_APushIsVisibleThroughEveryNodeAtOnce", aPushIsVisibleThroughEveryNodeAtOnce},
		{"F13_AFetchFromABehindNodeFindsItsWants", aFetchFromABehindNodeFindsItsWants},
		{"F22_ABloblessCloneChecksOut", aBloblessCloneChecksOut},
		{"F23_AFilterABitmapCannotAnswerIsRefused", aFilterABitmapCannotAnswerIsRefused},
		{"F24_AShallowCloneChecksOut", aShallowCloneChecksOut},
		{"F26_AReadIsAnsweredByTheHome", aReadIsAnsweredByTheHome},
		{"F14_APushOverTheBlobLimitIsRefusedWhole", aPushOverTheBlobLimitIsRefusedWhole},
		{"F15_ProtectionRulesRefuseByName", protectionRulesRefuseByName},
		{"F16_AnAtomicPushLandsWholeOrNotAtAll", anAtomicPushLandsWholeOrNotAtAll},
		{"F25_ARefBesideADirectoryOfItsNameIsRefused", aRefBesideADirectoryOfItsNameIsRefused},
		{"F17_AnUnboundTokenFetchesAndNeverPushes", anUnboundTokenFetchesAndNeverPushes},
		{"F18_ADeletedRepositoryIsRestoredByDeclaringItAgain", aDeletedRepositoryIsRestoredByDeclaringItAgain},
		{"F19_ConcurrentPushesShareRecords", concurrentPushesShareRecords},
		{"F21_ACrashAtEveryPushStepLeavesATruthfulState", aCrashAtEveryPushStepLeavesATruthfulState},
		{"F32_AnEngineCommitLands", anEngineCommitLands},
		{"F33_AnEngineWriteRetriedIsOnePush", anEngineWriteRetriedIsOnePush},
		{"F34_ARevertThatConflictsStoresNothing", aRevertThatConflictsStoresNothing},
		{"F35_AWriteTheGrantRefusesIsNotPermitted", aWriteTheGrantRefusesIsNotPermitted},
		{"F36_ASeatPastItsQuotaIsRefused", aSeatPastItsQuotaIsRefused},
		{"F37_ASeatsPushToAPersonsBranchIsRefused", aSeatsPushToAPersonsBranchIsRefused},
		{"F38_AnEngineWriteCrashedAtEveryPointIsOnePush", anEngineWriteCrashedAtEveryPointIsOnePush},
		{"F43_ABoxPushesAndFetchesItsGrants", aBoxPushesAndFetchesItsGrants},
		{"F44_AnEndedRunIsRefused", anEndedRunIsRefused},
		{"F45_ABoxReadsItsOwnPushThroughAnotherNode", aBoxReadsItsOwnPushThroughAnotherNode},
		{"F46_ABoxPushesOnTopOfWhatAnotherNodeShowedIt", aBoxPushesOnTopOfWhatAnotherNodeShowedIt},
		{"F47_ABoxNeverSeesABranchGoBack", aBoxNeverSeesABranchGoBack},
		{"F51_ABoxPushIsRecordedByItsRunWhateverItsTrailersSay", aBoxPushIsRecordedByItsRunWhateverItsTrailersSay},
		{"R4_ARestartSweepsScratch", aRestartSweepsScratch},
		{"R6_APurgedRepositoryLeavesNothing", aPurgedRepositoryLeavesNothing},
		{"R7_AnEvictedNodesPushesAreRefused", anEvictedNodesPushesAreRefused},
		{"C1_ACompactionMergesPacksAndEveryCacheFollows", aCompactionMergesPacksAndEveryCacheFollows},
		{"C2_CompactionDuringPush", aCompactionDuringAPush},
		{"C3_CompactionDuringFetch", aCompactionDuringAFetch},
		{"C4_ARetiredPackOutlivesItsLongestRead", aRetiredPackOutlivesItsLongestRead},
		{"C5_CompactionBehindRefusesAPack", compactionBehindRefusesAPack},
		{"C6_ACrashAtEveryCompactionStepLeavesATruthfulState", aCrashAtEveryCompactionStepLeavesATruthfulState},
		{"C7_ACrashMidSwapLeavesTheOldIncarnation", aCrashMidSwapLeavesTheOldIncarnation},
		{"C8_ARestartedNodeKeepsTheCommitGraphOfACompactedRepository",
			aRestartedNodeKeepsTheCommitGraphOfACompactedRepository},
		{"F48_ExpungeVsAPushCheckedBeforeIt", expungeVsAPushCheckedBeforeIt},
		{"F49_ExpungeUnderAPushEvery100ms", expungeUnderAPushEvery100ms},
		{"F50_ExpungeRetiredPackOutlivesItsLongestRead", expungeRetiredPackOutlivesItsLongestRead},
		{"LY1_ALayerIsInstalled", aLayerIsInstalled},
		{"LY2_ALayerTheStoreLostFallsBack", aLayerTheStoreLostFallsBack},
		{"LY3_ACorruptLayerFallsBack", aCorruptLayerFallsBack},
		{"LY4_APushRacingAPublicationStaysInTheTail", aPushRacingAPublicationStaysInTheTail},
		{"LY5_ARepackingPublicationKeepsTheBitmap", aRepackingPublicationKeepsTheBitmap},
		{"LY6_ACompactionKeepsTheChainWhole", aCompactionKeepsTheChainWhole},
		{"LY7_ALayerFromANewerGitIsNotInstalled", aLayerFromANewerGitIsNotInstalled},
		{"LY10_AMaintainerKilledAtEveryPublishPoint", aMaintainerKilledAtEveryPublishPoint},
	}
}

// assertConverged fails t unless every cache of f holds exactly the rows.
func assertConverged(t *testing.T, f Fleet) {
	t.Helper()
	if err := converged(t.Context(), f); err != nil {
		t.Errorf("the caches are not the rows: %v", err)
	}
}

// R1: a node whose cache directory is gone rebuilds it from the rows alone,
// and serves exactly what it served before.
func aDeletedCacheRebuildsIdentically(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	p.Commit(work, map[string]string{"src/a.go": "package a\n"}, "two")
	c.MustPush(p, work, "main", "main:refs/heads/topic")
	p.Git().Run(t, work, "tag", "-a", "v1", "-m", "v1")
	head := p.Commit(work, map[string]string{"src/b.go": "package a\n"}, "three")
	c.MustPush(p, work, "main", "v1")
	before := p.LsRemote(c.URL(0), 0)

	node := c.Fleet.Nodes()[0]
	dir := node.CacheDir()
	c.Fleet.Kill(t, node.ID())
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove %s: %v", dir, err)
	}
	c.Fleet.Restart(t, node.ID())

	after := p.LsRemote(c.URL(0), 0)
	if !equalRefs(before, after) {
		t.Errorf("the rebuilt cache advertises %v, before it advertised %v", after, before)
	}
	clone := p.Clone(c.URL(0))
	if got := p.Head(clone); got != head {
		t.Errorf("a clone of the rebuilt cache is at %s, want %s", got, head)
	}
	if got := strings.TrimSpace(p.Git().Run(t, clone, "rev-parse", "v1^{commit}")); got == "" {
		t.Error("a clone of the rebuilt cache has no v1")
	}
	assertConverged(t, c.Fleet)
}

// equalRefs reports whether two advertisements name the same refs.
func equalRefs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// within polls ok until it holds or d passes.
func within(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
