package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A DEFERRED READ IS NOT AN INVITATION TO TRY AGAIN.
//
// A node holding a change it cannot apply — one a newer build wrote — refuses
// a read of the item that change covers, and waiting does not clear that: the
// seat's next call asks the same node the same question. So the model is told
// what resolves it and to say it could not check, and never "try again",
// which spends its rounds on a loop that cannot end. A refusal that DOES clear
// by waiting keeps the invitation, and neither is ever an empty result.
//
// Mutation: drop the deferred arm from unservedRead and both tools tell the
// model to try again.
func TestADeferredReadIsNotAnInvitationToTryAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		refusal   *statelog.Refused
		tryAgain  bool
		newerHint bool
	}{
		{"deferred", &statelog.Refused{
			Code: statelog.RefuseDeferred, Level: statelog.ReadSession,
			Detail: "this node retains 1 record(s) covering task i1",
		}, false, true},
		{"behind", &statelog.Refused{
			Code: statelog.RefuseBehind, Level: statelog.ReadSession,
			Detail: "this node has not reached the floor",
		}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			trk.readErr = tc.refusal
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
			for name, args := range map[string]map[string]any{
				builtin.GetWorkItemTool:   {"item": "ENG-1"},
				builtin.CommentOnWorkTool: {"item": "ENG-1", "body": "still right?"},
			} {
				got := callWork(t, reg, name, args)
				if !got.Failed || !strings.Contains(got.Output, "NOT an empty result") {
					t.Errorf("%s on a %s refusal gave %q", name, tc.name, got.Output)
				}
				if said := strings.Contains(got.Output, "Try again"); said != tc.tryAgain {
					t.Errorf("%s on a %s refusal says try again = %v, want %v: %q",
						name, tc.name, said, tc.tryAgain, got.Output)
				}
				if said := strings.Contains(got.Output, "newer build"); said != tc.newerHint {
					t.Errorf("%s on a %s refusal names a newer build = %v, want "+
						"%v: %q", name, tc.name, said, tc.newerHint, got.Output)
				}
			}
		})
	}
}

// EVERY REFUSAL TELLS THE MODEL WHAT IT MEANS, AND ONLY A RETRYABLE ONE SAYS
// TRY AGAIN.
//
// The framework states which codes clear by coming back to this node
// ([statelog.ReadRefusal.Retryable]); each of those is an invitation, and each
// of the others names its own cause in its own sentence — an eviction is not a
// trimmed log, and a model told either that "trying again" helps sends the
// same call to the same node until its rounds run out. Driven off
// [statelog.ReadRefusals], so a code the framework adds fails here until it is
// given a sentence.
//
// Mutation: answer every non-retryable code with the deferred sentence, and
// the distinctness check fails; drop a case from unservedAdvice and that
// code's check fails.
func TestEveryReadRefusalTellsTheModelWhatItMeans(t *testing.T) {
	t.Parallel()
	// `too_stale` and `log_full` clear by WAITING, which is a different
	// statement from "ask this node again now" and is made as one.
	waits := map[statelog.ReadRefusal]string{
		statelog.RefuseTooStale: "Try again in a moment",
		statelog.RefuseLogFull:  "try again later",
	}
	seen := map[string]statelog.ReadRefusal{}
	for _, code := range statelog.ReadRefusals {
		trk := newFakeTracker()
		trk.readErr = &statelog.Refused{Code: code, Level: statelog.ReadLinearizable}
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
		got := callWork(t, reg, builtin.GetWorkItemTool, map[string]any{"item": "ENG-1"})
		if !got.Failed || !strings.Contains(got.Output, "NOT an empty result") {
			t.Errorf("a %s refusal gave %q", code, got.Output)
			continue
		}
		if code.Retryable() {
			if !strings.Contains(got.Output, "Try again, or say you could not check.") {
				t.Errorf("a %s refusal clears by coming back and does not say so: %q",
					code, got.Output)
			}
			continue
		}
		if want, ok := waits[code]; ok {
			if !strings.Contains(got.Output, want) {
				t.Errorf("a %s refusal clears by waiting and does not say %q: %q",
					code, want, got.Output)
			}
		} else if strings.Contains(strings.ToLower(got.Output), "try again") {
			t.Errorf("a %s refusal does not clear by coming back and still "+
				"invites it: %q", code, got.Output)
		}
		// ITS OWN SENTENCE: the explanation between the error and the
		// empty-result warning is this code's, and no other code's.
		why := explanation(got.Output)
		if why == "" || strings.Contains(why, "with no advice here") {
			t.Errorf("a %s refusal carries no explanation of its own: %q",
				code, got.Output)
			continue
		}
		if other, dup := seen[why]; dup {
			t.Errorf("%s and %s are explained by the same sentence %q", other, code, why)
		}
		seen[why] = code
	}
}

// explanation is the sentence a refusal's text carries between the error in
// parentheses and the empty-result warning.
func explanation(output string) string {
	_, after, found := strings.Cut(output, "). ")
	if !found {
		return ""
	}
	why, _, found := strings.Cut(after, " This is NOT an empty result")
	if !found {
		return ""
	}
	return why
}
