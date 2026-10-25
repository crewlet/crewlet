package code_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
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
	var made []code.Command
	var branches []string
	for i := range 23 {
		ref := fmt.Sprintf("refs/heads/b%02d", i)
		made = append(made, set(ref, code.ZeroOID, oidOf(ref)))
		branches = append(branches, ref)
	}
	for _, ref := range []string{"refs/heads/seats/dev/x", "refs/heads/seats/dev/y", "refs/heads/zz",
		"refs/heads/gone", "refs/tags/v1"} {
		made = append(made, set(ref, code.ZeroOID, oidOf(ref)))
	}
	branches = append(branches, "refs/heads/seats/dev/x", "refs/heads/seats/dev/y", "refs/heads/zz")
	slices.Sort(branches)
	n.push(id, entry("p1", made...))
	n.push(id, entry("p2", set("refs/heads/gone", oidOf("refs/heads/gone"), code.ZeroOID)))

	// listing follows a listing to its end, answering every ref and the
	// pages it took, and holds each page's detail to the first page alone.
	listing := func(prefix string, limit int) ([]string, int) {
		t.Helper()
		var refs []string
		q := code.ReadQuery{Asker: asker, Kind: code.ReadRefs, Repo: "app", Prefix: prefix, Limit: limit}
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

	for name, q := range map[string]code.ReadQuery{
		"a prefix with no slash":    {Kind: code.ReadRefs, Repo: "app", Prefix: "refs/heads"},
		"a namespace no ref has":    {Kind: code.ReadRefs, Repo: "app", Prefix: "refs/notes/"},
		"a prefix no ref holds":     {Kind: code.ReadRefs, Repo: "app", Prefix: "refs/heads/a..b/"},
		"a prefix on another kind":  {Kind: code.ReadRepo, Repo: "app", Prefix: "refs/heads/"},
		"a limit past a page":       {Kind: code.ReadRefs, Repo: "app", Limit: code.MaxRefsPerPage + 1},
		"a listing naming no repo":  {Kind: code.ReadRefs},
		"a range on a refs listing": {Kind: code.ReadRefs, Repo: "app", Introduced: &code.IntroducedQuery{RepoID: id}},
	} {
		q.Asker = asker
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s: Read = %v, want ErrInvalid", name, err)
		}
	}
	_, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRefs, Repo: "app",
		Prefix: "refs/tags/", Cursor: "refs/heads/b03"})
	if !errors.Is(err, code.ErrBadCursor) {
		t.Errorf("a cursor outside the prefix = %v, want ErrBadCursor", err)
	}
}

// RESOLVEREF PREFERS HEADS, THEN TAGS: a short name is a branch where one is
// live, else a tag — a branch deleted falls through to a tag of its name — a
// whole ref is itself, and a name none of them hold is not found, naming
// every name tried. The answer carries the repository and the checkpoint it
// was read at. A name no ref can have, no level, no asker and a deleted
// repository are refused, the last in the sentence the code host's admission
// says.
//
// Mutation: the tags tried before the heads → v1 resolves to the tag.
func TestResolveRefPrefersHeadsThenTags(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, entry("p1",
		set("refs/heads/v1", code.ZeroOID, oidOf("branch v1")),
		set("refs/tags/v1", code.ZeroOID, oidOf("tag v1")),
		set("refs/tags/rel", code.ZeroOID, oidOf("rel")),
		set("refs/heads/old", code.ZeroOID, oidOf("branch old")),
		set("refs/tags/old", code.ZeroOID, oidOf("tag old"))))
	n.push(id, entry("p2", set("refs/heads/old", oidOf("branch old"), code.ZeroOID)))
	resolve := func(ref string) (code.RefAnswer, error) {
		return n.reader.ResolveRef(t.Context(), code.RefQuery{Repo: "app", Ref: ref, Fresh: stale, Asker: asker})
	}
	for ref, want := range map[string][2]string{
		"v1":            {"refs/heads/v1", oidOf("branch v1")},
		"refs/tags/v1":  {"refs/tags/v1", oidOf("tag v1")},
		"rel":           {"refs/tags/rel", oidOf("rel")},
		"old":           {"refs/tags/old", oidOf("tag old")},
		"refs/heads/v1": {"refs/heads/v1", oidOf("branch v1")},
	} {
		got, err := resolve(ref)
		if err != nil || got.Ref != want[0] || got.OID != want[1] || got.Repo.ID != id ||
			got.Position != n.waiter.Committed() {
			t.Errorf("ResolveRef(%s) = %+v, %v; want %s at %s, read at the checkpoint %v", ref, got, err,
				want[0], want[1], n.waiter.Committed())
		}
	}
	for ref, names := range map[string]string{
		"refs/heads/old": "refs/heads/old",
		"nope":           "refs/heads/nope or refs/tags/nope",
		"HEAD":           "refs/tags/HEAD",
	} {
		_, err := resolve(ref)
		if !errors.Is(err, code.ErrRefNotFound) || !strings.Contains(err.Error(), names) {
			t.Errorf("ResolveRef(%s) = %v, want ErrRefNotFound naming %s", ref, err, names)
		}
	}
	for name, q := range map[string]code.RefQuery{
		"a ref no repository holds": {Repo: "app", Ref: "a..b", Fresh: stale, Asker: asker},
		"a whole ref git refuses":   {Repo: "app", Ref: "refs/heads/a..b", Fresh: stale, Asker: asker},
		"no asker":                  {Repo: "app", Ref: "v1", Fresh: stale},
		"no repository":             {Ref: "v1", Fresh: stale, Asker: asker},
	} {
		if _, err := n.reader.ResolveRef(t.Context(), q); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s: ResolveRef = %v, want ErrInvalid", name, err)
		}
	}
	for name, fresh := range map[string]statelog.Freshness{
		"no level":                {},
		"a session with no floor": {Level: statelog.ReadSession},
	} {
		_, err := n.reader.ResolveRef(t.Context(), code.RefQuery{Repo: "app", Ref: "v1", Fresh: fresh, Asker: asker})
		if !errors.Is(err, code.ErrBadLevel) {
			t.Errorf("%s: ResolveRef = %v, want ErrBadLevel", name, err)
		}
	}
	if _, err := n.store.DeleteRepo(t.Context(), n.activate(t2), id); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	if _, err := resolve("v1"); !errors.Is(err, code.ErrRepoNotFound) || !strings.Contains(err.Error(), "is deleted") {
		t.Fatalf("ResolveRef on a deleted repository = %v, want ErrRepoNotFound saying it is deleted", err)
	}
}
