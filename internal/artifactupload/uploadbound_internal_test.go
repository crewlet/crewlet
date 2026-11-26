package artifactupload

import (
	"bytes"
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
	"github.com/crewlet/crewlet/internal/objstore/memobj"
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
		FinalizeKey: strings.Repeat(`"`, statelog.MaxCallerOpIDBytes), Claimed: widestInstant, Kept: math.MaxInt64}
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
			Key: objstore.KeyAt(widestInstant), Transport: tr, State: fileStreaming, Offered: true})
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

// THE WIDEST UPLOAD RECORD IS WRITTEN WITHIN ITS BOUND, by its writer, as it
// is stored: every path at its bound in the byte JSON writes as two, and
// every path of `<`, which HTML escaping would write as six, in its own bytes;
// warnings cut to what a report keeps, each a byte JSON writes as six. Staged,
// before its outcome, it is written within the room it leaves for one.
//
// Mutation: encode the record with json.Marshal (HTML escaping on) in
// writeRecord or encodeRecord, drop the message cut from cutWarnings, or
// lower coord.MaxUploadRecordBytes below the widest, and this is red.
func TestTheWidestUploadRecordFitsItsBound(t *testing.T) {
	t.Parallel()
	s := &Service{res: memory.NewFleet(), now: func() time.Time { return widestInstant }}
	for _, path := range []string{`"`, "\u2028", "<"} {
		r := widestRecord(t, path)
		staged := *r
		staged.ID, staged.Outcome = uuid.NewString(), nil
		for _, c := range []struct {
			what string
			r    *record
		}{{"closed", r}, {"staged", &staged}} {
			if wrote, err := s.writeRecord(t.Context(), c.r, 0); err != nil || wrote == 0 {
				t.Errorf("the widest %s upload record of %q paths was written = %d, %v; want it written",
					c.what, path, wrote, err)
			}
			raw, err := encodeRecord(c.r)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("the widest %s upload record of %q paths is %d bytes", c.what, path, len(raw))
		}
	}
}

// THE WIDEST OUTCOME FITS WHAT A RECORD KEEPS FOR IT: close adds the outcome
// after the version has landed, so maxOutcomeBytes, the room every write
// before it leaves, holds the most a report keeps (its warnings at their
// widest paths and messages) and a published version at its widest, as the
// record encodes them.
//
// Mutation: count a message's bytes once rather than six times in
// maxOutcomeBytes, and this is red.
func TestTheWidestOutcomeFitsWhatARecordKeepsForIt(t *testing.T) {
	t.Parallel()
	for _, path := range []string{`"`, "\u2028", "<"} {
		closed := widestRecord(t, path)
		staged := *closed
		staged.Outcome = nil
		with, err := encodeRecord(closed)
		if err != nil {
			t.Fatal(err)
		}
		without, err := encodeRecord(&staged)
		if err != nil {
			t.Fatal(err)
		}
		if added := len(with) - len(without); added > maxOutcomeBytes {
			t.Errorf("the widest outcome of %q paths adds %d bytes to its record, past the %d kept for it",
				path, added, maxOutcomeBytes)
		}
	}
}

// A STAGED RECORD AT ITS BOUND CLOSES WITH ITS OUTCOME: a publish whose record
// staging admitted, grown to just under what a record without its outcome may
// be, lands and records the widest outcome on it, so a retry of the same
// operation is answered from the record rather than refused as moved.
//
// Mutation: hold a record without its outcome to coord.MaxUploadRecordBytes
// in recordBound, and close's write is refused.
func TestAStagedRecordAtItsBoundClosesWithItsOutcome(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := &Service{res: memory.NewFleet(), now: func() time.Time { return widestInstant }}
	rec := &record{ID: uuid.NewString(), Principal: "ada.lovelace", Purpose: purposeStage, Created: widestInstant,
		Expires: widestInstant, State: stateFinalizing, FinalizeKey: strings.Repeat(`"`, statelog.MaxCallerOpIDBytes),
		Claimed: widestInstant}
	// FILLED IN BULK, then topped up: a carried path of `"` adds its two
	// bytes a byte, its quotes and a comma.
	room := func() int {
		raw, err := encodeRecord(rec)
		if err != nil {
			t.Fatal(err)
		}
		return recordBound(rec) - len(raw)
	}
	per := 2*artifacts.MaxPathBytes + len(`"",`)
	for left := room(); left >= per+len(`"carried":[],`); left = room() {
		for range max(left/per-1, 1) {
			rec.Carried = append(rec.Carried, widestPath(`"`))
		}
	}
	rec.Carried = append(rec.Carried, strings.Repeat("a", max(room()-len(`"",`), 0)))
	if left := room(); left < 0 || left > 8 {
		t.Fatalf("premise: the staged record is %d bytes short of its bound, want just under it", left)
	}
	if wrote, err := s.writeRecord(ctx, rec, 0); err != nil || wrote == 0 {
		t.Fatalf("premise: the record staged to just under its bound was written = %d, %v", wrote, err)
	}
	var found []lint.Finding
	for range 2 * MaxReportWarnings {
		found = append(found, lint.Finding{Path: widestPath(`"`), Line: math.MaxInt,
			Code: lint.CodeKitFileUnserved, Message: strings.Repeat("\x01", 64<<10)})
	}
	r := &run{s: s, stage: rec, warnings: found, counts: &FileCounts{Stored: math.MaxInt, Carried: math.MaxInt}}
	r.close(ctx, artifactsdomain.Published{Number: math.MaxInt, VersionID: uuid.New(),
		Result: statelog.Result{OpID: strings.Repeat(`"`, statelog.MaxCallerOpIDBytes)}}, artifacts.Manifest{})
	closed, _, err := s.readRecord(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != stateClosed || closed.Outcome == nil {
		t.Errorf("the publish closed its record as %q, outcome recorded %v; want it closed with its outcome",
			closed.State, closed.Outcome != nil)
	}
}

// A SIZE REFUSAL UNDOES NO EARLIER ATTEMPT'S OBJECTS: a retry of one publish
// whose files would outgrow the record is refused before it stores a byte,
// and the files an earlier attempt stored, whose outcome is unknown and which
// a version it landed may name, stay stored, dropped from the record so no
// later close or early release of it deletes them.
//
// Mutation: let fail delete every held generated file of the record, as it
// did before a run kept what it staged, and the earlier object is deleted.
func TestASizeRefusalUndoesNoEarlierAttemptsObjects(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := func() time.Time { return widestInstant }
	objects, err := objstore.NewStoreAt(memobj.NewAt(now), now)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{res: memory.NewFleet(), objects: objects, now: now}
	body := []byte("p { color: red }")
	earlier := file{Path: "style.css", ContentType: "text/css; charset=utf-8", SHA256: objstore.HashOf(body),
		Size: int64(len(body)), Key: objects.Mint(), Transport: transportGenerated, State: fileStored}
	if _, err := objects.PutMinted(ctx, earlier.Key, bytes.NewReader(body), earlier.Size,
		objstore.PutMeta{ContentType: earlier.ContentType}); err != nil {
		t.Fatal(err)
	}
	rec := &record{ID: uuid.NewString(), Principal: "ada.lovelace", Purpose: purposeStage, Created: widestInstant,
		Expires: widestInstant, State: stateFinalizing, FinalizeKey: "a-publish", Files: []file{earlier}}
	if wrote, err := s.writeRecord(ctx, rec, 0); err != nil || wrote == 0 {
		t.Fatalf("premise: write the earlier attempt's record = %d, %v", wrote, err)
	}
	var fresh []file
	for range 3 * artifacts.MaxFilesPerVersion {
		fresh = append(fresh, file{Path: widestPath(`"`), ContentType: earlier.ContentType, SHA256: earlier.SHA256,
			Size: earlier.Size, Key: objects.Mint(), Transport: transportGenerated, State: filePending})
	}
	r := &run{s: s, stage: rec}
	refusal := r.recordFresh(ctx, fresh, widestInstant)
	if !errors.Is(refusal, ErrInvalid) || !errors.Is(refusal, coord.ErrValueTooLarge) {
		t.Fatalf("premise: the retry's files were recorded = %v, want them refused as too large", refusal)
	}
	if got := r.fail(ctx, refusal); !errors.Is(got, refusal) {
		t.Fatalf("fail answered %v, want the refusal", got)
	}
	rc, err := objects.Open(ctx, earlier.object())
	if err != nil {
		t.Fatalf("the earlier attempt's object after the refusal: %v, want it stored", err)
	}
	_ = rc.Close()
	after, _, err := s.readRecord(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Files) != 0 {
		t.Errorf("the record holds %+v after the refusal, want the earlier attempt's file dropped", after.Files)
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
	case !strings.Contains(err.Error(), r.ID) || !strings.Contains(err.Error(), "new operation key"):
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
// instants, a direct put and an attempt at a uint32's widest.
//
// Mutation: raise MaxOutstandingUploads to 80, or lower
// coord.MaxUploadLedgerBytes below the widest, and this is red.
func TestTheWidestReservationLedgerFitsItsBound(t *testing.T) {
	t.Parallel()
	l := ledger{V: ledgerVersion}
	for range artifacts.CompanyPendingFactor * MaxOutstandingUploads {
		l.Holds = append(l.Holds, hold{Upload: uuid.NewString(), Principal: strings.Repeat("p", iam.MaxLogin),
			Bytes: math.MaxInt64, Until: widestInstant, Expires: widestInstant, Direct: true,
			Attempt: math.MaxUint32})
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
