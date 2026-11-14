package coord

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// AN ISSUE REPORT is the company's record that one engine defect was reported,
// or is being (ADR-0101): one record per fingerprint, the current value only.
//
// # Who has to agree on it
//
// The whole company, now. "At most once per fingerprint per seven days" is a
// promise about the company, and every node detects on its own, so a record on
// any one node's file lets each of them send. A claim here is a compare-and-set
// the broker arbitrates: of the nodes that detect one defect, one sends.
//
// # Bounded, and holding nothing of the company's
//
// A record is the typed report that was sent and what the relay answered:
// [IssueReport.Validate] holds its strings to shapes that carry no words and
// its payload to [MaxIssueReportPayloadBytes], the relay's own limit. There is
// one record per fingerprint, each a catalogue class at one engine function,
// and it is replaced rather than aged, so it lives in the positions register,
// the bucket with no retention.

// IssueReportStatus is how far a report got.
type IssueReportStatus string

const (
	// IssueReportSending is a node holding the claim and sending.
	IssueReportSending IssueReportStatus = "sending"
	// IssueReportFiled is the relay having opened an issue for it.
	IssueReportFiled IssueReportStatus = "filed"
	// IssueReportDuplicate is the relay having found the issue already open.
	IssueReportDuplicate IssueReportStatus = "duplicate"
	// IssueReportFailed is the relay never having accepted it.
	IssueReportFailed IssueReportStatus = "failed"
	// IssueReportHeld is a report kept in this record and never sent: a defect
	// an outside party could raise at will, for a person to report privately.
	IssueReportHeld IssueReportStatus = "held"
)

// Valid reports whether the status is one this build names.
func (s IssueReportStatus) Valid() bool {
	switch s {
	case IssueReportSending, IssueReportFiled, IssueReportDuplicate, IssueReportFailed, IssueReportHeld:
		return true
	}
	return false
}

// IssueReport is one fingerprint's record.
type IssueReport struct {
	// Fingerprint is the report's identity, 64 lowercase hex digits.
	Fingerprint string `json:"fingerprint"`
	// Class is the catalogue id it was detected under.
	Class string `json:"class"`
	// Status is how far the latest send got.
	Status IssueReportStatus `json:"status"`
	// FirstSent is when the company first claimed this fingerprint, kept
	// across every later send.
	FirstSent time.Time `json:"first_sent"`
	// ClaimedAt is when the latest send was claimed: what the seven days and
	// a stuck send's lease are counted from.
	ClaimedAt time.Time `json:"claimed_at"`
	// Issue is the address the relay answered, "" where it answered none.
	Issue string `json:"issue,omitempty"`
	// Payload is the exact report sent, bytes as they went out.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Version is the store's version of the record as it was read. Opaque,
	// as [ModelRetry.Version] is. Not on the wire.
	Version uint64 `json:"-"`
}

// MaxIssueReportPayloadBytes is the most a report's body may hold: the relay
// refuses anything past 8 KiB, so nothing larger was ever sent.
const MaxIssueReportPayloadBytes = 8 << 10

// MaxIssueReportAddressBytes bounds the address kept for a report. An issue's
// address is well under this; the relay's word is held to it all the same.
const MaxIssueReportAddressBytes = 256

// The shapes a report's fingerprint and class are held to, compiled at first
// use for the insight records' reason: a bounded repeat compiles to a program
// holding every count, which no push's hook should pay for.
var (
	issueFingerprintShape = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{64}$`) })
	issueClassShape       = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	})
)

// Validate refuses a record that is not the shape this file promises, which is
// what keeps a mistaken caller's words out of the fleet's store.
func (r IssueReport) Validate() error {
	switch {
	case !issueFingerprintShape().MatchString(r.Fingerprint):
		return fmt.Errorf("coord: an issue report's fingerprint must be 64 lowercase hex digits")
	case !issueClassShape().MatchString(r.Class):
		return fmt.Errorf("coord: an issue report's class must be a catalogue id")
	case !r.Status.Valid():
		return fmt.Errorf("coord: an issue report's status %q is not one this build names", r.Status)
	case r.FirstSent.IsZero() || r.ClaimedAt.IsZero():
		return fmt.Errorf("coord: an issue report needs its first send and its claim's instant")
	case len(r.Issue) > MaxIssueReportAddressBytes:
		return fmt.Errorf("coord: an issue report's address is past %d bytes", MaxIssueReportAddressBytes)
	case len(r.Payload) > MaxIssueReportPayloadBytes:
		return fmt.Errorf("coord: an issue report's payload is past %d bytes", MaxIssueReportPayloadBytes)
	}
	return nil
}

// IssueReportClass is the key class of these records in the positions register.
const IssueReportClass = "issue_report"

// IssueReportKey is a fingerprint's record's key.
func IssueReportKey(fingerprint string) string {
	return DocumentKey(IssueReportClass, fingerprint)
}

// IssueReportFingerprintOf recovers the fingerprint from a key of this class,
// reporting false for a key of another class or one this grammar did not write.
func IssueReportFingerprintOf(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != IssueReportClass || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}

// IssueReports is the fleet's record of the engine defects it reported.
//
// Every read RAISES rather than answering empty when the store cannot be read:
// "never reported" and "could not read" are two facts, and a claim that took
// the second for the first would send twice.
type IssueReports interface {
	// IssueReport reads one fingerprint's record.
	IssueReport(ctx context.Context, fingerprint string) (IssueReport, bool, error)

	// ListIssueReports returns every record, ordered by fingerprint.
	ListIssueReports(ctx context.Context) ([]IssueReport, error)

	// ClaimIssueReport writes want as the fingerprint's record where it holds
	// none, or where open says the record it holds may be replaced, and
	// reports whether this caller did. A lost race is read again and asked of
	// open again, so of any number of callers one wins. Whoever does not win
	// is handed the record that held them off.
	ClaimIssueReport(ctx context.Context, want IssueReport, open func(held IssueReport) bool) (IssueReport, bool, error)

	// SettleIssueReport writes r at r.Version, reporting whether that version
	// still held. A settle that lost is a send another node took over after
	// this one's lease ran out, and its record stands.
	SettleIssueReport(ctx context.Context, r IssueReport) (IssueReport, bool, error)
}
