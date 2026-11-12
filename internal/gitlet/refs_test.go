package gitlet_test

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REFS LISTING PAGES BY NAME: a repository's live refs under a prefix — its
// branches by default, its tags, or a seat's own namespace — in name order,
// a limit at a time, its cursor the last ref served; never a tombstone, never
// a ref outside the prefix; and its first page, alone, carries the
// repository in detail, so a seat's first question of a repository is one
// read. A prefix no ref can begin with, a limit past a page, a prefix on
// another kind and a cursor from outside the prefix are refused.
//
// Mutations: the upper bound dropped from refsUnderSQL → the seat's namespace
// lists every branch after it; the tombstones read (the key's range, no
// predicate) → "gone" is listed; the detail on every page, or on none → the
// detail checks fail.
func TestARefsListingPagesByName(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	var made []gitlet.Command
	var branches []string
	for i := range 23 {
		ref := fmt.Sprintf("refs/heads/b%02d", i)
		made = append(made, set(ref, gitlet.ZeroOID, oidOf(ref)))
		branches = append(branches, ref)
	}
	for _, ref := range []string{"refs/heads/seats/dev/x", "refs/heads/seats/dev/y", "refs/heads/zz",
		"refs/heads/gone", "refs/tags/v1"} {
		made = append(made, set(ref, gitlet.ZeroOID, oidOf(ref)))
	}
	branches = append(branches, "refs/heads/seats/dev/x", "refs/heads/seats/dev/y", "refs/heads/zz")
	slices.Sort(branches)
	n.push(id, entry("p1", made...))
	n.push(id, entry("p2", set("refs/heads/gone", oidOf("refs/heads/gone"), gitlet.ZeroOID)))

	// listing follows a listing to its end, answering every ref and the
	// pages it took, and holds each page's detail to the first page alone.
	listing := func(prefix string, limit int) ([]string, int) {
		t.Helper()
		var refs []string
		q := gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app", Prefix: prefix, Limit: limit}
		for pages := 1; pages <= 20; pages++ {
			answer, err := n.reader.Read(t.Context(), q)
			if err != nil {
				t.Fatalf("Read(refs under %q) page %d: %v", prefix, pages, err)
			}
			switch {
			case pages == 1 && (answer.Repo == nil || answer.Repo.Name != "app" || answer.Repo.Branches != 26 ||
				answer.Repo.Tags != 1 || answer.Repo.Tombstones != 1):
				t.Fatalf("the first page's detail is %+v, want app's: 26 branches, one tag, one tombstone",
					answer.Repo)
			case pages > 1 && answer.Repo != nil:
				t.Fatalf("page %d carries the detail again: %+v", pages, answer.Repo)
			}
			for _, r := range answer.Refs {
				if r.OID != oidOf(r.Ref) || r.Position.IsZero() {
					t.Fatalf("%s is served at %q, %v; want its own oid and the position that set it",
						r.Ref, r.OID, r.Position)
				}
				refs = append(refs, r.Ref)
			}
			if answer.Next == "" {
				return refs, pages
			}
			q.Cursor = answer.Next
		}
		t.Fatalf("the listing under %q did not end in twenty pages", prefix)
		return nil, 0
	}
	if got, pages := listing("", 7); fmt.Sprint(got) != fmt.Sprint(branches) || pages != 4 {
		t.Fatalf("the branches read %v in %d pages, want %v in four", got, pages, branches)
	}
	if got, _ := listing("refs/tags/", 0); fmt.Sprint(got) != "[refs/tags/v1]" {
		t.Fatalf("the tags read %v", got)
	}
	if got, _ := listing("refs/heads/seats/dev/", 0); fmt.Sprint(got) != "[refs/heads/seats/dev/x refs/heads/seats/dev/y]" {
		t.Fatalf("dev's namespace reads %v", got)
	}

	for name, q := range map[string]gitlet.ReadQuery{
		"a prefix with no slash":    {Kind: gitlet.ReadRefs, Repo: "app", Prefix: "refs/heads"},
		"a namespace no ref has":    {Kind: gitlet.ReadRefs, Repo: "app", Prefix: "refs/notes/"},
		"a prefix no ref holds":     {Kind: gitlet.ReadRefs, Repo: "app", Prefix: "refs/heads/a..b/"},
		"a prefix on another kind":  {Kind: gitlet.ReadRepo, Repo: "app", Prefix: "refs/heads/"},
		"a limit past a page":       {Kind: gitlet.ReadRefs, Repo: "app", Limit: gitlet.MaxRefsPerPage + 1},
		"a listing naming no repo":  {Kind: gitlet.ReadRefs},
		"a range on a refs listing": {Kind: gitlet.ReadRefs, Repo: "app", Introduced: &gitlet.IntroducedQuery{RepoID: id}},
	} {
		q.Asker = asker
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s: Read = %v, want ErrInvalid", name, err)
		}
	}
	_, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app",
		Prefix: "refs/tags/", Cursor: "refs/heads/b03"})
	if !errors.Is(err, gitlet.ErrBadCursor) {
		t.Errorf("a cursor outside the prefix = %v, want ErrBadCursor", err)
	}
}

// RESOLVEREF PREFERS HEADS, THEN TAGS, AND ANSWERS EVERY NAME IN ONE READ: a
// short name is a branch where one is live, else a tag — a branch deleted
// falls through to a tag of its name — and a whole ref is itself, every name
// of the query answered in its order at the one checkpoint the read was at,
// with where its ref last moved: the position of the push that set it, never
// a later one's or the checkpoint's. A name none of them hold is answered as
// such — naming every ref tried, refused ErrRefNotFound by its Err — beside
// the names that resolved, never failing them. A query naming no ref, more
// than MaxRefsPerQuery, or any name no ref can have, no level, no asker and a
// deleted repository are refused whole, the last in the sentence Gitlet's
// admission says.
//
// Mutation: the tags tried before the heads → v1 resolves to the tag; the
// answer's Moved the checkpoint → v1 moved at the read; a missing name
// failing the query → v1 is not answered beside it.
func TestResolveRefPrefersHeadsThenTags(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	p1 := n.push(id, entry("p1",
		set("refs/heads/v1", gitlet.ZeroOID, oidOf("branch v1")),
		set("refs/tags/v1", gitlet.ZeroOID, oidOf("tag v1")),
		set("refs/tags/rel", gitlet.ZeroOID, oidOf("rel")),
		set("refs/heads/old", gitlet.ZeroOID, oidOf("branch old")),
		set("refs/tags/old", gitlet.ZeroOID, oidOf("tag old"))))
	n.push(id, entry("p2", set("refs/heads/old", oidOf("branch old"), gitlet.ZeroOID)))
	p3 := n.push(id, entry("p3", set("refs/heads/rel", gitlet.ZeroOID, oidOf("branch rel"))))
	n.push(id, entry("p4", set("refs/heads/later", gitlet.ZeroOID, oidOf("later"))))
	resolve := func(refs ...string) (gitlet.RefAnswer, error) {
		return n.reader.ResolveRef(t.Context(), gitlet.RefQuery{Repo: "app", Refs: refs, Fresh: stale, Asker: asker})
	}
	want := map[string]struct {
		ref, oid string
		moved    statelog.Position
	}{
		"v1":            {"refs/heads/v1", oidOf("branch v1"), p1.Position},
		"refs/tags/v1":  {"refs/tags/v1", oidOf("tag v1"), p1.Position},
		"rel":           {"refs/heads/rel", oidOf("branch rel"), p3.Position},
		"refs/tags/rel": {"refs/tags/rel", oidOf("rel"), p1.Position},
		"old":           {"refs/tags/old", oidOf("tag old"), p1.Position},
		"refs/heads/v1": {"refs/heads/v1", oidOf("branch v1"), p1.Position},
	}
	names := slices.Sorted(maps.Keys(want))
	got, err := resolve(names...)
	if err != nil || got.Repo.ID != id || got.Position != n.waiter.Committed() || len(got.Refs) != len(names) {
		t.Fatalf("ResolveRef(%v) = %+v, %v; want an answer for each, read at the checkpoint %v", names, got, err,
			n.waiter.Committed())
	}
	for i, name := range names {
		w, r := want[name], got.Refs[i]
		if r.Name != name || r.Ref != w.ref || r.OID != w.oid || r.Moved != w.moved || r.Missing != "" {
			t.Errorf("ResolveRef's answer %d = %+v; want %s as %s at %s, moved at %v", i, r, name, w.ref, w.oid,
				w.moved)
		}
	}
	if err := got.Err(); err != nil {
		t.Errorf("an answer resolving every name refuses %v", err)
	}
	for ref, tried := range map[string]string{
		"refs/heads/old": `"refs/heads/old"`,
		"nope":           `"refs/heads/nope" or "refs/tags/nope"`,
		gitlet.HeadRef:   `"refs/heads/main", its default branch`,
	} {
		got, err := resolve(ref, "v1")
		if err != nil || len(got.Refs) != 2 || got.Refs[1].OID != oidOf("branch v1") {
			t.Errorf("ResolveRef(%s, v1) = %+v, %v; want v1 answered beside the missing name", ref, got, err)
			continue
		}
		m := got.Refs[0]
		if m.Name != ref || m.Ref != "" || m.OID != "" || !strings.Contains(m.Missing, tried) {
			t.Errorf("ResolveRef(%s)'s answer = %+v, want it missing, naming %s", ref, m, tried)
		}
		if err := got.Err(); !errors.Is(err, gitlet.ErrRefNotFound) || !strings.Contains(err.Error(), tried) {
			t.Errorf("ResolveRef(%s, v1) refuses %v, want ErrRefNotFound naming %s", ref, err, tried)
		}
	}
	both, err := resolve("nope", "gone")
	if err != nil {
		t.Fatalf("ResolveRef(nope, gone) = %v, want both answered missing", err)
	}
	if refusal := both.Err(); refusal == nil || !strings.Contains(refusal.Error(),
		`"refs/tags/nope"; repository app holds no live "refs/heads/gone"`) {
		t.Errorf("ResolveRef(nope, gone) refuses %v; want both missing names named, in order", refusal)
	}
	past := make([]string, gitlet.MaxRefsPerQuery+1)
	for i := range past {
		past[i] = "v1"
	}
	for name, q := range map[string]gitlet.RefQuery{
		"a ref no repository holds":      {Repo: "app", Refs: []string{"a..b"}, Fresh: stale, Asker: asker},
		"a whole ref git refuses":        {Repo: "app", Refs: []string{"refs/heads/a..b"}, Fresh: stale, Asker: asker},
		"one such name among good ones":  {Repo: "app", Refs: []string{"v1", "a..b"}, Fresh: stale, Asker: asker},
		"no ref":                         {Repo: "app", Fresh: stale, Asker: asker},
		"more refs than a resolve reads": {Repo: "app", Refs: past, Fresh: stale, Asker: asker},
		"no asker":                       {Repo: "app", Refs: []string{"v1"}, Fresh: stale},
		"no repository":                  {Refs: []string{"v1"}, Fresh: stale, Asker: asker},
	} {
		if _, err := n.reader.ResolveRef(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s: ResolveRef = %v, want ErrInvalid", name, err)
		}
	}
	for name, fresh := range map[string]statelog.Freshness{
		"no level":                {},
		"a session with no floor": {Level: statelog.ReadSession},
	} {
		_, err := n.reader.ResolveRef(t.Context(), gitlet.RefQuery{Repo: "app", Refs: []string{"v1"}, Fresh: fresh,
			Asker: asker})
		if !errors.Is(err, gitlet.ErrBadLevel) {
			t.Errorf("%s: ResolveRef = %v, want ErrBadLevel", name, err)
		}
	}
	if _, err := n.store.DeleteRepo(t.Context(), n.activate(t2), id); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	if _, err := resolve("v1"); !errors.Is(err, gitlet.ErrRepoNotFound) || !strings.Contains(err.Error(), "is deleted") {
		t.Fatalf("ResolveRef on a deleted repository = %v, want ErrRepoNotFound saying it is deleted", err)
	}
}

// HEAD IS THE DEFAULT BRANCH THE ROW NAMES, IN THE SAME READ: a resolve
// naming gitlet.HeadRef answers the branch the repository's row names its
// default — read in the resolve's own transaction, so no asker reads the row
// first to learn it — by the name HEAD, with its whole ref, its object and
// where it last moved, beside the other names of the query; never a tag
// called HEAD, and never the branch a short name would reach. Where the
// default branch holds no live ref it is missing, the sentence naming it as
// the default branch.
//
// Mutation: HEAD not resolved through the row → it is missing naming no
// branch; the default branch tried as a short name, its tag after it → the
// tag trunk answers a HEAD whose branch holds no ref.
func TestResolveRefReadsHEADAsTheDefaultBranch(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	settings := described("app")
	settings.DefaultBranch = "trunk"
	id := n.ensure("app", settings, t1).RepoID
	p1 := n.push(id, entry("p1",
		set("refs/heads/main", gitlet.ZeroOID, oidOf("branch main")),
		set("refs/tags/HEAD", gitlet.ZeroOID, oidOf("tag HEAD")),
		set("refs/tags/trunk", gitlet.ZeroOID, oidOf("tag trunk"))))
	resolve := func(refs ...string) gitlet.RefAnswer {
		t.Helper()
		got, err := n.reader.ResolveRef(t.Context(), gitlet.RefQuery{Repo: "app", Refs: refs, Fresh: stale,
			Asker: asker})
		if err != nil || len(got.Refs) != len(refs) || got.Repo.DefaultBranch != "trunk" {
			t.Fatalf("ResolveRef(%v) = %+v, %v; want an answer for each, of app whose default is trunk", refs,
				got, err)
		}
		return got
	}

	absent := resolve(gitlet.HeadRef, "main").Refs
	if m := absent[0]; m.Name != gitlet.HeadRef || m.Ref != "" || m.OID != "" ||
		m.Missing != `repository app holds no live "refs/heads/trunk", its default branch` {
		t.Errorf("HEAD of a repository whose default branch holds no ref resolved %+v, want it missing, "+
			"naming refs/heads/trunk as the default branch — never the tag HEAD or the tag trunk", m)
	}
	if absent[1].Ref != "refs/heads/main" || absent[1].OID != oidOf("branch main") {
		t.Errorf("main beside a missing HEAD resolved %+v, want refs/heads/main", absent[1])
	}

	p2 := n.push(id, entry("p2", set("refs/heads/trunk", gitlet.ZeroOID, oidOf("branch trunk"))))
	got := resolve("main", gitlet.HeadRef)
	if h := got.Refs[1]; h.Name != gitlet.HeadRef || h.Ref != "refs/heads/trunk" || h.OID != oidOf("branch trunk") ||
		h.Moved != p2.Position || h.Missing != "" {
		t.Errorf("HEAD resolved %+v, want refs/heads/trunk at %s, moved at %v", h, oidOf("branch trunk"),
			p2.Position)
	}
	if m := got.Refs[0]; m.Ref != "refs/heads/main" || m.Moved != p1.Position {
		t.Errorf("main beside HEAD resolved %+v, want refs/heads/main moved at %v", m, p1.Position)
	}
}
