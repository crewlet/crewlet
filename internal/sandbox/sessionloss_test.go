package sandbox

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// NO SESSION IS SAID NOT KEPT OR NOT CONTINUED AROUND ITS SAYER: every
// `executor_session_not_kept` and `executor_session_not_continued` line in the
// tree is said by one function per package, each taking the loss in the one
// vocabulary, so no refusal is said, and none stamped, without one.
//
// Mutation: log either line directly anywhere in the tree, and this goes red.
func TestNoNotContinuedLineIsSaidAroundTheSayer(t *testing.T) {
	t.Parallel()
	lines := map[string]bool{"executor_session_not_kept": true, "executor_session_not_continued": true}
	sayers := map[string]bool{
		"internal/sandbox.sayNotKept": true, "internal/sandbox.sayNotContinued": true,
		"internal/engine.sayNotContinued": true,
	}
	root := sourcetree.Root(t)
	said := 0
	for _, top := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			dir, _ := filepath.Rel(root, filepath.Dir(path))
			for _, decl := range file.Decls {
				fn, _ := decl.(*ast.FuncDecl)
				ast.Inspect(decl, func(n ast.Node) bool {
					lit, ok := n.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					if value, err := strconv.Unquote(lit.Value); err != nil || !lines[value] {
						return true
					}
					said++
					if fn == nil || !sayers[filepath.ToSlash(dir)+"."+fn.Name.Name] {
						t.Errorf("%s says %s around its package's sayer", path, lit.Value)
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if said != len(sayers) {
		t.Errorf("the tree says the two lines %d times, and its %d sayers once each", said, len(sayers))
	}
}

// EVERY LOSS THIS BUILD SAYS IS ONE IT NAMES: the executor log's writer and
// its reader hold a loss to its bound alone, so a newer build's is relayed
// (executorkeep's package doc, "A session not continued"), and the one place a
// loss this build does not name could be born is a site that spells one. So no
// code outside executorkeep converts to executorkeep.Loss or sets a field named
// Loss to a word of its own: every loss it stamps or decides is a constant
// executorkeep lists (TestEveryDeclaredLossIsListed holds that side).
//
// Mutation: stamp a capture refused as Loss: "a_newer_loss", or decide a
// verdict on executorkeep.Loss(why), and this goes red.
func TestEveryLossThisBuildSaysIsNamed(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	spelled := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		return ok && lit.Kind == token.STRING && lit.Value != `""` && lit.Value != "``"
	}
	for _, top := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			if dir, _ := filepath.Rel(root, filepath.Dir(path)); filepath.ToSlash(dir) == "internal/executorkeep" {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				var at token.Pos
				switch n := n.(type) {
				case *ast.CallExpr:
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Loss" {
						if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "executorkeep" {
							at = n.Pos()
						}
					}
				case *ast.KeyValueExpr:
					if key, ok := n.Key.(*ast.Ident); ok && key.Name == "Loss" && spelled(n.Value) {
						at = n.Pos()
					}
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Loss" && i < len(n.Rhs) &&
							spelled(n.Rhs[i]) {
							at = n.Pos()
						}
					}
				}
				if at.IsValid() {
					t.Errorf("%s spells a loss of its own; say one of executorkeep's constants", fset.Position(at))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// EVERY REFUSAL OF A CAPTURE IS STAMPED WITH ITS LOSS, and the turn's end
// carries it: a take with no spool room, one its box could not hand over, one
// past the bound, an upload the store refused and a record the log never
// answered each stamp the run's row with the loss in the one vocabulary, and
// the end's unkept capture names the same, so the turn's next run says why.
// A take with no room once stamped nothing at all: the end named no unkept
// capture, never said the turn went on, and the next turn read the turn's
// older capture as its whole work.
//
// Mutation: stamp any of them without its loss, or drop the spool room's stamp,
// and its case goes red.
func TestEveryRefusalOfACaptureIsStampedWithItsLoss(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		arm  func(*coordRig, *sessionWrites, *sessionKeep) func()
		want executorkeep.Loss
	}{
		"no spool room": {want: executorkeep.LossNoSpoolRoom,
			arm: func(rig *coordRig, _ *sessionWrites, _ *sessionKeep) func() {
				return rig.coordinator.trySpoolRoom(spoolRoomBytes).release
			}},
		"a take its box refused": {want: executorkeep.LossKeepFailed,
			arm: func(rig *coordRig, _ *sessionWrites, _ *sessionKeep) func() {
				rig.runner.Session.TakeErr = errors.New("the box's session could not be read")
				return func() {}
			}},
		"a session past the bound": {want: executorkeep.LossTooLarge,
			arm: func(rig *coordRig, _ *sessionWrites, _ *sessionKeep) func() {
				rig.runner.Session.TakeErr = fmt.Errorf("executor: the session's archive is past its bound: %w",
					executorkeep.ErrSessionTooLarge)
				return func() {}
			}},
		"an upload the store refused": {want: executorkeep.LossStoreRefused,
			arm: func(_ *coordRig, _ *sessionWrites, keep *sessionKeep) func() {
				keep.putErr = objstore.ErrFull
				return func() {}
			}},
		"a record the log never answered": {want: executorkeep.LossLogRefused,
			arm: func(_ *coordRig, writes *sessionWrites, _ *sessionKeep) func() {
				writes.refuse = errors.New("no data node answered")
				return func() {}
			}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, keep := keepingRig(t)
				run := rig.launchKept(t, "t1")
				release := c.arm(rig, writes, keep)
				defer release()
				spooled, _ := finalTake(t, rig, run, rig.runner)
				if spooled != nil {
					rig.coordinator.keepLater(t.Context(), spooled)
					rig.awaitKeeps()
				}
				row := rig.get("t1")
				if row.Capture == nil || row.Capture.State == CaptureLanded || row.Capture.Loss != c.want {
					t.Fatalf("the run's row stamps %+v, want the capture refused for %s", row.Capture, c.want)
				}
				if unkept := unkeptOf(row); unkept == nil || unkept.Loss != c.want {
					t.Errorf("the turn's end names %+v, want its unkept capture lost for %s", unkept, c.want)
				}
			})
		})
	}
}

// carrySpy is a [SessionMeter] that keeps what it was told.
type carrySpy struct {
	mu   sync.Mutex
	told []*executorkeep.NotContinued
}

func (s *carrySpy) SessionCarried(verdict *executorkeep.NotContinued) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.told = append(s.told, verdict)
}

func (s *carrySpy) verdicts() []*executorkeep.NotContinued {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.told)
}

// A RUN WHOSE BOX FELL BACK TO A FRESH SESSION IS A RESTORE THAT FAILED, said
// on its launch's record with its script's reason, and counted ONCE however
// often its collection is retried: the record's own once-per-launch guard is
// the meter's, so a retried tail never counts a second loss.
//
// Mutation: drop the fell-back verdict, or meter outside the published guard,
// and a case goes red.
func TestAFellBackRunIsRestoreFailedAndCountedOnce(t *testing.T) {
	t.Parallel()
	spy := &carrySpy{}
	rig := newCoordRigWith(t, func(o *CoordinatorOptions) { o.Carries = spy })
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done",
		FellBack: "the box's harness could not import the session"})
	rig.resumer.failWith(errors.New("the node lost the seat mid-resume"))
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}
	rig.resumer.failWith(nil)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	recs := rig.collected()
	if len(recs) != 1 {
		t.Fatalf("published %d records for one launch, want 1", len(recs))
	}
	said := recs[0].SessionNotContinued
	if said == nil || said.Loss != executorkeep.LossRestoreFailed || said.Instead != executorkeep.InsteadFresh ||
		!strings.Contains(said.Detail, "could not import") {
		t.Errorf("the record says %+v, want a restore that failed, the run fresh, its script's reason", said)
	}
	if told := spy.verdicts(); len(told) != 1 || told[0] == nil || told[0].Loss != executorkeep.LossRestoreFailed {
		t.Errorf("the meter was told %v, want the one restore that failed, once", told)
	}
}

// A TURN'S CONVERSATION IS TOLD ITS FIRST VERDICT, ONCE ITS END IS ON THE LOG:
// what the conversation's session lost going into the turn, never each launch's.
// A turn whose runs lost nothing tells it nothing.
//
// Mutation: tell it the turn's newest verdict, tell it before the end is
// recorded, or tell a turn that lost nothing, and a case goes red.
func TestATurnsConversationIsToldWhatItsRunsLost(t *testing.T) {
	t.Parallel()
	first := *executorkeep.Lost(executorkeep.LossHarnessChanged, executorkeep.InsteadFresh, "a codex member")
	first.LostTurn = "t0"
	newest := *executorkeep.Lost(executorkeep.LossCaptureLate, executorkeep.InsteadFresh, "")
	for name, tc := range map[string]struct {
		verdicts []executorkeep.NotContinued
		want     *executorkeep.NotContinued
	}{
		"its runs lost a session": {[]executorkeep.NotContinued{first, newest}, &first},
		"its runs lost none":      {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				teller := &tellings{}
				rig, writes, _ := keepingRigWith(t, func(o *CoordinatorOptions) { o.Conversations = teller })
				teller.ended = writes.endedRecords
				rig.launchKept(t, "t1")
				ctx := t.Context()
				if _, err := rig.coordinator.OweEnd(ctx, "t1",
					executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}); err != nil {
					t.Fatalf("OweEnd: %v", err)
				}
				run := rig.get("t1")
				run.NotContinued = tc.verdicts
				if _, err := rig.coordinator.publishEnd(ctx, run); err != nil {
					t.Fatalf("publishEnd: %v", err)
				}
				told := teller.said()
				switch {
				case tc.want == nil && len(told) != 0:
					t.Fatalf("a turn whose runs lost nothing told its conversation %+v", told)
				case tc.want == nil:
				case len(told) != 1 || told[0].turn != "t1" || told[0].verdict != *tc.want || !told[0].afterEnd:
					t.Fatalf("the conversation was told %+v, want turn t1's first verdict %+v once, after "+
						"its end was recorded", told, *tc.want)
				}
			})
		})
	}
}

// A TURN'S CONVERSATION IS TOLD WITHIN ITS BUDGET: the tell is a chat write on
// the turn's ending, which clears the end from the run's row only after it, so
// a chat that never answers holds the ending no longer than
// [conversationTellBudget].
//
// Mutation: tell it under the ending's own context, and the tell has no bound.
func TestATurnsConversationIsToldWithinItsBudget(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		teller := &tellings{}
		rig, writes, _ := keepingRigWith(t, func(o *CoordinatorOptions) { o.Conversations = teller })
		teller.ended = writes.endedRecords
		rig.launchKept(t, "t1")
		ctx := t.Context()
		if _, err := rig.coordinator.OweEnd(ctx, "t1",
			executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}); err != nil {
			t.Fatalf("OweEnd: %v", err)
		}
		run := rig.get("t1")
		run.NotContinued = []executorkeep.NotContinued{
			*executorkeep.Lost(executorkeep.LossCaptureLate, executorkeep.InsteadFresh, "")}
		if _, err := rig.coordinator.publishEnd(ctx, run); err != nil {
			t.Fatalf("publishEnd: %v", err)
		}
		told := teller.said()
		if len(told) != 1 || told[0].deadline.IsZero() ||
			!told[0].deadline.Equal(told[0].at.Add(conversationTellBudget)) {
			t.Fatalf("the conversation was told %+v, want once, within %v of the tell",
				told, conversationTellBudget)
		}
	})
}

// tellings is a [ConversationTeller] that keeps what it was told, and whether
// the turn's end was on the log by then.
type tellings struct {
	ended func() []EndRecord
	mu    sync.Mutex
	told  []told
}

type told struct {
	turn     string
	verdict  executorkeep.NotContinued
	afterEnd bool
	// at is when it was told, and deadline the bound it was told within, zero
	// for none.
	at, deadline time.Time
}

func (t *tellings) NotContinued(ctx context.Context, run PendingRun, verdict executorkeep.NotContinued) {
	t.mu.Lock()
	defer t.mu.Unlock()
	deadline, _ := ctx.Deadline()
	t.told = append(t.told, told{turn: run.TurnID, verdict: verdict, afterEnd: len(t.ended()) > 0,
		at: time.Now(), deadline: deadline})
}

func (t *tellings) said() []told {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.told)
}
