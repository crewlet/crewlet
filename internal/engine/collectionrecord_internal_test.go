package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
)

// heldCollection is a coordination store holding one collection record.
type heldCollection []byte

func (h heldCollection) ObjectCollection(context.Context) ([]byte, bool, error) { return h, true, nil }

// THE WIDEST COLLECTION REPORT IS CUT TO ITS RECORD, KEEPING EVERY COUNT: a
// pass whose sentences and whose hundred unreadable files' names are each far
// past what the record keeps, every byte one JSON writes as six, against the
// widest store, is recorded as a small value: each sentence and name cut to
// its bound as stored, the list the longest opening of its files that fits,
// and every count whole, so a reader says how many it leaves out. A report
// that fits is kept whole.
//
// Mutation: record the files' list whole in encodeCollection, or name the
// store by its identity rather than its digest, and this is red.
func TestTheWidestCollectionReportIsCutToItsRecordKeepingEveryCount(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 24, 10, 0, 0, 123456789, time.UTC)
	words := strings.Repeat("<", 10_000)
	identity := "s3:https://" + strings.Repeat("😀", 2040) + "/" + strings.Repeat("😀", 255) + "/" +
		strings.Repeat("😀", 1024)
	found := &collect.AuditFindings{At: at, Completed: true, Referenced: 1 << 40, Missing: 1 << 40,
		Damaged: 1 << 40, Unanswered: 1 << 40}
	for i := range collect.MissingShown {
		found.MissingFiles = append(found.MissingFiles, collect.MissingFile{
			Object: objstore.KeyAt(at.Add(time.Duration(i))), NamedBy: words, Finding: collect.FindingUnanswered})
	}
	status := collect.Status{
		Collect: collect.CollectionReport{Listed: 1 << 40, Aged: 1 << 40, Deleted: 1 << 40,
			Referenced: 1 << 40, Abandoned: 1 << 40, Skipped: words, SweepError: words, At: at, Error: words},
		Audit: collect.AuditReport{Referenced: 1 << 40, Missing: 1 << 40, Damaged: 1 << 40,
			Unanswered: 1 << 40, At: at, Error: words, Found: found},
	}
	raw, err := encodeCollection(strings.Repeat("n", coord.MaxNodeIDBytes), identity, status)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > coord.SmallValueBytes {
		t.Fatalf("the widest collection report is %d bytes, past the %d its record holds", len(raw),
			coord.SmallValueBytes)
	}
	got, ok, err := ObjectCollection(t.Context(), heldCollection(raw), identity)
	if err != nil || !ok {
		t.Fatalf("the widest report reads back = %v, %v, want it found against its store", ok, err)
	}
	kept := got.Status.Audit.Found
	if got.Backend != identity || kept.Lost() != found.Lost() || got.Status.Collect.Listed != 1<<40 ||
		got.Status.Audit.Unanswered != 1<<40 {
		t.Errorf("the cut report kept %d lost of %d and listed %d, want every count whole", kept.Lost(),
			found.Lost(), got.Status.Collect.Listed)
	}
	if n := len(kept.MissingFiles); n == 0 || n >= collect.MissingShown {
		t.Errorf("the cut report names %d of its %d files, want the opening that fits", n, collect.MissingShown)
	}
	for i, m := range kept.MissingFiles {
		if m.Object != found.MissingFiles[i].Object || coord.TextBytes(m.NamedBy) > maxMissingNameBytes {
			t.Errorf("file %d is %s named in %d bytes, want %s within %d", i, m.Object,
				coord.TextBytes(m.NamedBy), found.MissingFiles[i].Object, maxMissingNameBytes)
		}
	}
	for _, w := range []string{got.Status.Collect.Error, got.Status.Collect.SweepError,
		got.Status.Collect.Skipped, got.Status.Audit.Error} {
		if coord.TextBytes(w) > maxCollectionWordsBytes {
			t.Errorf("a sentence is kept in %d bytes as stored, past %d", coord.TextBytes(w), maxCollectionWordsBytes)
		}
	}

	small := collect.Status{Audit: collect.AuditReport{Missing: 2, At: at, Found: &collect.AuditFindings{
		At: at, Missing: 2, MissingFiles: []collect.MissingFile{{Object: objstore.KeyAt(at), NamedBy: "ENG/a.md"},
			{Object: objstore.KeyAt(at.Add(1)), NamedBy: "ENG/b.md"}}}}}
	raw, err = encodeCollection("data-a", "nats", small)
	if err != nil {
		t.Fatal(err)
	}
	whole, _, _ := ObjectCollection(t.Context(), heldCollection(raw), "nats")
	if len(whole.Status.Audit.Found.MissingFiles) != 2 || whole.Status.Audit.Found.MissingFiles[1].NamedBy != "ENG/b.md" {
		t.Errorf("a report that fits reads back naming %+v, want it whole", whole.Status.Audit.Found.MissingFiles)
	}
	if _, ok, _ := ObjectCollection(t.Context(), heldCollection(raw), "s3:https://other.example/b/"); ok {
		t.Error("a report of another store was read as this one's")
	}
}
