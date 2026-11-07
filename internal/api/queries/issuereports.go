package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/issuereport"
)

// IssueReportReader reads the company's issue reports, as the reporter keeps
// them, and what a report of each class would say.
type IssueReportReader interface {
	Enabled() bool
	Inert() issuereport.InertReason
	List(ctx context.Context) ([]issuereport.Row, error)
	Previews(ctx context.Context) ([]issuereport.Preview, error)
}

// IssueReportsAnswer is `issue_reports`: what Settings › Diagnostics shows. Both
// lists are ALWAYS LISTS.
type IssueReportsAnswer struct {
	// Enabled is the setting as this node applied it.
	Enabled bool `json:"enabled"`
	// Inert is why this build sends nothing whatever the setting says, "" for
	// one that may: `development`, `modified` or `no_commit`.
	Inert issuereport.InertReason `json:"inert"`
	// Relay is where a report goes, a constant of the build.
	Relay    string                `json:"relay"`
	Reports  []issuereport.Row     `json:"reports"`
	Previews []issuereport.Preview `json:"previews"`
}

// issueReports answers `issue_reports`. A store it cannot read raises: "never
// reported" and "could not read" are two facts.
func (s Sources) issueReports(ctx context.Context, _ Params) (any, error) {
	reports, err := s.IssueReports.List(ctx)
	if err != nil {
		return nil, err
	}
	previews, err := s.IssueReports.Previews(ctx)
	if err != nil {
		return nil, err
	}
	return IssueReportsAnswer{Enabled: s.IssueReports.Enabled(), Inert: s.IssueReports.Inert(), Relay: issuereport.RelayURL,
		Reports: reports, Previews: previews}, nil
}
