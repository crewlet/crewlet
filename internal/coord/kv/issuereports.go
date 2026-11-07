package kv

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- issue reports ------------------------------------------------------- //

// One record per fingerprint, filed in the positions register under a class of
// its own: see internal/coord's issuereport.go. The model retries' verbs
// (modelrecords.go), over the fingerprint.

var issueReportClass = entryClass[coord.IssueReport]{
	name:   "the issue reports",
	filter: coord.DocumentFilter(coord.IssueReportClass),
	key:    coord.IssueReportKey,
	entry:  coord.IssueReportFingerprintOf,
	stored: func(r coord.IssueReport, fingerprint string, version uint64) coord.IssueReport {
		// A RECORD DESCRIBES THE FINGERPRINT IT IS FILED UNDER, whatever its
		// body says.
		r.Fingerprint, r.Version = fingerprint, version
		r.FirstSent, r.ClaimedAt = r.FirstSent.UTC(), r.ClaimedAt.UTC()
		return r
	},
}

// IssueReport reads one fingerprint's record.
func (f *FleetStore) IssueReport(ctx context.Context, fingerprint string) (coord.IssueReport, bool, error) {
	return issueReportClass.read(ctx, f, fingerprint)
}

// ListIssueReports returns every record, ordered by fingerprint.
func (f *FleetStore) ListIssueReports(ctx context.Context) ([]coord.IssueReport, error) {
	return issueReportClass.list(ctx, f)
}

// ClaimIssueReport writes want where the fingerprint holds no record or open
// admits the one it holds: a read, then a create or an update at the version
// read, retried on a lost race, as [FleetStore.PutMCPCheck] is. Its contenders
// are the nodes that detected one defect, which the fleet's head count bounds.
func (f *FleetStore) ClaimIssueReport(ctx context.Context, want coord.IssueReport,
	open func(held coord.IssueReport) bool,
) (coord.IssueReport, bool, error) {
	if err := want.Validate(); err != nil {
		return coord.IssueReport{}, false, err
	}
	var stored coord.IssueReport
	claimed := false
	settled, err := casRetry(ctx, &f.positionTrips, "claim the issue report "+want.Fingerprint, func() (bool, error) {
		held, found, err := issueReportClass.read(ctx, f, want.Fingerprint)
		if err != nil {
			return false, err
		}
		if found && !open(held) {
			stored, claimed = held, false
			return true, nil
		}
		var wrote bool
		if found {
			stored, wrote, err = issueReportClass.update(ctx, f, want.Fingerprint, want, held.Version)
		} else {
			stored, wrote, err = issueReportClass.create(ctx, f, want.Fingerprint, want)
		}
		claimed = wrote
		return wrote, err
	})
	switch {
	case err != nil:
		return coord.IssueReport{}, false, err
	case !settled:
		return coord.IssueReport{}, false, contended("ClaimIssueReport", want.Fingerprint)
	}
	return stored, claimed, nil
}

// SettleIssueReport writes r at the version it names.
func (f *FleetStore) SettleIssueReport(ctx context.Context, r coord.IssueReport) (coord.IssueReport, bool, error) {
	if err := r.Validate(); err != nil {
		return coord.IssueReport{}, false, err
	}
	return issueReportClass.update(ctx, f, r.Fingerprint, r, r.Version)
}
