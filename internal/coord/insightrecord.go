package coord

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The fleet's record of the operating insights it shares (ADR-0119): one per
// catalogue class and ISO week, from first seen to sent, so of any number of
// nodes one sends a class's week at most once. It holds the sealed record's
// bytes and nothing else of the company's, and lives in the positions register.

// InsightStatus is how far a class's week got.
type InsightStatus string

const (
	// InsightObserved is a class raised on Day, waiting for a second day.
	InsightObserved InsightStatus = "observed"
	// InsightQueued is an eligible record waiting for an administrator.
	InsightQueued InsightStatus = "queued"
	// InsightScheduled is a record to send at SendAt.
	InsightScheduled InsightStatus = "scheduled"
	// InsightSending is a node holding the claim and sending.
	InsightSending InsightStatus = "sending"
	// InsightSent is the relay having taken it.
	InsightSent InsightStatus = "sent"
	// InsightFailed is a send the relay never took, after its last attempt.
	InsightFailed InsightStatus = "failed"
	// InsightDeclined is an administrator having said no to it.
	InsightDeclined InsightStatus = "declined"
)

// Valid reports whether the status is one this build names.
func (s InsightStatus) Valid() bool {
	switch s {
	case InsightObserved, InsightQueued, InsightScheduled, InsightSending, InsightSent, InsightFailed,
		InsightDeclined:
		return true
	}
	return false
}

// InsightRecord is one catalogue class's record for one ISO week.
type InsightRecord struct {
	Class  string        `json:"class"`
	Week   string        `json:"week"`
	Status InsightStatus `json:"status"`
	// Day is the company day the class was last observed, `2030-01-07`.
	Day string `json:"day,omitempty"`
	// SendAt is when a scheduled record goes, and Attempts how many sends failed.
	SendAt   time.Time `json:"send_at,omitzero"`
	Attempts int       `json:"attempts,omitempty"`
	// ClaimedAt is when a node claimed it to send, and Answer the relay's last
	// status code, 0 where it did not answer.
	ClaimedAt time.Time `json:"claimed_at,omitzero"`
	Answer    int       `json:"answer,omitempty"`
	// Payload is the sealed record as it is, or was, sent.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Version is the store's version of the record as it was read. Not on the wire.
	Version uint64 `json:"-"`
}

// MaxInsightPayloadBytes is the most a record's body may hold: the relay's limit.
const MaxInsightPayloadBytes = 2 << 10

var (
	insightClassShape = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	insightWeekShape  = regexp.MustCompile(`^\d{4}-W\d{2}$`)
	insightDayShape   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Validate refuses a record that is not the shape this file promises.
func (r InsightRecord) Validate() error {
	switch {
	case !insightClassShape.MatchString(r.Class):
		return fmt.Errorf("coord: an insight record's class must be a catalogue id")
	case !insightWeekShape.MatchString(r.Week):
		return fmt.Errorf("coord: an insight record's week must be an ISO week")
	case !r.Status.Valid():
		return fmt.Errorf("coord: an insight record's status %q is not one this build names", r.Status)
	case r.Day != "" && !insightDayShape.MatchString(r.Day):
		return fmt.Errorf("coord: an insight record's day must be a company day")
	case len(r.Payload) > MaxInsightPayloadBytes:
		return fmt.Errorf("coord: an insight record's payload is past %d bytes", MaxInsightPayloadBytes)
	case r.Attempts < 0:
		return fmt.Errorf("coord: an insight record's attempts cannot be negative")
	case r.Answer != 0 && (r.Answer < 100 || r.Answer > 599):
		return fmt.Errorf("coord: an insight record's answer %d is no HTTP status", r.Answer)
	}
	return nil
}

// InsightRecordClass is the key class of these records in the positions register.
const InsightRecordClass = "insight_record"

// InsightEntry is a record's entry name: its class and week.
func InsightEntry(class, week string) string { return class + "@" + week }

// InsightRecordKey is the key of an entry's record.
func InsightRecordKey(entry string) string {
	class, week, _ := strings.Cut(entry, "@")
	return DocumentKey(InsightRecordClass, class, week)
}

// InsightEntryOf recovers the entry from a key of this class, reporting false
// for a key of another class or one this grammar did not write.
func InsightEntryOf(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 3 || segments[0] != InsightRecordClass || segments[1] == "" || segments[2] == "" {
		return "", false
	}
	return InsightEntry(segments[1], segments[2]), true
}

// InsightRecords is the fleet's record of the operating insights it shares.
//
// Every read RAISES rather than answering empty when the store cannot be read:
// "never seen" and "could not read" are two facts, and a claim that took the
// second for the first would send twice.
type InsightRecords interface {
	// InsightRecord reads one class's record for one week.
	InsightRecord(ctx context.Context, class, week string) (InsightRecord, bool, error)

	// ListInsightRecords returns every record, ordered by entry.
	ListInsightRecords(ctx context.Context) ([]InsightRecord, error)

	// ClaimInsightRecord writes want where its entry holds no record, or where
	// open admits the one it holds, and reports whether this caller did. Of any
	// number of callers one wins; the others are handed the record that won.
	ClaimInsightRecord(ctx context.Context, want InsightRecord, open func(held InsightRecord) bool) (InsightRecord, bool, error)

	// DeleteInsightRecord removes r where it still holds at r.Version.
	DeleteInsightRecord(ctx context.Context, r InsightRecord) (bool, error)
}
