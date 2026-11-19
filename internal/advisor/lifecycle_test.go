package advisor_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ONE CONDITION IS ONE KEY: every finding of an answer keys on its own, so the
// store holds one live finding per condition. Mutation: drop the focus a
// config warning or an item finding sets, and two of them share a key.
func TestEveryFindingOfAnAnswerHasAKeyOfItsOwn(t *testing.T) {
	t.Parallel()
	for name, a := range map[string]advisor.Answer{"evidence": richAnswer(t), "setup": read(liveShape(t), "r1")} {
		seen := map[string]bool{}
		for _, f := range a.Findings {
			key := advisor.Key(f)
			if seen[key] {
				t.Errorf("%s: two findings key on %s", name, key)
			}
			seen[key] = true
			if len(key) > advisor.MaxKeyBytes {
				t.Errorf("%s: key %s is %d bytes, over %d", name, key, len(key), advisor.MaxKeyBytes)
			}
		}
	}
}

// AN ITEM IS ITS FINDING'S FOCUS: two items one agent holds that work waits on
// are two findings about one agent, each keyed by its item.
func TestTwoItemsOneAgentHoldsAreTwoFindings(t *testing.T) {
	t.Parallel()
	ev := advisor.Evidence{Until: monday, Work: worklet.WorkPatterns{Open: worklet.OpenWork{
		Blockers: []worklet.WaitedOn{{Key: "ENG-1", Holder: "dev", Blocks: 5}, {Key: "ENG-2", Holder: "dev", Blocks: 6}}}}}
	found := stalls(t, stallShape, ev, advisor.ClassWorkBottleneck)
	if len(found) != 2 || advisor.Key(found[0]) == advisor.Key(found[1]) {
		t.Fatalf("want two findings with two keys, got %d: %v", len(found), keysOf(found))
	}
	if want := "work.bottleneck:seat:dev#item:ENG-1"; advisor.Key(found[0]) != want && advisor.Key(found[1]) != want {
		t.Errorf("keys %v, want one of them %s", keysOf(found), want)
	}
}

func keysOf(fs []advisor.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, advisor.Key(f))
	}
	return out
}

// EVERY FINDING IS CONCLUDED IN A SCOPE ITS GROUP RUNS, or a run of its group
// would never resolve or confirm it.
func TestEveryFindingIsConcludedInAScopeItsGroupRuns(t *testing.T) {
	t.Parallel()
	for _, f := range append(richAnswer(t).Findings, read(liveShape(t), "r1").Findings...) {
		if scope := advisor.ScopeOf(f); !slices.Contains(advisor.ScopesOf(advisor.GroupOf(f)), scope) {
			t.Errorf("%s concludes in %s, which group %s does not run", f.Class, scope, advisor.GroupOf(f))
		}
	}
}

// at is the instant every reconcile test runs at.
var at = monday

// roundCap is a finding of an evidence class about one seat.
func roundCap(seat string, runs int64) advisor.Finding {
	return advisor.Finding{Class: advisor.ClassRoundCap, Severity: advisor.SeverityCosts, Actor: advisor.ActorOwner,
		Source: advisor.SourceEvidence, Area: advisor.AreaSetup, Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: seat},
		Evidence: []advisor.Fact{{Key: "runs", Value: runs}},
		Fix:      advisor.Fix{Kind: advisor.FixOpenSetting, Setting: "roles", Options: []advisor.Option{}, Sentence: "Raise its cap."}}
}

func observed(f advisor.Finding) advisor.Observation {
	return advisor.Observe(f, advisor.Span{From: at.Add(-advisor.Window), To: at}, "r1")
}

func storedAs(t *testing.T, o advisor.Observation, state advisor.State) advisor.Stored {
	t.Helper()
	body, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return advisor.Stored{ID: "e-" + o.Key(), Version: 7, State: state, Revision: 1, Observation: o, Body: body}
}

func evidenceRun() advisor.Reading {
	return advisor.Reading{At: at, Groups: []advisor.Group{advisor.GroupEvidence}}
}

func steps(ts []advisor.Transition) []string {
	out := []string{}
	for _, tr := range ts {
		s := string(tr.Step)
		if tr.Step == advisor.StepClose {
			s += ":" + string(tr.State) + ":" + string(tr.Reason)
		}
		out = append(out, s)
	}
	return out
}

func wantSteps(t *testing.T, got []advisor.Transition, want ...string) {
	t.Helper()
	if !slices.Equal(steps(got), append([]string{}, want...)) {
		t.Errorf("steps %v, want %v", steps(got), want)
	}
}

// A CONDITION THAT ENDED IS RESOLVED BY THE NEXT RUN OF ITS CLASS, without
// anybody pressing anything; one its run could not read is left as it was.
func TestAFindingWhoseConditionEndedIsResolvedOnTheNextRun(t *testing.T) {
	t.Parallel()
	s := storedAs(t, observed(roundCap("dev", 3)), advisor.StateOpen)
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, nil, evidenceRun()), "close:resolved:condition_cleared")

	unread := evidenceRun()
	unread.Unread = []string{advisor.UnreadRuns}
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, nil, unread))
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, nil, advisor.Reading{At: at, Groups: []advisor.Group{advisor.GroupSetup}}))
}

// A VERDICT OR A FIX THAT MOVED IS A NEW REVISION; numbers that moved under the
// same verdict are refreshed in place; the same observation records nothing.
func TestAFindingWhoseVerdictMovedIsRevised(t *testing.T) {
	t.Parallel()
	s := storedAs(t, observed(roundCap("dev", 3)), advisor.StateOpen)

	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, []advisor.Observation{observed(roundCap("dev", 3))}, evidenceRun()))

	more := advisor.Reconcile([]advisor.Stored{s}, []advisor.Observation{observed(roundCap("dev", 5))}, evidenceRun())
	wantSteps(t, more, "refresh")
	if len(more) == 1 && (len(more[0].Moved) != 1 || more[0].Moved[0].Was != int64(3) || more[0].Moved[0].Now != int64(5)) {
		t.Errorf("moved %+v, want runs 3 -> 5", more[0].Moved)
	}

	worse := roundCap("dev", 3)
	worse.Severity = advisor.SeverityBlocks
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, []advisor.Observation{observed(worse)}, evidenceRun()), "revise")

	refixed := roundCap("dev", 3)
	refixed.Fix.Setting = "providers.sandbox"
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, []advisor.Observation{observed(refixed)}, evidenceRun()), "revise")

	reworded := roundCap("dev", 3)
	reworded.Fix.Sentence = "Raise its cap from 30 rounds."
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, []advisor.Observation{observed(reworded)}, evidenceRun()), "refresh")
}

// ONE LIVE FINDING PER KEY: a condition observed twice in one run opens once,
// and one already open is not opened again; one that closed and returned is a
// new episode.
func TestOneOpenFindingPerClassAndSubject(t *testing.T) {
	t.Parallel()
	o := observed(roundCap("dev", 3))
	wantSteps(t, advisor.Reconcile(nil, []advisor.Observation{o, o}, evidenceRun()), "open")
	wantSteps(t, advisor.Reconcile([]advisor.Stored{storedAs(t, o, advisor.StateOpen)}, []advisor.Observation{o}, evidenceRun()))
	wantSteps(t, advisor.Reconcile([]advisor.Stored{storedAs(t, o, advisor.StateResolved)}, []advisor.Observation{o}, evidenceRun()), "open")
}

// AN APPLIED FIX IS CHECKED FOR ITS EFFECT: a condition gone is fixed by it; one
// still there at its effect horizon returns to open as ineffective.
func TestAnAppliedFixIsCheckedForItsEffect(t *testing.T) {
	t.Parallel()
	s := storedAs(t, observed(roundCap("dev", 3)), advisor.StateApplied)
	s.EffectDueAt = at.Add(time.Hour)
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, nil, evidenceRun()), "close:resolved:fixed_by_this")
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, []advisor.Observation{observed(roundCap("dev", 3))}, evidenceRun()))
	s.EffectDueAt = at
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, []advisor.Observation{observed(roundCap("dev", 3))}, evidenceRun()), "ineffective")
}

// A FINDING ABOUT A SEAT THE COMPANY NO LONGER HAS IS RETIRED, not resolved.
func TestAFindingWhoseSubjectLeftIsRetired(t *testing.T) {
	t.Parallel()
	s := storedAs(t, observed(roundCap("gone", 3)), advisor.StateOpen)
	r := evidenceRun()
	r.Held = func(s advisor.Subject) bool { return s.Seat != "gone" }
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, nil, r), "close:retired:subject_gone")
	if !advisor.SubjectHeld(company(t, stallShape), advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev"}) ||
		advisor.SubjectHeld(company(t, stallShape), advisor.Subject{Kind: advisor.SubjectSeat, Seat: "gone"}) {
		t.Error("SubjectHeld does not read the chart")
	}
}

// A FINDING NOBODY COULD CONFIRM GOES STALE AFTER THREE MISSED RUNS OF ITS
// SCOPE, and is closed as not confirmed a week later.
func TestAFindingIsStaleAfterThreeMissedRunsAndExpiresAfterSevenDays(t *testing.T) {
	t.Parallel()
	g := advisor.GroupEvidence
	if advisor.Stale(g, at.Add(-3*time.Hour+time.Minute), at) || !advisor.Stale(g, at.Add(-3*time.Hour-time.Minute), at) {
		t.Error("an hourly finding is not stale at exactly three missed runs")
	}
	s := storedAs(t, observed(roundCap("dev", 3)), advisor.StateOpen)
	scope := advisor.ScopeOf(s.Observation.Finding)
	r := advisor.Reading{At: at, Groups: []advisor.Group{advisor.GroupSetup}}
	r.LastDone = map[string]time.Time{scope: at.Add(-3*time.Hour - advisor.ExpireAfter + time.Minute)}
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, nil, r))
	r.LastDone[scope] = at.Add(-3*time.Hour - advisor.ExpireAfter - time.Minute)
	wantSteps(t, advisor.Reconcile([]advisor.Stored{s}, nil, r), "close:expired:not_confirmed")
}

// EVERY FACT A DETECTOR STATES HAS WORDS, so a person reads a label and a unit
// rather than a key. Read off the package's own source.
func TestEveryFactADetectorStatesHasWords(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		check := func(lit *ast.CompositeLit) {
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, isID := kv.Key.(*ast.Ident); !isID || id.Name != "Key" {
					continue
				}
				if v, ok := kv.Value.(*ast.BasicLit); ok && v.Kind == token.STRING {
					key, _ := strconv.Unquote(v.Value)
					found++
					if advisor.FactLabel(key) == key {
						t.Errorf("%s states fact %q, which has no words", fset.Position(kv.Pos()), key)
					}
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			switch typ := lit.Type.(type) {
			case *ast.Ident:
				if typ.Name == "Fact" {
					check(lit)
				}
			case *ast.ArrayType:
				if id, ok := typ.Elt.(*ast.Ident); ok && id.Name == "Fact" {
					for _, el := range lit.Elts {
						if inner, ok := el.(*ast.CompositeLit); ok {
							check(inner)
						}
					}
				}
			}
			return true
		})
	}
	if found < 30 {
		t.Fatalf("found %d facts in the detectors' source; the scan reads nothing", found)
	}
}
