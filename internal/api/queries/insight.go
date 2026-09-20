package queries

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
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
	// THE READ STOPPED AT THE CAP, not at the end of the turn. EventLog.Turn
	// is ordered oldest first, because a turn is read forwards — so the rows
	// a long turn loses are its ENDING, which is where `agent_turn_completed`
	// and `turn_completed` are: the two records a reader takes the outcome,
	// the wall clock and the plan summary from. A turn cut at the cap was
	// therefore indistinguishable from one that died before finishing, and
	// the screen said so out loud, printing "no turn record" directly above
	// the rows it did get.
	//
	// So a cut view gets its ENDING BACK and reports the gap in the MIDDLE,
	// which is the part nothing can stand in for. Two cheap seeks on the same
	// (turn_id, event_time, event_id) index rather than one, and only on the
	// turns that need it.
	//
	// ASKED, NOT INFERRED. `len(records) == cap` is not "the read stopped
	// early": a turn of exactly the cap holds every row it has, and the
	// recovery below widens that misreading rather than narrowing it — a turn
	// a little past the cap ends up whole on the page, under a banner saying
	// part of it is missing. The count runs only on a read that filled, which
	// is the only case where a cut is possible at all.
	//
	// THE SECOND READ DEGRADES, it does not fail the first. The rows are
	// already in hand and they are what the reader came for; discarding a
	// successful 500-row read because a follow-up count could not be taken
	// turns the largest turns — the only ones that reach this branch at all,
	// and the ones most worth opening — into `query_failed`. So a failure
	// here leaves `truncated` false and logs: the page renders, and the worst
	// case is a missing caution badge rather than a missing screen.
	truncated := false
	if len(records) >= store.MaxTurnEvents {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		total, err := s.Events.TurnEventCount(ctx, id)
		switch {
		case err != nil:
			log.WarnContext(ctx, "turn_extent_unavailable", "turn", id, "error", err)
		case total > len(records):
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			closing, err := s.Events.TurnClosing(ctx, id, TurnClosingEvents)
			if err != nil {
				log.WarnContext(ctx, "turn_ending_unavailable", "turn", id, "error", err)
			} else {
				records = mergeByID(records, closing)
			}
			// AFTER the merge, because the merge is what decides it: the
			// recovered ending closes the gap outright on a turn only a
			// little past the cap, and leaves one on a turn that is
			// genuinely long.
			truncated = total > len(records)
		}
	}
	// EVERY TRACE THIS TURN TOUCHED, asked rather than derived.
	//
	// A turn RESUMED on another node after a restart spans more than one
	// trace, and that is exactly the turn somebody opens this page to
	// understand. The client derived the set from the rows it was handed,
	// which loses any trace whose events fell in the middle a capped read
	// dropped — so one button labelled "trace" led to half the story with
	// nothing saying a second half existed, on precisely the turns where it
	// did.
	//
	// DEGRADES LIKE THE RECOVERY ABOVE, for the same reason: the rows are
	// what the reader came for, and discarding a good read because a cheap
	// follow-up seek failed turns the largest turns into `query_failed`.
	traces, err := s.Events.TurnTraces(ctx, id)
	if err != nil {
		log.WarnContext(ctx, "turn_traces_unavailable", "turn", id, "error", err)
		traces = []string{}
	}
	// WHICH ATTEMPT THIS IS, and where the others are.
	//
	// A turn id names one RUN (ADR-0017), so a trigger that failed without
	// acting and was redelivered is several turns — and this screen is the
	// destination of every deep link in the product. Landing on one of them
	// with nothing saying the other exists is how a reader concludes the
	// company did the work twice, or that it failed when in fact the next
	// attempt succeeded.
	//
	// DEGRADES like the two reads above, and for the same reason: the rows
	// are what the caller came for.
	key, siblings := s.attemptsOf(ctx, id, records)
	return map[string]any{
		"turn_id": id,
		// The unit of work this run was an attempt at, and every run of it
		// this store holds, oldest first. One element — this turn — is the
		// ordinary case; empty means the turn carries no work key at all,
		// which is a trigger with nothing to collapse on.
		"work_key": key,
		"attempts": siblings,
		"events":   records,
		// SAYS WHAT IS MISSING, exactly as `trace` does. Additive, so a
		// client that predates the field is unaffected — and one that has it
		// can say the gap is the middle rather than warning that the page
		// cannot answer its own headline question.
		"truncated": truncated,
		"trace_ids": traces,
	}, nil
}

// attemptsOf reports the unit of work a run was an attempt at, and every run
// of it this store still holds, oldest first.
//
// THE KEY COMES OFF THE ROWS THIS READ ALREADY HAS rather than from a second
// seek: every event of the turn carries it, and a turn with none is answered
// as having none rather than searched for.
//
// OFF [store.EventRecord.WorkKey], which is the column, and neither off the
// tags blob nor off [store.EventRecord.Spend]. Spend is set by the WRITE path
// and never by a read — `finishRecord` does not populate it — so a reader
// reaching through it would find nil on every row and quietly answer "no
// attempts" for every turn in the company. The tags blob is populated on
// read, but only from what the WRITER extracted: schema/0029 backfilled the
// column for history and could not rewrite every stored blob, so a tag read
// answers nothing for every turn written before the split. See the field.
//
// THE WINDOW IS THE DETAIL READ'S, not the turns list's default. [store.Turn]
// is a listing type and its query takes DefaultTurnDays — a week — when asked
// for nothing, while the rows above came from [store.EventLog.Turn], which
// floors at [store.EventHistory]. Left implicit, opening a turn between eight
// and thirty days old found its work key and then reported no attempt at all,
// including the one being read. MaxTurnDays is that same horizon, so the two
// halves of this answer describe one window.
func (s Sources) attemptsOf(ctx context.Context, id string,
	records []store.EventRecord,
) (string, []store.Turn) {
	key := ""
	for _, rec := range records {
		if rec.WorkKey != "" {
			key = rec.WorkKey
			break
		}
	}
	if key == "" {
		return "", []store.Turn{}
	}
	// EVERY RUN, which is why the page is the ceiling rather than the
	// default. This is an enumeration bounded by the broker's own delivery
	// budget — a trigger gets twenty-five attempts before it dead-letters
	// (internal/queue/jetstream) — not a page somebody scrolls, and it is
	// an index seek on schema/0029's partial index over one key. Taking
	// DefaultTurnPage would have tied "attempt 3 of 4" to a knob sized for
	// a scannable list, so shrinking that list would silently start
	// miscounting attempts.
	rows, err := s.Events.Turns(ctx, store.TurnQuery{
		WorkKey:   key,
		SinceDays: store.MaxTurnDays,
		Limit:     store.MaxTurnPage,
	})
	if err != nil {
		log.WarnContext(ctx, "turn_attempts_unavailable", "turn", id,
			"work_key", key, "error", err)
		return key, []store.Turn{}
	}
	// OLDEST FIRST, which the listing is not: "attempt 2 of 3" has to count
	// from the one that ran first, whatever order the list was built in.
	slices.Reverse(rows)
	return key, rows
}

// TurnClosingEvents is how many of a long turn's last rows are recovered
// beside its opening.
//
// Twenty rather than two, because the two records a reader came for are not
// reliably the last two. A turn ends with its final review phase, then
// `agent_turn_completed` and `turn_completed` — and then the REFLECTION PASS,
// which publishes after them: an episode, a persist decision, a counterparty
// profile, a synthesized, refined or promoted skill, and its own sentinel,
// each of them a model call that also files its own auxiliary
// `agent_phase_completed`. Two would be swallowed by that tail on any turn
// with learning enabled, and the review phase — the one a reader who came for
// "how did it end" wants beside the words — would go with them.
//
// Twenty clears that with headroom while staying small enough that the second
// read is a seek rather than a scan. The recovered rows replace nothing: they
// are appended to the head, so a cut view holds MaxTurnEvents opening rows
// plus at most this many closing ones, which is the same payload budget the
// cap exists for with a bounded addition.
const TurnClosingEvents = 20

// mergeByID appends the rows of `tail` that `head` does not already hold.
//
// The two reads come from opposite ends of one index, so on a turn that only
// just reached the cap they OVERLAP — the same rows, read the other way round
// — and concatenating would render a phase card twice. Order is preserved:
// head is oldest first and so is tail, so the result stays the sequence a turn
// is read in, with whatever gap the cap left between them.
func mergeByID(head, tail []store.EventRecord) []store.EventRecord {
	if len(tail) == 0 {
		return head
	}
	// KEYED ON THE STORE'S OWN IDENTITY, (event_time, event_id), which is the
	// events table's PRIMARY KEY — the id alone is indexed but NOT unique, and
	// the schema says so in as many words. [store.EventLog.ByID] already reads
	// it as "take the newest match", which is only a meaningful thing to say
	// because more than one row can carry one id.
	//
	// A key narrower than the table's drops a row the two reads legitimately
	// both carry, and the drop is not where it would be noticed: `truncated`
	// below is `total > len(records)`, so a wrongly dropped row leaves the
	// page one short of the count and reports a gap in the middle of a turn
	// that is whole on the screen.
	//
	// Microseconds rather than the time.Time, because a time.Time carries a
	// monotonic reading and a location and is not a safe map key; micros is
	// exactly what the column holds.
	type identity struct {
		At int64
		ID string
	}
	keyOf := func(r store.EventRecord) identity {
		return identity{At: store.EncodeTime(r.Time), ID: r.ID}
	}
	seen := make(map[identity]struct{}, len(head))
	for _, r := range head {
		seen[keyOf(r)] = struct{}{}
	}
	for _, r := range tail {
		if _, dup := seen[keyOf(r)]; dup {
			continue
		}
		head = append(head, r)
	}
	return head
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

// MaxA2AChannels bounds one page of the channel record.
//
// TWO HUNDRED, which is the activity feed's own order of magnitude and far
// past what one company has open at once: a channel is one ask and lives for
// one exchange, so the open set is bounded by how many asks are in flight. The
// CLOSED set is what makes a bound necessary at all — it is kept until the
// purge horizon, so a busy company's history is thousands of rows and a
// listing that returned all of them would page a coordination bucket through
// this process to draw one screen.
const MaxA2AChannels = 200

// a2aChannelStates is what `state=` selects, against the one predicate a
// channel has.
var a2aChannelStates = map[string]func(coord.Channel) bool{
	"open":   coord.Channel.Open,
	"closed": func(c coord.Channel) bool { return !c.Open() },
	"all":    func(coord.Channel) bool { return true },
}

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
func (s Sources) a2aChannels(ctx context.Context, p Params) (any, error) {
	// OPEN BY DEFAULT, which is what already shipped and what a screen
	// watching a working company is for.
	state := firstOf(p.String("state"), "open")
	keep, known := a2aChannelStates[state]
	if !known {
		return nil, badParams("state", state, slices.Sorted(maps.Keys(a2aChannelStates)))
	}
	seat := strings.TrimSpace(p.String("seat"))
	// THE RECORD IS READ WHOLE ONLY WHEN IT HAS TO BE. `OpenChannels` is
	// the idle sweep's listing and is deliberately narrower — a closed
	// channel re-reported is a second close for one channel — so the two
	// stay two calls rather than one with a flag.
	read := s.Channels.OpenChannels
	if state != "open" {
		read = s.Channels.AllChannels
	}
	channels, err := read(ctx)
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
		// EITHER END, because a seat's page asks one question — "what
		// did this seat ask, and what was it asked" — and splitting it
		// into two params would make the common case two reads.
		if !keep(c) || (seat != "" && c.Requester != seat && c.Target != seat) {
			continue
		}
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
	// CUT AFTER THE SORT, so a page is the most recent N rather than
	// whichever N the bucket happened to walk first.
	limit := Clamp(p.Int("limit", 0), MaxA2AChannels, MaxA2AChannels)
	truncated := len(out) > limit
	if truncated {
		out = out[:limit]
	}
	return map[string]any{
		"channels":  out,
		"available": true,
		"state":     state,
		// SAYS WHAT IS MISSING, like every other cut in this tree: a page
		// that filled is indistinguishable from a company with exactly
		// that many channels.
		"truncated": truncated,
	}, nil
}

// knowledgeSearch runs the company's own knowledge search.
//
// Deliberately identical to what a seat's own turn does: same seam, same
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
		return unavailable(KnowledgeNoScope, "the "+searcher.Backend()+" backend is configured but knowledge.scope lists no container, so an org-wide search has nothing to read")
	}
	// A BACKEND THAT KEEPS AN INDEX can be behind its own projection, and
	// during that window every search answers empty. The optional
	// interface rather than a method on the seam: a live vendor search has
	// no index and nothing to report, so requiring it of every backend
	// would be four implementations of "false".
	if builder, ok := searcher.(interface {
		Building(ctx context.Context) bool
	}); ok && builder.Building(ctx) {
		return unavailable(KnowledgeBuilding,
			"this node is still indexing what it has projected, so a search "+
				"here would answer empty for pages that exist")
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

	// KnowledgeBuilding — the backend is wired and its index is still
	// catching up with what this node has projected.
	//
	// SEPARATE FROM AN EMPTY RESULT, and that is the entire reason it
	// exists: "the company has written nothing down" and "this node has
	// not finished reading what it wrote" are opposite facts, and the
	// second is true for the whole first index build on a freshly joined
	// node. A screen that showed the first for the second would send
	// somebody looking for a wiki that is right there.
	//
	// Only a backend that keeps a local index can report it — a live
	// vendor search has nothing to build — so it is read through an
	// optional interface rather than added to [knowledge.Searcher].
	KnowledgeBuilding KnowledgeReason = "building"
)

// Valid reports whether r is a reason this package emits, so an unknown value
// off the wire is a value rather than a panic.
func (r KnowledgeReason) Valid() bool {
	switch r {
	case KnowledgeRan, KnowledgeNoCompany, KnowledgeNoBackend, KnowledgeNoScope,
		KnowledgeBuilding:
		return true
	}
	return false
}

// KnowledgeHitLimit bounds one search. It matches what a turn-start prefetch
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
