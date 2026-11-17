package gitlet

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PERSON'S VIEWED FILES, inside the package: the digests' bytes, and the applies pure over their
// records and their own subject's rows.

// viewedVectorKey is the bytes 0 to 31: the key the digests' vectors are pinned under, here and in
// the screen's test of the same digests.
const viewedVectorKey = ViewedKey("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")

// A VIEWED DIGEST IS HMAC-SHA256 OF ITS BYTES, under the key's 32 bytes, in lowercase hex: each
// vector computed by two independent tools (openssl's HMAC and Python's hmac), the path as UTF-8 and
// a file's state as the path, a NUL, the object id, a NUL and the mode. A fresh key is Valid and
// unlike the last; Valid refuses any other length, case or digit.
//
// Mutations: a bare SHA-256, red; upper-case hex, red; the file digest without its NULs, red.
func TestAViewedDigestIsHMACSHA256OfItsBytes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ got, want string }{
		"a path": {viewedVectorKey.PathDigest("a.go"),
			"bcdb15ce46ab4093e6370ab63b6ad3cef3c6966b1f27eaf0fa1d9e843b45bb42"},
		"a path in UTF-8": {viewedVectorKey.PathDigest("docs/é.md"),
			"84a975442843988e8698741521651f5b0086d13fe9197b4b14066a1336e0e3c6"},
		"a file's state": {viewedVectorKey.FileDigest("a.go", "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391", "100644"),
			"5159a10d2fee5f8baf65e96c8808352ce3849371ffe5b591b1385b30ea6d697e"},
		"a deleted file's state": {viewedVectorKey.FileDigest("a.go", "", ""),
			"9b9c0df1558001db2b764fbfd2810afd8a7d1ab1711a554eeab55da873462efe"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s digests to %s, want %s", name, tc.got, tc.want)
		}
	}
	first, second := newViewedKey(), newViewedKey()
	if !first.Valid() || !second.Valid() || first == second {
		t.Errorf("two fresh keys are valid %v and %v and equal %v; want two valid keys that differ",
			first.Valid(), second.Valid(), first == second)
	}
	for _, bad := range []string{strings.Repeat("a", 63), strings.Repeat("a", 65), "A" + strings.Repeat("a", 63),
		"g" + strings.Repeat("a", 63)} {
		if ViewedKey(bad).Valid() {
			t.Errorf("%q is valid; a key is 64 lowercase hex digits", bad)
		}
	}
}

// viewedOn is a viewed record of handle's marks on change, as the decide forms one.
func viewedOn(t testing.TB, handle string, m ViewedMarks) built {
	m.Handle = handle
	return rec(t, PersonSubject(handle), OpViewed, Scope{ViewedPath(handle)}, m)
}

// viewedOf is the mark of path at oid and mode under viewedVectorKey.
func viewedOf(path, oid, mode string) ViewedMark {
	return ViewedMark{PathDigest: viewedVectorKey.PathDigest(path),
		FileDigest: viewedVectorKey.FileDigest(path, oid, mode)}
}

// A VIEWED APPLY IS THE SAME IN EITHER ORDER: the marks of change 1 forgotten by a later record that
// carries 1 in its forget list are gone whether that record applies before or after 1's reopen, the
// order a node that held the second viewed record back applies it in, because the apply reads no
// change's state but deletes what its record names.
//
// Mutation: read the change's state in the apply instead of carrying forget, and the second order
// keeps change 1's marks, red.
func TestAViewedApplyIsTheSameInEitherOrder(t *testing.T) {
	t.Parallel()
	const repo = "repo-v"
	setup := func(t *testing.T) *estate {
		e := openApplyEstate(t)
		e.apply(create(t, repo, "v"), ccBranches(t, repo, "p0", 1, 2), ccOpen(t, repo, 1, false),
			ccOpen(t, repo, 2, false),
			viewedOn(t, "founder", ViewedMarks{ChangeID: ccChange(1), Viewed: []ViewedMark{
				viewedOf("a.go", oid("a"), "100644"), viewedOf("b.go", oid("b"), "100644")}}),
			ccRecord(t, repo, OpChangeClose, ChangeClose{RepoID: repo, ChangeID: ccChange(1), By: ccSeat}))
		return e
	}
	forgets := viewedOn(t, "founder", ViewedMarks{ChangeID: ccChange(2),
		Viewed: []ViewedMark{viewedOf("c.go", oid("c"), "100755")}, Forget: []string{ccChange(1)}})
	reopen := ccRecord(t, repo, OpChangeReopen, ChangeReopen{RepoID: repo, ChangeID: ccChange(1), By: ccSeat,
		Head: ccHead(1)})
	inOrder, heldBack := setup(t), setup(t)
	inOrder.apply(forgets, reopen)
	heldBack.apply(reopen, forgets)
	const marks = `SELECT handle, change_id, path_digest, file_digest FROM code_viewed_files
		ORDER BY handle, change_id, path_digest`
	got, want := heldBack.dump(marks), inOrder.dump(marks)
	if got != want {
		t.Fatalf("the reprocessing order holds\n%s\nand the log's order\n%s", got, want)
	}
	if n := inOrder.num(`SELECT COUNT(*) FROM code_viewed_files WHERE change_id = ?`, ccChange(1)); n != 0 {
		t.Fatalf("change 1's %d marks stand after a record forgot them", n)
	}
}

// THE VIEWED APPLY WRITES ONLY THE PERSON'S ROWS: every statement the viewed apply runs names
// code_viewed_files and no other code table, and the key's apply names its key and its repository's
// row alone, so each is a function of its record and its own subject's rows.
//
// Mutation: join code_changes into the forget statement, red.
func TestTheViewedApplyWritesOnlyThePersonsRows(t *testing.T) {
	t.Parallel()
	tables := regexp.MustCompile(`\bcode_[a-z_]+\b`)
	for _, tc := range []struct {
		statements []string
		allowed    []string
	}{
		{[]string{forgetViewedSQL, unviewSQL, viewSQL, viewedCountSQL, dropOldestViewedSQL},
			[]string{"code_viewed_files"}},
		{[]string{viewedKeyInsertSQL, touchedSQL}, []string{"code_viewed_keys", "code_repos"}},
	} {
		for _, stmt := range tc.statements {
			for _, table := range tables.FindAllString(stmt, -1) {
				if !slices.Contains(tc.allowed, table) && !strings.HasPrefix(table, "code_viewed_files_") {
					t.Errorf("%q names %s; the apply writes %v alone", stmt, table, tc.allowed)
				}
			}
		}
	}
}

// THE VIEWED KEY APPLY KEEPS THE FIRST KEY: a second viewed_key record for one repository leaves the
// first key on every node and moves the repository's version to its own position, and a key that is
// not Valid stops the apply with an error naming the repository and never the value.
//
// Mutation: ON CONFLICT DO UPDATE, and the second key replaces the first, red.
func TestTheViewedKeyApplyKeepsTheFirstKey(t *testing.T) {
	t.Parallel()
	const repo = "repo-k"
	first, second := viewedVectorKey, ViewedKey(strings.Repeat("ab", ViewedKeyBytes))
	for range 2 {
		e := openApplyEstate(t)
		e.apply(create(t, repo, "k"),
			ccRecord(t, repo, OpViewedKey, ViewedKeyRecord{RepoID: repo, Key: first}),
			ccRecord(t, repo, OpViewedKey, ViewedKeyRecord{RepoID: repo, Key: second}))
		if got := e.str(`SELECT secret FROM code_viewed_keys WHERE repo_id = ?`, repo); got != string(first) {
			t.Fatalf("the repository holds the key %s after two key records, want the first", got)
		}
		at := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: e.next - 1}
		if version := e.num(`SELECT version FROM code_repos WHERE id = ?`, repo); version != at.Packed() {
			t.Fatalf("the repository's version is %d, want the second key record's position %s", version, at)
		}
		bad := ViewedKey(strings.Repeat("Z", 2*ViewedKeyBytes))
		err := e.applyErr(ccRecord(t, repo, OpViewedKey, ViewedKeyRecord{RepoID: repo, Key: bad}))
		if err == nil || !strings.Contains(err.Error(), repo) || strings.Contains(err.Error(), string(bad)) {
			t.Fatalf("a key that is not valid applied with %v; want an error naming %s and not the key", err, repo)
		}
	}
}
