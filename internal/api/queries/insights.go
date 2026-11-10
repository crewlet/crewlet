package queries

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/insights"
	"github.com/crewlet/crewlet/internal/issuereport"
)

// InsightsAnswer is `diagnostics_insights`: the company's consent to share
// operating insights, the catalogue it covers, and every record the fleet keeps.
type InsightsAnswer struct {
	Share     config.InsightsShare `json:"share"`
	Accepted  int                  `json:"accepted"`
	Schema    int                  `json:"schema"`
	Relay     string               `json:"relay"`
	Catalogue json.RawMessage      `json:"catalogue"`
	Records   []InsightRow         `json:"records"`
}

// InsightRow is one class's week as the fleet keeps it, its record as sent or
// as it would be.
type InsightRow struct {
	Class    string              `json:"class"`
	Week     string              `json:"week"`
	Status   coord.InsightStatus `json:"status"`
	SendAt   *time.Time          `json:"send_at,omitempty"`
	Attempts int                 `json:"attempts,omitempty"`
	// Answer is the relay's last status code for it, absent where none came.
	Answer int             `json:"answer,omitempty"`
	Record json.RawMessage `json:"record,omitempty"`
}

// insightsReading answers `diagnostics_insights`.
func (s Sources) insightsReading(ctx context.Context, _ Params) (any, error) {
	consent, err := s.insightConsent(ctx)
	if err != nil {
		return nil, err
	}
	records, err := s.Insights.ListInsightRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("diagnostics_insights: list the records: %w", err)
	}
	return insightsAnswerOf(consent, records)
}

// insightsAnswerOf is the answer for a consent and the records kept.
func insightsAnswerOf(consent config.InsightsSharing, records []coord.InsightRecord) (InsightsAnswer, error) {
	catalogue, err := insights.CatalogueJSON()
	if err != nil {
		return InsightsAnswer{}, err
	}
	rows := make([]InsightRow, 0, len(records))
	for _, r := range records {
		row := InsightRow{Class: r.Class, Week: r.Week, Status: r.Status, Attempts: r.Attempts, Answer: r.Answer,
			Record: r.Payload}
		if !r.SendAt.IsZero() {
			at := r.SendAt
			row.SendAt = &at
		}
		rows = append(rows, row)
	}
	return InsightsAnswer{Share: consent.Share, Accepted: consent.Accepted, Schema: insights.SchemaVersion,
		Relay: insights.RelayURL, Catalogue: catalogue, Records: rows}, nil
}

// insightConsent is the fleet's company's consent; a fleet with no company yet
// has given none.
func (s Sources) insightConsent(ctx context.Context) (config.InsightsSharing, error) {
	company, _, err := s.Config.FleetCompany(ctx)
	switch {
	case errors.Is(err, configapi.ErrNoActiveRevision):
		return config.InsightsSharing{}, nil
	case err != nil:
		return config.InsightsSharing{}, fmt.Errorf("insights: read the company: %w", err)
	}
	return company.Diagnostics.Insights, nil
}

// insightFindings is the advisor's findings, as `GET /advisor` answers them.
func (s Sources) insightFindings(ctx context.Context) ([]advisor.Finding, error) {
	answer, err := s.advisorRead(ctx, s.clock())
	if err != nil {
		return nil, err
	}
	return answer.Findings, nil
}

// InsightSharer is the sharer over this node's sources: the advisor's findings,
// the fleet's consent and record, the company's day, sending on send.
func (s Sources) InsightSharer(origin issuereport.Origin, send func(context.Context, []byte) (int, error)) *insights.Sharer {
	return &insights.Sharer{
		Store: s.Insights, Origin: origin, Findings: s.insightFindings, Consent: s.insightConsent,
		Day:   func(at time.Time) string { return at.In(s.zone()).Format("2006-01-02") },
		Delay: func() time.Duration { return rand.N(insights.MaxDelay) },
		Send:  send, Now: s.clock,
	}
}
