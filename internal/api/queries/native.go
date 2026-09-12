package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The native tracker and knowledge base, read for a screen.
//
// # These read THIS NODE'S OWN COPY, and that is the whole reason they are cheap
//
// Every other way to answer "what is on the board" is a listing over the
// fleet's own record — O(keys) message deliveries, on a request path, for a
// page a dashboard refreshes. This node's copy is what makes it a SQL query
// instead, and it is the same copy a seat's own tools read, so an operator and
// an agent looking at one task see one task.
//
// The two halves reach that copy differently, and the difference is visible in
// the answers rather than hidden here: the tracker's rows are derived by an
// applier from an ordered log, so every answer carries the level it was served
// at and what it could not account for; the wiki's are maintained by a
// projector following a bucket's change feed, which could only say hydrated or
// not.
//
// # A COPY THAT HAS NOT CAUGHT UP SAYS SO rather than answering empty
//
// Both native backends are state-log domains now, so both say it the same way:
// through the coverage every answer carries, which is a POSITION rather than a
// boolean. "This company has no work" is an answer a person acts on — they
// file the duplicate, they conclude the migration failed — and a node that has
// not caught up must never be able to say it without saying so.

// WorkReader is the tracker read side this surface calls. Declared here, by
// the consumer, so the package depends on the shape rather than on the store.
//
// THE QUERY GRAMMAR IS THE PARAMETERS. This surface hands the request's own
// parameters straight to the tracker's parser rather than translating them
// into a second filter type — one grammar serves the board, the socket, the
// REST route and a seat's own tool, and a second parser here would be the one
// place a filter meant something slightly different.
type WorkReader interface {
	Tasks(ctx context.Context, q tracker.Query, now time.Time) (tracker.Answer, error)
	Task(ctx context.Context, idOrKey string, want tracker.DetailWants,
		level statelog.ReadLevel) (tracker.TaskDetail, error)
	Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error)
	ExpandedQuery(ctx context.Context, params map[string]any, viewer string,
		now time.Time, loc *time.Location) (tracker.Query, error)
	Goals(ctx context.Context, q tracker.GoalQuery) (tracker.GoalListing, error)
	Catalogue(ctx context.Context, q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error)
	Projects(ctx context.Context, q tracker.ProjectQuery, now time.Time) (
		tracker.ProjectListing, error)
	Project(ctx context.Context, q tracker.ProjectDetailQuery, now time.Time) (
		tracker.ProjectDetail, error)
	Sprints(ctx context.Context, q tracker.SprintQuery, now time.Time) (
		tracker.SprintListing, error)
	Person(ctx context.Context, q tracker.PersonQuery, now time.Time) (tracker.PersonState, error)
}

// PageReader is the knowledge read side this surface calls.
type PageReader interface {
	List(ctx context.Context, f pages.Filter, level statelog.ReadLevel) (pages.Listing, error)
	Get(ctx context.Context, ref string, level statelog.ReadLevel) (pages.Detail, error)
	Containers(ctx context.Context, level statelog.ReadLevel) ([]pages.Container, error)
}

// ---- work -------------------------------------------------------------- //

func (s Sources) workItems(ctx context.Context, p Params) (any, error) {
	now := time.Now().UTC()
	// THROUGH THE EXPANSION, so a `view=` or a `preset=` is the set of
	// defaults it stands for rather than a key nothing reads. The viewer
	// is a parameter here for the reason it is on the view strip: this
	// surface is guarded, so the caller already holds the company's own
	// credential, and what it selects is whose queue to render.
	q, err := s.Work.ExpandedQuery(ctx, p.Values(),
		strings.TrimSpace(p.String("viewer")), now, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrBadParams, err)
	}
	// THIS SURFACE'S OWN DEFAULT, applied where an absent level resolves.
	// A dashboard tile polls, and a poll that took a quorum round trip
	// would put the fleet's read rate on the raft log to remove a
	// staleness the screen redraws through anyway — see the arithmetic in
	// the dashboard section of the design: ten tabs at seven polls a
	// minute is a 9x increase in barrier appends.
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	answer, err := s.Work.Tasks(ctx, q, now)
	switch {
	case errors.Is(err, tracker.ErrTooBroad):
		// A REFUSAL ABOUT THE REQUEST, so it is [ErrBadParams] and not
		// [ErrUnavailable]: the caller narrows and asks again, where
		// "unavailable" tells a polling screen to send the identical
		// query back every few seconds for ever.
		return nil, fmt.Errorf("%w: %s", ErrBadParams, err)
	case err != nil:
		return nil, unavailableIfBehind(err)
	}
	out := map[string]any{
		"items": answer.Rows,
		// A SEPARATE COUNT, not len(items): the listing is a page, and a
		// board header reporting the page size as the project's size
		// would say "50 items" for every project with more than fifty.
		// It is a HINT and says so — an exact total over an unbounded set
		// is the one query in this grammar that turns a poll into a scan.
		"total_hint": answer.TotalHint,
		"read_level": answer.Level,
		"log_seq":    answer.LogSeq,
		// APPLIED_THROUGH BESIDE LOG_SEQ, never instead of it: a node
		// applying nothing while its position advances looks identical
		// to a caught-up one from either number alone, and a screen
		// showing only the position renders a deferral as lag.
		"applied_through": answer.AppliedThrough,
		"complete":        answer.Complete,
	}
	if answer.LogLag != nil {
		// ABSENT RATHER THAN ZERO when the broker could not be reached.
		// A read asks how far behind an answer may be, and an
		// unreachable broker answering "not at all" is the confident
		// wrong answer this whole shape exists to avoid.
		out["log_lag"] = *answer.LogLag
	}
	if answer.NextCursor != "" {
		out["next_cursor"] = answer.NextCursor
	}
	// THE GROUPED HALF, and it is not optional decoration: a grouped
	// answer has NO flat rows by construction, so a payload that carried
	// only `items` renders a populated board as an empty one.
	if answer.Groups != nil {
		out["groups"] = answer.Groups
		out["groups_dropped"] = answer.GroupsDropped
		out["groups_overlap"] = answer.GroupsOverlap
	}
	if answer.Totals != nil {
		out["totals"] = answer.Totals
	}
	// WHAT THIS ANSWER WAS EXPANDED FROM, so a payload that arrives
	// detached from its request can still say which saved view it is.
	if answer.View != "" {
		out["view"] = answer.View
	}
	if answer.Preset != "" {
		out["preset"] = answer.Preset
	}
	if answer.Incomplete != nil {
		// WHAT THE ANSWER COULD NOT ACCOUNT FOR, rendered rather than
		// dropped: "this company has no work" is a thing a person acts
		// on, and a node holding records it cannot read must never be
		// able to say it without saying so.
		out["incomplete"] = answer.Incomplete
	}
	return out, nil
}

func (s Sources) workItem(ctx context.Context, p Params) (any, error) {
	ref := strings.TrimSpace(p.String("id"))
	if ref == "" {
		return nil, badParams("id", "", nil)
	}
	detail, err := s.Work.Task(ctx, ref, tracker.DetailWants{
		Comments: true, History: true, Links: true,
	}, statelog.ReadStale)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		return nil, ErrNotFound
	case err != nil:
		return nil, unavailableIfBehind(err)
	}
	return detail, nil
}

// workViews answers one container's view strip.
//
// # Why `viewer` is a parameter here and not the caller's own identity
//
// A personal view is private to its OWNER, and the reader enforces that in
// SQL. What `viewer=` selects is which person's strip to render — their
// personal views and their pins — and it is safe to let a caller name one
// because this whole surface is guarded: the caller already holds the
// company's own credential and can read every task, comment and page in it.
// The privacy a personal view has is from other COMPANY MEMBERS reading
// through their own seats, which is a boundary the seat tools enforce and this
// route is on the other side of.
//
// Absent is the SHARED strip: no pins and no personal views but the shared
// ones, which is what a screen draws before it knows who is looking.
func (s Sources) workViews(ctx context.Context, p Params) (any, error) {
	container, err := viewContainer(p)
	if err != nil {
		return nil, err
	}
	listing, err := s.Work.Views(ctx, tracker.ViewQuery{
		Container: container,
		Viewer:    strings.TrimSpace(p.String("viewer")),
		// STALE, like every other dashboard poll — see
		// [Sources.workItems] for the arithmetic.
		Level: statelog.ReadStale,
	})
	if err != nil {
		return nil, unavailableIfBehind(err)
	}
	out := map[string]any{
		"views": listing.Views, "read_level": listing.Level,
		"log_seq": listing.LogSeq, "applied_through": listing.AppliedThrough,
		"complete": listing.Complete,
	}
	if listing.LogLag != nil {
		out["log_lag"] = *listing.LogLag
	}
	if listing.Incomplete != nil {
		out["incomplete"] = listing.Incomplete
	}
	return out, nil
}

// viewContainer reads the container a strip belongs to.
//
// ONE PARAMETER IN THE QUERY GRAMMAR'S OWN SPELLING — `container=project:ENG`,
// `container=workspace` — because a screen that reaches a strip and then the
// tasks in it must not have to write the same container two ways.
func viewContainer(p Params) (tracker.Container, error) {
	raw := strings.TrimSpace(p.String("container"))
	if raw == "" {
		return tracker.Container{}, badParams("container", "",
			[]string{"workspace", "project:<KEY>", "unit:<name>", "person:<handle>"})
	}
	if raw == tracker.ContainerWorkspace {
		return tracker.Container{Kind: tracker.ContainerWorkspace}, nil
	}
	kind, id, found := strings.Cut(raw, ":")
	if !found || !tracker.ValidContainerKind(kind) || id == "" {
		return tracker.Container{}, badParams("container", raw,
			[]string{"workspace", "project:<KEY>", "unit:<name>", "person:<handle>"})
	}
	if kind == tracker.ContainerProject {
		// A PROJECT KEY IS UPPER-CASE wherever it is minted, and the
		// tracker's own scope parser upper-cases it for the same reason:
		// a strip asked for as `project:eng` must be the strip a task
		// query scoped to `project:ENG` belongs to.
		id = strings.ToUpper(id)
	}
	return tracker.Container{Kind: kind, ID: id}, nil
}

// workGoals answers the company's goals, each with what its targets say.
//
// THE PROGRESS IS COMPUTED, never stored — see [tracker.Reader.Goals]. A
// stored percentage is a second answer to a question that already has one, and
// the two drift the moment a task in a target closes without anybody editing
// the goal.
func (s Sources) workGoals(ctx context.Context, p Params) (any, error) {
	listing, err := s.Work.Goals(ctx, tracker.GoalQuery{
		ID:       strings.TrimSpace(p.String("id")),
		Owner:    strings.TrimSpace(p.String("owner")),
		Group:    strings.TrimSpace(p.String("group")),
		Archived: p.Bool("archived", false),
		// STALE, like every other dashboard poll — see
		// [Sources.workItems] for the arithmetic.
		Level: statelog.ReadStale,
	})
	if err != nil {
		return nil, unavailableIfBehind(err)
	}
	out := map[string]any{
		"goals": listing.Goals, "read_level": listing.Level,
		"log_seq": listing.LogSeq, "applied_through": listing.AppliedThrough,
		"complete": listing.Complete,
	}
	if listing.LogLag != nil {
		out["log_lag"] = *listing.LogLag
	}
	if listing.Incomplete != nil {
		out["incomplete"] = listing.Incomplete
	}
	return out, nil
}

// workCatalogue answers what a task may BE and what it may carry.
//
// ONE QUESTION rather than two, because every caller wants both: a screen
// draws types and fields together, and a model told which types exist and not
// which fields are required would file work that is refused on the next
// breath.
func (s Sources) workCatalogue(ctx context.Context, p Params) (any, error) {
	answer, err := s.Work.Catalogue(ctx, tracker.CatalogueQuery{
		Archived: p.Bool("archived", false),
		Level:    statelog.ReadStale,
	})
	if err != nil {
		return nil, unavailableIfBehind(err)
	}
	return answer, nil
}

// workPerson answers one human's own state — their inbox, their queue and
// their pins.
//
// THE HANDLE IS A PARAMETER for the reason [Sources.workViews]' viewer is: this
// whole surface is guarded, so the caller already holds the company's own
// credential. What the parameter selects is whose day to render, and the
// engine's own write side is where the authority lives — a read here can no
// more mark somebody's work read than a screen can.
func (s Sources) workPerson(ctx context.Context, p Params) (any, error) {
	handle := strings.TrimSpace(p.String("handle"))
	if handle == "" {
		return nil, badParams("handle", "", nil)
	}
	state, err := s.Work.Person(ctx, tracker.PersonQuery{
		Handle: handle,
		Level:  statelog.ReadStale,
	}, time.Now().UTC())
	if err != nil {
		return nil, unavailableIfBehind(err)
	}
	return state, nil
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

	// STALE, like every other dashboard poll: a screen that redraws every
	// twenty seconds and took a quorum round trip to do it would put the
	// fleet's whole read rate on the log. See [Sources.workItems].
	list, err := s.Pages.List(ctx, f, statelog.ReadStale)
	if err != nil {
		return nil, unavailableIfBehind(err)
	}
	return map[string]any{
		"pages": list.Pages, "limit": f.Limit, "offset": f.Offset,
		"read_level": list.Level, "complete": list.Complete,
		"position": list.Position, "log_lag": list.LogLag,
	}, nil
}

func (s Sources) page(ctx context.Context, p Params) (any, error) {
	ref := strings.TrimSpace(p.String("id"))
	if ref == "" {
		return nil, badParams("id", "", nil)
	}
	detail, err := s.Pages.Get(ctx, ref, statelog.ReadStale)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, unavailableIfBehind(err)
	}
	return detail, nil
}

func (s Sources) containers(ctx context.Context, _ Params) (any, error) {
	list, err := s.Pages.Containers(ctx, statelog.ReadStale)
	if err != nil {
		return nil, unavailableIfBehind(err)
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

// unavailableIfBehind turns a read this node could not serve YET into
// [ErrUnavailable], leaving every other failure alone.
//
// THE CLASSIFICATION IS THE STATE LOG'S OWN — [statelog.ReadRefusal.Retryable]
// — rather than a second list here. It is exactly the question the two differ
// on: a node that is behind will catch up, and a node holding a record it
// cannot decode will not, however long a caller waits.
//
// Without this every refusal reached the surface as a plain failure and was
// rendered as `query_failed` / 500 — telling a client to give up on a screen
// that would have worked in a few seconds, which is the one thing the 503 and
// its Retry-After exist to avoid. The reference documented the 503 the whole
// time; nothing produced it.
func unavailableIfBehind(err error) error {
	var refused *statelog.Refused
	if !errors.As(err, &refused) || !refused.Code.Retryable() {
		return err
	}
	// WRAPPED, NOT REPLACED, so the refusal's own code, detail and derived
	// hint survive for [RetryAfter] and for the log.
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// RetryAfter is how long a caller should wait before asking again, or zero
// when nothing here can say.
//
// DERIVED FROM THE REFUSAL rather than a constant, because the refusal derives
// it from the observed drain: a flat hint sends a caller back too early on a
// node grinding through a bulk apply and holds one waiting on a node that
// caught up in milliseconds.
func RetryAfter(err error) time.Duration {
	var refused *statelog.Refused
	if errors.As(err, &refused) {
		return refused.RetryAfter
	}
	return 0
}

// ---- projects and sprints ----------------------------------------------- //

// workProjects answers the company's projects with their maintained counts.
func (s Sources) workProjects(ctx context.Context, p Params) (any, error) {
	listing, err := s.Work.Projects(ctx, tracker.ProjectQuery{
		Q:        strings.TrimSpace(p.String("q")),
		Unit:     strings.TrimSpace(p.String("unit")),
		Archived: p.Bool("archived", false),
		Limit:    p.Int("limit", 0),
		Units:    s.chartUnits(),
		// STALE, like every other dashboard poll — see
		// [Sources.workItems] for the arithmetic.
		Level: statelog.ReadStale,
	}, time.Now().UTC())
	if err != nil {
		return nil, unavailableIfBehind(err)
	}
	return listing, nil
}

// workProject answers one project in full — the Overview tab, and the answer a
// seat learns a project's vocabulary from.
func (s Sources) workProject(ctx context.Context, p Params) (any, error) {
	key := strings.TrimSpace(p.String("key"))
	if key == "" {
		key = strings.TrimSpace(p.String("project"))
	}
	if key == "" {
		return nil, badParams("key", "", nil)
	}
	detail, err := s.Work.Project(ctx, tracker.ProjectDetailQuery{
		Project: key,
		ForType: strings.TrimSpace(p.String("for_type")),
		Units:   s.chartUnits(),
		Level:   statelog.ReadStale,
	}, time.Now().UTC())
	switch {
	case errors.Is(err, tracker.ErrNoProject):
		// NOT FOUND, NOT UNAVAILABLE, and the message survives the
		// classification: the refusal names the nearest keys, which is
		// what a caller who typed one wrong needs.
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	case err != nil:
		return nil, unavailableIfBehind(err)
	}
	return detail, nil
}

// workSprints answers a project's sprints with every figure computed.
func (s Sources) workSprints(ctx context.Context, p Params) (any, error) {
	project := strings.TrimSpace(p.String("project"))
	if project == "" {
		return nil, badParams("project", "", nil)
	}
	listing, err := s.Work.Sprints(ctx, tracker.SprintQuery{
		Project:  project,
		Number:   p.Int("sprint", 0),
		Sprints:  p.Int("sprints", 0),
		Archived: p.Bool("archived", false),
		Level:    statelog.ReadStale,
	}, time.Now().UTC())
	switch {
	case errors.Is(err, tracker.ErrNoProject):
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	case err != nil:
		return nil, unavailableIfBehind(err)
	}
	return listing, nil
}

// chartUnits is the running org as the tracker's unit seam.
//
// THE ADAPTER IS THE ENGINE'S, not a second copy: resolving a project's
// chart-owned unit means deciding what an unresolvable one looks like and
// which lead counts as this unit's, and two answers to that would render one
// project orphaned on a screen and led on a tool call.
//
// NIL WHEN THERE IS NO CHART, which the reader renders as every unit
// unresolved rather than as every project naming none — see [tracker.Units].
func (s Sources) chartUnits() tracker.Units {
	organization := s.organization()
	if organization == nil {
		return nil
	}
	return engine.ChartUnits(organization)
}
