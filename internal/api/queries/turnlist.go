// The list of turns — the one view of a working company that did not exist.

package queries

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/store"
)

// turns lists one row per unit of agent work, newest first.
//
// A TURN IS THE UNIT OF WORK THIS ENGINE DOES — a wake, a decision, some tool
// rounds, a reply — and everything else is a projection of one: the spend
// rollup, the seat page, an item's history. None of them is a LIST of them.
// The dashboard faked one by paging the raw event feed sixty-one times and
// folding the rows in the browser: slow, capped at whatever the caller gave up
// on, and wrong at the page boundary, where a turn straddling two pages
// appeared twice.
//
// THE CURSOR IS ON THE TURN, not on an event, because that is what the listing
// is ordered by — and it is exactly the defect the browser-side fold had: a
// keyset on any one event pages a turn twice. It is OPAQUE ([turnCursor]).
//
// ONE SEAT'S TURNS ARE ITS TRAIL, and every seat's are the company's — see
// [Sources.trailOf], which decides both before anything is read.
func (s Sources) turns(ctx context.Context, p Params) (any, error) {
	q := store.TurnQuery{
		SinceDays: p.Int("days", 0),
		Model:     strings.TrimSpace(p.String("model")),
		// EVERY ATTEMPT AT ONE TRIGGER. A turn id names one run now, so
		// a redelivered trigger is several rows here — and this is how a
		// reader asks for the others. See ADR-0017.
		WorkKey: strings.TrimSpace(p.String("work_key")),
		Limit:   p.Int("limit", 0),
	}
	// FAILED IS THREE-VALUED, and the third value is the default: nil is
	// every turn, true is the ones that carried a failure, false is the
	// ones that did not. Folding the absent case into `false` would make
	// an unparameterised list hide every failing turn — which is the one
	// an operator opens this screen for.
	if raw := strings.TrimSpace(p.String("failed")); raw != "" {
		switch raw {
		case "true":
			yes := true
			q.Failed = &yes
		case "false":
			no := false
			q.Failed = &no
		default:
			return nil, badParams("failed", raw, []string{"true", "false"})
		}
	}
	// THE ORDER, a closed set the store owns: newest first, or the most
	// tokens first — the spend screen's "which turns cost the most", which
	// the daily usage rows cannot answer because they hold no turn.
	if raw := strings.TrimSpace(p.String("sort")); raw != "" {
		q.Sort = store.TurnSort(raw)
		if !q.Sort.Valid() {
			return nil, badParams("sort", raw, names(store.TurnSorts))
		}
	}
	// ONE SEAT'S TURNS, by its HANDLE and nothing else, resolved through
	// the chart to the agent id every row of its turns carries. The list
	// took a role name and a raw agent id instead, so the sidebar's seat
	// rows — which link here with `seat=<handle>` — landed on every seat's
	// turns, and a role name could not tell apart two unit seats stamped
	// from one template. See [Sources.seatAgentID].
	agentID, principal, err := s.trailOf(ctx, "turns", p)
	if err != nil {
		return nil, err
	}
	q.AgentID = agentID
	// THE TURNS ON ONE WORK ITEM, by its identity across trackers — see
	// workItemParam for why a malformed one is refused rather than matched.
	item, err := workItemParam(p)
	if err != nil {
		return nil, err
	}
	// AND ONLY FOR WHOEVER THE TRACKER SHOWS A TASK TO. [turnsFor] withholds
	// the item each turn was charged to from anybody else, and a filter by
	// that item would hand it straight back: which of an agent's turns were
	// on ENG-42 is the tracker's fact, asked of the trail. Refused naming the
	// tracker rather than dropped, because the unfiltered page answers a
	// different question from the one that was asked.
	if item != "" && !authz.Shows(principal, authz.KindTask) {
		return nil, NewRefusal("turns on a work item", authz.Decision{
			Reason: authz.ReasonNoGrant, Grants: []iam.Grant{authz.AppFor(authz.KindTask)}})
	}
	q.WorkItem = item
	// THE WINDOW AS TWO INSTANTS ON THE TURN'S START, or as whole days back
	// from now — never both, since one would silently lose. `days` alone
	// cannot name a window in the past: a bar picked three days ago was
	// answered with the last day's turns, every one outside it.
	since, err := instantParam(p, "since")
	if err != nil {
		return nil, err
	}
	until, err := instantParam(p, "until")
	if err != nil {
		return nil, err
	}
	if p.Has("days") && (!since.IsZero() || !until.IsZero()) {
		return nil, fmt.Errorf("%w: name the window by days or by since and until, "+
			"not both", ErrBadParams)
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return nil, fmt.Errorf("%w: since=%s is not before until=%s — the window "+
			"starts at since and ends before until", ErrBadParams,
			since.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	q.Since, q.Until = since, until
	before, err := turnCursorParam(p)
	if err != nil {
		return nil, err
	}
	q.Before = before
	if q.Sort == store.TurnSortTokens && before != nil {
		// A RANKING HAS NO POSITION TO RESUME FROM, and paging one by
		// start time would mix two orders on one screen.
		return nil, fmt.Errorf("%w: sort=%s is a ranking and takes no before=; "+
			"narrow the window with days= instead", ErrBadParams, q.Sort)
	}

	// ONE READING OF THE CLOCK, handed to every node: a window of whole
	// days is cut from it, and each node cutting its own would merge pages
	// cut a moment apart.
	q.At = s.clock()
	page, coverage, err := s.Events.Turns(ctx, q)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"turns": turnsFor(page.Turns, principal), "next": nil,
		"coverage": coverage}
	if page.Next != nil {
		// THE CURSOR THE CALLER RESUMES FROM, echoed rather than left for
		// a client to assemble — the same rule the event list follows,
		// because a client that built it from the last row's fields would
		// be reimplementing the one thing that must not drift. It is the
		// FLEET's, and so it can be present on an empty page: when a
		// node holding more than its page stopped before any turn above
		// it could be shown, the cursor is where that node stopped rather
		// than the end.
		out["next"] = encodeTurnCursor(*page.Next)
	}
	return out, nil
}

// trailOf is the seat a TRAIL LISTING — `turns`, `phases` — is narrowed to by
// `seat=`, as the agent id its rows are filed under, decided before anything
// is read; and the principal the answer is shown to.
//
// # One seat's, or the company's
//
// ONE SEAT'S TURNS AND PHASES ARE ITS TRAIL — the prompts it was sent, the
// responses it gave, what woke it — and are read by whoever reads that seat's
// trail ([authz.ActionSeatTrailRead]: an admin, or the seat's lead, who
// steers, pauses and answers for the agents they run and could do none of it
// well without reading what those agents did). EVERY SEAT'S AT ONCE is the
// company-wide trail ([authz.ActionAuditRead], an admin's), which no lead's
// relation reaches: a listing naming no seat is every seat in the company.
//
// DECIDED BEFORE THE HANDLE IS RESOLVED, so a seat the caller may not read is
// refused in the same words whether or not it is an agent's.
func (s Sources) trailOf(ctx context.Context, what string, p Params) (string, iam.Principal, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return "", principal, unresolved(ctx, what)
	}
	handle := strings.TrimSpace(p.String("seat"))
	if handle == "" {
		d := authz.Decide(ctx, principal, authz.ActionAuditRead,
			authz.Object{Kind: authz.KindCompany}, s.Chart, s.clock())
		switch {
		case d.Unknown():
			return "", principal, fmt.Errorf("%w: this node cannot decide %s yet: %w",
				ErrUnavailable, authz.ActionAuditRead, d.Err)
		case !d.Allowed:
			return "", principal, NewRefusal(what, d)
		}
		return "", principal, nil
	}
	if err := s.mayRead(ctx, principal, authz.ActionSeatTrailRead, handle); err != nil {
		return "", principal, err
	}
	agentID, err := s.seatAgentID(handle)
	return agentID, principal, err
}

// turnsFor is a page of turns as p is shown it: each turn's work item — the
// task it was charged to — only where [authz.Shows] shows p a task (ADR-0033).
// A lead reads their agents' trail whether or not they use Worklet, and
// their agents' turns are not a way into it. A COPY wherever it changes
// anything.
func turnsFor(turns []store.Turn, p iam.Principal) []store.Turn {
	if turns == nil || authz.Shows(p, authz.KindTask) {
		return turns
	}
	out := make([]store.Turn, len(turns))
	for i, turn := range turns {
		turn.WorkItem = nil
		out[i] = turn
	}
	return out
}

// A PAGE OF TURNS RESUMES FROM AN OPAQUE CURSOR: `next` is a token, and `before`
// takes exactly the token a page answered, and nothing else.
//
// OPAQUE, unlike the event list's `before_time`/`before_id`, because the
// position is not a row's own fields. The event list resumes from its last
// row's key, which the row carries; a page of turns resumes from where its last
// turn is LISTED — the earliest start at which any node's page lists it, which
// is not its `started_at` when its earliest half is a half no node lists — and
// from that turn's id, without which two turns starting at one microsecond
// either side of a page's cut were one position, and the one past the cut was
// on no page. And the fleet's cursor can stand on an empty page, where a node's
// page stopped, which is no row at all. So a client that composed one from a
// row would resume from a position the walk did not stop at; one token says
// "hand this back as it is", and leaves the engine free to say more in it.
//
// Its content is the position — the listing instant to the microsecond and the
// turn id — in URL-safe base64, so it passes through a query string untouched.
// A token this endpoint did not hand out is refused as `bad_params`, never read
// as some other position.
func encodeTurnCursor(c store.TurnCursor) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(c.Start.UTC().Format(time.RFC3339Nano) + " " + c.TurnID))
}

// turnCursorParam reads `before`, or nil when it is absent.
func turnCursorParam(p Params) (*store.TurnCursor, error) {
	raw := strings.TrimSpace(p.String("before"))
	if raw == "" {
		return nil, nil
	}
	refused := badParams("before", raw, []string{"the `next` a page of turns answered"})
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, refused
	}
	stamp, id, found := strings.Cut(string(body), " ")
	if !found || id == "" {
		return nil, refused
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return nil, refused
	}
	return &store.TurnCursor{Start: at.UTC(), TurnID: id}, nil
}
