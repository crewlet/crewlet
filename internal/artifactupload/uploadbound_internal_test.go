package artifactupload

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// widestInstant is an instant at its longest as a record encodes it: UTC, to
// the nanosecond.
var widestInstant = time.Date(2026, 11, 22, 10, 0, 0, 123456789, time.UTC)

// widestPath is a path of artifacts.MaxPathBytes at most, of one character
// repeated.
func widestPath(of string) string { return strings.Repeat(of, artifacts.MaxPathBytes/len(of)) }

// widestRecord is an upload's record at the most its writers make it: an
// intent's 255 uploaded files and 256 carried paths (a version's 511 declared
// in all), a publish's 511 generated files, every path at its bound of path,
// and the outcome's warnings at what a report keeps of many long ones.
func widestRecord(t *testing.T, path string) *record {
	t.Helper()
	id := uuid.NewString()
	r := &record{ID: id, Principal: strings.Repeat("p", iam.MaxLogin), Purpose: purposePublish,
		Artifact: uuid.NewString(), TurnID: strings.Repeat("t", 64), Created: widestInstant,
		Expires: widestInstant, State: stateFinalizing,
		FinalizeKey: strings.Repeat(`"`, statelog.MaxCallerOpIDBytes), Claimed: widestInstant}
	widestType := ""
	for _, ext := range []string{".xml", ".js", ".md", ".html"} {
		if typ, ok := typeFor(ext); ok && len(typ) > len(widestType) {
			widestType = typ
		}
	}
	for i := range artifacts.MaxFilesPerPublish + artifacts.MaxFilesPerVersion {
		tr := transportEngine
		if i >= artifacts.MaxFilesPerPublish {
			tr = transportGenerated
		}
		r.Files = append(r.Files, file{Path: widestPath(path), ContentType: widestType,
			SHA256: objstore.Hash(strings.Repeat("f", 64)), Size: math.MaxInt64,
			Key: objstore.KeyAt(widestInstant), Transport: tr, State: fileStreaming})
	}
	for range artifacts.MaxFilesPerVersion - artifacts.MaxFilesPerPublish {
		r.Carried = append(r.Carried, widestPath(path))
	}
	var found []lint.Finding
	for range 4 * MaxReportWarnings {
		found = append(found, lint.Finding{Path: widestPath(path), Line: math.MaxInt,
			Code: lint.CodeKitFileUnserved, Message: strings.Repeat("\x01", 64<<10)})
	}
	r.Outcome = &outcome{
		Published: artifactsdomain.Published{Number: math.MaxInt, VersionID: uuid.New(),
			Result: statelog.Result{OpID: strings.Repeat(`"`, statelog.MaxCallerOpIDBytes)}},
		Report: Report{Warnings: cutWarnings(found), Files: &FileCounts{Stored: math.MaxInt, Carried: math.MaxInt}},
	}
	return r
}

// typeFor is the type the media table serves a file of ext as.
func typeFor(ext string) (string, bool) {
	typ, err := typeOf("a"+ext, "")
	return typ.Served, err == nil
}

// THE WIDEST UPLOAD RECORD FITS ITS BOUND, as the record is stored: every path
// at its bound in the byte JSON writes as two, and every path of `<`, which
// HTML escaping would write as six, in its own bytes; warnings cut to what a
// report keeps, each a byte JSON writes as six.
//
// Mutation: encode the record with json.Marshal (HTML escaping on), drop the
// message cut from cutWarnings, or lower coord.MaxUploadRecordBytes below the
// widest, and this is red.
func TestTheWidestUploadRecordFitsItsBound(t *testing.T) {
	t.Parallel()
	for _, path := range []string{`"`, " ", "<"} {
		raw, err := encodeRecord(widestRecord(t, path))
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > coord.MaxUploadRecordBytes {
			t.Errorf("the widest upload record of %q paths is stored as %d bytes, past its class's %d",
				path, len(raw), coord.MaxUploadRecordBytes)
		}
		t.Logf("the widest upload record of %q paths is %d bytes", path, len(raw))
	}
}

// AN UPLOAD RECORD PAST ITS BOUND IS REFUSED BY ITS WRITER, before it is
// sent: as invalid (a definite refusal, which undoes what the publish
// staged, never an outcome left for a retry), and as too large, naming the
// upload and what to do. Nothing is written.
//
// Mutation: drop writeRecord's HoldValue, and the store's own refusal is not
// one the pipeline undoes the publish on.
func TestAnUploadRecordPastItsBoundIsRefusedByItsWriter(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	s := &Service{res: fleet, now: func() time.Time { return widestInstant }}
	r := widestRecord(t, `"`)
	r.Files = append(r.Files, r.Files...)
	_, err := s.writeRecord(t.Context(), r, 0)
	switch {
	case !errors.Is(err, ErrInvalid) || !errors.Is(err, coord.ErrValueTooLarge):
		t.Fatalf("a record past its bound written = %v, want it refused as invalid and too large", err)
	case !strings.Contains(err.Error(), r.ID) || !strings.Contains(err.Error(), "fewer files"):
		t.Errorf("the refusal %q names neither the upload nor what to do", err)
	case !refused(err):
		t.Errorf("the refusal %q is not one a publish undoes its staging on", err)
	}
	if raw, _, err := fleet.UploadRecord(t.Context(), r.ID); err != nil || raw != nil {
		t.Errorf("the refused record reads back as %d bytes, %v; want nothing written", len(raw), err)
	}
}

// THE WIDEST RESERVATION LEDGER FITS ITS BOUND: every hold the budgets admit
// (MaxOutstandingUploads a principal, CompanyPendingFactor times that for the
// company), each at its widest principal, an int64's count of bytes, two
// instants and a direct put.
//
// Mutation: raise MaxOutstandingUploads to 80, or lower
// coord.MaxUploadLedgerBytes below the widest, and this is red.
func TestTheWidestReservationLedgerFitsItsBound(t *testing.T) {
	t.Parallel()
	l := ledger{V: ledgerVersion}
	for range artifacts.CompanyPendingFactor * MaxOutstandingUploads {
		l.Holds = append(l.Holds, hold{Upload: uuid.NewString(), Principal: strings.Repeat("p", iam.MaxLogin),
			Bytes: math.MaxInt64, Until: widestInstant, Expires: widestInstant, Direct: true})
	}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > coord.MaxUploadLedgerBytes {
		t.Errorf("the widest reservation ledger is stored as %d bytes, past its class's %d", len(raw),
			coord.MaxUploadLedgerBytes)
	}
	t.Logf("the widest reservation ledger is %d bytes", len(raw))
}
