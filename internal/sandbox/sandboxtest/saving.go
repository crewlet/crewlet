package sandboxtest

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A save under way on its run's row: the cases [Run] drives for it.
//
// EVERY ONE IS A PROPERTY OF A WRITE OF THE ROW. A save, the capture of a job
// still working, reserves its place only while its job runs undecided
// (sandbox.PendingStore.ReserveSave), and keeps a second place beside the
// row's newest capture until it can no longer become the turn's kept one: a
// final take of the job that overtakes it never hides it. Each is decided by
// the store on the row as it stands, so each is certified here, on the store,
// rather than in the code that will call it.
//
// EACH CASE RUNS IN A BUBBLE, so the store's clock is the case's: a save is
// told from one whose taker stopped by its age on that clock.

// savingCases are the save cases that need the store alone, in the shape [Run]
// drives.
var savingCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"AFinalTakeAfterASaveTakesThePlaceAfterIt", testAFinalTakeAfterASaveTakesThePlaceAfterIt},
	{"ASaveIsSettledOnlyOverItsBegunStamp", testASaveIsSettledOnlyOverItsBegunStamp},
	{"AStampTheRowHoldsInAnySectionIsAnsweredTaken", testAStampTheRowHoldsInAnySectionIsAnsweredTaken},
}

// savingRawCases are the save cases that read or write the row's raw record:
// what a write left untouched, and the rows a build without the save's
// section leaves.
var savingRawCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore, coord.SandboxRuns)
}{
	{"ASaveReservesItsPlaceOnlyWhileItsJobRunsUndecided", testASaveReservesItsPlaceOnlyWhileItsJobRunsUndecided},
	{"ASaveStaysInSightUntilItCanNoLongerBeTheTurnsKeptCapture",
		testASaveStaysInSightUntilItCanNoLongerBeTheTurnsKeptCapture},
}

// savingFleetCases are the save cases of two nodes' stores over one
// coordination backend.
var savingFleetCases = []struct {
	name string
	fn   func(t *testing.T, s, other sandbox.PendingStore)
}{
	{"OneSaveOfARunIsInFlightAcrossTheFleet", testOneSaveOfARunIsInFlightAcrossTheFleet},
}

// saveTaker is the taker lease every save here is taken under, and
// finalTaker every final take's.
const (
	saveTaker  = "node-a:" + incarnationSuffix
	finalTaker = "node-b:" + incarnationSuffix
)

// runningRow launches turnID's run on node-a and answers its row, running: a
// job working, its end unread.
func runningRow(t *testing.T, s sandbox.PendingStore, turnID string) sandbox.PendingRun {
	t.Helper()
	r := run(turnID)
	r.LaunchNode = "node-a"
	mustLaunched(t, s, r)
	return mustGet(t, s, turnID)
}

// saveAt is a save of r's job at place seq of its launch, taken at at: begun,
// cut off, holding no note, placed at a save point.
func saveAt(r sandbox.PendingRun, seq int64, at time.Time) sandbox.SessionCapture {
	return sandbox.SessionCapture{LaunchSeq: r.LaunchSeq, CaptureSeq: seq, Launch: r.LaunchID,
		State: sandbox.CaptureBegun, Interrupted: true, Through: new(0), At: at, Taker: saveTaker, TakerEpoch: 3,
		Harness: "claude-code", SessionID: "s-1"}
}

// finalAt is the final take of r's job at place seq, taken at at, begun.
func finalAt(r sandbox.PendingRun, seq int64, at time.Time) sandbox.SessionCapture {
	return sandbox.SessionCapture{LaunchSeq: r.LaunchSeq, CaptureSeq: seq, Launch: r.LaunchID,
		State: sandbox.CaptureBegun, Final: true, Through: new(0), At: at, Taker: finalTaker, TakerEpoch: 5,
		Harness: "claude-code", SessionID: "s-1"}
}

// stampedAs is c's take come out as state, as its taker stamps it.
func stampedAs(c sandbox.SessionCapture, state sandbox.CaptureState) sandbox.SessionCapture {
	c.State = state
	switch state {
	case sandbox.CaptureLanded:
		c.Object, c.ObjectHash, c.ObjectSize = objstore.KeyAt(c.At).String(), "h-1", 1
	case sandbox.CaptureUnresolved:
		c.Object = objstore.KeyAt(c.At).String()
	case sandbox.CaptureRefused:
		c.Reason = "the session could not be uploaded"
	}
	return c
}

// mustStamp stamps c on turnID's row, which must take it.
func mustStamp(t *testing.T, s sandbox.PendingStore, turnID string, c sandbox.SessionCapture) {
	t.Helper()
	if took, err := s.StampCapture(t.Context(), turnID, c); err != nil || !took {
		t.Fatalf("stamping (%d, %d) %s = %v, %v; want it taken", c.LaunchSeq, c.CaptureSeq, c.State, took, err)
	}
}

// mustReserve reserves save on turnID's row, which must take it.
func mustReserve(t *testing.T, s sandbox.PendingStore, turnID string, save sandbox.SessionCapture) {
	t.Helper()
	if took, err := s.ReserveSave(t.Context(), turnID, save); err != nil || !took {
		t.Fatalf("reserving the save at (%d, %d) = %v, %v; want it taken", save.LaunchSeq, save.CaptureSeq,
			took, err)
	}
}

// sameTake reports whether got is a stamp of want's take (its place, its job
// and its instant) in state.
func sameTake(got *sandbox.SessionCapture, want sandbox.SessionCapture, state sandbox.CaptureState) bool {
	return got != nil && got.LaunchSeq == want.LaunchSeq && got.CaptureSeq == want.CaptureSeq &&
		got.Launch == want.Launch && got.At.Equal(want.At) && got.State == state
}

// rawVersion is the version of turnID's raw record: a write that changed
// nothing leaves it as it was.
func rawVersion(t *testing.T, runs coord.SandboxRuns, turnID string) uint64 {
	t.Helper()
	record, found, err := runs.SandboxRun(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("read the raw row of %s: found=%v err=%v", turnID, found, err)
	}
	return record.Version
}

// writtenAsOlderBuild rewrites turnID's row as a build without the save's
// section would: it reads the row, changes it by edit, and writes back what it
// carried of the section untouched, as a key it does not know.
func writtenAsOlderBuild(t *testing.T, runs coord.SandboxRuns, turnID string, edit func(*sandbox.PendingRun)) {
	t.Helper()
	if _, ok, err := runs.MutateSandboxRun(t.Context(), turnID, func(current coord.Record) (coord.SandboxRunChange, error) {
		var row sandbox.PendingRun
		if err := json.Unmarshal(current.Value, &row); err != nil {
			return coord.SandboxRunChange{}, err
		}
		edit(&row)
		raw, err := json.Marshal(row)
		return coord.SandboxRunChange{Put: raw}, err
	}); err != nil || !ok {
		t.Fatalf("rewrite %s as a build without the save's section: %v %v", turnID, ok, err)
	}
}

// A SAVE RESERVES ITS PLACE ONLY WHILE ITS JOB RUNS UNDECIDED. Each refusing
// row breaks one conjunct of the license with every other holding, and nothing
// is written: a run not running; a save of another launch; a job recorded over
// or its completion announced (each arm of [PendingRun.jobOver], a loss and a
// harness's end a hand-back wrote among them); an ending, an end owed or
// recorded; a capture or save of the turn that may still land; a place off by
// one. A stale or expired stamp licenses the next save; a
// malformed save is an error before any write, and a repeat is answered taken.
//
// Mutation: drop any conjunct or check, and its row reserves; read the license
// without a take's or a record's window, or a stale save as in flight, and a
// licensing row refuses.
func testASaveReservesItsPlaceOnlyWhileItsJobRunsUndecided(t *testing.T, s sandbox.PendingStore,
	runs coord.SandboxRuns,
) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		now := time.Now()
		later := now.Add(time.Second)
		rows := []struct {
			name     string
			licenses bool
			// set brings turnID's row to the shape the row names, and answers
			// the save to reserve there.
			set func(t *testing.T, turnID string) sandbox.SessionCapture
		}{
			{"a job running undecided", true, func(t *testing.T, turnID string) sandbox.SessionCapture {
				return saveAt(runningRow(t, s, turnID), 1, now)
			}},
			{"a save whose taker stopped longer ago than a take keeps", true,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					// Past the kept bound's budget on purpose: the most any reader holds.
					mustReserve(t, s, turnID, saveAt(r, 1, now.Add(-sandbox.SessionCaptureBudget-time.Minute)))
					return saveAt(r, 2, now)
				}},
			{"the newest capture's record past the window any record names it in", true,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					old := saveAt(r, 1, now.Add(-objstore.RecordWithin-2*objstore.AssumedClockSkew-time.Minute))
					old.Final, old.Interrupted = false, false
					mustStamp(t, s, turnID, old)
					mustStamp(t, s, turnID, stampedAs(old, sandbox.CaptureUnresolved))
					return saveAt(r, 2, now)
				}},
			{"a save a build without the section settled refused in the newest place", true,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					first := saveAt(r, 1, now)
					mustReserve(t, s, turnID, first)
					writtenAsOlderBuild(t, runs, turnID, func(row *sandbox.PendingRun) {
						settled := stampedAs(first, sandbox.CaptureRefused)
						row.Capture = &settled
					})
					return saveAt(r, 2, later)
				}},
			{"a save a build without the let-go left behind a kept capture", true,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					stale := saveAt(r, 1, now)
					mustReserve(t, s, turnID, stale)
					take := saveAt(r, 2, now)
					take.Interrupted = false
					mustStamp(t, s, turnID, take)
					mustStamp(t, s, turnID, stampedAs(take, sandbox.CaptureLanded))
					writtenAsOlderBuild(t, runs, turnID, func(row *sandbox.PendingRun) { row.Saving = &stale })
					return saveAt(r, 3, later)
				}},
			{"its completion claimed", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				r := runningRow(t, s, turnID)
				mustClaim(t, s, turnID)
				return saveAt(r, 1, now)
			}},
			{"its run parked", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				r := runningRow(t, s, turnID)
				park(t, s, turnID)
				return saveAt(r, 1, now)
			}},
			{"another launch, by its ordinal", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				save := saveAt(runningRow(t, s, turnID), 1, now)
				save.LaunchSeq++
				return save
			}},
			{"another launch, by its job", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				save := saveAt(runningRow(t, s, turnID), 1, now)
				save.Launch = "another-job"
				return save
			}},
			{"a final take of the job, the newest, refused", false,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					final := finalAt(r, 1, now)
					mustStamp(t, s, turnID, final)
					mustStamp(t, s, turnID, stampedAs(final, sandbox.CaptureRefused))
					return saveAt(r, 2, later)
				}},
			{"a final take of the job kept beside a newer one refused", false,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					mustStamp(t, s, turnID, stampedAs(finalAt(r, 1, now), sandbox.CaptureLanded))
					mustStamp(t, s, turnID, stampedAs(saveAt(r, 2, now), sandbox.CaptureRefused))
					return saveAt(r, 3, later)
				}},
			{"how its harness ended, read with no capture", false,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					if ok, err := s.MarkHarnessEnd(t.Context(), turnID, r.LaunchID,
						sandbox.EndRead{End: sandbox.HarnessEndCutOff}); err != nil || !ok {
						t.Fatalf("MarkHarnessEnd = %v, %v", ok, err)
					}
					return saveAt(r, 1, now)
				}},
			{"how its harness ended, in a hand-back that cleared its failures", false,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					failed := releaseOf(mustClaim(t, s, turnID))
					failed.CollectFailedAt = now
					if ok, err := s.ReleaseClaim(t.Context(), turnID, failed); err != nil || !ok {
						t.Fatalf("ReleaseClaim = %v, %v", ok, err)
					}
					answered := releaseOf(mustClaim(t, s, turnID))
					answered.BoxAnswered, answered.HarnessEnd = true, sandbox.HarnessEndOwnRun
					if ok, err := s.ReleaseClaim(t.Context(), turnID, answered); err != nil || !ok {
						t.Fatalf("ReleaseClaim = %v, %v", ok, err)
					}
					if got := mustGet(t, s, turnID); got.Status != sandbox.StatusRunning ||
						got.Launch.CollectFailures != 0 || got.Launch.Collected {
						t.Fatalf("the hand-back left status %s, %d failures and collected %v; want a running job "+
							"with its failures cleared and nothing collected", got.Status,
							got.Launch.CollectFailures, got.Launch.Collected)
					}
					return saveAt(r, 1, now)
				}},
			{"its cap seen ending it", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				r := runningRow(t, s, turnID)
				if ok, err := s.MarkEndedAtDeadline(t.Context(), turnID, r.LaunchID); err != nil || !ok {
					t.Fatalf("MarkEndedAtDeadline = %v, %v", ok, err)
				}
				return saveAt(r, 1, now)
			}},
			{"its box read back by a collection handed back", false,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					release := releaseOf(mustClaim(t, s, turnID))
					release.Collected, release.CollectedAt = true, now
					if ok, err := s.ReleaseClaim(t.Context(), turnID, release); err != nil || !ok {
						t.Fatalf("ReleaseClaim = %v, %v", ok, err)
					}
					return saveAt(r, 1, now)
				}},
			{"a collection of it whose read failed", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				r := runningRow(t, s, turnID)
				release := releaseOf(mustClaim(t, s, turnID))
				release.CollectFailedAt = now
				if ok, err := s.ReleaseClaim(t.Context(), turnID, release); err != nil || !ok {
					t.Fatalf("ReleaseClaim = %v, %v", ok, err)
				}
				return saveAt(r, 1, now)
			}},
			{"a refusal of its completion owed, nothing else of its end recorded", false,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					if ok, err := s.OweRefusal(t.Context(), turnID, r.LaunchID,
						sandbox.OwedRefusal{Condition: "provider", Since: now},
						sandbox.Fence{Owner: r.Owner, Epoch: r.OwnerEpoch}); err != nil || !ok {
						t.Fatalf("OweRefusal = %v, %v", ok, err)
					}
					if got := mustGet(t, s, turnID); got.Status != sandbox.StatusRunning || got.Launch.Collected ||
						got.Launch.HarnessEnd != "" || got.Capture != nil {
						t.Fatalf("the owed refusal left status %s, collected %v, harness end %q and capture %+v; "+
							"want a running job with nothing else of its end recorded", got.Status,
							got.Launch.Collected, got.Launch.HarnessEnd, got.Capture)
					}
					return saveAt(r, 1, now)
				}},
			{"a loss of its job marked", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				r := runningRow(t, s, turnID)
				if ok, err := s.MarkLost(t.Context(), turnID, r.LaunchID, sandbox.JobLoss{
					Cause: sandbox.LossEngineRestarted, Node: "node-a", At: now}); err != nil || !ok {
					t.Fatalf("MarkLost = %v, %v", ok, err)
				}
				return saveAt(r, 1, now)
			}},
			{"its ending decided", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				r := runningRow(t, s, turnID)
				decide(t, s, turnID, everyJob(sandbox.Fence{}, sandbox.Active))
				return saveAt(r, 1, now)
			}},
			{"its turn's end owed", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				r := runningRow(t, s, turnID)
				if _, ok, err := s.OweEnd(t.Context(), turnID, json.RawMessage(`{"outcome":"lost"}`)); err != nil || !ok {
					t.Fatalf("OweEnd = %v, %v", ok, err)
				}
				return saveAt(r, 1, now)
			}},
			{"its turn's end recorded", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				r := runningRow(t, s, turnID)
				owed := json.RawMessage(`{"outcome":"lost"}`)
				if _, ok, err := s.OweEnd(t.Context(), turnID, owed); err != nil || !ok {
					t.Fatalf("OweEnd = %v, %v", ok, err)
				}
				if ok, err := s.ClearEnd(t.Context(), turnID, owed); err != nil || !ok {
					t.Fatalf("ClearEnd = %v, %v", ok, err)
				}
				return saveAt(r, 1, now)
			}},
			{"the newest capture being kept", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				r := runningRow(t, s, turnID)
				take := saveAt(r, 1, now)
				take.Interrupted = false
				mustStamp(t, s, turnID, take)
				return saveAt(r, 2, later)
			}},
			{"a read standing by for the newest capture", false,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					take := saveAt(r, 1, now)
					mustStamp(t, s, turnID, take)
					standby := take
					standby.At, standby.Taker = later, finalTaker
					if ok, err := s.StandBy(t.Context(), turnID, standby); err != nil || !ok {
						t.Fatalf("StandBy = %v, %v", ok, err)
					}
					mustStamp(t, s, turnID, stampedAs(take, sandbox.CaptureRefused))
					return saveAt(r, 2, later)
				}},
			{"the newest capture's record awaiting its answer", false,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					take := saveAt(r, 1, now)
					mustStamp(t, s, turnID, take)
					mustStamp(t, s, turnID, stampedAs(take, sandbox.CaptureUnresolved))
					return saveAt(r, 2, later)
				}},
			{"an earlier launch's final take still being kept as the newest capture", false,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					mustStamp(t, s, turnID, finalAt(r, 1, now))
					mustClaim(t, s, turnID)
					next := run(turnID)
					next.LaunchNode = "node-a"
					mustLaunched(t, s, next)
					relaunched := mustGet(t, s, turnID)
					if relaunched.LaunchSeq != r.LaunchSeq+1 || relaunched.Capture == nil ||
						relaunched.Capture.LaunchSeq != r.LaunchSeq || relaunched.Capture.State != sandbox.CaptureBegun {
						t.Fatalf("the relaunch is launch %d beside the newest capture %+v, want launch %d beside "+
							"the earlier launch's final take still begun", relaunched.LaunchSeq, relaunched.Capture,
							r.LaunchSeq+1)
					}
					return saveAt(relaunched, 1, later)
				}},
			{"a save of an earlier launch still being kept behind its refused final take", false,
				func(t *testing.T, turnID string) sandbox.SessionCapture {
					r := runningRow(t, s, turnID)
					mustReserve(t, s, turnID, saveAt(r, 1, now))
					final := finalAt(r, 2, now)
					mustStamp(t, s, turnID, final)
					mustStamp(t, s, turnID, stampedAs(final, sandbox.CaptureRefused))
					mustClaim(t, s, turnID)
					next := run(turnID)
					next.LaunchNode = "node-a"
					mustLaunched(t, s, next)
					relaunched := mustGet(t, s, turnID)
					if relaunched.LaunchSeq != r.LaunchSeq+1 {
						t.Fatalf("the relaunch is launch %d, want %d", relaunched.LaunchSeq, r.LaunchSeq+1)
					}
					return saveAt(relaunched, 1, later)
				}},
			{"a place one past the next", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				return saveAt(runningRow(t, s, turnID), 2, now)
			}},
			{"a place one short of the next", false, func(t *testing.T, turnID string) sandbox.SessionCapture {
				r := runningRow(t, s, turnID)
				take := saveAt(r, 1, now)
				take.Interrupted = false
				mustStamp(t, s, turnID, take)
				mustStamp(t, s, turnID, stampedAs(take, sandbox.CaptureLanded))
				return saveAt(r, 1, later)
			}},
		}
		for i, row := range rows {
			turnID := "t-save-license-" + strconv.Itoa(i)
			save := row.set(t, turnID)
			before := rawVersion(t, runs, turnID)
			took, err := s.ReserveSave(t.Context(), turnID, save)
			if err != nil || took != row.licenses {
				t.Errorf("%s: a save reserved = %v, %v; want %v", row.name, took, err, row.licenses)
				continue
			}
			got := mustGet(t, s, turnID)
			if !row.licenses {
				if after := rawVersion(t, runs, turnID); after != before {
					t.Errorf("%s: a refused save wrote the row (version %d to %d)", row.name, before, after)
				}
				continue
			}
			if !sameTake(got.Capture, save, sandbox.CaptureBegun) || !sameTake(got.Saving, save, sandbox.CaptureBegun) {
				t.Errorf("%s: the row's newest capture is %+v and its save under way %+v; want the save in both",
					row.name, got.Capture, got.Saving)
			}
		}

		// A SAVE THAT IS NOT ONE is refused before anything is written.
		r := runningRow(t, s, "t-save-malformed")
		for name, edit := range map[string]func(*sandbox.SessionCapture){
			"final":                     func(c *sandbox.SessionCapture) { c.Final = true },
			"landed":                    func(c *sandbox.SessionCapture) { *c = stampedAs(*c, sandbox.CaptureLanded) },
			"not cut off":               func(c *sandbox.SessionCapture) { c.Interrupted = false },
			"holding a note":            func(c *sandbox.SessionCapture) { c.NoteCursor = 1 },
			"stating no save point":     func(c *sandbox.SessionCapture) { c.Through = nil },
			"past a bound":              func(c *sandbox.SessionCapture) { c.SessionID = strings.Repeat("s", 1<<20) },
			"naming a read standing by": func(c *sandbox.SessionCapture) { c.StandbyTaker, c.StandbyAt = finalTaker, later },
		} {
			save := saveAt(r, 1, now)
			edit(&save)
			before := rawVersion(t, runs, r.TurnID)
			if took, err := s.ReserveSave(ctx, r.TurnID, save); err == nil || took {
				t.Errorf("a save %s was reserved = %v, %v; want an error", name, took, err)
			}
			if after := rawVersion(t, runs, r.TurnID); after != before {
				t.Errorf("a save %s wrote the row (version %d to %d)", name, before, after)
			}
		}

		// THE SAME RESERVATION ASKED AGAIN is answered taken, writing nothing.
		save := saveAt(r, 1, now)
		mustReserve(t, s, r.TurnID, save)
		before := rawVersion(t, runs, r.TurnID)
		mustReserve(t, s, r.TurnID, save)
		if after := rawVersion(t, runs, r.TurnID); after != before {
			t.Errorf("the reservation asked again wrote the row (version %d to %d)", before, after)
		}
	})
}

// A FINAL TAKE AFTER A SAVE TAKES THE PLACE AFTER IT. A save reserved, then its
// job's completion claimed: the collection's final take, placed after the row's
// newest capture as the row now reads, holds the place after the save, and the
// save stays in sight beside it. The other way round (the claim, a stop's
// ending, or a loss first) the save is refused, and the final take holds the
// place it would have taken.
//
// Mutation: reserve a save whatever the run's status, and the claim first
// reserves it; keep the save in its own section alone and not as the newest
// capture, and the final take places itself at the save's own place.
func testAFinalTakeAfterASaveTakesThePlaceAfterIt(t *testing.T, s sandbox.PendingStore) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		now := time.Now()
		r := runningRow(t, s, "t-save-then-final")
		save := saveAt(r, 1, now)
		mustReserve(t, s, r.TurnID, save)
		claimed := mustClaim(t, s, r.TurnID)
		next := int64(1)
		if newest := claimed.Capture; newest != nil && newest.LaunchSeq == claimed.LaunchSeq {
			next = newest.CaptureSeq + 1
		}
		if next != save.CaptureSeq+1 {
			t.Fatalf("the final take's place, read off the claimed row, is %d; want the one after the save's, %d",
				next, save.CaptureSeq+1)
		}
		final := finalAt(r, next, now.Add(time.Second))
		mustStamp(t, s, r.TurnID, final)
		got := mustGet(t, s, r.TurnID)
		if !sameTake(got.Capture, final, sandbox.CaptureBegun) || !sameTake(got.Saving, save, sandbox.CaptureBegun) {
			t.Fatalf("the row's newest capture is %+v and its save under way %+v; want the final take after the "+
				"save, and the save in sight", got.Capture, got.Saving)
		}

		for name, first := range map[string]func(t *testing.T, turnID string){
			"the claim": func(t *testing.T, turnID string) { mustClaim(t, s, turnID) },
			"a stop's ending": func(t *testing.T, turnID string) {
				decide(t, s, turnID, everyJob(sandbox.Fence{}, sandbox.Active))
			},
			"a loss": func(t *testing.T, turnID string) {
				if ok, err := s.MarkLost(t.Context(), turnID, mustGet(t, s, turnID).LaunchID, sandbox.JobLoss{
					Cause: sandbox.LossEngineRestarted, Node: "node-a", At: now}); err != nil || !ok {
					t.Fatalf("MarkLost = %v, %v", ok, err)
				}
			},
		} {
			turnID := "t-final-first-" + strings.ReplaceAll(name, " ", "-")
			fresh := runningRow(t, s, turnID)
			first(t, turnID)
			if took, err := s.ReserveSave(ctx, turnID, saveAt(fresh, 1, now)); err != nil || took {
				t.Errorf("a save after %s = %v, %v; want it refused", name, took, err)
			}
			if got := mustGet(t, s, turnID); got.Capture != nil || got.Saving != nil {
				t.Errorf("a save refused after %s left newest %+v and save %+v on the row", name, got.Capture,
					got.Saving)
			}
		}
	})
}

// ONE SAVE OF A RUN IS IN FLIGHT ACROSS THE FLEET. Two nodes reserve a save of
// one run at one place at once: exactly one is taken. The other, at the next
// place, is refused while the first may still land, and taken once it has
// landed. And a save of an earlier launch still being kept refuses a save of
// the next one on any node.
//
// Mutation: drop the license's read of the save under way, and the next
// launch's save is taken beside an earlier one's; drop its read of the newest
// capture, and the loser's save is taken beside the winner's.
func testOneSaveOfARunIsInFlightAcrossTheFleet(t *testing.T, s, other sandbox.PendingStore) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		now := time.Now()
		r := runningRow(t, s, "t-save-fleet")
		mine, theirs := saveAt(r, 1, now), saveAt(r, 1, now.Add(time.Millisecond))
		theirs.Taker = finalTaker
		var (
			wg        sync.WaitGroup
			took      [2]bool
			errs      [2]error
			contested = []struct {
				store sandbox.PendingStore
				save  sandbox.SessionCapture
			}{{s, mine}, {other, theirs}}
		)
		for i, c := range contested {
			wg.Go(func() { took[i], errs[i] = c.store.ReserveSave(ctx, r.TurnID, c.save) })
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || took[0] == took[1] {
			t.Fatalf("two nodes reserved one place: %v, %v (%v, %v); want exactly one taken", took[0], took[1],
				errs[0], errs[1])
		}
		winner, loser := contested[0], contested[1]
		if took[1] {
			winner, loser = loser, winner
		}
		next := loser.save
		next.CaptureSeq, next.At = 2, now.Add(time.Second)
		if got, err := loser.store.ReserveSave(ctx, r.TurnID, next); err != nil || got {
			t.Fatalf("the loser's save at the next place = %v, %v; want it refused while the first may land",
				got, err)
		}
		mustStamp(t, winner.store, r.TurnID, stampedAs(winner.save, sandbox.CaptureLanded))
		if got, err := loser.store.ReserveSave(ctx, r.TurnID, next); err != nil || !got {
			t.Fatalf("the loser's save after the first landed = %v, %v; want it taken", got, err)
		}

		// AN EARLIER LAUNCH'S SAVE, still being kept behind its job's refused
		// final take, holds off the next launch's on another node.
		earlier := runningRow(t, s, "t-save-fleet-relaunch")
		mustReserve(t, s, earlier.TurnID, saveAt(earlier, 1, now))
		final := finalAt(earlier, 2, now)
		mustStamp(t, s, earlier.TurnID, final)
		mustStamp(t, s, earlier.TurnID, stampedAs(final, sandbox.CaptureRefused))
		mustClaim(t, s, earlier.TurnID)
		relaunch := run(earlier.TurnID)
		relaunch.LaunchNode = "node-a"
		mustLaunched(t, s, relaunch)
		relaunched := mustGet(t, s, earlier.TurnID)
		if got, err := other.ReserveSave(ctx, earlier.TurnID, saveAt(relaunched, 1, now.Add(time.Second))); err != nil ||
			got {
			t.Fatalf("the next launch's save beside an earlier launch's still being kept = %v, %v; want it "+
				"refused", got, err)
		}
	})
}

// A SAVE STAYS IN SIGHT UNTIL IT CAN NO LONGER BE THE TURN'S KEPT CAPTURE. Past
// a final take that overtook it, its own stamps move it on: unresolved stays,
// refused goes, landed behind a refused final goes and is kept; a final that
// lands lets it go. A stale save stays beside a stamp that does not take, and a
// stamp that takes decides on the newest capture as it FOUND it, letting go a
// save settled refused there and reconciling one left unresolved.
//
// Mutation: never let the save go; let it go on a stamp that did not take; keep
// nothing landing behind a refused final; decide on the replaced newest place;
// or skip the reconcile.
func testASaveStaysInSightUntilItCanNoLongerBeTheTurnsKeptCapture(t *testing.T, s sandbox.PendingStore,
	runs coord.SandboxRuns,
) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		overtaken := func(t *testing.T, turnID string) (sandbox.SessionCapture, sandbox.SessionCapture) {
			r := runningRow(t, s, turnID)
			save, final := saveAt(r, 1, now), finalAt(r, 2, now.Add(time.Second))
			mustReserve(t, s, turnID, save)
			mustStamp(t, s, turnID, final)
			return save, final
		}

		// UNRESOLVED, IT STAYS; then landed behind the refused final, it goes
		// and is kept.
		save, final := overtaken(t, "t-save-sight-landed")
		mustStamp(t, s, "t-save-sight-landed", stampedAs(save, sandbox.CaptureUnresolved))
		if got := mustGet(t, s, "t-save-sight-landed"); !sameTake(got.Saving, save, sandbox.CaptureUnresolved) {
			t.Fatalf("the save's record left unanswered leaves %+v in sight; want the save, unresolved", got.Saving)
		}
		mustStamp(t, s, "t-save-sight-landed", stampedAs(final, sandbox.CaptureRefused))
		mustStamp(t, s, "t-save-sight-landed", stampedAs(save, sandbox.CaptureLanded))
		if got := mustGet(t, s, "t-save-sight-landed"); got.Saving != nil ||
			!sameTake(got.Kept, save, sandbox.CaptureLanded) {
			t.Errorf("the save landed behind a refused final leaves %+v in sight and keeps %+v; want it let go "+
				"and kept", got.Saving, got.Kept)
		}

		// REFUSED, IT GOES.
		save, _ = overtaken(t, "t-save-sight-refused")
		mustStamp(t, s, "t-save-sight-refused", stampedAs(save, sandbox.CaptureRefused))
		if got := mustGet(t, s, "t-save-sight-refused"); got.Saving != nil {
			t.Errorf("a refused save is still in sight: %+v", got.Saving)
		}

		// A FINAL THAT LANDS LETS IT GO.
		_, final = overtaken(t, "t-save-sight-final")
		mustStamp(t, s, "t-save-sight-final", stampedAs(final, sandbox.CaptureLanded))
		if got := mustGet(t, s, "t-save-sight-final"); got.Saving != nil {
			t.Errorf("a save behind a kept final take is still in sight: %+v", got.Saving)
		}

		// A STALE SAVE BESIDE A STAMP THAT DOES NOT TAKE stays, and the stamp
		// is answered not taken: a build without the let-go landed the final.
		save, final = overtaken(t, "t-save-sight-stale")
		writtenAsOlderBuild(t, runs, "t-save-sight-stale", func(row *sandbox.PendingRun) {
			landed := stampedAs(final, sandbox.CaptureLanded)
			kept := landed
			row.Capture, row.Kept = &landed, &kept
		})
		before := rawVersion(t, runs, "t-save-sight-stale")
		stray := stampedAs(save, sandbox.CaptureRefused)
		stray.At = now.Add(-time.Minute)
		if took, err := s.StampCapture(t.Context(), "t-save-sight-stale", stray); err != nil || took {
			t.Errorf("a stamp that does not take = %v, %v; want it answered not taken", took, err)
		}
		if after := rawVersion(t, runs, "t-save-sight-stale"); after != before {
			t.Errorf("a stamp that does not take wrote the row (version %d to %d)", before, after)
		}

		// A SAVE SETTLED REFUSED IN THE NEWEST PLACE by a build without the
		// section (its taker gone, the log naming no place) is let go by the
		// next take, decided on the row as found.
		r := runningRow(t, s, "t-save-sight-settled")
		save = saveAt(r, 1, now)
		mustReserve(t, s, r.TurnID, save)
		writtenAsOlderBuild(t, runs, r.TurnID, func(row *sandbox.PendingRun) {
			settled := stampedAs(save, sandbox.CaptureRefused)
			row.Capture = &settled
		})
		mustStamp(t, s, r.TurnID, finalAt(r, 2, now.Add(time.Second)))
		if got := mustGet(t, s, r.TurnID); got.Saving != nil {
			t.Errorf("a save that build settled refused is in sight past the final take: %+v", got.Saving)
		}

		// ONE IT LEFT UNRESOLVED THERE is reconciled into the save's section.
		r = runningRow(t, s, "t-save-sight-asked")
		save = saveAt(r, 1, now)
		mustReserve(t, s, r.TurnID, save)
		writtenAsOlderBuild(t, runs, r.TurnID, func(row *sandbox.PendingRun) {
			unresolved := stampedAs(save, sandbox.CaptureUnresolved)
			row.Capture = &unresolved
		})
		mustStamp(t, s, r.TurnID, finalAt(r, 2, now.Add(time.Second)))
		if got := mustGet(t, s, r.TurnID); !sameTake(got.Saving, save, sandbox.CaptureUnresolved) {
			t.Errorf("a save that build left unresolved reads %+v past the final take; want it unresolved",
				got.Saving)
		}
	})
}

// A SAVE IS SETTLED ONLY OVER ITS BEGUN STAMP. A save a final take overtook,
// still begun, takes a settle written over it in its own section, refused or
// landed; one its taker moved on since, or a settle of another take at its
// place, is not written.
//
// Mutation: hold a settle to the newest capture alone, and a save overtaken is
// never settled.
func testASaveIsSettledOnlyOverItsBegunStamp(t *testing.T, s sandbox.PendingStore) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		now := time.Now()
		overtaken := func(turnID string) sandbox.SessionCapture {
			r := runningRow(t, s, turnID)
			save := saveAt(r, 1, now)
			mustReserve(t, s, turnID, save)
			mustStamp(t, s, turnID, finalAt(r, 2, now.Add(time.Second)))
			return save
		}
		refused := overtaken("t-save-settle-refused")
		if took, err := s.SettleBegun(ctx, "t-save-settle-refused", stampedAs(refused, sandbox.CaptureRefused)); err != nil ||
			!took {
			t.Errorf("a settle over a save overtaken, still begun = %v, %v; want it written", took, err)
		}
		if got := mustGet(t, s, "t-save-settle-refused"); got.Saving != nil {
			t.Errorf("a save settled refused is still in sight: %+v", got.Saving)
		}

		landed := overtaken("t-save-settle-landed")
		if took, err := s.SettleBegun(ctx, "t-save-settle-landed", stampedAs(landed, sandbox.CaptureLanded)); err != nil ||
			!took {
			t.Errorf("a settle landed over a save overtaken = %v, %v; want it written", took, err)
		}
		if got := mustGet(t, s, "t-save-settle-landed"); !sameTake(got.Kept, landed, sandbox.CaptureLanded) {
			t.Errorf("a save settled landed leaves %+v kept; want the save", got.Kept)
		}

		moved := overtaken("t-save-settle-moved")
		mustStamp(t, s, "t-save-settle-moved", stampedAs(moved, sandbox.CaptureUnresolved))
		if took, err := s.SettleBegun(ctx, "t-save-settle-moved", stampedAs(moved, sandbox.CaptureLanded)); err != nil ||
			took {
			t.Errorf("a settle over a save its taker moved on = %v, %v; want nothing written", took, err)
		}
		another := stampedAs(moved, sandbox.CaptureRefused)
		another.At = now.Add(-time.Minute)
		if took, err := s.SettleBegun(ctx, "t-save-settle-moved", another); err != nil || took {
			t.Errorf("a settle of another take at the save's place = %v, %v; want nothing written", took, err)
		}
		if got := mustGet(t, s, "t-save-settle-moved"); !sameTake(got.Saving, moved, sandbox.CaptureUnresolved) {
			t.Errorf("the save its taker moved on reads %+v; want it unresolved, as its taker stamped it",
				got.Saving)
		}
	})
}

// A STAMP THE ROW HOLDS IN ANY SECTION IS ANSWERED TAKEN. A save's outcome
// asked again after a final take moved the newest place on is held by the
// save's own section where it is still in sight, and by the kept capture where
// it landed; a refused one, let go, is held by none and answered not taken.
//
// Mutation: hold a stamp to the newest capture alone, and a save's outcome
// asked again is answered not taken.
func testAStampTheRowHoldsInAnySectionIsAnsweredTaken(t *testing.T, s sandbox.PendingStore) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		now := time.Now()
		r := runningRow(t, s, "t-save-held")
		save := saveAt(r, 1, now)
		mustReserve(t, s, r.TurnID, save)
		mustStamp(t, s, r.TurnID, finalAt(r, 2, now.Add(time.Second)))
		unresolved := stampedAs(save, sandbox.CaptureUnresolved)
		mustStamp(t, s, r.TurnID, unresolved)
		if took, err := s.StampCapture(ctx, r.TurnID, unresolved); err != nil || !took {
			t.Errorf("the save's unresolved stamp asked again = %v, %v; want it held by its section", took, err)
		}
		landed := stampedAs(save, sandbox.CaptureLanded)
		landed.Object = unresolved.Object
		mustStamp(t, s, r.TurnID, landed)
		if took, err := s.StampCapture(ctx, r.TurnID, landed); err != nil || !took {
			t.Errorf("the save's landed stamp asked again = %v, %v; want it held by the kept capture", took, err)
		}

		r = runningRow(t, s, "t-save-held-refused")
		save = saveAt(r, 1, now)
		mustReserve(t, s, r.TurnID, save)
		mustStamp(t, s, r.TurnID, finalAt(r, 2, now.Add(time.Second)))
		refused := stampedAs(save, sandbox.CaptureRefused)
		mustStamp(t, s, r.TurnID, refused)
		if took, err := s.StampCapture(ctx, r.TurnID, refused); err != nil || took {
			t.Errorf("the save's refused stamp asked again = %v, %v; want it held by no section", took, err)
		}
	})
}
