package gitlet_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PERSON'S VIEWED FILES (viewed.go): a gesture over a change's files decided on the person's own
// subject, each file kept as two digests under its repository's key, and the read of their own.

// bea is a person holding the seat cto: a second person beside ada, who holds founder.
var bea = gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "pat:bea", Seat: "cto"}

// The object ids and modes a case marks files at.
var (
	blobX, blobY, blobZ = oidOf("X"), oidOf("Y"), oidOf("Z")
)

// viewedChange is one change a viewed case marks the files of.
type viewedChange struct {
	repo, id string
	number   int64
	version  int64
	at       statelog.Position
}

// viewedRepo is a fresh repository with a change open by dev from each branch into main, in order.
func viewedRepo(t *testing.T, n *node, name string, branches ...string) []viewedChange {
	t.Helper()
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, name)
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/topic", gitlet.ZeroOID, oidOf("T"))))
	out := make([]viewedChange, 0, len(branches))
	for _, branch := range branches {
		opened, err := c.ProposeChange(t.Context(), opening(repo, branch, mainRef, dev))
		requireChanged(t, opened, err)
		out = append(out, viewedChange{repo: repo, id: opened.ChangeID, number: opened.Number,
			version: opened.Version, at: opened.Position})
	}
	return out
}

// viewing is a gesture over change's files by who for handle, marking viewed and taking unviewed back.
func viewing(who gitlet.Principal, handle string, change viewedChange, viewed []gitlet.ViewedFile,
	unviewed ...string) gitlet.MarkWrite {

	return gitlet.MarkWrite{OpID: statelog.NewOpID(time.Now(), "code_mark_viewed"), Asker: who, Handle: handle,
		Seen: change.at, Files: &gitlet.ViewedGesture{RepoID: change.repo, ChangeID: change.id, Viewed: viewed,
			Unviewed: unviewed}}
}

// file is path marked at oid and mode.
func file(path, oid, mode string) gitlet.ViewedFile {
	return gitlet.ViewedFile{Path: path, BlobOID: oid, Mode: mode}
}

// markFiles asks m for w and requires it applied at a record of its own.
func markFiles(t *testing.T, m *gitlet.Marks, w gitlet.MarkWrite) gitlet.MarkResult {
	t.Helper()
	res, err := m.Mark(t.Context(), w)
	if err != nil || res.Refusal != "" || res.Outcome != statelog.OutcomeApplied || res.Unchanged ||
		res.Position.IsZero() {
		t.Fatalf("the mark %+v answered %+v, %v; want it applied at a record", w.Files, res, err)
	}
	return res
}

// viewedKeyOf is repo's viewed key as n holds it, "" for none.
func viewedKeyOf(t *testing.T, n *node, repo string) gitlet.ViewedKey {
	t.Helper()
	var secret string
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		err := tx.QueryRowContext(t.Context(), `SELECT secret FROM code_viewed_keys WHERE repo_id = ?`,
			repo).Scan(&secret)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}); err != nil {
		t.Fatalf("read %s's viewed key: %v", repo, err)
	}
	return gitlet.ViewedKey(secret)
}

// readViewed is a page of who's viewed files of change number of repo.
func readViewed(t *testing.T, n *node, who gitlet.Principal, repo string, number int64, limit int,
	cursor string) (gitlet.ReadAnswer, error) {

	t.Helper()
	return n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: who, Kind: gitlet.ReadChange, Repo: repo,
		Change: number, Viewed: true, Limit: limit, Cursor: cursor, Fresh: stale})
}

// bodyAt is the record at p's bytes on n's log, and the broker's instant for it.
func bodyAt(t *testing.T, n *node, p statelog.Position) ([]byte, time.Time) {
	t.Helper()
	_, body, stored, ok, err := n.records.At(t.Context(), p.Seq)
	if err != nil || !ok {
		t.Fatalf("read the record at %s: %v (held: %v)", p, err, ok)
	}
	return body, stored
}

// viewedPayload is the viewed record at p's payload.
func viewedPayload(t *testing.T, n *node, p statelog.Position) gitlet.ViewedMarks {
	t.Helper()
	payload, err := gitlet.DecodeMutation(n.recordAt(p))
	marks, ok := payload.(gitlet.ViewedMarks)
	if err != nil || !ok {
		t.Fatalf("the record at %s carries %T (%v), want viewed marks", p, payload, err)
	}
	return marks
}

// A VIEWED MARK IS THE PERSON'S OWN: a seat marking its own files and founder's, a person marking
// another's, and a credential bound to no seat are each refused not_permitted, landing nothing.
//
// Mutation: compare the asker's kind alone, and ada's mark of cto's files lands, red.
func TestAViewedMarkIsThePersonsOwn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef)[0]
	m := markerOn(t, n, n.pub)
	files := []gitlet.ViewedFile{file("a.go", blobX, "100644")}
	before := n.end()
	for name, w := range map[string]gitlet.MarkWrite{
		"a seat's own":           viewing(dev, "dev", web, files),
		"a seat marking founder": viewing(dev, "founder", web, files),
		"a person marking cto's": viewing(ada, "cto", web, files),
		"an unbound credential":  viewing(unbound, "founder", web, files),
	} {
		res, err := m.Mark(t.Context(), w)
		if err != nil || res.Refusal != gitlet.MarkNotPermitted || !strings.Contains(res.Detail, "viewed files") {
			t.Errorf("%s: the mark answered %+v, %v; want it refused not_permitted", name, res, err)
		}
	}
	if got := n.end(); got != before {
		t.Fatalf("refused marks appended %d records", got-before)
	}
}

// THE FIRST VIEWED MARK OF A REPOSITORY RECORDS ITS KEY: founder's first mark lands a viewed_key
// record on the repository's subject, decided by the node as the system under the gesture's step op
// id and stamped ViewedVersion, then the viewed record on founder's; bea's next lands one record.
// Two persons' concurrent first marks on another repository land one key, both digesting under it.
//
// Mutations: skip the key step, and the first mark errs, red; drop the key publish's arbitration,
// and two key records land, red.
func TestTheFirstViewedMarkOfARepositoryRecordsItsKey(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef, releaseRef)
	m := markerOn(t, n, n.pub)
	before := n.end()
	w := viewing(ada, "founder", web[0], []gitlet.ViewedFile{file("a.go", blobX, "100644")})
	res := markFiles(t, m, w)
	if got := n.end(); got != before+2 || res.Position.Seq != before+2 {
		t.Fatalf("the first mark landed %d records and answered %s; want the key's and the mark's, at the second",
			got-before, res.Position)
	}
	keyAt := statelog.Position{Stream: res.Position.Stream, Generation: res.Position.Generation, Seq: before + 1}
	rec := n.recordAt(keyAt)
	payload, err := gitlet.DecodeMutation(rec)
	keyed, ok := payload.(gitlet.ViewedKeyRecord)
	switch {
	case err != nil || !ok:
		t.Fatalf("the first record carries %T (%v), want a viewed key", payload, err)
	case rec.Op != gitlet.OpViewedKey || rec.Subject != gitlet.RepoSubject(web[0].repo) ||
		!slices.Equal([]string(rec.Scope), []string{gitlet.RepoPath(web[0].repo)}) || rec.V != gitlet.ViewedVersion ||
		rec.Actor != n.id || rec.ActorKind != gitlet.ActorSystem || rec.OpID != statelog.StepOpID(w.OpID, "viewed-key"):
		t.Fatalf("the key's record is %+v, want viewed_key on the repository at its path, stamped %d, by the "+
			"node as the system under the gesture's step id", rec.RecordEnvelope, gitlet.ViewedVersion)
	case !keyed.Key.Valid() || keyed.Key != viewedKeyOf(t, n, web[0].repo):
		t.Fatal("the key record's key is not the valid key the repository holds")
	}
	before = n.end()
	markFiles(t, m, viewing(bea, "cto", web[1], []gitlet.ViewedFile{file("b.go", blobX, "100644")}))
	if got := n.end(); got != before+1 {
		t.Fatalf("a mark on a keyed repository landed %d records, want one", got-before)
	}

	api := viewedRepo(t, n, "api", featureRef)[0]
	var wg sync.WaitGroup
	results := make([]gitlet.MarkResult, 2)
	for i, who := range []gitlet.Principal{ada, bea} {
		wg.Go(func() {
			r, markErr := m.Mark(t.Context(), viewing(who, who.Seat, api, []gitlet.ViewedFile{file("a.go", blobX,
				"100644")}))
			if markErr != nil {
				t.Errorf("%s's mark: %v", who.Seat, markErr)
			}
			results[i] = r
		})
	}
	wg.Wait()
	key := viewedKeyOf(t, n, api.repo)
	if keys := n.count(`SELECT COUNT(*) FROM code_viewed_keys WHERE repo_id = ?`, api.repo); keys != 1 {
		t.Fatalf("api holds %d keys", keys)
	}
	records := 0
	for seq := before + 2; seq <= n.end(); seq++ {
		if n.recordAt(statelog.Position{Stream: res.Position.Stream, Generation: res.Position.Generation,
			Seq: seq}).Op == gitlet.OpViewedKey {
			records++
		}
	}
	if records != 1 {
		t.Fatalf("two first marks at once landed %d viewed_key records, want one", records)
	}
	for _, r := range results {
		if marks := viewedPayload(t, n, r.Position).Viewed; len(marks) != 1 || marks[0].PathDigest != key.PathDigest("a.go") {
			t.Errorf("a mark landed %+v, want a.go digested under api's one key", marks)
		}
	}
}

// A VIEWED MARK IS DECIDED ON THE PERSON'S RECORD: one viewed record on founder's subject, under
// their viewed path alone, by founder through ada's credential, stamped ViewedVersion, holding each
// file's digests under the repository's key and never its path, object id, mode, key or bare hash;
// the read answers both marks in digest order at the record's instant; the same gesture again lands
// nothing and answers where it was marked; and an inbox mark still lands as marks, stamped the base.
//
// Mutations: drop a versioned row (stamped 1, red); scope it under PersonPath (red); carry the path,
// the id or the mode beside the digests (red).
func TestAViewedMarkIsDecidedOnThePersonsRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef)[0]
	m := markerOn(t, n, n.pub)
	first := markFiles(t, m, viewing(ada, "founder", web, []gitlet.ViewedFile{file("a.go", blobX, "100644"),
		file("b.go", blobZ, "100755")}))
	rec := n.recordAt(first.Position)
	if rec.Op != gitlet.OpViewed || rec.Subject != gitlet.PersonSubject("founder") ||
		!slices.Equal([]string(rec.Scope), []string{gitlet.ViewedPath("founder")}) || rec.Actor != "founder" ||
		rec.ActorKind != gitlet.ActorOperator || rec.OperatorID != "pat:ada" || rec.V != gitlet.ViewedVersion {
		t.Fatalf("the mark's record is %+v, want viewed on person.founder under its viewed path, by founder "+
			"through pat:ada, stamped %d", rec.RecordEnvelope, gitlet.ViewedVersion)
	}
	key := viewedKeyOf(t, n, web.repo)
	body, stored := bodyAt(t, n, first.Position)
	bare := sha256.Sum256([]byte("a.go"))
	for _, want := range []string{key.PathDigest("a.go"), key.FileDigest("a.go", blobX, "100644"),
		key.PathDigest("b.go"), key.FileDigest("b.go", blobZ, "100755")} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("the record does not carry %s", want)
		}
	}
	for _, never := range []string{`"a.go"`, blobX, "100644", string(key), hex.EncodeToString(bare[:])} {
		if bytes.Contains(body, []byte(never)) {
			t.Errorf("the record carries %q in the clear:\n%s", never, body)
		}
	}
	answer, err := readViewed(t, n, ada, "web", web.number, 0, "")
	want := []string{key.PathDigest("a.go"), key.PathDigest("b.go")}
	slices.Sort(want)
	if err != nil || len(answer.Viewed) != 2 || answer.Viewed[0].PathDigest != want[0] ||
		answer.Viewed[1].PathDigest != want[1] || answer.ViewedKey != key {
		t.Fatalf("founder's viewed files read %+v (key matches %v), %v; want both marks in digest order with "+
			"the key", answer.Viewed, answer.ViewedKey == key, err)
	}
	for _, row := range answer.Viewed {
		if !row.At.Equal(stored.Truncate(time.Microsecond)) {
			t.Errorf("a mark reads marked at %s, want the record's broker instant %s", row.At, stored)
		}
	}
	before := n.end()
	again, err := m.Mark(t.Context(), viewing(ada, "founder", web, []gitlet.ViewedFile{file("a.go", blobX, "100644")}))
	if err != nil || !again.Unchanged || again.Position != first.Position || n.end() != before {
		t.Fatalf("the same mark again answered %+v, %v; want unchanged at %s, nothing landed", again, err,
			first.Position)
	}
	through := first.Position
	inbox := mark(t, m, marking(gitlet.MarkGesture{ReadThrough: &through}))
	if r := n.recordAt(inbox.Position); r.Op != gitlet.OpMarks || r.V != 1 ||
		!slices.Equal([]string(r.Scope), []string{gitlet.PersonPath("founder")}) {
		t.Fatalf("an inbox mark landed %+v, want marks under the person's path, stamped the base", r.RecordEnvelope)
	}
}

// A VIEWED DIGEST IS KEYED BY ITS REPOSITORY: one path at one id and mode marked on two repositories
// digests differently on each, each under its own repository's key, and neither is the bare SHA-256.
//
// Mutations: one key for every repository, red; a bare SHA-256, red.
func TestAViewedDigestIsKeyedByItsRepository(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef)[0]
	api := viewedRepo(t, n, "api", featureRef)[0]
	m := markerOn(t, n, n.pub)
	files := []gitlet.ViewedFile{file("a.go", blobX, "100644")}
	onWeb := viewedPayload(t, n, markFiles(t, m, viewing(ada, "founder", web, files)).Position).Viewed[0]
	onAPI := viewedPayload(t, n, markFiles(t, m, viewing(ada, "founder", api, files)).Position).Viewed[0]
	bare := sha256.Sum256([]byte("a.go"))
	webKey, apiKey := viewedKeyOf(t, n, web.repo), viewedKeyOf(t, n, api.repo)
	switch {
	case onWeb.PathDigest == onAPI.PathDigest || onWeb.FileDigest == onAPI.FileDigest:
		t.Fatalf("a.go digests alike on two repositories: %+v and %+v", onWeb, onAPI)
	case onWeb.PathDigest != webKey.PathDigest("a.go") || onAPI.PathDigest != apiKey.PathDigest("a.go") ||
		onWeb.FileDigest != webKey.FileDigest("a.go", blobX, "100644"):
		t.Fatal("a mark is not digested under its own repository's key")
	case onWeb.PathDigest == hex.EncodeToString(bare[:]):
		t.Fatal("a path's digest is its bare SHA-256")
	}
}

// A VIEWED MARK IS DECIDED WHERE THE REPOSITORY IS CURRENT: a node holding a record back on web's
// repository declines founder's mark of web's files, ErrNotServing for ReasonDeferred, with nothing
// appended, whether or not web holds a key; one held back on another repository leaves it landing,
// its record declaring the viewed path alone; and a gesture naming a change under another
// repository's id is refused invalid naming both.
//
// Mutations: drop RepoPath from the mark's request scope, and the held-back node decides, red;
// declare it in the record's scope, red.
func TestAViewedMarkIsDecidedWhereTheRepositoryIsCurrent(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef)[0]
	api := viewedRepo(t, n, "api", featureRef)[0]
	m := markerOn(t, n, n.pub)
	hold := func(repo string) {
		n.exec(`INSERT INTO code_log_deferred (position, subject, subject_kind, subject_id, version, payload,
			stored_at) VALUES (999999, ?, 'repo', ?, 9, x'7b7d', 0)`, "crewlet.code.log.repo."+repo, repo)
		n.exec(`INSERT INTO code_log_deferred_scope (position, path) VALUES (999999, ?)`, gitlet.RepoPath(repo))
	}
	release := func() {
		n.exec(`DELETE FROM code_log_deferred_scope WHERE position = 999999`)
		n.exec(`DELETE FROM code_log_deferred WHERE position = 999999`)
	}
	files := []gitlet.ViewedFile{file("a.go", blobX, "100644")}
	declined := func(name string) {
		t.Helper()
		before := n.end()
		_, err := m.Mark(t.Context(), viewing(ada, "founder", web, files))
		var refusal *statelog.Unavailable
		if !errors.Is(err, gitlet.ErrNotServing) || !errors.As(err, &refusal) ||
			refusal.Reason != statelog.ReasonDeferred || n.end() != before {
			t.Fatalf("%s: a mark on a repository held back answered %v, appending %d; want ErrNotServing for "+
				"a deferral, nothing appended", name, err, n.end()-before)
		}
	}
	hold(web.repo)
	declined("keyless")
	release()
	markFiles(t, m, viewing(ada, "founder", web, files))
	hold(web.repo)
	declined("keyed")
	release()

	hold(api.repo)
	res := markFiles(t, m, viewing(ada, "founder", web, []gitlet.ViewedFile{file("b.go", blobX, "100644")}))
	release()
	if scope := n.recordAt(res.Position).Scope; !slices.Equal([]string(scope), []string{gitlet.ViewedPath("founder")}) {
		t.Fatalf("the viewed record declares %v, want the person's viewed path alone", scope)
	}
	before := n.end()
	crossed := viewing(ada, "founder", web, files)
	crossed.Files.RepoID = api.repo
	got, err := m.Mark(t.Context(), crossed)
	if err != nil || got.Refusal != gitlet.MarkInvalid || !strings.Contains(got.Detail, "api") ||
		!strings.Contains(got.Detail, web.id) || n.end() != before {
		t.Fatalf("a gesture naming web's change under api answered %+v, %v; want invalid naming both", got, err)
	}
}

// A MARK IS CURRENT FOR THE ID AND MODE IT WAS MADE AT: a push moving the change's head leaves the
// row as it was, so a screen whose revision gives the file another id or mode digests another file
// digest and reads it unviewed; marking it again at a new mode or id moves the one row; a deleted
// file is marked with neither and reads back as the path with both empty; and unviewed takes a mark
// back. A re-mark at a new id or mode moves the row to its record's position and broker instant.
//
// Mutations: key the row by the file digest too (two rows, red); leave file_digest out of the
// upsert's SET (the digest stays, red); leave position or at out of it (red).
func TestAMarkIsCurrentForTheIDAndModeItWasMadeAt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef)[0]
	m := markerOn(t, n, n.pub)
	markFiles(t, m, viewing(ada, "founder", web, []gitlet.ViewedFile{file("a.go", blobX, "100644"),
		file("b.go", blobX, "100644")}))
	key := viewedKeyOf(t, n, web.repo)
	held := func() map[string]string {
		t.Helper()
		answer, err := readViewed(t, n, ada, "web", web.number, 0, "")
		if err != nil {
			t.Fatalf("read founder's viewed files: %v", err)
		}
		out := map[string]string{}
		for _, row := range answer.Viewed {
			out[row.PathDigest] = row.FileDigest
		}
		return out
	}
	n.push(web.repo, entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("B"), oidOf("C"))))
	if got := held()[key.PathDigest("a.go")]; got != key.FileDigest("a.go", blobX, "100644") {
		t.Fatalf("a push moved a.go's mark to %s", got)
	}
	for _, step := range []gitlet.ViewedFile{file("a.go", blobX, "100755"), file("a.go", blobY, "100755")} {
		res := markFiles(t, m, viewing(ada, "founder", web, []gitlet.ViewedFile{step}))
		marks := held()
		if len(marks) != 2 || marks[key.PathDigest("a.go")] != key.FileDigest("a.go", step.BlobOID, step.Mode) {
			t.Fatalf("after marking a.go at %s %s founder holds %v, want its one row at that state", step.BlobOID,
				step.Mode, marks)
		}
		if n.count(`SELECT COUNT(*) FROM code_viewed_files WHERE handle = 'founder' AND path_digest = ?
			AND position = ?`, key.PathDigest("a.go"), res.Position.Packed()) != 1 {
			t.Errorf("a.go marked again at %s %s keeps an older position, want %s", step.BlobOID, step.Mode,
				res.Position)
		}
		_, stored := bodyAt(t, n, res.Position)
		answer, err := readViewed(t, n, ada, "web", web.number, 0, "")
		for _, row := range answer.Viewed {
			if row.PathDigest == key.PathDigest("a.go") && (err != nil || !row.At.Equal(stored.Truncate(time.Microsecond))) {
				t.Errorf("a.go marked again reads marked at %s (%v), want its record's broker instant %s", row.At,
					err, stored)
			}
		}
	}
	markFiles(t, m, viewing(ada, "founder", web, []gitlet.ViewedFile{{Path: "gone.go"}}, "b.go"))
	marks := held()
	if marks[key.PathDigest("gone.go")] != key.FileDigest("gone.go", "", "") {
		t.Errorf("a deleted file's mark reads %s, want the path with neither id nor mode", marks[key.PathDigest("gone.go")])
	}
	if _, kept := marks[key.PathDigest("b.go")]; kept || len(marks) != 2 {
		t.Errorf("after taking b.go back founder holds %v", marks)
	}
}

// AN ENDED CHANGE KEEPS NO MARKS: with marks on three changes, the closed and the merged one read
// none and no key, a mark of either is refused ended with nothing appended, and founder's next mark
// on the third carries the two in change id order to forget, after which no row of theirs stands.
//
// Mutation: skip the walk, and the closed change's rows stay, red.
func TestAnEndedChangeKeepsNoMarks(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef, releaseRef, "refs/heads/topic")
	m := markerOn(t, n, n.pub)
	for _, c := range web {
		markFiles(t, m, viewing(ada, "founder", c, []gitlet.ViewedFile{file("a.go", blobX, "100644")}))
	}
	c, _ := committerOn(t, n, n.pub)
	closed, err := c.ProposeChange(t.Context(), closing(web[0].repo, web[0].id, dev, web[0].version))
	requireChanged(t, closed, err)
	chartless, _ := committerOn(t, n, n.pub, func(o *gitlet.CommitterOptions) {
		o.SeatRules = func() (gitlet.SeatRules, bool) { return gitlet.SeatRules{}, false }
	})
	n.push(web[0].repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("M"))))
	landed, err := chartless.ProposeChange(t.Context(), landing(web[1].repo, web[1].id, 1, oidOf("R"), mainRef,
		oidOf("M")))
	if err != nil || landed.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the landing answered %+v, %v", landed, err)
	}
	for i, ended := range web[:2] {
		answer, readErr := readViewed(t, n, ada, "web", ended.number, 0, "")
		if readErr != nil || len(answer.Viewed) != 0 || answer.ViewedKey != "" {
			t.Errorf("change %d reads %+v (with a key %v), %v; want none", i+1, answer.Viewed,
				answer.ViewedKey != "", readErr)
		}
		ended.at = landed.Position
		before := n.end()
		res, markErr := m.Mark(t.Context(), viewing(ada, "founder", ended, []gitlet.ViewedFile{file("b.go", blobX,
			"100644")}))
		if markErr != nil || res.Refusal != gitlet.MarkEnded || n.end() != before {
			t.Errorf("a mark of change %d answered %+v, %v; want ended, nothing appended", i+1, res, markErr)
		}
	}
	live := web[2]
	live.at = landed.Position
	res := markFiles(t, m, viewing(ada, "founder", live, []gitlet.ViewedFile{file("b.go", blobX, "100644")}))
	want := []string{web[0].id, web[1].id}
	slices.Sort(want)
	if forget := viewedPayload(t, n, res.Position).Forget; !slices.Equal(forget, want) {
		t.Fatalf("the next mark forgets %v, want %v", forget, want)
	}
	for _, ended := range web[:2] {
		if rows := n.count(`SELECT COUNT(*) FROM code_viewed_files WHERE change_id = ?`, ended.id); rows != 0 {
			t.Errorf("%d marks of an ended change stand", rows)
		}
	}
}

// A PURGE DELETES THE REPOSITORY'S VIEWED KEY: after web is deleted and purged, api's key stands and
// web's is gone, founder's marks of web's change stay as digests and are not forgotten (no row of the
// change stands), and a repository made again as web takes a new key at its first mark, under which
// none of the old digests names a path founder marked.
//
// Mutations: drop purgeViewedKeySQL (web's key stays, red); forget a change with no row (web's
// marks go, red).
func TestAPurgeDeletesTheRepositorysViewedKey(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef)[0]
	api := viewedRepo(t, n, "api", featureRef)[0]
	m := markerOn(t, n, n.pub)
	files := []gitlet.ViewedFile{file("a.go", blobX, "100644")}
	markFiles(t, m, viewing(ada, "founder", web, files))
	markFiles(t, m, viewing(ada, "founder", api, files))
	old := viewedKeyOf(t, n, web.repo)
	n.deleteAt(web.repo, harnessAt.Add(-2*keep))
	for {
		res, err := n.store.PurgeStep(t.Context(), web.repo, keep)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		if res.Done {
			break
		}
	}
	if keys := n.dump("code_viewed_keys"); len(keys) != 1 || viewedKeyOf(t, n, api.repo) == "" {
		t.Fatalf("after web's purge the keys held are %d, want api's alone", len(keys))
	}
	if rows := n.count(`SELECT COUNT(*) FROM code_viewed_files WHERE change_id = ?`, web.id); rows != 1 {
		t.Fatalf("founder holds %d marks of the purged change, want the one, left to the cap", rows)
	}
	again := markFiles(t, m, viewing(ada, "founder", api, []gitlet.ViewedFile{file("b.go", blobX, "100644")}))
	if forget := viewedPayload(t, n, again.Position).Forget; slices.Contains(forget, web.id) {
		t.Fatalf("a mark forgets %v, a change with no row among them", forget)
	}
	if _, err := readViewed(t, n, ada, "web", web.number, 0, ""); err == nil {
		t.Fatal("a purged repository's change answered a read")
	}

	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	again2 := n.ensure("web", s, t3).RepoID
	n.push(again2, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B"))))
	c, _ := committerOn(t, n, n.pub)
	opened, err := c.ProposeChange(t.Context(), opening(again2, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	markFiles(t, m, viewing(ada, "founder", viewedChange{repo: again2, id: opened.ChangeID, number: opened.Number,
		at: opened.Position}, files))
	fresh := viewedKeyOf(t, n, again2)
	if again2 == web.repo || fresh == "" || fresh == old {
		t.Fatalf("web made again is %s with a key equal to the purged one's %v", again2, fresh == old)
	}
	if fresh.PathDigest("a.go") == old.PathDigest("a.go") {
		t.Fatal("a path digests alike under the purged key and the new one")
	}
}

// A VIEWED MARK REPLAYS IDENTICALLY: node b, draining the log from zero, holds the keys and the
// marks byte for byte as node a, which made them: marks on two repositories, a forget and a purge.
//
// Mutation: mint the key in the apply rather than carry it, and b's key differs, red.
func TestAViewedMarkReplaysIdentically(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	web := viewedRepo(t, a, "web", featureRef, releaseRef)
	api := viewedRepo(t, a, "api", featureRef)[0]
	m := markerOn(t, a, a.pub)
	files := []gitlet.ViewedFile{file("a.go", blobX, "100644"), {Path: "gone.go"}}
	for _, c := range []viewedChange{web[0], web[1], api} {
		markFiles(t, m, viewing(ada, "founder", c, files))
	}
	c, _ := committerOn(t, a, a.pub)
	closed, err := c.ProposeChange(t.Context(), closing(web[0].repo, web[0].id, dev, web[0].version))
	requireChanged(t, closed, err)
	web[1].at = closed.Position
	markFiles(t, m, viewing(ada, "founder", web[1], []gitlet.ViewedFile{file("b.go", blobY, "100755")}, "a.go"))
	a.deleteAt(api.repo, harnessAt.Add(-2*keep))
	for {
		res, purgeErr := a.store.PurgeStep(t.Context(), api.repo, keep)
		if purgeErr != nil {
			t.Fatalf("PurgeStep: %v", purgeErr)
		}
		if res.Done {
			break
		}
	}
	b.drain()
	for _, table := range []string{"code_viewed_keys", "code_viewed_files"} {
		if got, want := b.dump(table), a.dump(table); !slices.Equal(got, want) || len(want) == 0 {
			t.Errorf("%s replayed as\n\t%v\nwant\n\t%v", table, got, want)
		}
	}
}

// THE PERSON'S OLDEST MARKS GO PAST THE CAP: with bea's marks oldest of all and founder filled to
// MaxViewedFiles over two changes, three more files of founder's drop founder's three oldest by
// position, then change, then path digest, keep the newest, and leave bea's whole. The oldest file,
// marked again at a new mode first, is newer for it and stays.
//
// Mutations: order by position descending (the newest go, red); count across persons (bea's go, red);
// leave position out of the re-mark's upsert (the re-marked file goes, red).
func TestThePersonsOldestMarksGoPastTheCap(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef, releaseRef)
	m := markerOn(t, n, n.pub)
	markFiles(t, m, viewing(bea, "cto", web[0], []gitlet.ViewedFile{file("bea.go", blobX, "100644")}))
	key := viewedKeyOf(t, n, web[0].repo)
	var firstBatch []string
	pathOf := map[string]string{}
	for i := 0; i < gitlet.MaxViewedFiles; i += gitlet.MaxViewedPerMark {
		change := web[i/(gitlet.MaxViewedFiles/2+1)]
		var files []gitlet.ViewedFile
		for j := i; j < min(i+gitlet.MaxViewedPerMark, gitlet.MaxViewedFiles); j++ {
			path := fmt.Sprintf("f/%04d.go", j)
			files = append(files, file(path, blobX, "100644"))
			if i == 0 {
				firstBatch = append(firstBatch, key.PathDigest(path))
				pathOf[key.PathDigest(path)] = path
			}
		}
		markFiles(t, m, viewing(ada, "founder", change, files))
	}
	slices.Sort(firstBatch)
	markFiles(t, m, viewing(ada, "founder", web[0], []gitlet.ViewedFile{file(pathOf[firstBatch[0]], blobX,
		"100755")}))
	newest := []gitlet.ViewedFile{file("n/1.go", blobX, "100644"), file("n/2.go", blobX, "100644"),
		file("n/3.go", blobX, "100644")}
	markFiles(t, m, viewing(ada, "founder", web[1], newest))
	if held := n.count(`SELECT COUNT(*) FROM code_viewed_files WHERE handle = 'founder'`); held != gitlet.MaxViewedFiles {
		t.Fatalf("founder holds %d marks, want the cap %d", held, gitlet.MaxViewedFiles)
	}
	for i, d := range firstBatch {
		gone := n.count(`SELECT COUNT(*) FROM code_viewed_files WHERE handle = 'founder' AND path_digest = ?`, d) == 0
		if gone != (i >= 1 && i <= 3) {
			t.Errorf("founder's %d-th oldest mark is gone = %v, want only the three oldest but the re-marked "+
				"first gone", i, gone)
		}
	}
	for _, f := range newest {
		if n.count(`SELECT COUNT(*) FROM code_viewed_files WHERE handle = 'founder' AND path_digest = ?`,
			key.PathDigest(f.Path)) != 1 {
			t.Errorf("the newest mark of %s is gone", f.Path)
		}
	}
	if held := n.count(`SELECT COUNT(*) FROM code_viewed_files WHERE handle = 'cto'`); held != 1 {
		t.Fatalf("bea holds %d marks after founder passed the cap, want her one", held)
	}
}

// THE WIDEST VIEWED RECORD FITS ITS BUDGETS: MaxViewedPerMark marks and MaxViewedForget forgotten
// changes encode within MaxRecordBytes; the widest apply's rows (every mark forgotten, and a gesture
// taken back, marked and dropped) fit MaxRecordRows; and the cap holds two of the widest changes a
// reviewer reads file by file.
//
// Mutation: MaxViewedFiles = 1_000, red on the third.
func TestTheWidestViewedRecordFitsItsBudgets(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("f", 2*sha256.Size)
	p := gitlet.ViewedMarks{Handle: "founder", ChangeID: uuid.Must(uuid.NewV7()).String()}
	for range gitlet.MaxViewedPerMark {
		p.Viewed = append(p.Viewed, gitlet.ViewedMark{PathDigest: digest, FileDigest: digest})
	}
	for range gitlet.MaxViewedForget {
		p.Forget = append(p.Forget, uuid.Must(uuid.NewV7()).String())
	}
	payload, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	body, err := gitlet.Encode(gitlet.MutationRecord{RecordEnvelope: gitlet.RecordEnvelope{
		OpID: statelog.NewOpID(time.Now(), "code_mark_viewed"), Subject: gitlet.PersonSubject(p.Handle),
		Op: gitlet.OpViewed, CreatedAt: harnessAt, Gen: 1, Writer: "node-a",
		Scope: gitlet.Scope{gitlet.ViewedPath(p.Handle)}}, Mutation: payload, Actor: p.Handle,
		ActorKind: gitlet.ActorOperator, OperatorID: "pat:" + strings.Repeat("o", 64)})
	switch {
	case err != nil:
		t.Fatal(err)
	case len(body) > gitlet.MaxRecordBytes:
		t.Errorf("the widest viewed record is %d bytes, past MaxRecordBytes %d", len(body), gitlet.MaxRecordBytes)
	case gitlet.MaxViewedFiles+3*gitlet.MaxViewedPerMark > gitlet.MaxRecordRows:
		t.Errorf("the widest viewed apply touches %d rows, past MaxRecordRows %d",
			gitlet.MaxViewedFiles+3*gitlet.MaxViewedPerMark, gitlet.MaxRecordRows)
	case gitlet.MaxViewedFiles < 2*gitlet.MaxTouchedPaths:
		t.Errorf("a person holds %d marks, under two of the widest changes (%d files each): a new cap is a "+
			"record format", gitlet.MaxViewedFiles, gitlet.MaxTouchedPaths)
	}
}

// NO ANSWER, LOG OR ERROR CARRIES A VIEWED KEY: web's key is in no mark's answer or error, no read
// answer but founder's own page of his marks, no ErrBadCursor, no format of a key record under any
// verb, and no log line of the key; founder's page carries it.
//
// Mutations: drop String or Format (%v or %d prints it, red); answer the key beside an empty page, red.
func TestNoAnswerLogOrErrorCarriesAViewedKey(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef, releaseRef)
	m := markerOn(t, n, n.pub)
	var said []string
	res, err := m.Mark(t.Context(), viewing(ada, "founder", web[0], []gitlet.ViewedFile{file("a.go", blobX,
		"100644")}))
	said = append(said, fmt.Sprintf("%+v %v", res, err))
	key := viewedKeyOf(t, n, web[0].repo)
	if !key.Valid() {
		t.Fatal("web holds no key after its first mark")
	}
	for _, w := range []gitlet.MarkWrite{
		viewing(ada, "founder", web[0], []gitlet.ViewedFile{file("a.go", blobX, "100644")}),
		viewing(ada, "cto", web[0], []gitlet.ViewedFile{file("a.go", blobX, "100644")}),
	} {
		r, markErr := m.Mark(t.Context(), w)
		said = append(said, fmt.Sprintf("%+v %v", r, markErr))
	}
	for name, read := range map[string]func() (gitlet.ReadAnswer, error){
		"a seat":                  func() (gitlet.ReadAnswer, error) { return readViewed(t, n, cto, "web", 1, 0, "") },
		"another person":          func() (gitlet.ReadAnswer, error) { return readViewed(t, n, bea, "web", 1, 0, "") },
		"founder on another":      func() (gitlet.ReadAnswer, error) { return readViewed(t, n, ada, "web", 2, 0, "") },
		"founder past his cursor": func() (gitlet.ReadAnswer, error) { return readViewed(t, n, ada, "web", 1, 0, "x") },
	} {
		answer, readErr := read()
		if answer.ViewedKey != "" {
			t.Errorf("%s is answered the key", name)
		}
		said = append(said, fmt.Sprintf("%+v %#v %v", answer, answer, readErr))
	}
	record := gitlet.ViewedKeyRecord{RepoID: web[0].repo, Key: key}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		said = append(said, fmt.Sprintf(verb, record), fmt.Sprintf(verb, key))
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("viewed", "key", key, "record_key", record.Key)
	said = append(said, logged.String())
	for _, s := range said {
		if strings.Contains(s, string(key)) {
			t.Errorf("the key is in %q", s)
		}
	}
	own, err := readViewed(t, n, ada, "web", 1, 0, "")
	if err != nil || own.ViewedKey != key {
		t.Fatalf("founder's own page answered the key %v, %v; want it", own.ViewedKey == key, err)
	}
}

// A VIEWED GESTURE IS REFUSED BEFORE ANYTHING IS READ: each malformed gesture is ErrInvalid naming
// its field, and the publisher is never asked.
//
// Mutation: drop the duplicate check, red.
func TestAViewedGestureIsRefusedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	m, err := gitlet.NewMarks(gitlet.MarksOptions{Publisher: refusingPublisher{t}, DB: newNode(t).db.Reader(),
		NodeID: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	change := viewedChange{repo: uuid.Must(uuid.NewV7()).String(), id: uuid.Must(uuid.NewV7()).String(),
		at: statelog.Position{Stream: gitlet.Domain{}.Stream().Name, Generation: 1, Seq: 9}}
	ok := file("a.go", blobX, "100644")
	many := make([]gitlet.ViewedFile, gitlet.MaxViewedPerMark+1)
	for i := range many {
		many[i] = file(fmt.Sprintf("%d.go", i), blobX, "100644")
	}
	for field, w := range map[string]gitlet.MarkWrite{
		"files": func() gitlet.MarkWrite {
			w := viewing(ada, "founder", change, []gitlet.ViewedFile{ok})
			w.Gesture.Read = []string{"notice"}
			return w
		}(),
		"seen": func() gitlet.MarkWrite {
			w := viewing(ada, "founder", change, []gitlet.ViewedFile{ok})
			w.Seen = statelog.Position{}
			return w
		}(),
		"files: a gesture names 1":      viewing(ada, "founder", change, nil),
		"files: a gesture names 1 to 3": viewing(ada, "founder", change, many),
		"files.unviewed[0]":             viewing(ada, "founder", change, []gitlet.ViewedFile{ok}, "a.go"),
		"files.viewed[0].path: \"./a":   viewing(ada, "founder", change, []gitlet.ViewedFile{file("./a.go", blobX, "100644")}),
		"files.viewed[0].path: a path holds no NUL": viewing(ada, "founder", change,
			[]gitlet.ViewedFile{file("a\x00.go", blobX, "100644")}),
		"files.viewed[0].path: 4097": viewing(ada, "founder", change,
			[]gitlet.ViewedFile{file(strings.Repeat("a", gitlet.MaxGitPathBytes+1), blobX, "100644")}),
		"files.viewed[0].path: not valid UTF-8": viewing(ada, "founder", change,
			[]gitlet.ViewedFile{file("a\xff.go", blobX, "100644")}),
		"files.viewed[0].blob_oid: \"" + strings.ToUpper(blobX): viewing(ada, "founder", change,
			[]gitlet.ViewedFile{file("a.go", strings.ToUpper(blobX), "100644")}),
		"files.viewed[0].blob_oid: \"" + gitlet.ZeroOID: viewing(ada, "founder", change,
			[]gitlet.ViewedFile{file("a.go", gitlet.ZeroOID, "100644")}),
		"files.viewed[0].mode: \"10064\"": viewing(ada, "founder", change,
			[]gitlet.ViewedFile{file("a.go", blobX, "10064")}),
		"files.viewed[0].mode: \"000000\"": viewing(ada, "founder", change,
			[]gitlet.ViewedFile{file("a.go", blobX, "000000")}),
		"files.viewed[0]: a file is marked with its new side's object id and mode": viewing(ada, "founder", change,
			[]gitlet.ViewedFile{file("a.go", blobX, "")}),
		"files.viewed[1]: a file is marked": viewing(ada, "founder", change,
			[]gitlet.ViewedFile{ok, file("b.go", "", "100644")}),
		"files.repo_id": func() gitlet.MarkWrite {
			w := viewing(ada, "founder", change, []gitlet.ViewedFile{ok})
			w.Files.RepoID = "web"
			return w
		}(),
		"files.change_id": func() gitlet.MarkWrite {
			w := viewing(ada, "founder", change, []gitlet.ViewedFile{ok})
			w.Files.ChangeID = uuid.NewString()
			return w
		}(),
	} {
		_, markErr := m.Mark(t.Context(), w)
		if !errors.Is(markErr, gitlet.ErrInvalid) || !strings.Contains(markErr.Error(), field) {
			t.Errorf("%s: the gesture answered %v, want ErrInvalid naming it", field, markErr)
		}
	}
}

// refusingPublisher fails a case whose write reaches the code log.
type refusingPublisher struct{ t *testing.T }

func (p refusingPublisher) Publish(_ context.Context, req statelog.Request) (statelog.Result, error) {
	p.t.Errorf("the publisher was asked for %s", req.OpID)
	return statelog.Result{}, errors.New("not published")
}

// A VIEWED READ ANSWERS THE ASKER'S OWN: ada reads founder's marks with web's key; bea reads her own;
// the seat cto, whose handle bea's marks are kept under, and a credential bound to no seat read none
// and no key; pages of two in digest order resume exactly by a digest cursor, each carrying the key;
// a cursor that is no digest is ErrBadCursor; and viewed beside a part, detail_only, brief,
// finding_ids or finding_paths is ErrInvalid.
//
// Mutations: read by the change alone, and bea reads founder's, red; answer any party its handle's
// marks, and the seat cto reads bea's, red.
func TestAViewedReadAnswersTheAskersOwn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web := viewedRepo(t, n, "web", featureRef)[0]
	m := markerOn(t, n, n.pub)
	markFiles(t, m, viewing(ada, "founder", web, []gitlet.ViewedFile{file("a.go", blobX, "100644"),
		file("b.go", blobX, "100644"), file("c.go", blobX, "100644"), file("d.go", blobX, "100644"),
		file("e.go", blobX, "100644")}))
	markFiles(t, m, viewing(bea, "cto", web, []gitlet.ViewedFile{file("z.go", blobX, "100644")}))
	key := viewedKeyOf(t, n, web.repo)
	for name, who := range map[string]gitlet.Principal{"bea": bea, "the seat cto": cto, "unbound": unbound} {
		answer, err := readViewed(t, n, who, "web", web.number, 0, "")
		switch {
		case err != nil:
			t.Errorf("%s's read: %v", name, err)
		case name == "bea" && (len(answer.Viewed) != 1 || answer.Viewed[0].PathDigest != key.PathDigest("z.go")):
			t.Errorf("bea reads %+v, want her own one mark", answer.Viewed)
		case name != "bea" && (len(answer.Viewed) != 0 || answer.ViewedKey != ""):
			t.Errorf("%s reads %+v, want none and no key", name, answer.Viewed)
		}
	}
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		answer, err := readViewed(t, n, ada, "web", web.number, 2, cursor)
		if err != nil || answer.ViewedKey != key || pages > 3 {
			t.Fatalf("page %d answered %v (with the key %v), want a page with the key", pages, err,
				answer.ViewedKey == key)
		}
		for _, row := range answer.Viewed {
			got = append(got, row.PathDigest)
		}
		if cursor = answer.Next; cursor == "" {
			break
		}
	}
	want := []string{}
	for _, p := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		want = append(want, key.PathDigest(p))
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("founder's pages read %v, want %v", got, want)
	}
	if _, err := readViewed(t, n, ada, "web", web.number, 0, "a.go"); !errors.Is(err, gitlet.ErrBadCursor) {
		t.Errorf("a cursor that is no digest answered %v, want ErrBadCursor", err)
	}
	for name, set := range map[string]func(q *gitlet.ReadQuery){
		"a part":      func(q *gitlet.ReadQuery) { q.Part = gitlet.PartReviewers },
		"detail_only": func(q *gitlet.ReadQuery) { q.DetailOnly = true },
		"brief":       func(q *gitlet.ReadQuery) { q.Brief = true },
		"finding_ids": func(q *gitlet.ReadQuery) {
			q.Part, q.FindingIDs = gitlet.PartFindings, []string{"f"}
		},
		"finding_paths": func(q *gitlet.ReadQuery) { q.FindingPaths = true },
	} {
		q := gitlet.ReadQuery{Asker: ada, Kind: gitlet.ReadChange, Repo: "web", Change: web.number, Viewed: true,
			Fresh: stale}
		set(&q)
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) ||
			!strings.Contains(err.Error(), "viewed") {
			t.Errorf("viewed beside %s answered %v, want ErrInvalid naming viewed", name, err)
		}
	}
}
