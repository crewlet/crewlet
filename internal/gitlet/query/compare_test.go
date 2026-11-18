package query_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// A COMPARISON COUNTS BOTH SIDES, finds the merge base and lists what head
// changed since it (the three-dot reading): for diverged branches, a
// fast-forward and its reverse, an empty commit ahead (its files an empty
// list, never a null), and an unrelated history (no merge base, each side
// counted whole). A tree is no commit to compare.
func TestACompareCountsBothSides(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	a := x.commit(map[string]string{"f": "a\n"}, "A")
	b := x.commit(map[string]string{"f": "b\n"}, "B")
	c := x.commit(map[string]string{"f": "c\n"}, "C")
	x.git("checkout", "-q", "-b", "topic", b)
	x.p.Commit(x.work, map[string]string{"g": "d\n"}, "D")
	e := x.p.Commit(x.work, map[string]string{"h": "e\n", "g": "e\n"}, "E")
	x.push(gitlettest.RepoName, "topic")
	x.git("checkout", "-q", "--orphan", "other")
	o := x.p.Commit(x.work, map[string]string{"z": "z\n"}, "O")
	x.push(gitlettest.RepoName, "other")
	x.git("checkout", "-q", "-b", "empty", c)
	f := x.p.Commit(x.work, nil, "F")
	x.push(gitlettest.RepoName, "empty")
	repo := x.repo(gitlettest.RepoName)
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
		{"an empty commit ahead", c, f, c, true, false, 1, 0, nil},
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
			if got.Files == nil {
				t.Errorf("compare %s %s: the files are null, want a list (empty where nothing changed)", tc.base,
					tc.head)
			}
			if !strings.HasPrefix(got.Base, tc.base) || !strings.HasPrefix(got.Head, tc.head) || len(got.Base) != 40 {
				t.Errorf("compared %s and %s, want the whole ids of %s and %s", got.Base, got.Head, tc.base, tc.head)
			}
		})
	}

	tree := strings.TrimSpace(x.git("rev-parse", c+"^{tree}"))
	if _, err := s.Read(t.Context(), compareQ(repo, tree, c)); !errors.Is(err, gitlet.ErrInvalid) {
		t.Errorf("comparing a tree = %v, want ErrInvalid", err)
	}
}
