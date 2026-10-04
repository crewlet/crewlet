package queries

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// WHO READ A PAGE, WHO LOADED IT AS A SKILL, AND WHO LINKS TO IT.
//
// Three signals a wiki cannot give and this engine can, because its readers
// are seats whose every read is a recorded act (`knowledge_read`, E-A2) and
// whose every body is indexed on this node:
//
//   - `page_reads{page, days}` — the seats that READ the page, how, how often
//     and in which turn, over up to [PageReadsMaxDays] company days, from the
//     replicated `usage` domain (ADR-0020): every node's reads, a departed
//     node's included, the same on whichever node answers.
//   - the `page` answer's `skill_loaded_by` — the seats a TOOL-SKILL page was
//     loaded or offered to, from the same rows.
//   - the `page` answer's `linked_from` — the pages and tasks whose bodies
//     carry this page's address, from this node's index ([search.Backlinks]).

// PageReadsMaxDays is how far back a page's readers are read: thirty company
// days, the event log's own retention and the window a knowledge base is
// curated over ("nobody has opened this in a month"). The usage domain holds
// more; a reader list over a quarter would be a list of who used to care.
const PageReadsMaxDays = 30

// MaxPageReaders is how many (seat, via) rows one answer names, newest read
// first. A hundred: a rail shows a handful, and a page read by more seats than
// this is one whose list is read by its count — which `readers_total` keeps.
const MaxPageReaders = 100

// readVias are the ways a page reaches a seat that count as READING it.
//
// NOT `skill_injected`: a phase's tool-skill catalogue offers every granted
// skill's one-line summary on every phase of every turn, so counted as a read
// it would make each skill page "read by every agent today" and bury the reads
// a seat chose. It is what `skill_loaded_by` counts instead.
var readVias = []types.KnowledgeReadVia{
	types.ReadViaGetPage, types.ReadViaSearch, types.ReadViaPrefetch, types.ReadViaSkillLoaded,
}

// skillVias are the two ways a tool-skill page reaches a seat as a skill.
var skillVias = []types.KnowledgeReadVia{types.ReadViaSkillLoaded, types.ReadViaSkillInjected}

// PageReadsAnswer is the `page_reads` answer.
type PageReadsAnswer struct {
	Page string `json:"page"`

	// Since and Until are the window's first and last company day, both
	// inclusive, and Days its length.
	Since string `json:"since"`
	Until string `json:"until"`
	Days  int    `json:"days"`

	// Readers is one row per (seat, via), newest read first — at most
	// [MaxPageReaders] of ReadersTotal.
	Readers      []PageReadRow `json:"readers"`
	ReadersTotal int           `json:"readers_total"`

	// DistinctSeatsToday is how many seats read the page on the company's
	// today — "read by n agents today", cut at the company's midnight rather
	// than the browser's.
	DistinctSeatsToday int `json:"distinct_seats_today"`

	// Elided is how many (page, via) entries the per-seat-day cap dropped
	// across the window, on every node — entries of which some may have
	// been this page's. Zero says the list is complete.
	Elided int64 `json:"elided"`
}

// PageReadRow is one seat's reads of the page through one via.
type PageReadRow struct {
	Handle string `json:"handle"`
	Role   string `json:"role,omitempty"`

	// Via is how the page reached the seat: `get_page`, `search`,
	// `prefetch` or `skill_loaded` — the `knowledge_read` vocabulary.
	Via string `json:"via"`

	Count  int64     `json:"count"`
	LastAt time.Time `json:"last_at"`

	// LastTurnID, LastWorkKey and LastQuery are the newest read's: the run
	// it happened in, the unit of work that run was dispatched for, and the
	// query a search or prefetch ran.
	LastTurnID  string `json:"last_turn_id,omitempty"`
	LastWorkKey string `json:"last_work_key,omitempty"`
	LastQuery   string `json:"last_query,omitempty"`

	// LastWorkItem is the task the newest read's run was charged to and
	// its "Turn n" there — "turn 2 on ENG-412". Absent for a run charged to
	// no task, or on a company whose tracker is not the engine's. A link to
	// it is built by [tracker.ItemAddress] — the key, or the id where
	// `key_collision` says another task claimed the key first and the key
	// would open that one.
	LastWorkItem *tracker.TurnPlace `json:"last_work_item,omitempty"`
}

// SkillLoad is one seat a tool-skill page reached as a skill.
type SkillLoad struct {
	Handle string    `json:"handle"`
	LastAt time.Time `json:"last_at"`

	// Count is Loaded plus Offered. Loaded is how often the seat asked for
	// the skill's body (load_tool_skill); Offered how often a phase's
	// catalogue put its summary in front of the seat.
	Count   int64 `json:"count"`
	Loaded  int64 `json:"loaded"`
	Offered int64 `json:"offered"`
}

// PageBacklinks is the "linked from" reader: this node's lexical index.
type PageBacklinks interface {
	LinkedFrom(ctx context.Context, pageID string) (search.Backlinks, error)
}

// PageAnswer is the `page` answer: the page as its reader serves it, and what
// the engine knows about it beside the page itself.
type PageAnswer struct {
	pages.Detail

	// SkillLoadedBy is the seats this TOOL-SKILL page reached as a skill
	// over the last [PageReadsMaxDays] company days, most recent first —
	// present only for a tool-skill page on a node that reads the usage
	// domain, and an empty list when none did.
	//
	// OMITZERO, NEVER OMITEMPTY: omitempty drops an empty non-nil slice as
	// well as a nil one, which would send "nobody loaded it" as the same
	// absent key as "this node cannot say" — the one distinction the field
	// exists to carry. omitzero drops only the nil.
	SkillLoadedBy []SkillLoad `json:"skill_loaded_by,omitzero"`

	// LinkedFrom is the pages and tasks whose bodies link here; absent on a
	// node with no index to read them from, and whenever LinkedFromStatus
	// says why this node could not answer.
	LinkedFrom *search.Backlinks `json:"linked_from,omitempty"`

	// LinkedFromStatus is why a node that HOLDS an index sent no
	// LinkedFrom — absent whenever LinkedFrom is present, and absent on a
	// node with no index at all, which has nothing to say about links.
	LinkedFromStatus LinkedFromStatus `json:"linked_from_status,omitempty"`
}

// LinkedFromStatus is why a page answer carries no `linked_from`.
//
// THREE ANSWERS, NEVER TWO: "these link here" (the list, possibly empty),
// "this node's index has not read every body yet" and "this node could not
// read its index" send a curator in three directions — trust the list, look
// again in a few minutes, or ask another node — and an empty list for either
// of the last two is the confident "nothing links here" a stale index cannot
// make.
type LinkedFromStatus string

const (
	// LinkedFromBuilding is sent while the index is on its first lap over pages and
	// tasks ([search.ErrIndexBuilding]).
	LinkedFromBuilding LinkedFromStatus = "building"

	// LinkedFromUnavailable is sent when the read failed — the node's own index, or
	// the replicated estate the names are read from (closed for an
	// adoption, say). The PAGE is still answered: the backlinks are a
	// signal beside the document, and a document made unreadable by its
	// rail would be the wrong way round.
	LinkedFromUnavailable LinkedFromStatus = "unavailable"
)

// LinkedFromStatuses is every [LinkedFromStatus] this package sends — held
// against the dashboard's `LinkedFromStatus` union, which draws one note per
// member.
var LinkedFromStatuses = []LinkedFromStatus{LinkedFromBuilding, LinkedFromUnavailable}

// Valid reports whether st is a status this package sends, so an unknown
// value off the wire is a value rather than a panic.
func (st LinkedFromStatus) Valid() bool { return slices.Contains(LinkedFromStatuses, st) }

func (s Sources) page(ctx context.Context, p Params) (any, error) {
	ref := strings.TrimSpace(p.String("id"))
	if ref == "" {
		return nil, badParams("id", "", nil)
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	detail, err := s.Pages.Get(ctx, ref, fresh)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	out := PageAnswer{Detail: detail}
	if detail.Skill && s.Usage != nil {
		window := tokens.LastDays(PageReadsMaxDays, s.clock(), s.zone())
		rows, err := usage.PageReads(ctx, s.Usage, usage.PageReadsQuery{
			PageIDs: []string{detail.Page.ID}, From: window.First.Label, To: window.Last.Label,
			Vias: names(skillVias)})
		if err != nil {
			return nil, usageErr(err)
		}
		out.SkillLoadedBy = foldSkillLoads(rows, s.chartHandles())
	}
	if s.Backlinks != nil {
		links, err := s.Backlinks.LinkedFrom(ctx, detail.Page.ID)
		switch {
		case errors.Is(err, search.ErrIndexBuilding):
			out.LinkedFromStatus = LinkedFromBuilding
		case err != nil:
			log.WarnContext(ctx, "page_backlinks_unavailable", "page", detail.Page.ID, "error", err)
			out.LinkedFromStatus = LinkedFromUnavailable
		default:
			out.LinkedFrom = &links
		}
	}
	return out, nil
}

// pageReads answers `page_reads{page, days}`.
func (s Sources) pageReads(ctx context.Context, p Params) (any, error) {
	page := strings.TrimSpace(p.String("page"))
	if page == "" {
		return nil, badParams("page", "", nil)
	}
	days := PageReadsMaxDays
	if p.Has("days") {
		days = p.Int("days", 0)
		if days < 1 || days > PageReadsMaxDays {
			return nil, fmt.Errorf("%w: days=%v, and a page's readers cover 1 to %d "+
				"company days — ask for at most %d", ErrBadParams, p.Values()["days"],
				PageReadsMaxDays, PageReadsMaxDays)
		}
	}
	now, loc := s.clock(), s.zone()
	window := tokens.LastDays(days, now, loc)
	rows, err := usage.PageReads(ctx, s.Usage, usage.PageReadsQuery{
		PageIDs: []string{page}, From: window.First.Label, To: window.Last.Label, Vias: names(readVias)})
	if err != nil {
		return nil, usageErr(err)
	}
	elided, err := usage.ReadsElided(ctx, s.Usage, window.First.Label, window.Last.Label)
	if err != nil {
		return nil, usageErr(err)
	}
	answer := foldPageReads(rows, pageReadsFold{
		today: window.Last.Label, handles: s.chartHandles()})
	answer.Page, answer.Elided = page, elided
	answer.Since, answer.Until, answer.Days = window.First.Label, window.Last.Label, window.Days()

	// "TURN n ON KEY", from the tracker's own rows: a read names the run it
	// happened in, and the run's task and its place there are the tracker's
	// to say. Best effort in one direction only — a tracker that cannot be
	// read leaves the lines naming no task rather than failing the list of
	// who read the page.
	if s.Work != nil {
		var runs []string
		for _, r := range answer.Readers {
			if r.LastTurnID != "" {
				runs = append(runs, r.LastTurnID)
			}
		}
		places, err := s.Work.TurnPlaces(ctx, runs, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			log.WarnContext(ctx, "page_reads_turns_unread", "page", page, "error", err.Error())
		}
		for i, r := range answer.Readers {
			if place, ok := places[r.LastTurnID]; ok {
				answer.Readers[i].LastWorkItem = &place
			}
		}
	}
	return answer, nil
}

// chartHandles maps a seat's agent id to what the running chart calls it now.
//
// THE CHART NAMES A SEAT IT HOLDS, and a day's own name is the fallback for a
// seat it no longer does: a day records what the seat was called when it read,
// which a retitled role name leaves out of date — the rule [tokens.Seats]
// states for every spend row, held here for every read row.
func (s Sources) chartHandles() map[string]chartSeat {
	out := map[string]chartSeat{}
	for _, seat := range chartSeats(s.organization()) {
		out[seat.agentID] = seat
	}
	return out
}

// pageReadsFold is what [foldPageReads] needs beside the rows.
type pageReadsFold struct {
	// today is the company's today, as a day label.
	today   string
	handles map[string]chartSeat
}

// foldPageReads sums every node's days into one row per (seat, via), keeps
// the newest read's details, and counts today's distinct seats.
//
// PURE over its inputs, so the arithmetic — the sums across nodes and days,
// which read is the newest, what "today" is — is tested without a database.
func foldPageReads(rows []usage.PageRead, f pageReadsFold) PageReadsAnswer {
	type key struct{ agent, via string }
	folded := map[key]*PageReadRow{}
	var order []key
	today := map[string]bool{}
	for _, row := range rows {
		if row.Day == f.today && row.Count > 0 {
			today[row.AgentID] = true
		}
		k := key{row.AgentID, row.Via}
		r, ok := folded[k]
		if !ok {
			r = &PageReadRow{Via: row.Via}
			folded[k] = r
			order = append(order, k)
		}
		r.Count += row.Count
		if namesSeat(row, r.Handle, r.LastAt) {
			r.Handle, r.Role = row.Handle, row.Role
		}
		if !row.LastAt.Before(r.LastAt) {
			r.LastAt = row.LastAt
			r.LastTurnID, r.LastWorkKey, r.LastQuery = row.LastTurnID, row.LastWorkKey, row.LastQuery
		}
	}
	out := PageReadsAnswer{Readers: make([]PageReadRow, 0, len(order)), DistinctSeatsToday: len(today)}
	for _, k := range order {
		r := folded[k]
		switch seat, inChart := f.handles[k.agent]; {
		case inChart:
			// THE CHART'S NAME FOR A SEAT IT HOLDS — see [Sources.chartHandles].
			r.Handle, r.Role = seat.handle, seat.role
		case r.Handle == "":
			// A SEAT THE CHART NO LONGER HOLDS, WHOSE DAYS NAMED NOBODY:
			// the id itself — never a blank row nobody can attribute.
			r.Handle = k.agent
		}
		out.Readers = append(out.Readers, *r)
	}
	slices.SortFunc(out.Readers, func(a, b PageReadRow) int {
		if c := b.LastAt.Compare(a.LastAt); c != 0 {
			return c
		}
		return cmp.Or(strings.Compare(a.Handle, b.Handle), strings.Compare(a.Via, b.Via))
	})
	out.ReadersTotal = len(out.Readers)
	out.Readers = out.Readers[:min(len(out.Readers), MaxPageReaders)]
	return out
}

// skillLoads is every listed tool-skill page's [SkillLoad]s over the last
// [PageReadsMaxDays] company days, keyed by page id — every page listed,
// an unloaded one as an empty list, so "nobody loaded it" is an answer rather
// than a missing key.
func (s Sources) skillLoads(ctx context.Context, listed []pages.Summary) (map[string][]SkillLoad, error) {
	window := tokens.LastDays(PageReadsMaxDays, s.clock(), s.zone())
	ids := make([]string, len(listed))
	for i, p := range listed {
		ids[i] = p.ID
	}
	rows, err := usage.PageReads(ctx, s.Usage, usage.PageReadsQuery{
		PageIDs: ids, From: window.First.Label, To: window.Last.Label, Vias: names(skillVias)})
	if err != nil {
		return nil, usageErr(err)
	}
	byPage := map[string][]usage.PageRead{}
	for _, row := range rows {
		byPage[row.PageID] = append(byPage[row.PageID], row)
	}
	handles := s.chartHandles()
	out := make(map[string][]SkillLoad, len(ids))
	for _, id := range ids {
		out[id] = foldSkillLoads(byPage[id], handles)
	}
	return out, nil
}

// namesSeat reports whether row's handle replaces the one a fold already
// holds for its seat (held, from rows whose newest read was heldAt): THE NAME
// THE NEWEST DAY GAVE THE SEAT, which is the one a reader knows it by now, and
// on a tie the row folded later. ONE RULE for both folds, because a seat whose
// rows name it differently would otherwise carry one name in a page's
// `skill_loaded_by` and another in its `page_reads` whenever two rows share a
// last read.
func namesSeat(row usage.PageRead, held string, heldAt time.Time) bool {
	return row.Handle != "" && (held == "" || !row.LastAt.Before(heldAt))
}

// foldSkillLoads sums a tool-skill page's loads and offers into one row per
// seat, most recent first.
func foldSkillLoads(rows []usage.PageRead, handles map[string]chartSeat) []SkillLoad {
	folded := map[string]*SkillLoad{}
	for _, row := range rows {
		s, ok := folded[row.AgentID]
		if !ok {
			s = &SkillLoad{}
			folded[row.AgentID] = s
		}
		if namesSeat(row, s.Handle, s.LastAt) {
			s.Handle = row.Handle
		}
		switch types.KnowledgeReadVia(row.Via) {
		case types.ReadViaSkillLoaded:
			s.Loaded += row.Count
		case types.ReadViaSkillInjected:
			s.Offered += row.Count
		}
		s.Count += row.Count
		if row.LastAt.After(s.LastAt) {
			s.LastAt = row.LastAt
		}
	}
	out := make([]SkillLoad, 0, len(folded))
	for agent, s := range folded {
		switch seat, inChart := handles[agent]; {
		case inChart:
			s.Handle = seat.handle
		case s.Handle == "":
			s.Handle = agent
		}
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b SkillLoad) int {
		if c := b.LastAt.Compare(a.LastAt); c != 0 {
			return c
		}
		return strings.Compare(a.Handle, b.Handle)
	})
	return out
}
