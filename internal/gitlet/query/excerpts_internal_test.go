package query

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
)

// THE CUTTER HOLDS NO LINE PAST ITS WINDOW while a patch streams past it in small writes, and
// a long line split across writes is described by its whole size.
//
// Mutation: hold pending lines unbounded, or count a long line's size from what was kept → red.
func TestTheCutterHoldsNoLinePastItsWindow(t *testing.T) {
	t.Parallel()
	q := &gitlet.ExcerptsQuery{Context: new(0), Anchors: []gitlet.ExcerptAnchor{
		{LineAnchor: gitlet.LineAnchor{ID: "old", Side: gitlet.SideOld, Start: 10, End: 11}},
		{LineAnchor: gitlet.LineAnchor{ID: "long", Side: gitlet.SideNew, Start: 10_012, End: 10_012}},
	}}
	cut := newExcerptCutter(q)
	cut.pairWith(0, "M", excerptPair{old: "a", new: "b"}, false)
	cut.pairWith(1, "M", excerptPair{old: "a", new: "b"}, false)
	var patch strings.Builder
	patch.WriteString("diff --git a/0 b/0\nindex 1..2 100644\n--- a/0\n+++ b/0\n@@ -1,12 +1,10012 @@\n")
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&patch, " line %d\n", i)
	}
	text := strings.Repeat("x", 500)
	for range 10_000 {
		fmt.Fprintf(&patch, "+%s\n", text)
	}
	long := strings.Repeat("y", 3*gitlet.MaxExcerptLineBytes)
	fmt.Fprintf(&patch, " line 11\n-line 12\n+%s\n\\ No newline at end of file\n", long)
	body := []byte(patch.String())
	for len(body) > 0 {
		n := min(7, len(body))
		if _, err := cut.Write(body[:n]); err != nil {
			t.Fatal(err)
		}
		body = body[n:]
	}
	if err := cut.close(); err != nil {
		t.Fatal(err)
	}
	got := cut.excerpts()
	if e := got[0]; len(e.Lines) != gitlet.MaxExcerptLines || !e.More || e.Lines[0].Text != "line 10" ||
		e.Lines[1].Text != text {
		t.Errorf("the insertion's window is %d lines, more %t, starting %+v", len(e.Lines), e.More, e.Lines[0])
	}
	if bound := gitlet.MaxExcerptLines * gitlet.MaxExcerptLineBytes; cut.peak > bound {
		t.Errorf("the cutter held %d bytes at once, past the %d one excerpt shows", cut.peak, bound)
	}
	want := gitlet.ExcerptLine{Mark: "+", New: 10_012, TextBytes: len(long)}
	if e := got[1]; len(e.Lines) != 1 || e.Lines[0] != want {
		t.Errorf("the long line's excerpt is %+v, want %+v", e.Lines, want)
	}
}

// AN EXCERPTS PAGE IS KEPT BY ALL IT IS A FUNCTION OF: each anchor field and the applied
// context move its memo key; equal spellings of one read keep it.
//
// Mutation: leave an anchor's end out of the key (two ranges answer one page) → red.
func TestAnExcerptsMemoKeyNamesAllItReads(t *testing.T) {
	t.Parallel()
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	whole := map[string]string{base: base, head: head, head[:12]: head}
	one := func(m func(q *gitlet.ExcerptsQuery)) cache.MemoKey {
		q := gitlet.ExcerptsQuery{Anchors: []gitlet.ExcerptAnchor{{LineAnchor: gitlet.LineAnchor{ID: "f",
			Path: "a.go", Side: gitlet.SideNew, BlobOID: strings.Repeat("c", 40), Start: 3, End: 4},
			Base: base, Head: head}}}
		m(&q)
		r := &read{s: &Server{}, q: gitlet.GitQuery{Kind: gitlet.GitExcerpts, Excerpts: &q}}
		return r.memoKey(whole, cursor{})
	}
	key := one(func(*gitlet.ExcerptsQuery) {})
	for name, m := range map[string]func(q *gitlet.ExcerptsQuery){
		"id":       func(q *gitlet.ExcerptsQuery) { q.Anchors[0].ID = "g" },
		"path":     func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Path = "b.go" },
		"side":     func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Side = gitlet.SideOld },
		"blob":     func(q *gitlet.ExcerptsQuery) { q.Anchors[0].BlobOID = strings.Repeat("d", 40) },
		"start":    func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Start = 2 },
		"end":      func(q *gitlet.ExcerptsQuery) { q.Anchors[0].End = 5 },
		"no base":  func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Base = "" },
		"the head": func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Head = base },
		"context":  func(q *gitlet.ExcerptsQuery) { q.Context = new(0) },
		"anchors":  func(q *gitlet.ExcerptsQuery) { q.Anchors = append(q.Anchors, q.Anchors[0]) },
	} {
		if one(m) == key {
			t.Errorf("an excerpts read with another %s keeps the first's memo key", name)
		}
	}
	for name, m := range map[string]func(q *gitlet.ExcerptsQuery){
		"the path as bytes":   func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Path, q.Anchors[0].PathB64 = "", "YS5nbw==" },
		"the default context": func(q *gitlet.ExcerptsQuery) { q.Context = new(gitlet.DefaultExcerptContext) },
		"an abbreviated head": func(q *gitlet.ExcerptsQuery) { q.Anchors[0].Head = head[:12] },
	} {
		if one(m) != key {
			t.Errorf("an excerpts read naming %s has another memo key", name)
		}
	}
}
