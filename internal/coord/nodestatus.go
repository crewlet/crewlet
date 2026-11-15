package coord

import (
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// What a node reports about ITSELF, fleet-wide.
//
// # What rides on the presence lease
//
// Only the node running a seat knows how many turns are in flight on it,
// whether it has begun draining, and what it concluded about its own config
// lag. `/health` answers those about the node that served the request, so
// behind a load balancer a refresh tells a different story each time.
//
// The alternative was request/reply: the lease table locates the owner, and
// a caller asks it directly. That was refused: every answer would become partial
// (some nodes reply, some time out), it opens a new trust edge (a request
// carrying an operator's authority across a node boundary), and it
// duplicates a mechanism that already works.
//
// The presence lease is renewed on a timer and its Meta is re-sent on EVERY
// heartbeat — that is not an addition, it is how a node already advertises
// its roles and labels. Its LIVE CORE goes beside them: the in-flight count,
// the drain, the posture, the build, the appliers and the firing alarms,
// each a few bytes and each bounded by something the engine declares (the
// alarm table bounds the marks).
//
// Freshness is the heartbeat interval, which is exactly the freshness every
// other column of the fleet view already has, and the view prints how long
// each lease has left — so a stale row reads as stale rather than as
// current.
//
// # What does not
//
// The rows that grow with a company's configuration — a row per MCP server
// and per model entry, the runner cells' findings and the node's infra
// summary — are a record of their own, [NodeStatusRecord], written when they
// change and joined to this lease by its epoch. The presence is rewritten on
// every beat and is what placement and liveness read, so it carries what they
// read and the core beside it, and nothing a person's configuration can grow:
// see nodestatusrecord.go for why, and for the join.

// StatusKey is where node status sits in a presence lease's Meta.
//
// Its own key rather than fields at the top level: placement reads `roles`
// and `labels` off the same map, and two concerns sharing a namespace is how
// one of them eventually shadows the other.
const StatusKey = "status"

// GitBackgroundKey is where a Gitlet node's presence lease carries what its git
// cache gives its background work: its MAINTENANCE BUDGET, the figure every
// run of a repository's maintenance on that node is admitted against — the
// node's memory less what its serving keeps — read again at every maintenance
// pass, so a container resized in place says its new figure within a pass and
// a beat (internal/gitlet/cache, Cache.BackgroundBudget). Every Gitlet node weighs
// every other by it to place a repository's maintenance on the first node of
// the repository's git order whose budget holds the work, the home on a fleet
// of alike nodes (internal/estate, Router.Maintainer).
//
// ON THE HEARTBEAT, for the reason a node's status is: which Gitlet node can hold
// a repository's maintenance is a question every Gitlet node asks about every
// other, and a request/reply asking each in turn would be the fan-out whose
// partial answers the file head refused. Its freshness is a heartbeat, and a
// budget moves only when a container is resized.
//
// BESIDE THE STATUS, NEVER IN IT. A status is assembled under a share of the
// beat, because answering reads the broker and the control plane, and a beat
// whose hook overruns that share publishes no status at all — right for a
// display column, which then reads as not saying, and wrong for a placement
// input, which would go with it: a Gitlet node whose status was dropped for a
// beat would say nothing of what it can hold, the large node of a fleet would
// be weighed as one known to hold nothing, and every repository only it can
// maintain would be left to a node whose own admission refuses the work. The
// budget is a figure in memory that nothing can overrun, so the seat host
// writes it on EVERY beat beside the status, as it writes its own held count
// (internal/seat/placement, HeldKey): a live Gitlet node of this build always
// says it.
//
// ZERO IS WRITTEN, never left out: it is a claim — a Gitlet node that has not
// built Gitlet yet, or whose transfers keep all of its memory, has nothing to
// give — and a row without the key is the other fact, a node NOT SAYING (a
// build or a host that writes none), which [GitBackgroundFromMeta] reports as
// such. A node without the gitlet role writes none.
const GitBackgroundKey = "git_background_bytes"

// GitBackgroundFromMeta reads a peer's maintenance budget off its presence
// lease's Meta ([GitBackgroundKey]), and reports whether the row says one at
// all: false where the key is absent — a node not saying, never one with
// nothing to give — or holds what no build writes (a negative figure, a value
// that is not a number), which says nothing this build can act on either.
//
// THROUGH THE ONE NUMBER READER, never a second one for int64: every target
// this module builds is 64-bit (internal/store/platform.go), so an int holds
// any budget a node can have, and the float64 a JSON round trip through the
// lease store hands back holds one exactly up to 2⁵³ bytes — eight pebibytes.
func GitBackgroundFromMeta(meta map[string]any) (int64, bool) {
	raw, present := meta[GitBackgroundKey]
	if !present {
		return 0, false
	}
	switch raw.(type) {
	case int, int64, float64:
	default:
		return 0, false
	}
	budget := int64(intFromMeta(raw))
	if budget < 0 {
		return 0, false
	}
	return budget, true
}

// NodeStatus is the live half of what a node is.
//
// It carries no struct tags: the wire shape is [NodeStatus.Meta] and
// [StatusFromMeta], which encode by hand because the lease's Meta is a
// `map[string]any` a peer merges into. A second encoding declared in tags
// would be one nothing calls and nothing keeps honest.
type NodeStatus struct {
	// InFlight is how many turns are running.
	InFlight int

	// Draining is true from the moment a drain begins.
	//
	// Reported even though a draining node DROPS its presence lease: the
	// drop is what takes it out of the placement count, and this is what
	// the last heartbeat before it said. A row that is both draining and
	// expiring is a node shutting down cleanly; one that vanished without
	// it is a node that died.
	Draining bool

	// Posture is what this node concluded about its own config lag —
	// serve, wait, shed, isolated or stuck. The only place an operator can
	// see WHY a node left rotation: /ready reports a bare 503 either way,
	// and "draining" and "cannot apply epoch 41" call for opposite
	// responses.
	//
	// EMPTY when the node could not read its control plane, beside a status
	// that is otherwise whole: the reader's fail-open answer there is serve,
	// which is right for the node's own readiness probe and a confident lie
	// on a peer's screen.
	Posture string

	// StartedAt is when the node's ENGINE was built, which is when the node
	// started: its API runs in the same process and reports the same
	// instant on its own health body.
	StartedAt time.Time

	// Build is the engine build the node runs, as internal/version names it,
	// and "" where a node said none. Whether every live node runs one build
	// is read off it: a fleet mid-upgrade hands work between builds by design.
	Build string

	// ParkedStateVersion is the version of a parked run's state this node's build
	// reads, zero where a node says none: a run parked at a lower version is
	// settled only once no live node reads it, or says nothing.
	ParkedStateVersion int

	// AppliersReady and AppliersTotal are how many of this node's
	// state-log appliers — one per registered domain, run from boot on
	// every data node whatever its company runs — have caught up with
	// their logs, out of how many it runs. Zero of zero is a node without
	// `data`, which runs no log.
	//
	// # Why it is here and not on /ready
	//
	// A node still catching up is HEALTHY. It serves its dashboard,
	// answers its probes and runs the duties it holds; what it does not do
	// is take on NEW seats, because a seat whose tools read a copy that is
	// behind answers "there is no such item" — and a seat acts on that.
	// So the fact belongs where an operator asking "why is the new node
	// holding nothing" can see it, which is the fleet view, and NOT where
	// it would take the node out of rotation.
	//
	// Two integers rather than a bool: "3 of 5" and "0 of 2" are the two
	// readings an operator needs to tell apart, and a bool collapses
	// them.
	AppliersReady int
	AppliersTotal int

	// AdminPosture is this node's own `api.auth.admin`: whether it grants
	// admin to those who hold it (`serve`) or clamps admin away and keeps
	// every app (`refuse`).
	//
	// # Why a fleet publishes this at all
	//
	// Because the posture is applied at DECISION time, per node, per
	// request, and nothing is written when it changes. That is what makes
	// a fleet whose nodes disagree a LEGAL state rather than a broken one —
	// one node published to colleagues refusing admin, another an
	// administrator signs in on serving it, or a rolling restart halfway
	// through a change.
	//
	// Legal and invisible is the problem. Without this, whether an
	// administrator can run the company depends on which node a load
	// balancer sent them to, and the symptom is a person who can revoke a
	// credential on one request and cannot on the next, with nothing
	// anywhere saying why. One word per node turns that into a row on the
	// fleet view.
	//
	// THE VALUE ITSELF AND NOT A DIGEST, which is what the list of grants it
	// replaced had to publish: two values are one short word, so there is
	// nothing to compress.
	//
	// EMPTY IS A NODE THAT IS NOT SAYING, never a node that refuses admin:
	// a node that serves no API has no posture to state and publishes
	// nothing here, and reading that as "refuses" would draw every such node
	// as locked down.
	AdminPosture iam.AdminPosture

	// Alarms are the alarms this node's own table has firing as of the
	// beat: ONE MARK PER KIND however many logs a kind stands on, the
	// longest-standing first — the order the node's health envelope takes
	// its `worst` from, so the fleet view and the node's own health name
	// the same condition as the one unanswered longest. Meaningful only
	// where AlarmsEvaluated is true.
	//
	// # Why the firing set rides the heartbeat
	//
	// An alarm is evaluated by the node it describes — its reading is that
	// node's applier, disk and refusals — and its standing set reached
	// nobody else: the gauge needs a collector, the log line somebody
	// reading that node's log, and the health envelope answers about
	// whichever node a load balancer sent the request to. "What is wrong
	// anywhere in the fleet" is therefore read here, for the reason every
	// other field of this struct is here: every node already re-sends its
	// status and every peer already reads it. It is also the CURRENT set,
	// which a stream of transitions cannot be: a clear that was never said
	// (lost with the process that would have said it) is read off the set
	// as it stands.
	//
	// KIND AND SINCE, NEVER THE MEASUREMENT. An alarm's detail moves on
	// every evaluation (a lag grows), so carrying it would make two beats
	// over the same alarms differ, and it is unbounded prose re-sent on
	// every renew; the node's own `alarm_raised` line carries it, and so
	// does its retention report. What is carried is bounded by the alarm
	// table itself — one mark a kind — so nothing here needs clipping.
	Alarms []AlarmMark

	// AlarmsEvaluated is whether this node has an alarm table it has
	// evaluated: false on a node running no state log, which has no table
	// (every node without `data`), and on one whose first evaluation has
	// not run yet.
	//
	// A FLAG BESIDE THE LIST, NOT A NIL LIST STANDING FOR IT. "Nothing is
	// firing" and "this node cannot say" are opposite claims, and the
	// second drawn as the first is the confident-zero mistake this struct
	// is written to avoid. A nil slice cannot carry the difference safely:
	// it is also what collecting no marks produces, so a builder that found
	// nothing firing would publish "not saying" — and a node whose table
	// never ran, built the other way, would read as healthy.
	AlarmsEvaluated bool
}

// AlarmMark is one alarm firing on a node: which, and since when.
type AlarmMark struct {
	// Kind is the alarm's name as the published table spells it
	// (docs/reference/alarms.md): `backup_age`, `apply_lag`.
	//
	// A STRING rather than the table's own type, because the table belongs
	// to the state-log framework, which sits above this package — and a
	// kind a newer build added is still an alarm firing on that node, which
	// a reader shows by its name rather than dropping.
	Kind string

	// Since is when the node raised it, on that node's own clock, to the
	// second: the evaluation that first found it, and for a kind standing
	// on several logs the oldest of them, so a log joining an alarm that is
	// already up does not make it look newer. An alarm raised again soon
	// after it cleared keeps its run's first raise (statelog.RelapseWindow).
	Since time.Time
}

// Meta renders the status for a lease's Meta map.
func (s NodeStatus) Meta() map[string]any {
	out := map[string]any{
		"in_flight": s.InFlight,
		"draining":  s.Draining,
	}
	if s.Posture != "" {
		out["posture"] = s.Posture
	}
	if !s.StartedAt.IsZero() {
		out["started_at"] = s.StartedAt.UTC().Format(time.RFC3339)
	}
	if s.Build != "" {
		out["build"] = s.Build
	}
	if s.ParkedStateVersion > 0 {
		out["parked_state_version"] = s.ParkedStateVersion
	}
	if s.AdminPosture != "" {
		// OMITTED WHEN UNSET, for [StatusFromMeta]'s reason: a node
		// that serves no API has no posture to state, and an empty
		// string on the wire would make it look like a peer whose
		// posture differs from everybody's.
		out["admin"] = string(s.AdminPosture)
	}
	if s.AppliersTotal > 0 {
		// OMITTED where the node runs none — a node without `data` — so
		// it reads absent rather than "0 of 0", which a reader would
		// otherwise have to know is not a stalled applier.
		out["appliers_ready"] = s.AppliersReady
		out["appliers_total"] = s.AppliersTotal
	}
	if s.AlarmsEvaluated {
		// PRESENT, AND A LIST, wherever the node evaluated its table — an
		// EMPTY list included, which is the claim that nothing is firing
		// (and must reach JSON as `[]`, never as the `null` a nil slice
		// marshals to). Absent only where the node cannot say.
		marks := make([]map[string]any, 0, len(s.Alarms))
		for _, m := range s.Alarms {
			mark := map[string]any{"kind": m.Kind}
			if !m.Since.IsZero() {
				mark["since"] = m.Since.UTC().Format(time.RFC3339)
			}
			marks = append(marks, mark)
		}
		out["alarms"] = marks
	}
	return out
}

// StatusFromMeta reads a peer's status off a presence lease's Meta, and
// reports whether the node published one at all.
//
// # Absent is not zero
//
// A node that publishes no status — its status hook did not answer within its
// share of that beat, so its heartbeat carried the placement half alone — is
// not a node with no work in flight. Reporting it as 0 would draw an idle row
// for a process that is simply not saying, which is the confident-zero mistake
// the whole surface is written to avoid.
//
// A key this build does not know is IGNORED rather than refused: a successor
// sharing the fleet publishes what it adds beside what this build reads, and a
// status that failed to decode over it would read as absent for as long as the
// successor ran.
func StatusFromMeta(meta map[string]any) (NodeStatus, bool) {
	raw, ok := meta[StatusKey].(map[string]any)
	if !ok {
		return NodeStatus{}, false
	}
	status := NodeStatus{
		InFlight:      intFromMeta(raw["in_flight"]),
		Posture:       stringFromMeta(raw["posture"]),
		AppliersReady: intFromMeta(raw["appliers_ready"]),
		AppliersTotal: intFromMeta(raw["appliers_total"]),
		AdminPosture:  iam.AdminPosture(stringFromMeta(raw["admin"])),
		Build:         stringFromMeta(raw["build"]),

		ParkedStateVersion: max(intFromMeta(raw["parked_state_version"]), 0),
	}
	status.Draining, _ = raw["draining"].(bool)
	if at, err := time.Parse(time.RFC3339, stringFromMeta(raw["started_at"])); err == nil {
		status.StartedAt = at
	}
	status.Alarms, status.AlarmsEvaluated = alarmsFromMeta(raw["alarms"])
	return status, true
}

// alarmsFromMeta reads a status's alarm marks, and whether it carried a list
// of them at all.
//
// A KEY THAT IS ABSENT, OR NOT A LIST, IS A NODE THAT DID NOT SAY — never one
// with nothing firing, which only a list (an empty one included) claims. A mark
// naming no kind describes nothing a reader could show, so it is dropped, as an
// MCP row naming no server is; a since that does not parse leaves the mark in
// place with no instant, because the alarm firing is the fact and its start a
// detail of it.
func alarmsFromMeta(v any) ([]AlarmMark, bool) {
	switch v.(type) {
	case []any, []map[string]any:
	default:
		return nil, false
	}
	var out []AlarmMark
	for _, r := range listFromMeta(v) {
		mark, ok := r.(map[string]any)
		if !ok {
			continue
		}
		kind := stringFromMeta(mark["kind"])
		if kind == "" {
			continue
		}
		m := AlarmMark{Kind: kind}
		if at, err := time.Parse(time.RFC3339, stringFromMeta(mark["since"])); err == nil {
			m.Since = at
		}
		out = append(out, m)
	}
	return out, true
}

// listFromMeta accepts the typed slice this build writes and the []any a JSON
// round trip returns, for the reason [intFromMeta] accepts both numbers.
func listFromMeta(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out
	default:
		return nil
	}
}

// intFromMeta accepts the int this build writes and the float64 a JSON round
// trip through the lease store returns.
//
// Both, because the same map is read locally (where it is an int) and after
// a store round trip (where it is not), and a reader that knew only one
// would report every peer's in-flight count as zero.
func intFromMeta(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func stringFromMeta(v any) string {
	s, _ := v.(string)
	return s
}

// seatFromMeta reads a seat's agent id, and uuid.Nil for anything that is not
// one in canonical form — a value no build of this one wrote is a seat the
// row does not name, never a seat named wrongly.
func seatFromMeta(v any) uuid.UUID {
	s := stringFromMeta(v)
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil
	}
	return id
}

// stringsFromMeta accepts the []string this build writes and the []any a JSON
// round trip returns, keeping only the strings in it; nil when there are none,
// so a row that carried no list reads back as one that carried none.
func stringsFromMeta(v any) []string {
	switch l := v.(type) {
	case []string:
		if len(l) == 0 {
			return nil
		}
		return slices.Clone(l)
	case []any:
		var out []string
		for _, item := range l {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
