package queries_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/integration"
)

// THE DUTY'S READING CARRIES EACH CUT RECORD: Slack's findings record dropped
// findings nobody owes, so what the duty observes holds that surface in part,
// and the run keeps open what the record may have dropped.
//
// Mutation: leave the answer's Partial out of what Observe returns, and the
// reading holds no surface in part.
func TestTheDutysReadingCarriesEachCutRecord(t *testing.T) {
	t.Parallel()
	surface, _ := configSurface(t, companyDoc)
	cut := integration.State{Kind: integration.KindSlack, Findings: []integration.Finding{
		{Kind: integration.FindingGrantPending, Subject: "pm"}}}
	cut.FindingsRecord.Omitted = 2
	s := queries.Sources{Config: surface, Reconciles: func(context.Context) []integration.State {
		return []integration.State{cut, {Kind: integration.KindDatadog}}
	}}
	o, err := s.ObserveAdvisor(t.Context(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	want := []advisor.Cut{{Path: advisor.IntegrationPath(integration.KindSlack), Unowed: true}}
	if !slices.Equal(o.Partial, want) {
		t.Errorf("the duty observes %+v held in part, want %+v", o.Partial, want)
	}
}
