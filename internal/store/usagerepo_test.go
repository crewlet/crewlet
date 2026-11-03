package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// unitFetch is one git fetch of the unit repository `platform` by principal,
// bound to seat ("" for nobody), that sent 100 bytes.
func unitFetch(principal, seat string) types.CodeFetched {
	return types.CodeFetched{RepoID: "repo-platform", RepoName: "platform",
		PrincipalKind: types.CodeReaderOperator, Principal: principal, Seat: seat,
		Via: types.FetchedViaGit, Reads: 1, Bytes: 100}
}

// A DAY COUNTS THE READS ITS RECORDS STAND FOR, not its records: a record is
// one reader's reads of a flush (internal/code/readaudit), so a CI token that
// fetched six times in two flushes is two records and six reads. A record that
// counts no read is no read — neither its bytes nor its instant are the
// reader's.
//
// Mutation: count the records (COUNT(*)), and the reader read three times;
// drop the filter on the count, and the record of none adds its bytes and its
// newer instant.
func TestADayCountsTheReadsItsRecordsStandFor(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	four, two, none := unitFetch("token:ci", ""), unitFetch("token:ci", ""), unitFetch("token:ci", "")
	four.Reads, four.Bytes = 4, 400
	two.Reads, two.Bytes = 2, 200
	none.Reads, none.Bytes = 0, 9999
	for i, rec := range []store.EventRecord{
		fetchRecord(t, "f-1", at(1), four),
		fetchRecord(t, "f-2", at(2), two),
		fetchRecord(t, "f-3", at(3), none),
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := db.UsageForDay(t.Context(), usageDayOf(day, day.Add(24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RepoReads) != 1 {
		t.Fatalf("the day's repository reads are %+v, want the CI token's alone", got.RepoReads)
	}
	if r := got.RepoReads[0]; r.Reads != 6 || r.Bytes != 600 || !r.LastAt.Equal(at(2)) {
		t.Errorf("the CI token read %d times, was sent %d bytes and last read at %s — want 6, 600 "+
			"and %s", r.Reads, r.Bytes, r.LastAt, at(2))
	}
}

// A DAY COUNTS ITS REPOSITORY READS, one entry per reader: per repository, per
// principal kind and principal, per way it was read and per seat it acted as,
// its reads counted, what they were sent summed and the newest kept — and the
// repository named as its newest read named it, a rename within the day
// folding its reader's two names into one entry. A read nobody can attribute
// is no entry, and a read landing moves the day's fingerprint, which is what
// makes the publisher derive the day again.
func TestADayCountsItsRepoReads(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }

	before, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	renamed := unitFetch("pat:maya", "maya")
	renamed.RepoName, renamed.Bytes = "platform-core", 50
	tool := types.CodeFetched{RepoID: "repo-platform", RepoName: "platform",
		PrincipalKind: types.CodeReaderAgent, Principal: "dev", Seat: "dev", Via: types.FetchedViaTool,
		Kind: "blob", Reads: 1, Bytes: 900}
	other := unitFetch("token:ci", "")
	other.RepoID, other.RepoName = "repo-billing", "billing"
	nobody := unitFetch("", "")
	for i, rec := range []store.EventRecord{
		fetchRecord(t, "f-1", at(1), unitFetch("pat:maya", "maya")),
		fetchRecord(t, "f-2", at(3), unitFetch("pat:maya", "maya")),
		// THE NEWEST read of the repository names it: a rename between two
		// of one reader's fetches is one entry under the new name.
		fetchRecord(t, "f-3", at(5), renamed),
		// THE SAME PERSON THROUGH ANOTHER CREDENTIAL is another reader.
		fetchRecord(t, "f-4", at(2), unitFetch("session:9f", "maya")),
		fetchRecord(t, "f-5", at(4), tool),
		fetchRecord(t, "f-6", at(4), other),
		fetchRecord(t, "f-7", at(4), nobody),
		// OUTSIDE THE WINDOW, an hour either side: another day's.
		fetchRecord(t, "f-8", day.Add(-time.Hour), unitFetch("pat:maya", "maya")),
		fetchRecord(t, "f-9", day.Add(25*time.Hour), unitFetch("pat:maya", "maya")),
	} {
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	after, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Error("the day's fingerprint did not move when its repository reads landed, so the " +
			"publisher would never derive the day again")
	}

	got, err := db.UsageForDay(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.UsageRepoRead{
		{RepoID: "repo-billing", RepoName: "billing", PrincipalKind: "operator", Principal: "token:ci",
			Via: "git", Reads: 1, Bytes: 100, LastAt: at(4)},
		{RepoID: "repo-platform", RepoName: "platform-core", PrincipalKind: "agent", Principal: "dev",
			Via: "tool", Seat: "dev", Reads: 1, Bytes: 900, LastAt: at(4)},
		{RepoID: "repo-platform", RepoName: "platform-core", PrincipalKind: "operator", Principal: "pat:maya",
			Via: "git", Seat: "maya", Reads: 3, Bytes: 250, LastAt: at(5)},
		{RepoID: "repo-platform", RepoName: "platform-core", PrincipalKind: "operator", Principal: "session:9f",
			Via: "git", Seat: "maya", Reads: 1, Bytes: 100, LastAt: at(2)},
	}
	if !slices.EqualFunc(got.RepoReads, want, func(a, b store.UsageRepoRead) bool {
		return a.RepoID == b.RepoID && a.RepoName == b.RepoName && a.PrincipalKind == b.PrincipalKind &&
			a.Principal == b.Principal && a.Via == b.Via && a.Seat == b.Seat && a.Reads == b.Reads &&
			a.Bytes == b.Bytes && a.LastAt.Equal(b.LastAt)
	}) {
		t.Errorf("the day's repository reads are\n %+v\nwant\n %+v", got.RepoReads, want)
	}
	if len(got.Seats) != 0 || len(got.People) != 0 {
		t.Errorf("repository reads made seats %+v and people %+v of the day", got.Seats, got.People)
	}
}
