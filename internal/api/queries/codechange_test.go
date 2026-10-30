package queries_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/code"
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
func twoRevisions(answeredAt statelog.Position) func(code.ReadQuery) (code.ReadAnswer, error) {
	rev := func(n int64, head, base string, at uint64) code.RevisionRow {
		return code.RevisionRow{N: n, Head: oid(head), Position: atCodeSeq(at), Facts: &code.RevisionFactsRow{
			TargetRef: "refs/heads/main", TargetOID: oid("a"), TargetPosition: atCodeSeq(at - 1),
			BaseOID: oid(base), Touched: []string{"upload/retry.go"}}}
	}
	return func(q code.ReadQuery) (code.ReadAnswer, error) {
		out := code.ReadAnswer{Level: q.Fresh.Level, Position: answeredAt}
		switch {
		case q.Part == code.PartFindings:
			out.Findings = []code.FindingRow{{ID: "f1", Revision: 1, Path: "upload/retry.go", Side: code.SideNew,
				BlobOID: oid("b"), LineStart: 4, LineEnd: 4}}
		case q.Cursor == "2":
			out.Revisions = []code.RevisionRow{rev(1, "1", "a", 10)}
		default:
			out.Change = &code.ChangeDetail{ChangeSummary: code.ChangeSummary{ChangeRow: code.ChangeRow{
				ID: "change-1", RepoID: webID, Number: 1, State: code.ChangeStateOpen, Revision: 2}}}
			out.Revisions = []code.RevisionRow{rev(2, "2", "a", 20)}
		}
		return out, nil
	}
}

// THE CHANGE QUESTIONS READ AT THE CALLER'S FLOOR. A screen reads its own
// write to a change back at `session` beside the position the write answered
// with, so each change question hands the reader the caller's WHOLE freshness
// on its first read of the rows; a part read by object id reads every later
// row of its answer at `session` floored at that first read's position — so
// it is never assembled from a copy older than the one its first read saw —
// and asks the git node at the floor of the records that moved each revision
// it reads and its target (code.ObjectFloor), never at the caller's floor or
// a row read's checkpoint.
//
// Mutations: read a later revision at the caller's freshness → its read is
// not floored at the first's answer, red; ask the git node at the caller's
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
		git      code.GitReadKind
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
		{"code_change", map[string]any{"part": "files"}, []statelog.Freshness{asked}, code.GitDiff,
			atCodeSeq(20)},
		// What changed since revision 1: the head, revision 1 floored at
		// the head's answer, the interdiff at the later of the two.
		{"code_change", map[string]any{"part": "diff", "since_revision": "1", "file": "upload/retry.go"},
			[]statelog.Freshness{asked, after}, code.GitInterdiff, atCodeSeq(20)},
		// Revision 1's files: the head, revision 1 floored, its diff at 10.
		{"code_change", map[string]any{"part": "files", "revision": "1"},
			[]statelog.Freshness{asked, after}, code.GitDiff, atCodeSeq(10)},
		// The findings placed in the head: the head, the findings page
		// floored at its answer, and a re-anchoring at the head's floor.
		{"code_change", map[string]any{"part": "findings"}, []statelog.Freshness{asked, after}, code.GitReanchor,
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
			git: func(q code.GitQuery) (code.GitAnswer, error) {
				out := code.GitAnswer{Kind: q.Kind, RepoID: q.RepoID}
				if q.Reanchor != nil {
					out.Reanchor = &code.ReanchorAnswer{}
					for _, a := range q.Reanchor.Anchors {
						out.Reanchor.Anchors = append(out.Reanchor.Anchors, code.Reanchored{ID: a.ID, Start: 6,
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
		case stub.read[0].RepoID != webID || stub.read[0].Asker == (code.Principal{}):
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
	stub = &stubCode{answer: func(code.ReadQuery) (code.ReadAnswer, error) {
		return code.ReadAnswer{}, errors.Join(code.ErrChangeNotFound, errors.New("web!9"))
	}}
	_, err = askNative(t, queries.Sources{Code: stub}, "code_change_routing", map[string]any{"name": "web",
		"change": "9"})
	if !errors.As(err, &missing) || missing.Reason != queries.ReasonNoChange {
		t.Errorf("a change web does not hold answered %v, want not_found (no_change)", err)
	}
}
