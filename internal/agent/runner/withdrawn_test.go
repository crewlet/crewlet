package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN WHOSE SEAT WITHDREW ITS QUESTION OR ITS CALL IS TOLD SO, AS ITS OWN
// (ADR-0111): never as a person's answer or a person's denial, with the reason
// its seat gave, the call not made, and told to carry on under its access as
// it stands, asking again where it still needs to. In its own session and on a
// fresh machine alike.
//
// Mutation: drop the withdrawn branch and the run reads its own seat's reason
// as somebody's answer, or as a person who denied the call.
func TestARunIsToldItsSeatWithdrewWhatItWaitedOn(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	const reason = "My access to the server is read only now."
	question := runner.Asked{Question: "Restart the database?", Answer: reason, By: "sre",
		Withdrawn: true, Branch: "wip/restart"}
	held := runner.Asked{Answer: reason, By: "sre", Withdrawn: true,
		Held: &runner.HeldCall{Tool: "ssh_operate", Server: "ssh", Choice: sandbox.HeldDeny}}
	for name, c := range map[string]struct {
		text string
		want []string
	}{
		"question, its own session": {question.Continuation(b), []string{
			"You withdrew your question", "Restart the database?", reason, "access as it stands now"}},
		"question, a fresh machine": {question.Reseed(b), []string{
			"your seat withdrew it", reason, "wip/restart"}},
		"held call, its own session": {held.Continuation(b), []string{
			"You withdrew your call", "ssh_operate", reason, "The call was not made."}},
		"held call, a fresh machine": {held.Reseed(b), []string{
			"your seat withdrew the call", reason, "The call was not made."}},
	} {
		for _, want := range c.want {
			if !strings.Contains(c.text, want) {
				t.Errorf("%s: the run is not told %q:\n%s", name, want, c.text)
			}
		}
		for _, wrong := range []string{"sre answered", "sre denied", "was answered"} {
			if strings.Contains(c.text, wrong) {
				t.Errorf("%s: the run is told %q:\n%s", name, wrong, c.text)
			}
		}
	}
}

// AN ANSWER IS ITS SEAT'S WITHDRAWAL ONLY WHEN THE RUN'S OWN AGENT SEAT GAVE
// IT: a person answering, or another seat's name, is an answer.
func TestOnlyTheRunsOwnSeatWithdrawsIt(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{TurnID: "t-1", AgentHandle: "sre"}
	cases := []struct {
		by, kind string
		want     bool
	}{
		{"sre", "agent", true},
		{"sre", "human", false},
		{"pm", "agent", false},
		{"", "agent", false},
	}
	for _, c := range cases {
		if got := sandbox.WithdrawnBy(run, c.by, c.kind); got != c.want {
			t.Errorf("an answer by %q (%s) reads as withdrawn %v, want %v", c.by, c.kind, got, c.want)
		}
	}
}
