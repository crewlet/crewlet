package executorkeep

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// EVERY LOSS IS A FAULT OR DELIBERATE, AND WORDED: the objective burns on the
// faults alone, and every surface says a loss in its own clause, never the
// words a loss this build does not name gets.
//
// Mutation: flip Fault for one loss, or drop a loss's clause from Words, and a
// case goes red.
func TestEveryLossIsFaultOrDeliberateAndWorded(t *testing.T) {
	t.Parallel()
	deliberate := map[Loss]bool{LossContextExhausted: true, LossRequestRefused: true, LossHarnessChanged: true,
		LossBudgetFloor: true, LossEntryChanged: true}
	seen := map[string]Loss{}
	for _, l := range Losses {
		if !l.Valid() {
			t.Errorf("%s is listed and not valid", l)
		}
		if l.Fault() == deliberate[l] {
			t.Errorf("%s: fault %v, want %v", l, l.Fault(), !deliberate[l])
		}
		words := l.Words()
		if words == UnnamedLossWords || words == "" {
			t.Errorf("%s is worded as a loss this build does not name", l)
		}
		if other, dup := seen[words]; dup {
			t.Errorf("%s and %s are worded alike: %q", l, other, words)
		}
		seen[words] = l
		if len(l) > WordMaxBytes {
			t.Errorf("%s is past the %d bytes an engine word is held to", l, WordMaxBytes)
		}
	}
	if !strings.Contains(LossTooLarge.Words(), "48 MiB") {
		t.Errorf("too_large is worded %q, which does not name the bound", LossTooLarge.Words())
	}
}

// EVERY LOSS THIS PACKAGE DECLARES IS LISTED, so one this build says is one it
// names (Valid), worded and split fault or deliberate: the other side of
// internal/sandbox's TestEveryLossThisBuildSaysIsNamed, which holds the tree to
// these constants.
//
// Mutation: declare a loss and leave it out of Losses, and this goes red.
func TestEveryDeclaredLossIsListed(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "loss.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	declared := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			if typ, ok := value.Type.(*ast.Ident); !ok || typ.Name != "Loss" {
				continue
			}
			for i, name := range value.Names {
				declared++
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || !slices.Contains(Losses, Loss(strings.Trim(lit.Value, `"`))) {
					t.Errorf("%s is declared and not listed in Losses", name.Name)
				}
			}
		}
	}
	if declared != len(Losses) {
		t.Errorf("loss.go declares %d losses and Losses lists %d", declared, len(Losses))
	}
}

// A LOSS A NEWER BUILD NAMES IS A VALUE, read, relayed and said, never a panic
// or a refusal of the record that carries it: a reader and a writer alike hold
// it, and an instead a newer build names, to its bound alone, it is said in the
// unnamed words, and counted a fault. A loss outside its bound is refused.
// Refused by the writer, a newer build's end routed to a data node of this one,
// or one this build relays off a row a newer node stamped, would never land.
//
// Mutation: hold Validate or Within to the named losses, or drop the bound,
// and a case goes red.
func TestAnUnknownLossIsAValue(t *testing.T) {
	t.Parallel()
	newer := NotContinued{Loss: "a_newer_loss", Instead: "a_newer_instead", Detail: "a newer build's"}
	if newer.Loss.Valid() || !newer.Loss.Fault() {
		t.Errorf("an unnamed loss: valid %v, fault %v; want false, true", newer.Loss.Valid(), newer.Loss.Fault())
	}
	if !newer.Within() {
		t.Error("a reader refuses a verdict whose loss it does not name")
	}
	if !strings.Contains(newer.Sentence(), UnnamedLossWords) {
		t.Errorf("an unnamed loss is said %q", newer.Sentence())
	}
	if err := newer.Validate(); err != nil {
		t.Errorf("a writer refuses a verdict whose loss it does not name: %v", err)
	}
	end := Ending{Outcome: OutcomeCompleted, NotContinued: []NotContinued{newer},
		Unkept: &Unkept{LaunchSeq: 1, CaptureSeq: 1, Loss: newer.Loss, Reason: "a newer build's"}}
	if err := end.Validate(); err != nil || !end.Within() {
		t.Errorf("an end relaying a loss this build does not name: validate %v, within %v", err, end.Within())
	}
	for name, loss := range map[string]Loss{"no loss": "", "past its bound": Loss(strings.Repeat("l",
		WordMaxBytes+1)), "not UTF-8": "\xff"} {
		if (NotContinued{Loss: loss, Instead: InsteadFresh}).Validate() == nil {
			t.Errorf("a verdict naming %s was accepted", name)
		}
		if (Ending{Outcome: OutcomeCompleted, Unkept: &Unkept{LaunchSeq: 1, CaptureSeq: 1, Loss: loss}}).Validate() ==
			nil {
			t.Errorf("an unkept capture naming %s was accepted", name)
		}
	}
}

// A VERDICT SAYS WHAT WAS LOST AND WHAT THE RUN DID INSTEAD, naming each turn,
// and holds nothing that is somebody else's words: its detail stays out of the
// sentence, fenced on its own field.
func TestAVerdictsSentenceNamesTheLossWhatTheRunDidAndTheTurns(t *testing.T) {
	t.Parallel()
	const secret = "box said: ghp_Q7wErTy9UiOp3AsDfGh5JkLzXcVbNm1PqRsT"
	for name, c := range map[string]struct {
		n    NotContinued
		want []string
	}{
		"fresh, its own turn": {NotContinued{Loss: LossTooLarge, Instead: InsteadFresh, Detail: secret},
			[]string{"did not continue its session", "48 MiB", "started a fresh session"}},
		"older turn": {NotContinued{Loss: LossTooLarge, Instead: InsteadOlder, LostTurn: "turn-n",
			OlderTurn: "turn-m", Detail: secret}, []string{"turn turn-n's session", "turn turn-m's older session"}},
		"older capture": {NotContinued{Loss: LossCaptureLate, Instead: InsteadOlder,
			Older: &CapturePlace{LaunchSeq: 2, CaptureSeq: 3}}, []string{"still being kept", "launch 2, capture 3"}},
	} {
		if err := c.n.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		said := c.n.Sentence()
		for _, w := range c.want {
			if !strings.Contains(said, w) {
				t.Errorf("%s: %q does not say %q", name, said, w)
			}
		}
		if strings.Contains(said, "ghp_") {
			t.Errorf("%s: the sentence carries the detail: %q", name, said)
		}
	}
	if err := (NotContinued{Loss: LossTooLarge, Instead: InsteadFresh, OlderTurn: "m"}).Validate(); err == nil {
		t.Error("a fresh session naming an older one it continued was accepted")
	}
	cut := NotContinued{Loss: LossKeepFailed, Instead: InsteadFresh,
		Detail: strings.Repeat("é", UnkeptReasonMaxBytes)}.Fit()
	if !cut.Within() {
		t.Errorf("a fitted verdict's detail is %d bytes, past its %d", len(cut.Detail), UnkeptReasonMaxBytes)
	}
}

// AN END KEEPS ITS TURN'S FIRST VERDICT AND ITS NEWEST, never more: the first
// says what the conversation lost going into the turn, the newest what its
// final session lacks, and the launches' records hold the rest.
func TestAnEndKeepsTheFirstAndNewestVerdict(t *testing.T) {
	t.Parallel()
	var held []NotContinued
	for _, l := range []Loss{LossTooLarge, LossKeepFailed, LossBoxLost} {
		held = HoldNotContinued(held, NotContinued{Loss: l, Instead: InsteadFresh})
	}
	if len(held) != NotContinuedMaxEntries || held[0].Loss != LossTooLarge || held[1].Loss != LossBoxLost {
		t.Errorf("the end holds %+v, want too_large then box_lost", held)
	}
	e := Ending{Outcome: OutcomeCompleted, NotContinued: append(held, NotContinued{Loss: LossTooLarge,
		Instead: InsteadFresh})}
	if e.Validate() == nil || e.Within() {
		t.Error("an end with three verdicts was accepted")
	}
	if err := (Ending{Outcome: OutcomeCompleted, Unkept: &Unkept{LaunchSeq: 1, CaptureSeq: 1,
		Reason: "refused"}}).Validate(); err == nil {
		t.Error("an unkept capture naming no loss was accepted")
	}
}
