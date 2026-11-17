package coord

import "context"

// AdvisorRecords holds the advisor analyst's records (ADR-0157): its last run
// and the answers people gave its findings, each internal/advisor's value,
// written only at the revision it was read at, so two nodes never both run.
type AdvisorRecords interface {
	// AdvisorRecord is one record and its revision, false where none.
	AdvisorRecord(ctx context.Context, name string) ([]byte, uint64, bool, error)
	// CreateAdvisorRecord writes a first record, false where one exists.
	CreateAdvisorRecord(ctx context.Context, name string, value []byte) (bool, error)
	// UpdateAdvisorRecord replaces the record read at revision, false where it moved.
	UpdateAdvisorRecord(ctx context.Context, name string, value []byte, revision uint64) (bool, error)
}

// AdvisorRecordClass is the key class the records are filed under in the
// positions register, which no clock ages.
const AdvisorRecordClass = "advisor"

// The analyst's records: its last run, and the answers to its findings.
const (
	AdvisorLastRun = "last_run"
	AdvisorAnswers = "answers"
)

// AdvisorRecordKey is the key of one record.
func AdvisorRecordKey(name string) string { return DocumentKey(AdvisorRecordClass, name) }
