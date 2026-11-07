package memory

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- issue reports ------------------------------------------------------- //

// The model retries' class machinery (modelrecords.go), keyed by the
// fingerprint: one record per defect. Nothing watches it, so the update shape
// is the record itself.

func newIssueReportRecords() entryRecords[coord.IssueReport, coord.IssueReport] {
	return entryRecords[coord.IssueReport, coord.IssueReport]{
		stamp: func(v coord.IssueReport, version uint64) coord.IssueReport {
			v.FirstSent, v.ClaimedAt, v.Version = v.FirstSent.UTC(), v.ClaimedAt.UTC(), version
			return v
		},
		update: func(v *coord.IssueReport, _ string, _ bool) coord.IssueReport {
			if v == nil {
				return coord.IssueReport{}
			}
			return *v
		},
	}
}

var errNoIssueFingerprint = errors.New("coord/memory: an issue report needs its fingerprint")

// IssueReport reads one fingerprint's record.
func (f *Fleet) IssueReport(_ context.Context, fingerprint string) (coord.IssueReport, bool, error) {
	if fingerprint == "" {
		return coord.IssueReport{}, false, errNoIssueFingerprint
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.issueReports.records[fingerprint]
	return r, ok, nil
}

// ListIssueReports returns every record, ordered by fingerprint.
func (f *Fleet) ListIssueReports(context.Context) ([]coord.IssueReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issueReports.sortedLocked(), nil
}

// ClaimIssueReport writes want where the fingerprint holds no record or open
// admits the one it holds.
func (f *Fleet) ClaimIssueReport(_ context.Context, want coord.IssueReport,
	open func(held coord.IssueReport) bool,
) (coord.IssueReport, bool, error) {
	if err := want.Validate(); err != nil {
		return coord.IssueReport{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.issueReports.records[want.Fingerprint]; ok && !open(held) {
		return held, false, nil
	}
	return f.issueReports.storeLocked(f, want.Fingerprint, want), true, nil
}

// SettleIssueReport writes r at the version it names.
func (f *Fleet) SettleIssueReport(_ context.Context, r coord.IssueReport) (coord.IssueReport, bool, error) {
	if err := r.Validate(); err != nil {
		return coord.IssueReport{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.issueReports.records[r.Fingerprint]
	if !ok || r.Version == 0 || held.Version != r.Version {
		return coord.IssueReport{}, false, nil
	}
	return f.issueReports.storeLocked(f, r.Fingerprint, r), true, nil
}
