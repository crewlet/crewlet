package usage_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// fetched records a record of reads of a unit repository the way the node's
// reads ledger publishes it (types.CodeFetched, internal/code/readaudit).
func (e *events) fetched(at time.Time, read types.CodeFetched) {
	e.t.Helper()
	e.seq++
	body, err := json.Marshal(read)
	if err != nil {
		e.t.Fatalf("encode a repository read: %v", err)
	}
	if err := e.db.Events().Append(e.t.Context(), store.EventRecord{
		ID: fmt.Sprintf("ev-%04d", e.seq), Type: read.EventType(), Category: "system", Time: at,
		Actor: read.Actor(), Tags: store.ExtractTags(body), Payload: body,
	}); err != nil {
		e.t.Fatalf("append a repository read: %v", err)
	}
}

// gitFetch is one fetch of repository repo by a person's credential.
func gitFetch(repo, principal, seat string, bytes int64) types.CodeFetched {
	return types.CodeFetched{RepoID: repo, RepoName: "name-of-" + repo,
		PrincipalKind: types.CodeReaderOperator, Principal: principal, Seat: seat,
		Via: types.FetchedViaGit, Reads: 1, Bytes: bytes}
}

// reader is one credential of person seat reading by git, reads times.
func reader(principal, seat string, reads int64) usage.RepoRead {
	return usage.RepoRead{PrincipalKind: "operator", Principal: principal, Seat: seat, Via: "git",
		Reads: reads, Bytes: reads * 100, LastAt: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)}
}

// repoReadRecord is repository repo's day on node, carrying readers.
func repoReadRecord(node, day, repo string, readers ...usage.RepoRead) usage.Record {
	return usage.Record{
		RecordEnvelope: usage.RecordEnvelope{Writer: node,
			Subject: usage.Subject{Kind: usage.KindRepoRead, Node: node, Day: day, Repo: repo}},
		RepoName:  "name-of-" + repo,
		RepoReads: readers,
	}
}

// readersOf is every reader of repo over from..to, read whole.
func readersOf(t *testing.T, db *store.DB, repo, from, to string) []usage.RepoReader {
	t.Helper()
	page, err := usage.RepoReadsPage(t.Context(), db.Replicated(),
		usage.RepoReadsQuery{RepoID: repo, From: from, To: to}, nil, 0)
	if err != nil {
		t.Fatalf("read the readers of %s: %v", repo, err)
	}
	if !page.Done {
		t.Fatalf("an unbounded read of %s's readers is not done", repo)
	}
	return page.Rows
}

// A REPOSITORY'S READERS ARE VERSION 2, AND ONLY THEY ARE.
//
// The kind is new, so a build at version 1 must retain its records rather
// than fault at a kind it has no applier for — and nothing else the domain
// writes may move with it, or the older half of a rolling upgrade would
// retain every seat's day too. And what the kind cannot carry is refused at
// the writer, before a node's applier could stop on it: a subject naming no
// repository, a seat's subject naming one, a day of no reader, more than a day
// holds, a reader this build cannot name, one named twice.
func TestARepoReadRecordIsVersionTwoAndCarriesOnlyItsReaders(t *testing.T) {
	t.Parallel()
	stamp := func(r usage.Record) (int, error) {
		t.Helper()
		body, err := r.Encode()
		if err != nil {
			return 0, err
		}
		env, err := usage.DecodeEnvelope(body)
		if err != nil {
			t.Fatalf("decode %s: %v", r.Subject, err)
		}
		return env.V, nil
	}
	if v, err := stamp(repoReadRecord("node-a", "2026-09-23", "repo-1", reader("pat:7", "maya", 2))); err != nil ||
		v != 2 {
		t.Errorf("a repository's readers are stamped %d (%v), want 2", v, err)
	}
	if v, err := stamp(seatRecord("node-a", "2026-09-23", "seat-1", "m")); err != nil || v != 1 {
		t.Errorf("a seat's day is stamped %d (%v), want the base format's 1", v, err)
	}
	for _, bad := range []usage.Subject{
		{Kind: usage.KindRepoRead, Node: "node-a", Day: "2026-09-23"},
		{Kind: usage.KindRepoRead, Node: "node-a", Day: "2026-09-23", Repo: "repo-1", Seat: "s"},
		{Kind: usage.KindSeat, Node: "node-a", Day: "2026-09-23", Seat: "s", Repo: "repo-1"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v validated", bad)
		}
	}
	many := make([]usage.RepoRead, usage.ReadersPerRepoDay+1)
	for i := range many {
		many[i] = reader(fmt.Sprintf("pat:%d", i), "maya", 1)
	}
	unknown := reader("pat:7", "maya", 1)
	unknown.PrincipalKind = "system"
	sideways := reader("pat:7", "maya", 1)
	sideways.Via = "mirror"
	long := reader(strings.Repeat("p", 256), "maya", 1)
	unread := reader("pat:7", "maya", 0)
	withTurns := repoReadRecord("node-a", "2026-09-23", "repo-1", reader("pat:7", "maya", 1))
	withTurns.Turns = &usage.Turns{Count: 1}
	seatWithReaders := seatRecord("node-a", "2026-09-23", "seat-1", "m")
	seatWithReaders.RepoReads = []usage.RepoRead{reader("pat:7", "maya", 1)}
	for name, r := range map[string]usage.Record{
		"no reader":               repoReadRecord("node-a", "2026-09-23", "repo-1"),
		"past the cap":            repoReadRecord("node-a", "2026-09-23", "repo-1", many...),
		"an unknown kind":         repoReadRecord("node-a", "2026-09-23", "repo-1", unknown),
		"an unknown way":          repoReadRecord("node-a", "2026-09-23", "repo-1", sideways),
		"a principal too long":    repoReadRecord("node-a", "2026-09-23", "repo-1", long),
		"a reader who never read": repoReadRecord("node-a", "2026-09-23", "repo-1", unread),
		"a reader named twice": repoReadRecord("node-a", "2026-09-23", "repo-1", reader("pat:7", "maya", 1),
			reader("pat:7", "maya", 2)),
		"a seat's turns beside the readers": withTurns,
		"a seat's day naming readers":       seatWithReaders,
	} {
		if _, err := r.Encode(); err == nil {
			t.Errorf("a repo_read record with %s was written", name)
		}
	}
}

// AN OLDER BUILD RETAINS A REPOSITORY'S READERS, under the identity their
// writer stated.
//
// A build at version 1 does not know the kind, so it files the record by the
// identity the record states (usage.RecordEnvelope.SubjectID) — and on this
// compacted domain a retained record supersedes whatever was retained under its
// subject, so two repositories read on one node's day must state two
// identities, each under that node and day, or the older build keeps the last
// one only and applies one repository's day after its upgrade. The stamp is
// what makes that build retain the record at all: it reads version 1, and the
// record is above it.
func TestAnOlderBuildRetainsARepoReadRecord(t *testing.T) {
	t.Parallel()
	ids := map[string]bool{}
	for _, repo := range []string{"repo-1", "repo-2"} {
		body := mustEncode(t, repoReadRecord("node-b", "2026-09-23", repo, reader("pat:7", "maya", 1)))
		env, err := usage.DecodeEnvelope(body)
		if err != nil {
			t.Fatal(err)
		}
		framework, err := usage.Domain{}.Envelope(body)
		if err != nil {
			t.Fatal(err)
		}
		if framework.ReadableBy(1) {
			t.Errorf("a build reading version 1 would apply %s's readers, which it has no applier for", repo)
		}
		want := coord.DocumentKey("node-b", "2026-09-23", repo)
		if env.SubjectID != want || framework.Subject.ID != want {
			t.Errorf("%s's readers state the identity %q (filed under %q), want %q — the node, the day "+
				"and the repository, which is what an older build files them under", repo, env.SubjectID,
				framework.Subject.ID, want)
		}
		if !strings.HasPrefix(env.SubjectID, coord.DocumentKey("node-b", "2026-09-23")+coord.KeySeparator) {
			t.Errorf("%s's identity %q is not under its own node and day, which an older build refuses",
				repo, env.SubjectID)
		}
		ids[env.SubjectID] = true
	}
	if len(ids) != 2 {
		t.Errorf("two repositories' days on one node state %d identities, want two", len(ids))
	}
}

// A REPOSITORY'S DAY IS REPLACED WHOLE, NEVER MOVED BACK, AND GOES WITH THE
// HORIZON.
//
// A record is the day's whole value, so a newer one replaces its readers — one
// gone from it is gone from the rows; an older or a repeated one changes
// nothing; and a record far enough past the day removes both its head and its
// readers.
//
// Mutation: reverse the head's guard, and the older record moves the day back.
func TestARepoReadRecordReplacesItsDay(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	const day = "2026-09-23"
	for i, rec := range []usage.Record{
		repoReadRecord("node-a", day, "repo-1", reader("pat:7", "maya", 2), reader("token:ci", "", 9)),
		repoReadRecord("node-a", day, "repo-1", reader("pat:7", "maya", 3)),
	} {
		if err := applyRecord(t.Context(), db, recordAt(t, rec, uint64(10+i))); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	got := readersOf(t, db, "repo-1", day, day)
	if len(got) != 1 || got[0].Principal != "pat:7" || got[0].Reads != 3 || got[0].Seat != "maya" {
		t.Fatalf("after the newer record the day's readers are %+v, want maya's one credential read 3 times", got)
	}
	for _, seq := range []uint64{9, 11} {
		if err := applyRecord(t.Context(), db, recordAt(t,
			repoReadRecord("node-a", day, "repo-1", reader("pat:old", "dev", 99)), seq)); err != nil {
			t.Fatalf("apply at %d: %v", seq, err)
		}
		if again := readersOf(t, db, "repo-1", day, day); !slices.Equal(again, got) {
			t.Fatalf("a record at %d moved the day to %+v", seq, again)
		}
	}
	late := seatRecord("node-a", "2027-06-01", "seat-1", "m")
	if err := applyRecord(t.Context(), db, recordAt(t, late, 20)); err != nil {
		t.Fatalf("apply the late record: %v", err)
	}
	rows := dump(t, db)
	if len(rows["usage_repo_reads"]) != 0 || len(rows["usage_repo_read_days"]) != 0 {
		t.Fatalf("a repository's day older than the history survived: %v / %v",
			rows["usage_repo_read_days"], rows["usage_repo_reads"])
	}
}

// A FULL REPOSITORY DAY FITS ONE MESSAGE. A record is published whole, so one
// the transport cannot carry is refused on every attempt and the day never
// replicates. [usage.ReadersPerRepoDay] is what keeps it small, and this
// measures it at its worst: every reader present, each principal the longest a
// reader may name and spelled in the bytes JSON escapes to six, each seat the
// longest handle, every figure at its widest. It has to fit the largest record
// the log declares, and it is held under the 2 MiB the cap's own comment
// states, so a record that outgrows the figure goes red here rather than
// leaving the prose to go stale.
//
// Mutation: raise ReadersPerRepoDay to 2048, and the record outgrows the
// documented figure.
func TestAFullRepoReadRecordFitsOneMessage(t *testing.T) {
	t.Parallel()
	const documented = 2 << 20
	at := time.Date(2026, 9, 23, 23, 59, 59, 999_999_999, time.UTC)
	r := repoReadRecord("node-b", "2026-09-23", uuid.NewString())
	r.RepoName = strings.Repeat("<", 255)
	for i := range usage.ReadersPerRepoDay {
		principal := fmt.Sprintf("%04d", i) + strings.Repeat("<", 251)
		r.RepoReads = append(r.RepoReads, usage.RepoRead{PrincipalKind: "operator", Principal: principal,
			Seat: strings.Repeat("s", 64), Via: "tool", Reads: math.MaxInt64, Bytes: math.MaxInt64, LastAt: at})
	}
	r.RepoReadersElided = math.MaxInt32
	encoded, err := r.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	t.Logf("a repository's day at the cap encodes to %d bytes (%.0f KiB)", len(encoded),
		float64(len(encoded))/1024)
	if int64(len(encoded)) > usage.LogMaxRecordBytes {
		t.Fatalf("a repository's day at the cap is %d bytes and the log takes a record of at most %d: it "+
			"could never be published", len(encoded), usage.LogMaxRecordBytes)
	}
	if len(encoded) >= documented {
		t.Errorf("a repository's day at the cap is %d bytes, past the %d usage.ReadersPerRepoDay states — "+
			"measure it again and correct both", len(encoded), documented)
	}
}

// THE MOST-READ READERS SURVIVE THE CAP, and the rest are counted.
//
// A repository read by more readers in a day than a record holds keeps the
// most-read — the question the record answers is who leans on the code — and
// says how many it dropped, so the answer over a range is never silently
// short.
//
// Mutation: rank the cap by principal rather than by reads, and readers read
// once outrank ones read many times.
func TestTheMostReadReadersSurviveTheCap(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	const extra = 6
	// THE ONCE-READ come first by principal, so a cap ranked by key would
	// keep them; the busy ones are read twice.
	for i := range usage.ReadersPerRepoDay + extra {
		ev.fetched(noon.Add(-time.Duration(i)*time.Second),
			gitFetch("repo-1", fmt.Sprintf("pat:%04d", i), "", 10))
		if i >= extra {
			ev.fetched(noon.Add(-time.Duration(i)*time.Second-time.Millisecond),
				gitFetch("repo-1", fmt.Sprintf("pat:%04d", i), "", 10))
		}
	}
	p, _ := newPublisher(t, "node-a", own, own, noon)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := readersOf(t, own, "repo-1", "2026-09-23", "2026-09-23")
	if len(got) != usage.ReadersPerRepoDay {
		t.Fatalf("the day keeps %d readers, want the cap's %d", len(got), usage.ReadersPerRepoDay)
	}
	for _, r := range got {
		if r.Reads != 2 {
			t.Fatalf("reader %s, read %d times, survived the cap over readers read twice", r.Principal, r.Reads)
		}
	}
	elided, err := usage.RepoReadsElided(t.Context(), own.Replicated(),
		usage.RepoReadsQuery{RepoID: "repo-1", From: "2026-09-23", To: "2026-09-23"})
	if err != nil || elided != extra {
		t.Fatalf("the day elided %d readers (%v), want %d", elided, err, extra)
	}
}

// A REPOSITORY'S READERS SUM ACROSS NODES AND DAYS.
//
// Each node publishes the reads it served, so one person's reads through two
// nodes over two days are four rows — and one reader in the answer, its reads
// and bytes summed and its newest read the latest; a person's two credentials
// are two readers naming one seat; a range names only its own days; and the
// answer is read off every node's rows, whichever node answers.
func TestRepoReadersSumAcrossNodesAndDays(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	later := func(r usage.RepoRead, d time.Duration) usage.RepoRead {
		r.LastAt = r.LastAt.Add(d)
		return r
	}
	for i, rec := range []usage.Record{
		repoReadRecord("node-a", "2026-09-22", "repo-1", reader("pat:7", "maya", 2)),
		repoReadRecord("node-b", "2026-09-22", "repo-1", reader("pat:7", "maya", 1),
			reader("session:9f", "maya", 4)),
		repoReadRecord("node-a", "2026-09-23", "repo-1", later(reader("pat:7", "maya", 3), 24*time.Hour)),
		repoReadRecord("node-b", "2026-09-24", "repo-1", reader("pat:7", "maya", 50)),
		repoReadRecord("node-a", "2026-09-23", "repo-2", reader("pat:7", "maya", 8)),
	} {
		if err := applyRecord(t.Context(), db, recordAt(t, rec, uint64(i+1))); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	got := readersOf(t, db, "repo-1", "2026-09-22", "2026-09-23")
	want := []usage.RepoReader{
		{PrincipalKind: "operator", Principal: "pat:7", Seat: "maya", Via: "git", Reads: 6, Bytes: 600,
			LastAt: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)},
		{PrincipalKind: "operator", Principal: "session:9f", Seat: "maya", Via: "git", Reads: 4, Bytes: 400,
			LastAt: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)},
	}
	if !slices.EqualFunc(got, want, func(a, b usage.RepoReader) bool {
		return a.PrincipalKind == b.PrincipalKind && a.Principal == b.Principal && a.Seat == b.Seat &&
			a.Via == b.Via && a.Reads == b.Reads && a.Bytes == b.Bytes && a.LastAt.Equal(b.LastAt)
	}) {
		t.Errorf("repo-1's readers over two days are\n %+v\nwant\n %+v", got, want)
	}
	if _, err := usage.RepoReadsPage(t.Context(), db.Replicated(), usage.RepoReadsQuery{RepoID: "repo-1",
		From: "2026-01-01", To: "2026-09-23"}, nil, 0); err == nil {
		t.Error("a range longer than the history was read")
	}
}

// A PAGE OF READERS IS CUT BETWEEN READERS, and resumes where the page before
// it ended: walked a reader at a time, every reader is read once and none
// repeats, in reader order, across principal kinds — a seat reading by a tool
// beside persons and tokens fetching.
//
// Mutation: resume a principal kind without the keyset over the rest of its
// readers, and the walk repeats the reader it resumed at for ever.
func TestRepoReadersPageByReader(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	agent := usage.RepoRead{PrincipalKind: "agent", Principal: "dev", Seat: "dev", Via: "tool", Reads: 5,
		Bytes: 900, LastAt: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)}
	readers := []usage.RepoRead{agent, reader("pat:1", "maya", 1), reader("pat:2", "maya", 2),
		reader("token:ci", "", 3)}
	viaTool := reader("pat:1", "maya", 7)
	viaTool.Via = "tool"
	readers = append(readers, viaTool)
	for i, node := range []string{"node-a", "node-b"} {
		if err := applyRecord(t.Context(), db, recordAt(t,
			repoReadRecord(node, "2026-09-23", "repo-1", readers...), uint64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	whole := readersOf(t, db, "repo-1", "2026-09-23", "2026-09-23")
	if len(whole) != 5 {
		t.Fatalf("the whole read names %d readers, want 5: %+v", len(whole), whole)
	}
	var walked []usage.RepoReader
	var after usage.Group
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the walk did not end")
		}
		page, err := usage.RepoReadsPage(t.Context(), db.Replicated(),
			usage.RepoReadsQuery{RepoID: "repo-1", From: "2026-09-23", To: "2026-09-23"}, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		walked = append(walked, page.Rows...)
		if page.Done {
			break
		}
		after = page.Next
	}
	if !slices.Equal(walked, whole) {
		t.Errorf("walked a reader a page the readers are\n %+v\nread whole\n %+v", walked, whole)
	}
	if whole[0].PrincipalKind != "agent" || whole[0].Reads != 10 {
		t.Errorf("the first reader is %+v, want the seat, read 10 times over two nodes", whole[0])
	}
}

// A UNIT REPOSITORY'S READERS ARE PUBLISHED WITH THEIR DAY: the tick that
// derives a day publishes one record for each repository read on it, beside the
// seats' — named as its newest read named it — and a reader this build cannot
// name, a newer peer's, is counted with the elided rather than dropped
// silently or refused with the whole day.
//
// Mutation: leave the repository reads out of [usage.Publisher.Records], and
// the day's readers never reach the replicated rows.
func TestAUnitRepositorysReadersArePublishedWithTheirDay(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.fetched(noon.Add(-3*time.Hour), gitFetch("repo-1", "pat:7", "maya", 100))
	renamed := gitFetch("repo-1", "pat:7", "maya", 50)
	renamed.RepoName = "platform-core"
	ev.fetched(noon.Add(-time.Hour), renamed)
	newer := gitFetch("repo-1", "mirror:1", "", 10)
	newer.PrincipalKind = "system"
	ev.fetched(noon.Add(-2*time.Hour), newer)
	p, log := newPublisher(t, "node-a", own, own, noon)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if log.sent() != 1 {
		t.Fatalf("the tick sent %d record(s), want the repository's day", log.sent())
	}
	got := readersOf(t, own, "repo-1", "2026-09-23", "2026-09-23")
	if len(got) != 1 || got[0].Principal != "pat:7" || got[0].Reads != 2 || got[0].Bytes != 150 {
		t.Fatalf("after one tick repo-1's readers are %+v, want maya's credential read twice", got)
	}
	elided, err := usage.RepoReadsElided(t.Context(), own.Replicated(),
		usage.RepoReadsQuery{RepoID: "repo-1", From: "2026-09-23", To: "2026-09-23"})
	if err != nil || elided != 1 {
		t.Fatalf("the newer peer's reader is counted %d (%v), want 1 elided", elided, err)
	}
	held, ok, err := usage.Applied(t.Context(), own.Replicated(), usage.Subject{Kind: usage.KindRepoRead,
		Node: "node-a", Day: period.At(period.Day, noon, santiago).Label, Repo: "repo-1"})
	if err != nil || !ok || held.RepoName != "platform-core" {
		t.Fatalf("the day's record is %+v (%v, %v), want it named platform-core, its newest read's name",
			held, ok, err)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if log.sent() != 1 {
		t.Fatalf("an unchanged repository's day was sent again (%d more)", log.sent()-1)
	}
}

// A REPOSITORY'S DAY IS PUBLISHED JOINED WITH THE FLEET'S RECORD.
//
// A node whose event log lost rows its record counted — a restore beside a
// newer usage log — derives a shorter day, and published alone it would
// replace the fleet's record with less. So what goes out is the join: every
// reader either holds, each at the larger of its figures; and the readers the
// derivation lost are kept.
//
// Mutation: publish the derivation alone, and the reader the restore lost is
// gone from the fleet.
func TestARepoReadJoinsTheFleetsRecord(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	day := period.At(period.Day, noon, santiago).Label
	// THE FLEET'S RECORD, published before the restore: two readers.
	if err := applyRecord(t.Context(), own, recordAt(t, repoReadRecord("node-a", day, "repo-1",
		reader("pat:7", "maya", 5), reader("token:ci", "", 2)), 1)); err != nil {
		t.Fatal(err)
	}
	// THE RESTORED LOG holds one of maya's reads and none of the token's.
	ev := &events{t: t, db: own}
	ev.fetched(noon.Add(-time.Hour), gitFetch("repo-1", "pat:7", "maya", 100))
	p, log := newPublisher(t, "node-a", own, own, noon)
	// THE LOG IS PAST THE FLEET'S RECORD, as the stream is after a restore: the
	// publish lands above it rather than at its position, where the guard
	// would discard it and the case would pass whatever the publish carried.
	log.seq = 1
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if log.sent() != 1 {
		t.Fatalf("the tick sent %d record(s), want the repository's day", log.sent())
	}
	got := readersOf(t, own, "repo-1", day, day)
	if len(got) != 2 || got[0].Principal != "pat:7" || got[0].Reads != 5 || got[1].Principal != "token:ci" ||
		got[1].Reads != 2 {
		t.Fatalf("after the short derivation the fleet holds %+v, want both readers at the fleet's figures", got)
	}
}

// A READERS QUESTION THE READ CANNOT TAKE IS REFUSED IN ITS CALLER'S WORDS:
// every range the read refuses — a label that is not a day, a range that ends
// before it starts, one longer than the records keep, a question naming no
// repository — is usage.ErrBadRange, and every cursor no page issued
// usage.ErrBadCursor, each with a sentence past its sentinel naming what to
// change, the words the readers route and tool quote alone. The longest range
// the records keep is answered, and a page's own cursor is the group it ended
// on.
//
// Mutation: let ValidRange take a range one day longer, and the first refusal
// is answered.
func TestAReadersQuestionIsRefusedInItsCallersWords(t *testing.T) {
	t.Parallel()
	to, err := period.Parse(period.Day, "2026-09-23", nil)
	if err != nil {
		t.Fatal(err)
	}
	longest := to.Shift(-(usage.RepoReadsMaxDays - 1)).Label
	if err := usage.ValidRange(longest, to.Label); err != nil {
		t.Errorf("the %d days the records keep, %s..%s, were refused: %v", usage.RepoReadsMaxDays, longest,
			to.Label, err)
	}
	said := func(err, sentinel error) string {
		_, rest, _ := strings.Cut(err.Error(), sentinel.Error()+": ")
		return rest
	}
	cursor := func(s string) error {
		_, err := usage.ParseCursor(s)
		return err
	}
	for _, tc := range []struct {
		name     string
		err      error
		sentinel error
		says     string
	}{
		{"a range a day longer than the records keep",
			usage.ValidRange(to.Shift(-usage.RepoReadsMaxDays).Label, to.Label), usage.ErrBadRange, "longer than"},
		{"a range that ends before it starts", usage.ValidRange("2026-09-10", "2026-09-01"), usage.ErrBadRange,
			"ends before it starts"},
		{"a from that is not a day", usage.ValidRange("yesterday", "2026-09-01"), usage.ErrBadRange,
			`from "yesterday" is not a day`},
		{"a to the calendar does not have", usage.ValidRange("2026-02-01", "2026-02-30"), usage.ErrBadRange,
			`to "2026-02-30" is not a day`},
		{"a question naming no repository",
			usage.RepoReadsQuery{From: "2026-09-01", To: "2026-09-23"}.Validate(), usage.ErrBadRange,
			"names no repository"},
		{"a cursor that does not decode", cursor("not a cursor!"), usage.ErrBadCursor, "does not decode"},
		{"a cursor naming no group", cursor(base64.RawURLEncoding.EncodeToString([]byte(`{"a":1}`))),
			usage.ErrBadCursor, "names no page's end"},
	} {
		if !errors.Is(tc.err, tc.sentinel) || !strings.Contains(said(tc.err, tc.sentinel), tc.says) {
			t.Errorf("%s was refused %v, want %v saying %q past it", tc.name, tc.err, tc.sentinel, tc.says)
		}
	}
	group := usage.Group{"operator", "pat:7", "tool", "maya"}
	if got, err := usage.ParseCursor(group.Cursor()); err != nil || !slices.Equal(got, group) {
		t.Errorf("a page's own cursor parsed to %v (%v), want %v", got, err, group)
	}
	if got, err := usage.ParseCursor(""); err != nil || got != nil {
		t.Errorf("no cursor parsed to %v (%v), want the start", got, err)
	}
}
