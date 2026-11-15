package sandbox

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// claimBeforeReserve claims a run's completion the moment a save asks to
// reserve its place, as a completion processed on the seat's control topic can
// land between a save's read of the row and its reservation.
type claimBeforeReserve struct {
	PendingStore
	t *testing.T
}

func (s *claimBeforeReserve) ReserveSave(ctx context.Context, turnID string, save SessionCapture) (bool, error) {
	run, _, err := s.PendingStore.Get(ctx, turnID)
	if err != nil {
		s.t.Fatalf("Get: %v", err)
	}
	if _, won, err := s.PendingStore.ClaimForResume(ctx, turnID, CompletionTail(run.LaunchID),
		rigLease); err != nil || !won {
		s.t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	return s.PendingStore.ReserveSave(ctx, turnID, save)
}

// A FINAL TAKE NEVER LOSES ITS PLACE TO A SAVE. A save placed off its read of
// the row asks to reserve the place after the row's newest capture just as the
// job's completion is claimed: the store decides the reservation on the row as
// the claim left it and refuses it, and the collection's final take holds the
// place the save was placed at, with no save standing beside it.
//
// Mutation: license a save whatever the run's status, and the save holds the
// place, the final take the one after it, and a save stands in sight beside
// the job's end.
func TestAFinalTakeNeverLosesItsPlaceToASave(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		running := rig.launchKept(t, "t1")
		// THE SAVE'S ONE READ OF THE ROW, before its pack: its place is the
		// next one there.
		save := SessionCapture{LaunchSeq: running.LaunchSeq, CaptureSeq: nextCaptureSeq(running),
			Launch: running.LaunchID, State: CaptureBegun, Interrupted: true, Through: new(0), At: time.Now(),
			Taker: rigLease.Owner, TakerEpoch: rigLease.Epoch, Harness: "claude-code", SessionID: "s-1"}
		racing := &claimBeforeReserve{PendingStore: rig.pending, t: t}
		if took, err := racing.ReserveSave(t.Context(), "t1", save); err != nil || took {
			t.Fatalf("a save reserved beside its job's claim = %v, %v; want it refused", took, err)
		}
		claimed := rig.get("t1")
		if claimed.Status != StatusResumed || claimed.Saving != nil {
			t.Fatalf("the row is %s with save %+v, want the claim and no save", claimed.Status, claimed.Saving)
		}
		spooled, kept := rig.coordinator.takeCapture(t.Context(), claimed, rig.provider.Box(claimed.SandboxID),
			rig.runner, readCapture(BoxReadCollect, Result{}, true))
		if spooled == nil {
			t.Fatalf("the collection's take spooled nothing: %+v", kept)
		}
		rig.coordinator.keepLater(t.Context(), spooled)
		rig.awaitKeeps()
		got := rig.get("t1")
		if final := got.Capture; final == nil || !final.Final || final.LaunchSeq != save.LaunchSeq ||
			final.CaptureSeq != save.CaptureSeq {
			t.Errorf("the row's newest capture is %+v, want the final take at the save's place (%d, %d)", final,
				save.LaunchSeq, save.CaptureSeq)
		}
		if got.Saving != nil {
			t.Errorf("a save stands beside the job's final take: %+v", got.Saving)
		}
	})
}

// AN OLDER BUILD'S WRITE CARRIES A SAVE UNDER WAY. A fleet mid-upgrade rewrites
// the run's row on a build that does not know the save's section, and this
// build's waits and stamps read the save there: so the save rides the row under
// its own key, and a build that does not declare that key keeps it verbatim and
// writes it back (wirekeys.go), whatever it wrote of the rest.
//
// Mutation: leave the field off the wire, and no row holds a key of it.
func TestAnOlderBuildsWriteCarriesASaveUnderWay(t *testing.T) {
	t.Parallel()
	save := SessionCapture{LaunchSeq: 1, CaptureSeq: 2, Launch: "job-1", State: CaptureBegun, Interrupted: true,
		Through: new(7), At: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC), Taker: "node-a:1", TakerEpoch: 3}
	raw, err := json.Marshal(PendingRun{TurnID: "t1", Status: StatusRunning, Saving: &save})
	if err != nil {
		t.Fatal(err)
	}
	// THE OLDER BUILD'S ROW: every key this one declares but the save's.
	declared := pendingRunKeys()
	if !declared["session_saving"] {
		t.Fatalf("the run's row declares no key session_saving: %v", declared)
	}
	older := maps.Clone(declared)
	delete(older, "session_saving")
	var read PendingRun
	extra, err := unmarshalCarrying(raw, &read, older)
	if err != nil {
		t.Fatal(err)
	}
	// ITS STRUCT HAS NO SUCH FIELD, so what it writes is its own change and
	// what it carried.
	read.Saving, read.Status = nil, StatusResumed
	written, err := marshalCarrying(read, older, extra)
	if err != nil {
		t.Fatal(err)
	}
	var back PendingRun
	if _, err := unmarshalCarrying(written, &back, declared); err != nil {
		t.Fatal(err)
	}
	if got := back.Saving; got == nil || !got.sameTake(save) || got.State != save.State || got.Through == nil ||
		*got.Through != *save.Through || back.Status != StatusResumed {
		t.Errorf("after the older build's write the row reads save %+v and status %s, want the save beside "+
			"its own write: %s", got, back.Status, written)
	}
}

// savingRule is every function the one rule of a save under way lets read the
// field ([PendingRun.Saving]): the rule itself ([PendingRun.savingTake]), the
// stamp that keeps it ([stampOnto], [savingAfter]), and what answers a stamp
// or a settle by the take it names ([holdsStamp], [holdsSave], [stillBegun]).
// [CoordStore.ReserveSave] writes it and reads nothing of it.
var savingRule = map[string]bool{"savingTake": true, "stampOnto": true, "savingAfter": true, "stillBegun": true,
	"holdsStamp": true, "holdsSave": true}

// EVERY READER OF A SAVE UNDER WAY READS IT THROUGH ONE RULE. A row may carry a
// stale save, which a build without its section wrote back unchanged, so a
// reader that read the field as a fact would wait on, or refuse beside, a save
// that can change nothing ([PendingRun.savingTake]). So a walk of the module's
// sources, tests and the test packages that certify the store aside, finds
// every selector of the field outside the rule and the stamps that keep it,
// and outside the reservation's write: only package sandbox declares it, and a
// reader anywhere is reported, by its place. The walk must
// have read the rule and the write, and a package beyond this one, or it read
// nothing.
//
// Mutation: read the field raw in the turn's end's wait (captureInFlight), and
// the walk reports it.
func TestEveryReaderOfASaveUnderWayReadsItThroughOneRule(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	seen := map[string]bool{}
	beyond := false
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		// A TEST'S OWN PACKAGE (sandboxtest, the contract suite) reads the
		// raw field on purpose: what the store wrote is its subject.
		if strings.HasSuffix(f.Name.Name, "test") {
			return nil
		}
		if rel, relErr := filepath.Rel(root, path); relErr == nil && filepath.Dir(rel) == filepath.Join("cmd", "crewlet") {
			beyond = true
		}
		for _, decl := range f.Decls {
			name := ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				name = fn.Name.Name
			}
			written := map[ast.Expr]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				if assign, ok := n.(*ast.AssignStmt); ok {
					for _, lhs := range assign.Lhs {
						written[lhs] = true
					}
				}
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Saving" {
					return true
				}
				switch {
				case savingRule[name]:
					seen[name] = true
				case name == "ReserveSave" && written[sel]:
					seen[name] = true
				default:
					where := "a declaration outside any function"
					if name != "" {
						where = name
					}
					t.Errorf("%s: %s reads a run's save under way raw (.Saving); read it through "+
						"PendingRun.savingTake or PendingRun.saving", fset.Position(sel.Pos()), where)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !beyond || !seen["savingTake"] || !seen["stampOnto"] || !seen["ReserveSave"] {
		t.Errorf("the walk read the rule %v, the stamp %v, the reservation's write %v and cmd/crewlet %v; "+
			"want all four, or it read nothing of the module", seen["savingTake"], seen["stampOnto"],
			seen["ReserveSave"], beyond)
	}
}
