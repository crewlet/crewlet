package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/work"
)

// The native tracker and knowledge base, read for a screen.
//
// # These read the PROJECTION, and that is the whole reason they are cheap
//
// Every other way to answer "what is on the board" is a listing over the
// fleet's coordination record — O(keys) message deliveries, on a request
// path, for a page a dashboard refreshes. The projection is exactly the copy
// that makes this a SQL query instead, and it is the same copy a seat's own
// tools read, so an operator and an agent looking at one item see one item.
//
// # A projection that has not caught up RAISES rather than answering empty
//
// [work.Reader] and [pages.Reader] refuse with [projection.ErrNotHydrated],
// and this surface passes that through as unavailable rather than flattening
// it to an empty list. "This company has no work" is an answer a person acts
// on — they file the duplicate, they conclude the migration failed — and a
// node that has not finished its boot reconcile must never be able to say it.

// WorkReader is the tracker read side this surface calls. Declared here, by
// the consumer, so the package depends on the shape rather than on the store.
type WorkReader interface {
	List(ctx context.Context, f work.Filter) ([]work.Summary, error)
	Get(ctx context.Context, idOrKey string) (work.Detail, error)
	Counters(ctx context.Context) (map[string]int, error)
}

// PageReader is the knowledge read side this surface calls.
type PageReader interface {
	List(ctx context.Context, f pages.Filter) ([]pages.Summary, error)
	Get(ctx context.Context, ref string) (pages.Detail, error)
	Containers(ctx context.Context) ([]pages.Container, error)
}

// notHydrated turns the projection's refusal into this surface's own.
//
// ONE PLACE, because every one of these answers has to do it and the failure
// of forgetting is silent: a screen that rendered an empty board during a
// boot reconcile looks exactly like a company with no work.
func notHydrated(err error) error {
	if errors.Is(err, projection.ErrNotHydrated) {
		return ErrUnavailable
	}
	return err
}

// ---- work -------------------------------------------------------------- //

func (s Sources) workItems(ctx context.Context, p Params) (any, error) {
	f := work.Filter{
		Project:  strings.ToUpper(strings.TrimSpace(p.String("project"))),
		Assignee: strings.TrimSpace(p.String("assignee")),
		Reporter: strings.TrimSpace(p.String("reporter")),
		Label:    strings.TrimSpace(p.String("label")),
		Parent:   strings.TrimSpace(p.String("parent")),
		Watcher:  strings.TrimSpace(p.String("watcher")),
		Text:     strings.TrimSpace(p.String("q")),
		Limit:    Clamp(p.Int("limit", 0), work.DefaultLimit, work.MaxLimit),
		Offset:   p.Int("offset", 0),
	}
	for _, name := range splitList(p.String("status")) {
		status := work.Status(name)
		if !status.Valid() {
			return nil, badParams("status", name, names(work.Statuses()))
		}
		f.Status = append(f.Status, status)
	}
	// PRESENCE, not truth: `open` absent asks for everything, and
	// `open=false` asks for the closed items. Reading an absent filter as
	// false would make the default board show only finished work.
	if p.Has("open") {
		open := p.Bool("open", true)
		f.Open = &open
	}

	items, err := s.Work.List(ctx, f)
	if err != nil {
		return nil, notHydrated(err)
	}
	// A SEPARATE COUNT, not len(items): the listing is a page, and a board
	// header that reported the page size as the project's size would say
	// "50 items" for every project with more than fifty.
	counters, err := s.Work.Counters(ctx)
	if err != nil {
		return nil, notHydrated(err)
	}
	return map[string]any{
		"items":  items,
		"limit":  f.Limit,
		"offset": f.Offset,
		// The last number minted per project — the board header's "ENG-42
		// was the last key", which is a different fact from how many are
		// open and is the one nothing else can supply.
		"minted": counters,
	}, nil
}

func (s Sources) workItem(ctx context.Context, p Params) (any, error) {
	ref := strings.TrimSpace(p.String("id"))
	if ref == "" {
		return nil, badParams("id", "", nil)
	}
	detail, err := s.Work.Get(ctx, ref)
	switch {
	case errors.Is(err, work.ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, notHydrated(err)
	}
	return detail, nil
}

// ---- pages ------------------------------------------------------------- //

func (s Sources) pageList(ctx context.Context, p Params) (any, error) {
	f := pages.Filter{
		Container: strings.ToUpper(strings.TrimSpace(p.String("container"))),
		ParentID:  strings.TrimSpace(p.String("parent")),
		Label:     strings.TrimSpace(p.String("label")),
		Watcher:   strings.TrimSpace(p.String("watcher")),
		Title:     strings.TrimSpace(p.String("title")),
		Limit:     Clamp(p.Int("limit", 0), pages.DefaultLimit, pages.MaxLimit),
		Offset:    p.Int("offset", 0),
	}
	for _, name := range splitList(p.String("status")) {
		status := pages.Status(name)
		if !status.Valid() {
			return nil, badParams("status", name, names(pages.Statuses()))
		}
		f.Status = append(f.Status, status)
	}
	if p.Has("skills") {
		// Three states, and all three are real: only the tool-skill
		// pages (what an operator auditing the catalogue wants), every
		// page but those (an ordinary browse), and everything.
		skills := p.Bool("skills", false)
		f.Skills = &skills
	}
	f.Onboarding = p.Bool("onboarding", false)

	list, err := s.Pages.List(ctx, f)
	if err != nil {
		return nil, notHydrated(err)
	}
	return map[string]any{"pages": list, "limit": f.Limit, "offset": f.Offset}, nil
}

func (s Sources) page(ctx context.Context, p Params) (any, error) {
	ref := strings.TrimSpace(p.String("id"))
	if ref == "" {
		return nil, badParams("id", "", nil)
	}
	detail, err := s.Pages.Get(ctx, ref)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, notHydrated(err)
	}
	return detail, nil
}

func (s Sources) containers(ctx context.Context, _ Params) (any, error) {
	list, err := s.Pages.Containers(ctx)
	if err != nil {
		return nil, notHydrated(err)
	}
	return map[string]any{"containers": list}, nil
}

// ---- shared ------------------------------------------------------------ //

// splitList reads a comma-separated filter.
//
// COMMAS, because a query string is one of this surface's two transports and
// a socket frame's JSON object cannot carry a repeated key — so a filter
// expressed as `?status=a&status=b` would be a request only one transport
// could make, which is exactly the divergence this package exists to prevent.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// names renders a typed-enum slice as the strings a refusal lists.
//
// GENERIC over the enum rather than one helper per package: every enum in
// this tree is a named string type with a Valid method, and two copies of a
// three-line conversion is two places for one of them to start rendering a
// closed set differently from the other.
func names[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

// badParams refuses a filter naming the value and what would have been valid.
func badParams(field, got string, allowed []string) error {
	if got == "" {
		return fmt.Errorf("%w: %s is required", ErrBadParams, field)
	}
	if len(allowed) == 0 {
		return fmt.Errorf("%w: %s=%q", ErrBadParams, field, got)
	}
	return fmt.Errorf("%w: %s=%q (want one of %s)",
		ErrBadParams, field, got, strings.Join(allowed, ", "))
}
