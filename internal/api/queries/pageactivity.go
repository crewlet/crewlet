// What happened to the company's pages, and what one revision said.

package queries

import (
	"context"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/pages"
)

// pageActivity answers a page of the wiki's change feed.
//
// THE ROWS WERE ALWAYS HERE. `pages_history` has one row per change since the
// domain landed and ships two indexes naming readers nobody wrote — "one
// page's activity" and "the company-wide digest" — so a wiki that recorded
// every edit, replicated it and snapshotted it offered no way to ask what had
// changed. `work_activity`'s own shape, one domain over.
func (s Sources) pageActivity(ctx context.Context, p Params) (any, error) {
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	q := pages.PageActivityQuery{
		Page:      strings.TrimSpace(p.String("page")),
		Container: strings.TrimSpace(p.String("container")),
		Limit:     p.Int("limit", 0),
		Freshness: fresh,
	}
	for _, name := range splitList(p.String("kinds")) {
		kind := pages.ChangeKind(name)
		if !kind.Valid() {
			return nil, badParams("kinds", name, names(pages.ChangeKinds()))
		}
		q.Kinds = append(q.Kinds, kind)
	}
	// WHO WAS WRITING, which is the audit screen's whole question: every
	// page change a token or a person made, across the company. REFUSED
	// rather than dropped when it names a kind this build has never heard
	// of, because a filter silently ignored answers a wider question than
	// the caller asked and looks exactly like one that worked.
	for _, name := range splitList(p.String("actor_kinds")) {
		kind := pages.AuthorKind(name)
		if !kind.Valid() {
			return nil, badParams("actor_kinds", name, names(pages.AuthorKinds()))
		}
		q.ActorKinds = append(q.ActorKinds, kind)
	}
	// THE CURSOR AND THE WINDOW ARE THE SAME UNIT — a composed log position
	// — and they are two parameters because they are two questions: the
	// cursor moves with every page and the bound does not.
	if q.Since, err = positionParam(p, "since"); err != nil {
		return nil, err
	}
	if q.Cursor, err = positionParam(p, "cursor"); err != nil {
		return nil, err
	}
	answer, err := s.Pages.Activity(ctx, q)
	if err != nil {
		return nil, err
	}
	return answer, nil
}

// positionParam reads a composed log position, or zero when absent.
//
// THE COMPOSED FORM, which is what this domain's own rows carry: the pages
// applier writes `version` as `(generation << 40) | seq`, so the ordering
// survives a reanchor and a bare sequence would sort below every position on
// a stream that has been reanchored.
func positionParam(p Params, name string) (uint64, error) {
	raw := strings.TrimSpace(p.String(name))
	if raw == "" {
		return 0, nil
	}
	at, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, badParams(name, raw, nil)
	}
	return at, nil
}

// pageRevision answers one saved version of one page, body included.
//
// THE BODY IS WHAT WAS MISSING. The page detail carries revision SUMMARIES —
// a version, an author, a message, an instant — which says a page was edited
// eleven times and not what any of those edits did, so "what changed in
// version 7" had no answer and neither did the diff a wiki is expected to
// show.
//
// A VERSION THIS NODE NO LONGER HOLDS IS NOT FOUND, not empty: a page keeps a
// bounded number of revisions, so an old one is an ordinary absence — and a
// reader has to be able to tell it from a page that never existed, which is a
// different thing to do about.
func (s Sources) pageRevision(ctx context.Context, p Params) (any, error) {
	page := strings.TrimSpace(p.String("page"))
	if page == "" {
		return nil, badParams("page", "", nil)
	}
	version := p.Int("version", 0)
	if version <= 0 {
		return nil, badParams("version", strconv.Itoa(version), nil)
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	revision, held, err := s.Pages.Revision(ctx, page, version, fresh)
	switch {
	case err != nil:
		return nil, err
	case !held:
		return nil, ErrNotFound
	}
	return revision, nil
}
