package queries_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
)

// A CHANGE'S FINDINGS BY PATH ARE ASKED BY THEIR OWN KEY: `part=finding_paths` asks
// gitlet.ReadQuery.FindingPaths with no part; a revision or a file beside it is refused.
//
// Mutation: pass the part through as gitlet.ReadQuery.Part, and the reader is asked a part.
func TestAChangesFindingPathsAreAskedByTheirOwnKey(t *testing.T) {
	t.Parallel()
	groups := []gitlet.FindingPathRow{{Path: "a/b.go", State: gitlet.FindingOpen, Revision: 2, Findings: 3}}
	stub := &stubCode{answer: func(gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		return gitlet.ReadAnswer{FindingPaths: groups, Next: "2/open/a/b.go"}, nil
	}}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_change", map[string]any{"name": "web",
		"change": "1", "part": "finding_paths", "limit": "7", "cursor": "1/open/a.go"})
	if err != nil {
		t.Fatalf("part=finding_paths: %v", err)
	}
	if len(stub.asked) != 1 {
		t.Fatalf("the reader was asked %d times, want once", len(stub.asked))
	}
	q := stub.asked[0]
	if q.Kind != gitlet.ReadChange || !q.FindingPaths || q.Part != "" || q.Limit != 7 || q.Cursor != "1/open/a.go" {
		t.Errorf("the reader was asked kind %q, finding_paths %t, part %q, limit %d, cursor %q; want a change "+
			"read by the key alone at limit 7 after 1/open/a.go", q.Kind, q.FindingPaths, q.Part, q.Limit, q.Cursor)
	}
	got, ok := answer.(gitlet.ReadAnswer)
	if !ok || !reflect.DeepEqual(got.FindingPaths, groups) || got.Next != "2/open/a/b.go" {
		t.Errorf("answered %#v, want the reader's groups and cursor as given", answer)
	}

	for name, extra := range map[string]map[string]any{
		"a revision": {"revision": "2"},
		"a file":     {"file": "a"},
	} {
		params := map[string]any{"name": "web", "change": "1", "part": "finding_paths"}
		for k, v := range extra {
			params[k] = v
		}
		refused := &stubCode{answer: stub.answer}
		if _, err := askNative(t, queries.Sources{Code: refused}, "code_change", params); !errors.Is(err,
			queries.ErrBadParams) || len(refused.asked) != 0 {
			t.Errorf("%s beside finding_paths answered %v having asked the reader %d times, want bad_params "+
				"before it", name, err, len(refused.asked))
		}
	}
}

// A CHANGE'S VIEWED FILES ARE ASKED BY THEIR OWN KEY: `part=viewed` asks gitlet.ReadQuery.Viewed with
// no part, its limit and cursor passed through, and answers the reader's rows, key and cursor as
// given; the same read through an assistant grant is answered the rows and cursor without the key;
// a revision, a file or context beside it is refused before the reader is asked.
//
// Mutations: pass the part through as gitlet.ReadQuery.Part (red); drop the strip, and the assistant
// reads the key (red).
func TestAChangesViewedFilesAreAskedByTheirOwnKey(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("a", 64)
	rows := []gitlet.ViewedRow{{ViewedMark: gitlet.ViewedMark{PathDigest: digest, FileDigest: digest},
		At: time.Unix(1_700_000_000, 0).UTC()}}
	key := gitlet.ViewedKey(strings.Repeat("b", 64))
	stub := &stubCode{answer: func(gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		return gitlet.ReadAnswer{Viewed: rows, ViewedKey: key, Next: digest}, nil
	}}
	params := map[string]any{"name": "web", "change": "1", "part": "viewed", "limit": "7", "cursor": digest}
	answer, err := askNative(t, queries.Sources{Code: stub}, "code_change", params)
	if err != nil {
		t.Fatalf("part=viewed: %v", err)
	}
	if len(stub.asked) != 1 {
		t.Fatalf("the reader was asked %d times, want once", len(stub.asked))
	}
	q := stub.asked[0]
	if q.Kind != gitlet.ReadChange || !q.Viewed || q.Part != "" || q.Limit != 7 || q.Cursor != digest {
		t.Errorf("the reader was asked kind %q, viewed %t, part %q, limit %d, cursor %q; want a change read by "+
			"the key alone at limit 7 after the digest", q.Kind, q.Viewed, q.Part, q.Limit, q.Cursor)
	}
	got, ok := answer.(gitlet.ReadAnswer)
	if !ok || !reflect.DeepEqual(got.Viewed, rows) || got.ViewedKey != key || got.Next != digest {
		t.Errorf("answered %#v, want the reader's rows, key and cursor as given", answer)
	}

	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Code: stub})
	assisted, err := r.Answer(through(iam.Principal{ID: uuid.New(), Login: "pat:ada", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: iam.Apps}), "code_change", params)
	if err != nil {
		t.Fatalf("part=viewed through the assistant: %v", err)
	}
	if got, ok := assisted.(gitlet.ReadAnswer); !ok || !reflect.DeepEqual(got.Viewed, rows) || got.ViewedKey != "" ||
		got.Next != digest {
		t.Errorf("the assistant was answered %#v, want the rows and cursor without the key", assisted)
	}

	for name, extra := range map[string]map[string]any{
		"a revision": {"revision": "2"},
		"a file":     {"file": "a"},
		"context":    {"context": "3"},
	} {
		params := map[string]any{"name": "web", "change": "1", "part": "viewed"}
		for k, v := range extra {
			params[k] = v
		}
		refused := &stubCode{answer: stub.answer}
		if _, err := askNative(t, queries.Sources{Code: refused}, "code_change", params); !errors.Is(err,
			queries.ErrBadParams) || len(refused.asked) != 0 {
			t.Errorf("%s beside viewed answered %v having asked the reader %d times, want bad_params before it",
				name, err, len(refused.asked))
		}
	}
}
