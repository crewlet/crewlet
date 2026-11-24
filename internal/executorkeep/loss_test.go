package executorkeep

import (
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

// A LOSS A NEWER BUILD NAMES IS A VALUE, read and said, never a panic or a
// refusal of the record that carries it: a reader holds it to its bound
// alone, says it in the unnamed words, and counts it a fault.
func TestAnUnknownLossIsAValue(t *testing.T) {
	t.Parallel()
	newer := NotContinued{Loss: "entry_changed", Instead: InsteadFresh, Detail: "a newer build's"}
	if newer.Loss.Valid() || !newer.Loss.Fault() {
		t.Errorf("an unnamed loss: valid %v, fault %v; want false, true", newer.Loss.Valid(), newer.Loss.Fault())
	}
	if !newer.Within() {
		t.Error("a reader refuses a verdict whose loss it does not name")
	}
	if !strings.Contains(newer.Sentence(), UnnamedLossWords) {
		t.Errorf("an unnamed loss is said %q", newer.Sentence())
	}
	if err := newer.Validate(); err == nil {
		t.Error("a writer of this build wrote a loss it does not name")
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
