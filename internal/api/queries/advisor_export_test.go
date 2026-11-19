package queries

import (
	"context"
	"encoding/json"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// AdvisedOfAtlas is advisedOfAtlas for the external suite, which holds the
// atlas's harness: what the duty observes of the atlas, read as the engine reads it.
func (s Sources) AdvisedOfAtlas(ctx context.Context, answer advisor.Answer, now time.Time) advisor.Answer {
	return s.advisedOfAtlas(ctx, answer, now)
}

// AsRead is f stored as the duty stores it, then read back by the caller ctx
// carries at now: what a person is sent of a finding the duty kept.
func (s Sources) AsRead(ctx context.Context, f advisor.Finding, now time.Time) (AdvisorFinding, error) {
	body, err := json.Marshal(advisor.Observe(f, advisor.Span{From: now, To: now}, "r1"))
	if err != nil {
		return AdvisorFinding{}, err
	}
	principal, err := caller(ctx, "advisor_finding")
	if err != nil {
		return AdvisorFinding{}, err
	}
	row := advisordomain.FindingRow{ID: "f-1", Key: advisor.Key(f), Class: string(f.Class),
		State: advisordomain.State(advisor.StateOpen), Revision: 1, OpenedAt: now, Body: body}
	v := findingView{now: now, atlas: principal.Can(iam.GrantAtlas)}
	if v.atlas {
		v.entries = func() map[string][]advisor.AtlasEntry { return s.atlasEntriesFor(ctx, principal, now) }
	}
	return v.finding(row)
}
