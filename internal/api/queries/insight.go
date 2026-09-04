package queries

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// The three answers that let a dashboard show a company rather than a process:
// one unit of work end to end, who has been asking whom, and what the company
// already knows.
//
// All three are READS of surfaces the engine already maintains. None of them
// introduces a table, a cache or a write path — which matters, because each is
// adjacent to a stance the architecture takes deliberately: the engine keeps no
// task state, the A2A channel is an authorization record rather than a
// transport, and the knowledge base is searched live with no local copy. A
// surface that mirrored any of them would be re-introducing the staleness those
// stances exist to avoid.

// turn answers everything one unit of agent work published.
//
// Not a substitute for the trace, and not substitutable BY it: one trace can
// span several turns (a webhook that wakes two seats), and a turn resumed on
// another node after a restart can span several traces. What makes this its own
// question is that a long self-iterating turn pushes its own earlier phases out
// of every other window — the seat's history is bounded, the feed is bounded,
// and the phases in between are exactly what a reader is looking for.
//
// The payload rides along, unlike a listing: a turn is a handful of events and
// the caller renders all of them, so making it re-fetch each one would be an
// N+1 over a set the query already had in hand.
func (s Sources) turn(ctx context.Context, p Params) (any, error) {
	id := p.String("turn_id")
	if id == "" {
		return nil, fmt.Errorf("%w: turn needs a turn_id", ErrBadParams)
	}
	records, err := s.Events.Turn(ctx, id)
	if err != nil {
		return nil, err
	}
	if records == nil {
		// A named empty slice, not nil: nil marshals as `null` and the
		// client reads `.events` off it, which is the exact shape mismatch
		// that made the Trace screen answer "not found" for every trace.
		records = []store.EventRecord{}
	}
	return map[string]any{"turn_id": id, "events": records}, nil
}

// phases answers what the models have been doing, company-wide.
//
// The question the previous dashboard could not ask at all: its only phase
// history was per-seat, capped at fifty rows, with no pager and no filter — so
// "what is my money being spent on" and "what did the models actually do" had
// no screen, while the events behind them sat in the store addressable by id.
//
// PAYLOAD INCLUDED, unlike the event listing. That is the whole reason this is
// its own answer rather than `events?type=agent_phase_completed`: a phase
// record without its payload has no prompts, no response, no tool calls and no
// decision, which is everything a reader came for.
func (s Sources) phases(ctx context.Context, p Params) (any, error) {
	var before *store.Cursor
	if id := p.String("before_id"); id != "" {
		at, err := time.Parse(time.RFC3339Nano, p.String("before_time"))
		if err != nil {
			return nil, fmt.Errorf("%w: before_id needs a before_time: %w", ErrBadParams, err)
		}
		before = &store.Cursor{Time: at, ID: id}
	}
	limit := Clamp(p.Int("limit", 0), DefaultPhasePage, store.MaxPhasePage)
	records, err := s.Events.Phases(ctx, p.String("role"), limit, before)
	if err != nil {
		return nil, err
	}
	if records == nil {
		records = []store.EventRecord{}
	}
	// The cursor is the LAST row's key, echoed rather than left for a client
	// to assemble: (time, id) is the table's key, and a client rebuilding it
	// from a rendered timestamp would lose the sub-second precision the
	// tiebreak depends on. It is offered only on a FULL page — a short one is
	// the end of the record, and a cursor there would page forever.
	next := map[string]string{}
	if len(records) == limit {
		last := records[len(records)-1]
		next = map[string]string{
			"before_time": last.Time.UTC().Format(time.RFC3339Nano),
			"before_id":   last.ID,
		}
	}
	return map[string]any{
		"phases":    records,
		"next":      next,
		"exhausted": len(next) == 0,
	}, nil
}

// DefaultPhasePage is one screenful of phase records.
const DefaultPhasePage = 30

// a2aChannels answers who has been asking whom.
//
// The channel is an AUTHORIZATION RECORD, not a transport — nothing queues on
// it, and both the brief and the reply travel over the durable seat inbox — so
// what is answered is the record: the pair, the count, and the window. The
// message CONTENT is not here and is not missing: it is published as ordinary
// events, which the event log already serves and already indexes by channel.
//
// `available` is the load-bearing field. A node that cannot reach the
// coordination store must not answer an empty list, because "no channels have
// been opened" and "this node could not look" are different facts and only one
// of them is a measurement.
func (s Sources) a2aChannels(ctx context.Context, _ Params) (any, error) {
	channels, err := s.Channels.OpenChannels(ctx)
	if err != nil {
		// BEST EFFORT, and it says so. This is a read for a screen, and a
		// coordination blip must not turn it into a failure the reader has
		// to interpret — but it must not look like an empty company either.
		// The error is not swallowed: it becomes the answer's own `note`,
		// which is what a reader needs to tell "nothing opened" from
		// "nobody could look".
		//nolint:nilerr // Deliberate: see the paragraph above.
		return map[string]any{
			"channels": []any{}, "available": false,
			"note": err.Error(),
		}, nil
	}
	out := make([]map[string]any, 0, len(channels))
	for _, c := range channels {
		out = append(out, map[string]any{
			"id":        c.ID,
			"requester": c.Requester,
			"target":    c.Target,
			"messages":  c.Messages,
			"opened_at": isoOrEmpty(c.OpenedAt),
			"last_at":   isoOrEmpty(c.LastAt),
			"closed_at": isoOrEmpty(c.ClosedAt),
		})
	}
	// Most recently active first: an open channel that has not moved in a
	// week is the anomaly, and it should not be buried under an id sort.
	slices.SortStableFunc(out, func(a, b map[string]any) int {
		return cmp.Compare(b["last_at"].(string), a["last_at"].(string))
	})
	return map[string]any{"channels": out, "available": true}, nil
}

// knowledgeSearch runs the company's own knowledge search.
//
// Deliberately identical to what a seat's Plan phase does: same seam, same
// backend, same live read with no local copy. That is the point of the screen —
// an operator asking "what would an agent find" gets the answer an agent would
// get, not an answer from an index somebody would have to keep fresh.
//
// WHOSE IDENTITY IT SEARCHES AS is the one real decision here, and it is
// answered conservatively: as the ORG, with no seat. A search with no seat is a
// search with no per-seat credential, so the backend applies whatever the
// engine's own account can see and nothing more. Searching as a named seat
// would let a dashboard reader read, through that seat's account, material
// their own account may not have — which is the exact confusion the seam's
// "unscoped is not unbounded" rule exists to prevent.
func (s Sources) knowledgeSearch(ctx context.Context, p Params) (any, error) {
	text := p.String("q")
	if text == "" {
		text = p.String("text")
	}
	organization := s.organization()
	out := map[string]any{
		"backend":   "",
		"query":     text,
		"hits":      []any{},
		"available": true,
		"reason":    string(KnowledgeRan),
		"note":      "",
	}
	unavailable := func(reason KnowledgeReason, note string) (any, error) {
		out["available"] = false
		out["reason"] = string(reason)
		out["note"] = note
		return out, nil
	}
	if organization == nil {
		return unavailable(KnowledgeNoCompany, "no company configuration is active")
	}
	// A nil searcher is the ANSWER, not a reason to be unregistered: exactly
	// one backend serves a company, chosen by which integration is
	// configured, so "none is" is a fact the company establishes on its own.
	searcher := s.searcher()
	if searcher == nil {
		return unavailable(KnowledgeNoBackend, "no knowledge backend is configured for this company")
	}
	out["backend"] = searcher.Backend()
	// The seam's own pre-gate, and it is free: it answers "could this search
	// possibly hit anything" with no I/O, which is exactly what a screen with
	// no results needs in order to say WHY.
	//
	// A DIFFERENT state from the one above, and it must not borrow its
	// wording: the backend IS wired, it just has no org-wide scope to read.
	// Searching as the org means no per-seat credential, so the gate reduces
	// to whether a read scope was declared — and an operator told "no backend
	// is configured" here would go and check the integration they already
	// configured correctly, instead of the empty field that is the cause.
	if !searcher.CanSearch(nil, organization) {
		return unavailable(KnowledgeNoScope, "the "+searcher.Backend()+" backend is configured but knowledge.confluence_spaces lists no space, so an org-wide search has nothing to read")
	}
	if text == "" {
		return out, nil
	}
	hits := searcher.Search(ctx, knowledge.Query{
		Text:  text,
		Org:   organization,
		Limit: KnowledgeHitLimit,
	})
	rows := make([]map[string]any, 0, len(hits))
	for _, hit := range hits {
		rows = append(rows, map[string]any{
			"id":        hit.PageID,
			"title":     hit.Title,
			"url":       hit.URL,
			"container": hit.Container,
			"snippet":   hit.Snippet,
			// The seam carries no modified time, and a made-up one would be
			// worse than none: a reader deciding whether a page is current
			// would be deciding on a fiction.
			"updated_at": "",
		})
	}
	out["hits"] = rows
	return out, nil
}

// KnowledgeReason says WHY a knowledge search did not run, as a stable value
// a screen can branch on.
//
// It exists because `note` cannot serve both readers. The note is prose for a
// person and is free to be reworded; a UI deciding which remedy to offer must
// not be string-matching it, and inferring the state from the other fields is
// the same mistake wearing a different hat — an empty `backend` means "no
// backend" and "no company at all" alike, and offering to configure Confluence
// to somebody who has configured no company is advice for the wrong problem.
type KnowledgeReason string

const (
	// KnowledgeRan is the zero state: the search ran. Anything in `note`
	// alongside it describes a search that STARTED and degraded, which is a
	// different fact from one that never started.
	KnowledgeRan KnowledgeReason = ""
	// KnowledgeNoCompany — no company configuration is active on this node.
	KnowledgeNoCompany KnowledgeReason = "no_company"
	// KnowledgeNoBackend — a company is active and wired no knowledge backend.
	KnowledgeNoBackend KnowledgeReason = "no_backend"
	// KnowledgeNoScope — a backend is wired, with no org-wide read scope.
	KnowledgeNoScope KnowledgeReason = "no_scope"
)

// Valid reports whether r is a reason this package emits, so an unknown value
// off the wire is a value rather than a panic.
func (r KnowledgeReason) Valid() bool {
	switch r {
	case KnowledgeRan, KnowledgeNoCompany, KnowledgeNoBackend, KnowledgeNoScope:
		return true
	}
	return false
}

// KnowledgeHitLimit bounds one search. It matches what a Plan-phase prefetch
// asks for, so the screen and the agent see the same top slice.
const KnowledgeHitLimit = 10

// organization resolves the running company's org, or nil.
func (s Sources) organization() *org.Organization {
	if s.Company == nil {
		return nil
	}
	company := s.Company()
	if company == nil {
		return nil
	}
	organization, err := company.Organization()
	if err != nil {
		return nil
	}
	return organization
}

// --- memory projections -------------------------------------------------- //
//
// The learning types are DOMAIN types and carry no json tags, deliberately:
// tagging them would put a wire contract on a struct whose fields exist for the
// recall path, and would ship every row's embedding vector — up to a hundred
// float32 arrays per request — to a screen that has no use for one.
//
// So the wire shape is built here, at the boundary, which is where a wire shape
// belongs. It was NOT built here before, and the result was a memory tab whose
// every block read "None yet" and whose every episode row showed `NaN s`: Go
// marshalled the field names, the client read the documented ones, and nothing
// failed.

func diaryRow(e learning.DiaryEntry) map[string]any {
	return map[string]any{
		"id":         e.ID,
		"content":    e.Content,
		"retention":  string(e.Kind),
		"source":     e.Source,
		"turn_id":    e.TurnID,
		"created_at": isoOrEmpty(e.CreatedAt),
		"ttl_until":  isoOrEmpty(e.TTLUntil),
		// How often a memory has actually been recalled — the difference
		// between one that keeps proving useful and one written once and
		// never read.
		"retrievals": e.RetrievalCount,
	}
}

func episodeRow(e learning.Episode) map[string]any {
	return map[string]any{
		"id":               e.ID,
		"turn_id":          e.TurnID,
		"agent_handle":     e.Handle,
		"task_summary":     e.TaskSummary,
		"plan_summary":     e.PlanSummary,
		"review_outcome":   e.ReviewOutcome,
		"tool_sequence":    e.ToolSequence,
		"skills_used":      e.SkillsUsed,
		"conversation_key": e.ConversationKey,
		"work_key":         e.WorkKey,
		"created_at":       isoOrEmpty(e.StartedAt),
		"ended_at":         isoOrEmpty(e.EndedAt),
		// Milliseconds, because that is what the client formats. A
		// time.Duration marshals as an integer count of NANOSECONDS, which
		// renders as a plausible and wildly wrong number.
		"duration_ms": e.Duration.Milliseconds(),
		"compacted":   e.Kind != learning.KindRaw,
		"count":       e.Count,
	}
}

func skillRow(sk learning.Skill) map[string]any {
	return map[string]any{
		"id":         sk.ID,
		"key":        sk.Name,
		"title":      sk.Name,
		"summary":    sk.Description,
		"version":    sk.Version,
		"updated_at": isoOrEmpty(sk.UpdatedAt),
		"uses":       sk.UseCount,
	}
}

// searcher resolves the knowledge backend for this call.
//
// Per call rather than per process: see [Sources.Knowledge].
func (s Sources) searcher() knowledge.Searcher {
	if s.Knowledge == nil {
		return nil
	}
	return s.Knowledge()
}
