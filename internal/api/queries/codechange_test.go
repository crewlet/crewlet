package queries_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S QUESTIONS (codechange.go): its rows read at the caller's
// freshness, and the parts read by object id assembled from the rows they
// name, by the code domain's one rule.

// atCodeSeq is a position on the code log.
func atCodeSeq(seq uint64) statelog.Position {
	return statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: seq}
}

// webID is the repository web's id, as its rows name it.
const webID = "0192b4c8-7f3e-7a10-9c2d-5e6f7a8b9c0d"

// oid is a whole object id spelled by repeating c.
func oid(c string) string { return strings.Repeat(c, 40) }

// twoRevisions is a node's rows of web!1, an open change of two revisions —
// each measured against main, revision 1 at position 10 and revision 2 at 20
// — answered at position answeredAt; and of its one finding, raised on
// revision 1's new side.
func twoRevisions(answeredAt statelog.Position) func(gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	rev := func(n int64, head, base string, at uint64) gitlet.RevisionRow {
		return gitlet.RevisionRow{N: n, Head: oid(head), Position: atCodeSeq(at), Facts: &gitlet.RevisionFactsRow{
			TargetRef: "refs/heads/main", TargetOID: oid("a"), TargetPosition: atCodeSeq(at - 1),
			BaseOID: oid(base), Touched: []string{"upload/retry.go"}}}
	}
	return func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		out := gitlet.ReadAnswer{Level: q.Fresh.Level, Position: answeredAt}
		switch {
		case q.Part == gitlet.PartFindings:
			out.Findings = []gitlet.FindingRow{{ID: "f1", Revision: 1, Path: "upload/retry.go", Side: gitlet.SideNew,
				BlobOID: oid("b"), LineStart: 4, LineEnd: 4}}
		case q.Cursor == "2":
			out.Revisions = []gitlet.RevisionRow{rev(1, "1", "a", 10)}
		default:
			out.Change = &gitlet.ChangeDetail{ChangeSummary: gitlet.ChangeSummary{ChangeRow: gitlet.ChangeRow{
				ID: "change-1", RepoID: webID, Number: 1, State: gitlet.ChangeStateOpen, Revision: 2}}}
			out.Revisions = []gitlet.RevisionRow{rev(2, "2", "a", 20)}
		}
		return out, nil
	}
}

// THE CHANGE QUESTIONS READ AT THE CALLER'S FLOOR. A screen reads its own
// write back at `session` beside the position the write answered with, so each
// change question hands the reader the caller's WHOLE freshness on its first
// read of the rows; a part read by object id reads every later row at `session`
// floored at that first read's position, and asks the Gitlet node at the floor
// of the records that moved each revision it reads and its target
// (gitlet.ObjectFloor), never at the caller's floor or a row read's checkpoint.
//
// Mutations: read a later revision at the caller's freshness → its read is
// not floored at the first's answer, red; ask the Gitlet node at the caller's
// floor → the diff's floor is not the revisions', red.
func TestTheChangeQuestionsReadAtTheCallersFloor(t *testing.T) {
	t.Parallel()
	floor := atCodeSeq(4711)
	answered := atCodeSeq(4800)
	asked := statelog.Freshness{Level: statelog.ReadSession, Floors: []statelog.Position{floor}}
	after := statelog.FloorAt(statelog.ReadSession, answered)
	session := map[string]any{"read_level": "session", "min_position": floor.String()}
	with := func(params map[string]any) map[string]any {
		all := map[string]any{"name": "web", "change": "1"}
		for k, v := range session {
			all[k] = v
		}
		for k, v := range params {
			all[k] = v
		}
		return all
	}

	for _, tc := range []struct {
		what   string
		params map[string]any
		// reads is each row read's kind and freshness, in order; git the
		// read by object id the answer asks, if any, and gitFloor its floor.
		reads    []statelog.Freshness
		git      gitlet.GitReadKind
		gitFloor statelog.Position
	}{
		{"code_changes", map[string]any{"repo": "web", "name": nil, "change": nil},
			[]statelog.Freshness{asked}, "", statelog.Position{}},
		{"code_change_routing", nil, []statelog.Freshness{asked}, "", statelog.Position{}},
		{"code_reviews", map[string]any{"reviewer": "dev", "name": nil, "change": nil},
			[]statelog.Freshness{asked}, "", statelog.Position{}},
		{"code_change", nil, []statelog.Freshness{asked}, "", statelog.Position{}},
		{"code_change", map[string]any{"part": "reviews"}, []statelog.Freshness{asked}, "", statelog.Position{}},
		// The head's files: the head read, then the diff at the floor of
		// the record that moved its head (20).
		{"code_change", map[string]any{"part": "files"}, []statelog.Freshness{asked}, gitlet.GitDiff,
			atCodeSeq(20)},
		// What changed since revision 1: the head, revision 1 floored at
		// the head's answer, the interdiff at the later of the two.
		{"code_change", map[string]any{"part": "diff", "since_revision": "1", "file": "upload/retry.go"},
			[]statelog.Freshness{asked, after}, gitlet.GitInterdiff, atCodeSeq(20)},
		// Revision 1's files: the head, revision 1 floored, its diff at 10.
		{"code_change", map[string]any{"part": "files", "revision": "1"},
			[]statelog.Freshness{asked, after}, gitlet.GitDiff, atCodeSeq(10)},
		// The findings placed in the head: the head, the findings page
		// floored at its answer, and a re-anchoring at the head's floor.
		{"code_change", map[string]any{"part": "findings"}, []statelog.Freshness{asked, after}, gitlet.GitReanchor,
			atCodeSeq(20)},
	} {
		name := tc.what
		if part, ok := tc.params["part"]; ok {
			name += " " + part.(string)
		}
		params := with(tc.params)
		for k, v := range params {
			if v == nil {
				delete(params, k)
			}
		}
		stub := &stubCode{answer: twoRevisions(answered),
			git: func(q gitlet.GitQuery) (gitlet.GitAnswer, error) {
				out := gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID}
				if q.Reanchor != nil {
					out.Reanchor = &gitlet.ReanchorAnswer{}
					for _, a := range q.Reanchor.Anchors {
						out.Reanchor.Anchors = append(out.Reanchor.Anchors, gitlet.Reanchored{ID: a.ID, Start: 6,
							End: 6})
					}
				}
				return out, nil
			}}
		answer, err := askNative(t, queries.Sources{Code: stub}, tc.what, params)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var got []statelog.Freshness
		for _, q := range stub.asked {
			got = append(got, q.Fresh)
		}
		if !reflect.DeepEqual(got, tc.reads) {
			t.Errorf("%s read the rows at %+v, want %+v", name, got, tc.reads)
		}
		switch {
		case tc.git == "" && len(stub.read) != 0:
			t.Errorf("%s asked %d reads by object id, want none", name, len(stub.read))
		case tc.git == "":
		case len(stub.read) != 1 || stub.read[0].Kind != tc.git:
			t.Errorf("%s asked the reads by object id %+v, want one %s", name, stub.read, tc.git)
		case !reflect.DeepEqual(stub.read[0].Fresh, statelog.FloorAt(statelog.ReadSession, tc.gitFloor)):
			t.Errorf("%s asked its %s at %+v, want session at %s", name, tc.git, stub.read[0].Fresh, tc.gitFloor)
		case stub.read[0].RepoID != webID || stub.read[0].Asker == (gitlet.Principal{}):
			t.Errorf("%s asked its %s of %q as %+v, want the change's repository as the caller", name, tc.git,
				stub.read[0].RepoID, stub.read[0].Asker)
		}
		if found, ok := answer.(queries.CodeChangeFindings); ok {
			if at, placed := found.Placed["f1"]; !placed || at.Start != 6 || found.Revision != 2 {
				t.Errorf("%s placed the finding %+v in revision %d, want line 6 of revision 2", name,
					found.Placed, found.Revision)
			}
		}
	}
}

// A PAGE OF FINDINGS PAST ONE RE-ANCHORING IS PLACED WHOLE: the findings part
// takes any page the reader serves (up to gitlet.MaxFindingsPerPage, which a
// screen asks), and a Gitlet node's re-anchoring places at most
// gitlet.MaxReanchors — so a page of findings raised on an earlier revision
// past that is placed by several re-anchorings, each one a Gitlet node takes,
// all at the head's floor, never refused as the request's mistake naming a
// field it never sent.
//
// Mutation: place the page with one re-anchoring of every anchor → the
// question refuses the page, red.
func TestAPageOfFindingsPastOneReanchoringIsPlacedWhole(t *testing.T) {
	t.Parallel()
	const findings = 100
	rows := twoRevisions(atCodeSeq(4800))
	stub := &stubCode{answer: func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		out, err := rows(q)
		if q.Part == gitlet.PartFindings {
			out.Findings = nil
			for i := range findings {
				out.Findings = append(out.Findings, gitlet.FindingRow{ID: fmt.Sprintf("f%03d", i), Revision: 1,
					Path: "upload/retry.go", Side: gitlet.SideNew, BlobOID: oid("b"), LineStart: i + 1,
					LineEnd: i + 1})
			}
		}
		return out, err
	}, git: func(q gitlet.GitQuery) (gitlet.GitAnswer, error) {
		out := gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Reanchor: &gitlet.ReanchorAnswer{}}
		for _, a := range q.Reanchor.Anchors {
			out.Reanchor.Anchors = append(out.Reanchor.Anchors, gitlet.Reanchored{ID: a.ID, Start: a.Start + 2,
				End: a.End + 2})
		}
		return out, nil
	}}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_change", map[string]any{"name": "web",
		"change": "1", "part": "findings", "limit": fmt.Sprint(findings)})
	if err != nil {
		t.Fatalf("a page of %d findings raised on an earlier revision was refused: %v", findings, err)
	}
	found, ok := answer.(queries.CodeChangeFindings)
	if !ok {
		t.Fatalf("the findings part answered %T", answer)
	}
	for i := range findings {
		id := fmt.Sprintf("f%03d", i)
		if at := found.Placed[id]; at.Start != i+3 {
			t.Errorf("finding %s was placed at %+v, want line %d of revision 2", id, at, i+3)
		}
	}
	if len(stub.read) < 2 {
		t.Errorf("a page of %d findings was placed by %d re-anchorings, want one for every %d", findings,
			len(stub.read), gitlet.MaxReanchors)
	}
	for _, q := range stub.read {
		if q.Kind != gitlet.GitReanchor || len(q.Reanchor.Anchors) > gitlet.MaxReanchors ||
			!reflect.DeepEqual(q.Fresh, statelog.FloorAt(statelog.ReadSession, atCodeSeq(20))) {
			t.Errorf("asked %s of %d anchors at %+v, want a re-anchoring of at most %d at the head's floor", q.Kind,
				len(q.Reanchor.Anchors), q.Fresh, gitlet.MaxReanchors)
		}
	}
}

// A CHANGE'S TWO REVISIONS ARE READ AT ONCE: what changed in revision 2 since
// revision 1 of a change whose head is 3 reads the head, then the two
// revisions — neither needing the other's row — beside each other, each
// floored at the head's answer, so a node reading the estate through another
// pays one round trip for the two (gitlet.ChangeRevisions, which the code tools
// read by too). And a findings page is read beside the revision it is placed
// in.
//
// Mutation: read the two revisions one after the other → each is read
// alone, red; read the page after the revision → each is read alone, red.
func TestAChangesTwoRevisionsAreReadAtOnce(t *testing.T) {
	t.Parallel()
	answered := atCodeSeq(4800)
	// meet holds a read until another read after the head's has arrived,
	// for at most five seconds — far past a loaded runner's scheduling
	// delay — answering whether it did.
	both := make(chan struct{})
	var arrived atomic.Int32
	meet := func() bool {
		if arrived.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
			return true
		case <-time.After(5 * time.Second):
			return false
		}
	}
	rev := func(n int64) gitlet.RevisionRow {
		head := fmt.Sprint(n)
		return gitlet.RevisionRow{N: n, Head: oid(head), Position: atCodeSeq(uint64(10 * n)),
			Facts: &gitlet.RevisionFactsRow{TargetRef: "refs/heads/main", TargetOID: oid("a"),
				TargetPosition: atCodeSeq(uint64(10*n - 1)), BaseOID: oid("a"), Touched: []string{"upload/retry.go"}}}
	}
	stub := &stubCode{answer: func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		out := gitlet.ReadAnswer{Level: q.Fresh.Level, Position: answered}
		if q.Cursor == "" && q.Part == "" {
			out.Change = &gitlet.ChangeDetail{ChangeSummary: gitlet.ChangeSummary{ChangeRow: gitlet.ChangeRow{
				ID: "change-1", RepoID: webID, Number: 1, State: gitlet.ChangeStateOpen, Revision: 3}}}
			out.Revisions = []gitlet.RevisionRow{rev(3)}
			return out, nil
		}
		if !meet() {
			return gitlet.ReadAnswer{}, fmt.Errorf("the read %+v was made alone", q)
		}
		switch q.Cursor {
		case "3":
			out.Revisions = []gitlet.RevisionRow{rev(2)}
		case "2":
			out.Revisions = []gitlet.RevisionRow{rev(1)}
		}
		return out, nil
	}}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_change", map[string]any{"name": "web",
		"change": "1", "part": "diff", "revision": "2", "since_revision": "1", "file": "upload/retry.go"})
	if err != nil {
		t.Fatalf("revision 2 since revision 1 was refused: %v", err)
	}
	if got, ok := answer.(queries.CodeChangeFiles); !ok || got.Revision != 2 || got.SinceRevision != 1 ||
		len(stub.read) != 1 || stub.read[0].Interdiff == nil || stub.read[0].Interdiff.ToHead != oid("2") ||
		stub.read[0].Interdiff.FromHead != oid("1") {
		t.Errorf("revision 2 since revision 1 was read as %+v by %+v", got, stub.read)
	}
	for _, q := range stub.asked[1:] {
		if !reflect.DeepEqual(q.Fresh, statelog.FloorAt(statelog.ReadSession, answered)) {
			t.Errorf("a revision was read at %+v, want session at the head's answer", q.Fresh)
		}
	}

	// THE FINDINGS PAGE BESIDE THE REVISION IT IS PLACED IN.
	both = make(chan struct{})
	arrived.Store(0)
	stub.asked, stub.read = nil, nil
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_change", map[string]any{"name": "web",
		"change": "1", "part": "findings", "revision": "2"}); err != nil {
		t.Fatalf("the findings in revision 2 were refused: %v", err)
	}
}

// A CHANGE QUESTION'S PART IS READ AS IT ASKS, and a parameter its part does
// not take is the request's mistake, refused before anything is read: a
// revision to the revisions, a file to the files, an earlier revision that is
// not earlier, a patch that names no file. A revision the change does not
// hold is an absence naming it, and a change the repository does not hold
// another.
//
// Mutation: drop changeCallOf's check of since_revision against revision →
// the reader is asked, red.
func TestAChangeQuestionRefusesWhatItsPartDoesNotTake(t *testing.T) {
	t.Parallel()
	for name, params := range map[string]map[string]any{
		"a part that is not one":       {"part": "readiness"},
		"a revision to the revisions":  {"revision": "1"},
		"a file to the files":          {"part": "files", "file": "a.go"},
		"an earlier that is later":     {"part": "files", "revision": "1", "since_revision": "2"},
		"a patch of no file":           {"part": "diff"},
		"a revision that is no number": {"part": "files", "revision": "first"},
		"no change":                    {"change": "0"},
	} {
		all := map[string]any{"name": "web", "change": "1"}
		for k, v := range params {
			all[k] = v
		}
		stub := &stubCode{answer: twoRevisions(atCodeSeq(1))}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_change", all); !errors.Is(err,
			queries.ErrBadParams) || len(stub.asked) != 0 {
			t.Errorf("%s: answered %v having asked the reader %d times, want bad_params before it", name, err,
				len(stub.asked))
		}
	}
	stub := &stubCode{answer: twoRevisions(atCodeSeq(1))}
	_, err := askNative(t, queries.Sources{Code: stub}, "code_change",
		map[string]any{"name": "web", "change": "1", "part": "files", "revision": "3"})
	var missing *queries.NotFound
	if !errors.As(err, &missing) || missing.Reason != queries.ReasonNoRevision {
		t.Errorf("a revision web!1 does not hold answered %v, want not_found (no_revision)", err)
	}
	stub = &stubCode{answer: func(gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		return gitlet.ReadAnswer{}, errors.Join(gitlet.ErrChangeNotFound, errors.New("web!9"))
	}}
	_, err = askNative(t, queries.Sources{Code: stub}, "code_change_routing", map[string]any{"name": "web",
		"change": "9"})
	if !errors.As(err, &missing) || missing.Reason != queries.ReasonNoChange {
		t.Errorf("a change web does not hold answered %v, want not_found (no_change)", err)
	}
}

// A CHANGE'S DIFF PART ASKS EVERY PATCH WITH `patches`: of the revision's base..head, or with
// since_revision of the interdiff; beside a file, or on another part, it is refused unread.
//
// Mutation: drop the !call.patches from the file arm → a diff naming no file is refused, red.
func TestAChangesDiffPartAsksEveryPatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		params map[string]any
		diff   bool
	}{
		{map[string]any{"part": "diff", "patches": "true"}, true},
		{map[string]any{"part": "diff", "patches": "true", "since_revision": "1"}, false},
	} {
		all := map[string]any{"name": "web", "change": "1"}
		for k, v := range tc.params {
			all[k] = v
		}
		stub := &stubCode{answer: twoRevisions(atCodeSeq(1))}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_change", all); err != nil {
			t.Fatalf("%v: %v", tc.params, err)
		}
		q := stub.read[0]
		switch {
		case tc.diff && (q.Diff == nil || !q.Diff.Patches || q.Diff.Head != oid("2") || q.Diff.Base != oid("a")):
			t.Errorf("%v read %+v, want every patch of revision 2's diff", tc.params, q.Diff)
		case !tc.diff && (q.Interdiff == nil || !q.Interdiff.Patches || q.Interdiff.FromHead != oid("1")):
			t.Errorf("%v read %+v, want every patch of the interdiff since revision 1", tc.params, q.Interdiff)
		}
	}
	for name, params := range map[string]map[string]any{
		"patches to the files":    {"part": "files", "patches": "true"},
		"patches beside a file":   {"part": "diff", "patches": "true", "file": "a.go"},
		"patches that is no flag": {"part": "diff", "patches": "yes"},
	} {
		all := map[string]any{"name": "web", "change": "1"}
		for k, v := range params {
			all[k] = v
		}
		stub := &stubCode{answer: twoRevisions(atCodeSeq(1))}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_change", all); !errors.Is(err,
			queries.ErrBadParams) || len(stub.asked) != 0 {
			t.Errorf("%s: answered %v having asked the reader %d times, want bad_params before it", name, err,
				len(stub.asked))
		}
	}
}

// A CHANGE'S READ BY OBJECT ID REFUSES IN THIS SURFACE'S VOCABULARY. Its
// files, a file's patch and the placing of its findings are asked of the
// repository's home Gitlet node, which answers what no row does: an object or a
// path the revision does not hold is not_found (`no_object`) in the Gitlet
// node's own sentence; a read too large to answer whole is the request's to
// narrow; and no Gitlet node able to answer now — none holding the repository in
// time, or none serving Gitlet — is unavailable. None is a 500, and
// none an answer about the change.
//
// Mutation: drop codeRefusal's object arm — a file the revision lacks is an
// unclassified failure, red; drop its unready arm — a home still filling its
// copy is one too, red.
func TestAChangesReadByObjectIDRefusesInThisSurfacesVocabulary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		err   error
		check func(error) bool
	}{
		{"no file", fmt.Errorf("%w: revision 2 holds no file upload/gone.go", gitlet.ErrNotABlob),
			func(err error) bool {
				var absent *queries.NotFound
				return errors.As(err, &absent) && absent.Reason == queries.ReasonNoObject &&
					strings.Contains(absent.Detail, "upload/gone.go")
			}},
		{"no object", fmt.Errorf("%w: web holds no object %s", gitlet.ErrObjectNotFound, oid("2")),
			func(err error) bool {
				var absent *queries.NotFound
				return errors.As(err, &absent) && absent.Reason == queries.ReasonNoObject
			}},
		{"too large", fmt.Errorf("%w: past its budget", gitlet.ErrSliceTooLarge),
			func(err error) bool { return errors.Is(err, queries.ErrBadParams) }},
		{"no Gitlet node in time", &gitlet.ErrGitUnready{Reason: gitlet.UnreadyNotHeld, Detail: "filling its copy"},
			func(err error) bool { return errors.Is(err, queries.ErrUnavailable) }},
		{"Gitlet not served", fmt.Errorf("estate: %w", gitlet.ErrNotServing),
			func(err error) bool { return errors.Is(err, queries.ErrUnavailable) }},
	} {
		stub := &stubCode{answer: twoRevisions(atCodeSeq(30)),
			git: func(gitlet.GitQuery) (gitlet.GitAnswer, error) { return gitlet.GitAnswer{}, tc.err }}
		_, err := askNative(t, queries.Sources{Code: stub}, "code_change", map[string]any{"name": "web",
			"change": "1", "part": "diff", "file": "upload/gone.go"})
		if !tc.check(err) {
			t.Errorf("%s: a change's patch answered %v", tc.name, err)
		}
	}
}

// A CHANGE'S EXCERPTS ANCHOR EACH FINDING AT ITS OWN REVISION, at the floor of the revisions
// they name; an unmeasured revision's findings, and an old side with no base, have none yet.
//
// Mutation: anchor at the head revision, drop the second chunk, or the no-base arm → red.
func TestAChangesExcerptsAnchorEachFindingAtItsOwnRevision(t *testing.T) {
	t.Parallel()
	rev := func(n int64, base string, at uint64) gitlet.RevisionRow {
		r := gitlet.RevisionRow{N: n, Head: oid(fmt.Sprint(n)), Position: atCodeSeq(at)}
		if base != "" {
			r.Facts = &gitlet.RevisionFactsRow{TargetRef: "refs/heads/main", TargetOID: oid("a"),
				TargetPosition: atCodeSeq(at - 1), BaseOID: oid(base)}
		}
		return r
	}
	findings := []gitlet.FindingRow{
		{ID: "new", Revision: 1, CommitOID: oid("1"), Path: "a.go", Side: gitlet.SideNew, BlobOID: oid("c"),
			LineStart: 4, LineEnd: 5},
		{ID: "old", Revision: 2, CommitOID: oid("b"), Path: "b.go", Side: gitlet.SideOld, BlobOID: oid("d"),
			LineStart: 7, LineEnd: 7},
		{ID: "pending", Revision: 3, CommitOID: oid("3"), Path: "c.go", Side: gitlet.SideNew, BlobOID: oid("e"),
			LineStart: 1, LineEnd: 1},
	}
	rows := func(page []gitlet.FindingRow, first gitlet.RevisionRow) func(gitlet.ReadQuery) (gitlet.ReadAnswer,
		error) {
		return func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
			out := gitlet.ReadAnswer{Level: q.Fresh.Level, Position: atCodeSeq(4800)}
			switch {
			case q.Part == gitlet.PartFindings:
				out.Findings, out.Next = page, "after-the-page"
			case q.Cursor == "2":
				out.Revisions = []gitlet.RevisionRow{first}
			case q.Cursor == "3":
				out.Revisions = []gitlet.RevisionRow{rev(2, "b", 20)}
			default:
				out.Change = &gitlet.ChangeDetail{ChangeSummary: gitlet.ChangeSummary{ChangeRow: gitlet.ChangeRow{
					ID: "change-1", RepoID: webID, Number: 1, State: gitlet.ChangeStateOpen, Revision: 3}}}
				out.Revisions = []gitlet.RevisionRow{rev(3, "", 30)}
			}
			return out, nil
		}
	}
	// git answers a chunk's first page with its first excerpt alone, and the rest after it.
	git := func(q gitlet.GitQuery) (gitlet.GitAnswer, error) {
		out := gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Excerpts: &gitlet.ExcerptsAnswer{}}
		anchors := q.Excerpts.Anchors
		if q.Cursor == "" && len(anchors) > 1 {
			anchors, out.Next = anchors[:1], "x:1"
		} else if q.Cursor == "x:1" {
			anchors = anchors[1:]
		}
		for _, a := range anchors {
			out.Excerpts.Excerpts = append(out.Excerpts.Excerpts, gitlet.Excerpt{ID: a.ID, Status: "M",
				Lines: []gitlet.ExcerptLine{{Mark: "+", New: a.Start, Text: a.Path}}})
		}
		return out, nil
	}

	for _, context := range []string{"", "0", "2"} {
		stub := &stubCode{answer: rows(findings, rev(1, "a", 10)), git: git}
		params := map[string]any{"name": "web", "change": "1", "part": "excerpts"}
		if context != "" {
			params["context"] = context
		}
		answer, err := askNative(t, queries.Sources{Code: stub}, "code_change", params)
		if err != nil {
			t.Fatalf("context %q: %v", context, err)
		}
		got, ok := answer.(queries.CodeChangeExcerpts)
		if !ok {
			t.Fatalf("the excerpts part answered %T", answer)
		}
		wantContext := gitlet.DefaultExcerptContext
		if context != "" {
			wantContext = int(context[0] - '0')
		}
		if _, pending := got.Excerpts["pending"]; pending || len(got.Excerpts) != 2 || got.Context != wantContext ||
			got.Next != "after-the-page" {
			t.Errorf("context %q answered %+v, want the two placed findings at context %d beside the page's next",
				context, got, wantContext)
		}
		want := []gitlet.ExcerptAnchor{
			{LineAnchor: gitlet.LineAnchor{ID: "new", Path: "a.go", Side: gitlet.SideNew, BlobOID: oid("c"),
				Start: 4, End: 5}, Base: oid("a"), Head: oid("1")},
			{LineAnchor: gitlet.LineAnchor{ID: "old", Path: "b.go", Side: gitlet.SideOld, BlobOID: oid("d"),
				Start: 7, End: 7}, Base: oid("b"), Head: oid("2")},
		}
		first := stub.read[0]
		switch {
		case first.Kind != gitlet.GitExcerpts || !reflect.DeepEqual(first.Excerpts.Anchors, want):
			t.Errorf("context %q asked %+v, want the anchors %+v", context, first.Excerpts, want)
		case (context == "") != (first.Excerpts.Context == nil),
			context != "" && *first.Excerpts.Context != wantContext:
			t.Errorf("context %q asked the Gitlet node context %v", context, first.Excerpts.Context)
		case !reflect.DeepEqual(first.Fresh, statelog.FloorAt(statelog.ReadSession, atCodeSeq(20))):
			t.Errorf("context %q asked at %+v, want session at revision 2's record", context, first.Fresh)
		}
	}

	// SEVENTY FINDINGS are two reads, of 64 and 6, each page of each followed.
	var many []gitlet.FindingRow
	for i := range 70 {
		f := findings[0]
		f.ID = fmt.Sprintf("f%02d", i)
		many = append(many, f)
	}
	stub := &stubCode{answer: rows(many, rev(1, "a", 10)), git: git}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_change", map[string]any{"name": "web",
		"change": "1", "part": "excerpts", "limit": "70"})
	if err != nil {
		t.Fatal(err)
	}
	if got := answer.(queries.CodeChangeExcerpts); len(got.Excerpts) != len(many) {
		t.Errorf("seventy findings answered %d excerpts", len(got.Excerpts))
	}
	sizes := map[int]int{}
	for _, q := range stub.read {
		if q.Cursor == "" {
			sizes[len(q.Excerpts.Anchors)]++
		}
	}
	if len(stub.read) != 4 || sizes[gitlet.MaxExcerptAnchors] != 1 || sizes[len(many)-gitlet.MaxExcerptAnchors] != 1 {
		t.Errorf("seventy findings were asked as %d reads, first pages of sizes %v", len(stub.read), sizes)
	}

	// A REVISION SHARING NO HISTORY with its target: its new side is every line added, from no
	// base, and its old side holds no file to excerpt.
	unrelated := rev(1, "a", 10)
	unrelated.Facts.BaseOID = ""
	added, gone := findings[0], findings[0]
	added.ID = "added"
	gone.ID, gone.Side, gone.CommitOID = "gone", gitlet.SideOld, oid("f")
	stub = &stubCode{answer: rows([]gitlet.FindingRow{added, gone}, unrelated), git: git}
	answer, err = askNative(t, queries.Sources{Code: stub}, "code_change", map[string]any{"name": "web",
		"change": "1", "part": "excerpts"})
	if err != nil {
		t.Fatal(err)
	}
	wantUnrelated := []gitlet.ExcerptAnchor{{LineAnchor: gitlet.LineAnchor{ID: "added", Path: "a.go",
		Side: gitlet.SideNew, BlobOID: oid("c"), Start: 4, End: 5}, Head: oid("1")}}
	got := answer.(queries.CodeChangeExcerpts)
	if _, ok := got.Excerpts["gone"]; ok || len(got.Excerpts) != 1 || len(stub.read) != 1 ||
		!reflect.DeepEqual(stub.read[0].Excerpts.Anchors, wantUnrelated) {
		t.Errorf("an unrelated revision's findings answered %+v from reads %+v, want the anchors %+v", got,
			stub.read, wantUnrelated)
	}
}

// AN EXCERPT'S CONTEXT IS ITS PART'S ALONE: `context` on another part, or past 0 to
// gitlet.MaxExcerptContext lines, is refused before anything is read.
//
// Mutation: drop the part check (findings answered with a context) → red.
func TestAnExcerptsContextIsItsPartsAlone(t *testing.T) {
	t.Parallel()
	for name, params := range map[string]map[string]any{
		"on the findings":  {"part": "findings", "context": "2"},
		"on the revisions": {"context": "2"},
		"past the widest":  {"part": "excerpts", "context": "11"},
		"below none":       {"part": "excerpts", "context": "-1"},
		"no number":        {"part": "excerpts", "context": "x"},
		"with a revision":  {"part": "excerpts", "revision": "1"},
	} {
		all := map[string]any{"name": "web", "change": "1"}
		for k, v := range params {
			all[k] = v
		}
		stub := &stubCode{answer: twoRevisions(atCodeSeq(1))}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_change", all); !errors.Is(err,
			queries.ErrBadParams) || len(stub.asked) != 0 {
			t.Errorf("%s: answered %v having asked the reader %d times, want bad_params before it", name, err,
				len(stub.asked))
		}
	}
}
