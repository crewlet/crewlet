package assistant

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A RUN ANSWER'S RECEIPT IS READ AT THE RUN'S TRUST. The approval that sends
// the assistant's answer to a parked coding run resumes the turn that proposed
// it, which reads what the request answered: the run's receipt, carrying the
// question the run asked, a coding CLI's words, read by the one rule for them
// ([provenance.CodingRun]) as every other door to that question reads it. Any
// other write's answer is the engine's own records, a colleague's. And the
// turn is told each answer it reads at the trust it is fenced at, a page's
// report included, which is what taints it (D25).
//
// Mutation: read every write's answer by the hosted rule, as the resume did,
// and the run's case is red; fence an answer without telling the turn, and
// every case that answer is in is.
func TestARunAnswersReceiptIsReadAtTheRunsTrust(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	type told struct {
		source provenance.Source
		trust  provenance.Trust
	}
	for _, tc := range []struct {
		tool     string
		reported bool
		want     []told
	}{
		{tool: "answer_coding_run",
			want: []told{{provenance.SourceAPIAnswer, provenance.CodingRun("").Trust}}},
		{tool: "create_task",
			want: []told{{provenance.SourceAPIAnswer, provenance.TrustColleague}}},
		{tool: "create_task", reported: true, want: []told{
			{provenance.SourceAPIAnswer, provenance.TrustColleague},
			{provenance.SourcePageContext, provenance.TrustPage}}},
	} {
		var reads []told
		read := resultOf(approval.Resumption{State: wire.ProposalApplied,
			Answer:  []byte(`{"turn_id":"t1","question":"Which database?","outcome":"pending"}`),
			Receipt: &wire.Receipt{Summary: "Applied on the page."}, ClientReported: tc.reported,
			Card: wire.Card{Proposal: &wire.ProposalCard{Tool: tc.tool, Title: "Answer"}}}, b,
			func(source provenance.Source, trust provenance.Trust) {
				reads = append(reads, told{source, trust})
			})
		want := tc.want[0].trust
		if !strings.Contains(read, " source=api_answer trust="+string(want)+" ") {
			t.Errorf("%s's answer is read as:\n%s\nwant it fenced at %q", tc.tool, read, want)
		}
		if !slices.Equal(reads, tc.want) {
			t.Errorf("%s (a page's report %v): the turn is told it read %v, want %v, what "+
				"it is handed fenced", tc.tool, tc.reported, reads, tc.want)
		}
	}
}
