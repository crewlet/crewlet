package query_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/query"
)

// A COMPARISON COUNTS BOTH SIDES. Over main A-B-C and a branch D-E off B:
// comparing C with E finds their merge base B, neither containing the other,
// E two commits ahead and one behind, and the files E changed since B — the
// three-dot reading; comparing A with C finds A their merge base, a
// fast-forward from A to C; the reverse has C containing A and nothing changed
// since; and a history that shares no commit with main has no merge base and
// no files, each side counted whole. A tree is no commit to compare.
func TestACompareCountsBothSides(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	a := x.commit(map[string]string{"f": "a\n"}, "A")
	b := x.commit(map[string]string{"f": "b\n"}, "B")
	c := x.commit(map[string]string{"f": "c\n"}, "C")
	x.git("checkout", "-q", "-b", "topic", b)
	x.p.Commit(x.work, map[string]string{"g": "d\n"}, "D")
	e := x.p.Commit(x.work, map[string]string{"h": "e\n", "g": "e\n"}, "E")
	x.push(codetest.RepoName, "topic")
	x.git("checkout", "-q", "--orphan", "other")
	o := x.p.Commit(x.work, map[string]string{"z": "z\n"}, "O")
	x.push(codetest.RepoName, "other")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	for _, tc := range []struct {
		name          string
		base, head    string
		mergeBase     string
		baseAnc       bool
		headAnc       bool
		ahead, behind int
		files         []string
	}{
		{"diverged", c, e, b, false, false, 2, 1, []string{"g", "h"}},
		{"a fast-forward", a, c, a, true, false, 2, 0, []string{"f"}},
		{"behind", c, a, a, false, true, 0, 2, nil},
		{"unrelated", c, o, "", false, false, 1, 3, nil},
		{"by abbreviations", c[:8], e[:8], b, false, false, 2, 1, []string{"g", "h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRead(t, s, compareQ(repo, tc.base, tc.head)).Compare
			var files []string
			for _, f := range got.Files {
				files = append(files, f.Path)
			}
			if got.MergeBase != tc.mergeBase || got.BaseIsAncestor != tc.baseAnc || got.HeadIsAncestor != tc.headAnc ||
				got.Ahead != tc.ahead || got.Behind != tc.behind || !slices.Equal(files, tc.files) ||
				got.Total != len(tc.files) {
				t.Errorf("compare %s %s = %+v, want merge base %q, base ancestor %v, head ancestor %v, ahead %d, "+
					"behind %d, files %v", tc.base, tc.head, got, tc.mergeBase, tc.baseAnc, tc.headAnc, tc.ahead,
					tc.behind, tc.files)
			}
			if !strings.HasPrefix(got.Base, tc.base) || !strings.HasPrefix(got.Head, tc.head) || len(got.Base) != 40 {
				t.Errorf("compared %s and %s, want the whole ids of %s and %s", got.Base, got.Head, tc.base, tc.head)
			}
		})
	}

	tree := strings.TrimSpace(x.git("rev-parse", c+"^{tree}"))
	if _, err := s.Read(t.Context(), compareQ(repo, tree, c)); !errors.Is(err, code.ErrInvalid) {
		t.Errorf("comparing a tree = %v, want ErrInvalid", err)
	}
}
