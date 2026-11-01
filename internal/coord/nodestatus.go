package coord

import (
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/textcut"
)

// What a node reports about ITSELF, fleet-wide.
//
// # Why this rides on the presence lease
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
// its roles and labels. Live status goes beside them.
//
// Freshness is the heartbeat interval, which is exactly the freshness every
// other column of the fleet view already has, and the view prints how long
// each lease has left — so a stale row reads as stale rather than as
// current.

// StatusKey is where node status sits in a presence lease's Meta.
//
// Its own key rather than fields at the top level: placement reads `roles`
// and `labels` off the same map, and two concerns sharing a namespace is how
// one of them eventually shadows the other.
const StatusKey = "status"

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

	// MCP is what this node's MCP servers did when it last started them,
	// one row per configured server. Nil is "this node started none": a
	// node that did not say is one whose heartbeat carried no status at
	// all, which [StatusFromMeta] reports as absent.
	//
	// On the heartbeat rather than asked for, for the reason the rest of
	// this struct is: every node already re-sends it, every peer already
	// reads it, and the settings screen asking each node in turn would be
	// the fan-out whose partial answers the file head refused.
	MCP []MCPServerStatus

	// Models is what this node knows about each configured model entry,
	// one row per entry of its ACTIVE revision ([ModelStatus]). Nil is
	// "this node runs none".
	//
	// On the heartbeat for MCP's reason: a model's sit-out, its last answer
	// and its last refusal are each node's own (a node's process meets the
	// vendor's limit, and its memory keeps the window), every node already
	// re-sends its status, and the Models page reading one lease listing
	// answers for the fleet without asking each node in turn. A node that
	// did not report is a column of its own on that page, never a row of
	// zeros.
	Models []ModelStatus

	// Runners is whether each executor cell can start a box on this node,
	// as its runner probe last found it. Nil is a node that has not probed
	// (one that runs no executor, or one still booting).
	Runners *RunnerStatus
}

// ModelStatus is one configured model entry, as ONE node holds it at the
// beat.
//
// THE NODE'S OWN VIEW, never the fleet's: the sign-in route is what THIS node
// resolves (a variable may be set in one node's environment and not
// another's), and the sit-out, the last answer and the last failure are this
// node's memory of its own calls. The reader joins the rows across nodes.
type ModelStatus struct {
	// Key is the entry's key in `providers.llm`.
	Key string

	// Plan is the first [ModelPlanChars] hex characters of the entry's
	// sign-in identity, and "" when nothing signs it in. A PREFIX AND
	// NEVER THE WHOLE: it is enough for a reader to say "these entries
	// share one sign-in", which is all a reader of the lease needs, and the
	// whole identity is a credential's fingerprint nothing publishes.
	Plan string

	// SignIn is the route this node resolves the entry's sign-in through,
	// in harness's words (token, credentials, api-key, env, inherit-env,
	// environment, none), and Resolves whether its credential resolved
	// here.
	SignIn   string
	Resolves bool

	// SittingOut is the window the entry sits out on this node, nil when
	// it sits out none.
	SittingOut *ModelSitOut

	// LastAnswer is when the entry's sign-in last answered a call on this
	// node, zero when it has not since this process started.
	LastAnswer time.Time

	// LastFailure is the latest failure of the entry's sign-in on this
	// node, nil when there is none.
	LastFailure *ModelFailure

	// Parked is the seats THIS node holds parked on a chain naming the
	// entry, by agent id as MCP rows name seats (ADR-0013). On the wire it is
	// the [MaxModelParkedSeats] longest-waiting of them; ParkedCount is how
	// many there are and ParkedSince when the first of them parked, so a
	// reader says "and N more" and dates the wait without the whole list.
	Parked      []ParkedSeat
	ParkedCount int
	ParkedSince time.Time

	// HostLogin is the entry's harness login on this node's disk, only for an
	// entry that signs in with credential files: nil for every other.
	HostLogin *HostLogin
}

// ModelSitOut is a sit-out window on the wire.
type ModelSitOut struct {
	// Until is when the entry is tried again, ResetsAt when the vendor said
	// the limit lifts (zero where it named no instant), and Since the
	// report that opened the window.
	Until, ResetsAt, Since time.Time

	// Window is which of the vendor's limits it is, in harness's words
	// (session, weekly, model, unknown).
	Window string

	// Said is the head of the vendor's own sentence, bounded to
	// [MaxModelSaidBytes].
	Said string
}

// ModelFailure is a failure on the wire: its llm kind (auth, rate_limit,
// timeout, server, fatal), when, and the head of what the harness said, bounded to
// [MaxModelSaidBytes].
type ModelFailure struct {
	Kind string
	At   time.Time
	Said string
}

// ParkedSeat is one seat parked on a chain naming an entry, since when.
type ParkedSeat struct {
	Seat  uuid.UUID
	Since time.Time
}

// HostLoginState is a harness login's state as its own status command reports it.
type HostLoginState string

const (
	// HostSignedIn is a status command that exited 0.
	HostSignedIn HostLoginState = "signed_in"
	// HostSignedOut is one that exited 1, the harnesses' own "not logged in".
	HostSignedOut HostLoginState = "signed_out"
	// HostLoginUnknown is anything else: the command could not run, timed
	// out, or exited with a code that is no answer.
	HostLoginUnknown HostLoginState = "unknown"
)

// Valid reports whether the state is one this build names.
func (s HostLoginState) Valid() bool {
	switch s {
	case HostSignedIn, HostSignedOut, HostLoginUnknown:
		return true
	}
	return false
}

// HostLogin is an entry's harness login on one node's disk.
type HostLogin struct {
	State HostLoginState

	// Method is how it signed in, as the harness says (claude.ai, console,
	// chatgpt), and Account the account it names, bounded to
	// [MaxModelAccountBytes]. Either may be empty: not every harness says.
	Method, Account string

	// Detail is why the state is unknown, bounded to [MaxModelSaidBytes].
	Detail string

	// CheckedAt is when the status command last ran.
	CheckedAt time.Time
}

// RunnerState is whether one executor cell can start a box on a node.
type RunnerState string

const (
	// RunnerReady is a cell whose probe passed.
	RunnerReady RunnerState = "ready"
	// RunnerNotReady is a declared cell whose probe failed.
	RunnerNotReady RunnerState = "not_ready"
	// RunnerUnconfigured is a cell `providers.sandbox` does not declare.
	RunnerUnconfigured RunnerState = "unconfigured"
)

// Valid reports whether the state is one this build names.
func (s RunnerState) Valid() bool {
	switch s {
	case RunnerReady, RunnerNotReady, RunnerUnconfigured:
		return true
	}
	return false
}

// RunnerStatus is each executor cell's readiness on one node.
type RunnerStatus struct {
	Direct, Container, E2B RunnerCell
}

// RunnerCell is one cell's probe result: its state, what the probe found, and
// for a cell that is not ready the one thing to do about it. Detail and Fix
// are bounded to [MaxModelSaidBytes].
type RunnerCell struct {
	State     RunnerState
	Detail    string
	Fix       string
	CheckedAt time.Time
}

// ModelPlanChars is how much of a sign-in identity a heartbeat carries: twelve
// hex characters, 48 bits, which tells a company's handful of sign-ins apart
// and reverses to nothing.
const ModelPlanChars = 12

// MaxModelSaidBytes bounds the text a model row or a runner cell carries: the
// vendor's sentence behind a sit-out or a failure, a login's detail, a probe's
// finding and its fix.
//
// 240, because the useful part is the head: "You've hit your session limit ·
// resets 4:30pm (America/Vancouver)" is 66 bytes, a harness's auth refusal is a
// line, and a probe's fix is one sentence. A row carries text only while a
// failure or a sit-out stands, so the bound costs a healthy entry nothing on a
// beat re-sent every [ReconcileInterval]; the whole sentence is in the node's
// own `harness_limit_reported` line.
const MaxModelSaidBytes = 240

// MaxModelParkedSeats bounds the parked seats a model row names on the wire:
// EIGHT, the longest-waiting first. A row's size must not grow with the
// company: every seat of a hundred-seat company parked on one plan would put
// a hundred ids on a beat re-sent every [ReconcileInterval]. Eight is what a
// person reads before "and N more", and the count and the earliest instant
// travel whole beside them.
const MaxModelParkedSeats = 8

// MaxModelAccountBytes bounds a host login's account (an email or an org
// name): 120 bytes, past any real address, so a harness that printed something
// else there cannot grow the beat.
const MaxModelAccountBytes = 120

// MCPServerStatus is one configured MCP server, as ONE node runs it at the
// beat.
//
// AGGREGATED PER SERVER, NOT PER CHILD. A per-role server is a template with
// one child per seat this node holds, and a row per child would put a
// company's seat count times its per-role servers on a lease that is re-sent
// every heartbeat — to answer a question ("is the jira server working on this
// node?") the counts answer as well.
type MCPServerStatus struct {
	// Server is the config's own name for it — `mcp_servers[].name`, never
	// a per-seat instance name, since the row stands for every instance.
	Server string

	// Shared is true for the one company-wide child, false for a per-role
	// template.
	Shared bool

	// Started is how many instances are serving: started, listed their
	// tools, and still up as of this beat. A child that died since is not
	// counted here but under Failed until its bridge has brought it back.
	Started int

	// Failed is how many are not: a child that would not start or list
	// its tools, one that died after starting and is being restarted, a
	// config that did not resolve into a launchable spec, and a seat's
	// child whose credential resolves to nothing that nothing will mint,
	// or that the integration minting it cannot mint until a person acts.
	Failed int

	// Waiting is how many this node holds back UNSTARTED, and on purpose:
	// a seat's child whose credential a provisioning pass mints for it and
	// has not sealed yet, where nothing that pass last reported stops it.
	// Neither started nor failed, because nothing is wrong — it starts
	// once the seal reaches the node — and counting it with either would
	// tell the person reading it the opposite of that.
	Waiting int

	// Tools is how many tools one started instance serves — the largest,
	// where the instances of one template disagree.
	Tools int

	// Error is ONE failed instance's reason, and ErrorSeat the seat it
	// belonged to, by its AGENT ID (uuid.Nil for a shared server). One
	// rather than all, for the size argument above; the first by agent id,
	// so two beats with the same failures publish the same row.
	//
	// THE ID RATHER THAN THE HANDLE, because this row is published on a
	// lease every peer reads, and a peer reads it against its OWN org: the
	// id names the seat the instance was launched for — the agent id
	// derived from its handle (ADR-0013) — and the reader resolves it to
	// that handle.
	Error     string
	ErrorSeat uuid.UUID

	// Wait is ONE waiting instance's sentence — the variables it waits for
	// and the integration that mints them — and WaitSeat its seat, chosen
	// and bounded as Error and ErrorSeat are.
	Wait     string
	WaitSeat uuid.UUID

	// Unlisted are the entry's tool_annotations keys that NO instance this
	// node started lists a tool for, sorted. Such a key overrides nothing
	// here. Only the row can say it: one instance's listing is not the
	// server's, because a per-role child serves what its seat's credentials
	// reach, and a key one seat's child lacks may be exactly right for the
	// next one's. Across nodes the same intersection is the status answer's.
	//
	// Whole keys and every one of them, unlike Error: the list is bounded by
	// the entry's own configuration, as the row count is, rather than by
	// anything a seat count multiplies — and a key cut short would name a
	// tool nobody wrote.
	Unlisted []string
}

// MaxMCPErrorBytes bounds [MCPServerStatus.Error] and [MCPServerStatus.Wait]
// on the wire.
//
// Two kinds of text arrive here. A start failure's is a handshake error or a
// child's last stderr line, whose useful part — "command not found", "401",
// "connection refused" — is at its head. The engine's own sentence for a
// server it held back, for a value that resolves to nothing, is the other,
// and its useful part is all of it: the variable, then each remedy, the last
// naming the key to declare optional where a server can run without a host's
// setting. At 240 bytes that last remedy never arrived: the tools guide's own
// `NPM_CONFIG_REGISTRY` case is about 300 bytes, and a per-role child missing
// a setting while its seat's token is being minted about 380. 512 carries
// both under names of ordinary length. A row carries text only while
// an instance has failed or waits, so the bound costs a healthy server's row
// nothing on the heartbeat it is re-sent with; the full text is in the node's
// own log line either way (`mcp_server_failed`,
// `mcp_server_credential_missing`), which is where a person fixing it reads.
const MaxMCPErrorBytes = 512

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
	if len(s.MCP) > 0 {
		rows := make([]map[string]any, len(s.MCP))
		for i, m := range s.MCP {
			row := map[string]any{
				"server":  m.Server,
				"shared":  m.Shared,
				"started": m.Started,
				"failed":  m.Failed,
				"tools":   m.Tools,
			}
			if m.Error != "" {
				row["error"] = textcut.Within(m.Error, MaxMCPErrorBytes)
			}
			if m.ErrorSeat != uuid.Nil {
				row["error_seat"] = m.ErrorSeat.String()
			}
			// OMITTED WHEN NOTHING WAITS, which is every row but a
			// seat's during provisioning: absent reads as zero.
			if m.Waiting > 0 {
				row["waiting"] = m.Waiting
			}
			if m.Wait != "" {
				row["wait"] = textcut.Within(m.Wait, MaxMCPErrorBytes)
			}
			if m.WaitSeat != uuid.Nil {
				row["wait_seat"] = m.WaitSeat.String()
			}
			if len(m.Unlisted) > 0 {
				row["unlisted"] = m.Unlisted
			}
			rows[i] = row
		}
		out["mcp"] = rows
	}
	if len(s.Models) > 0 {
		rows := make([]map[string]any, len(s.Models))
		for i, m := range s.Models {
			rows[i] = m.meta()
		}
		out["models"] = rows
	}
	if s.Runners != nil {
		out["runners"] = map[string]any{
			"direct":    s.Runners.Direct.meta(),
			"container": s.Runners.Container.meta(),
			"e2b":       s.Runners.E2B.meta(),
		}
	}
	return out
}

// meta is one model row on the wire. Every optional part is OMITTED when
// unset, so a healthy entry's row is its key, its plan and its route.
func (m ModelStatus) meta() map[string]any {
	row := map[string]any{"key": m.Key, "sign_in": m.SignIn, "resolves": m.Resolves}
	if m.Plan != "" {
		row["plan"] = textcut.Bytes(m.Plan, ModelPlanChars)
	}
	if o := m.SittingOut; o != nil {
		sit := map[string]any{
			"until":  stamp(o.Until),
			"since":  stamp(o.Since),
			"window": o.Window,
			"said":   textcut.Within(o.Said, MaxModelSaidBytes),
		}
		if !o.ResetsAt.IsZero() {
			sit["resets_at"] = stamp(o.ResetsAt)
		}
		row["sitting_out"] = sit
	}
	if !m.LastAnswer.IsZero() {
		row["last_answer"] = stamp(m.LastAnswer)
	}
	if f := m.LastFailure; f != nil {
		row["last_failure"] = map[string]any{
			"kind": f.Kind, "at": stamp(f.At),
			"said": textcut.Within(f.Said, MaxModelSaidBytes),
		}
	}
	if len(m.Parked) > 0 {
		named := slices.Clone(m.Parked)
		slices.SortFunc(named, func(a, b ParkedSeat) int {
			if c := a.Since.Compare(b.Since); c != 0 {
				return c
			}
			return strings.Compare(a.Seat.String(), b.Seat.String())
		})
		count, since := max(m.ParkedCount, len(named)), named[0].Since
		if !m.ParkedSince.IsZero() && m.ParkedSince.Before(since) {
			since = m.ParkedSince
		}
		named = named[:min(len(named), MaxModelParkedSeats)]
		parked := make([]map[string]any, len(named))
		for i, p := range named {
			parked[i] = map[string]any{"seat": p.Seat.String(), "since": stamp(p.Since)}
		}
		row["parked"] = parked
		row["parked_count"] = count
		row["parked_since"] = stamp(since)
	}
	if h := m.HostLogin; h != nil {
		login := map[string]any{"state": string(h.State), "checked_at": stamp(h.CheckedAt)}
		if h.Method != "" {
			login["method"] = textcut.Within(h.Method, MaxModelSaidBytes)
		}
		if h.Account != "" {
			login["account"] = textcut.Within(h.Account, MaxModelAccountBytes)
		}
		if h.Detail != "" {
			login["detail"] = textcut.Within(h.Detail, MaxModelSaidBytes)
		}
		row["host_login"] = login
	}
	return row
}

// meta is one runner cell on the wire.
func (c RunnerCell) meta() map[string]any {
	cell := map[string]any{"state": string(c.State), "checked_at": stamp(c.CheckedAt)}
	if c.Detail != "" {
		cell["detail"] = textcut.Within(c.Detail, MaxModelSaidBytes)
	}
	if c.Fix != "" {
		cell["fix"] = textcut.Within(c.Fix, MaxModelSaidBytes)
	}
	return cell
}

// stamp is an instant on the wire: RFC 3339 in UTC to the nanosecond, so a
// model's last answer and its last failure on two nodes still order the way
// they happened when they land in the same second.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// instant reads [stamp]'s spelling back, zero for anything else.
func instant(v any) time.Time {
	at, err := time.Parse(time.RFC3339Nano, stringFromMeta(v))
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
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
	}
	status.Draining, _ = raw["draining"].(bool)
	if at, err := time.Parse(time.RFC3339, stringFromMeta(raw["started_at"])); err == nil {
		status.StartedAt = at
	}
	for _, r := range listFromMeta(raw["mcp"]) {
		row, ok := r.(map[string]any)
		if !ok {
			continue
		}
		server := stringFromMeta(row["server"])
		if server == "" {
			// A row naming no server describes nothing a reader could
			// attribute, so it is dropped rather than drawn unnamed.
			continue
		}
		m := MCPServerStatus{
			Server:    server,
			Started:   intFromMeta(row["started"]),
			Failed:    intFromMeta(row["failed"]),
			Tools:     intFromMeta(row["tools"]),
			Error:     stringFromMeta(row["error"]),
			ErrorSeat: seatFromMeta(row["error_seat"]),
			Waiting:   intFromMeta(row["waiting"]),
			Wait:      stringFromMeta(row["wait"]),
			WaitSeat:  seatFromMeta(row["wait_seat"]),
			Unlisted:  stringsFromMeta(row["unlisted"]),
		}
		m.Shared, _ = row["shared"].(bool)
		status.MCP = append(status.MCP, m)
	}
	for _, r := range listFromMeta(raw["models"]) {
		row, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if m, ok := modelFromMeta(row); ok {
			status.Models = append(status.Models, m)
		}
	}
	if runners, ok := raw["runners"].(map[string]any); ok {
		status.Runners = &RunnerStatus{
			Direct:    cellFromMeta(runners["direct"]),
			Container: cellFromMeta(runners["container"]),
			E2B:       cellFromMeta(runners["e2b"]),
		}
	}
	return status, true
}

// modelFromMeta reads one model row, reporting false for a row naming no
// entry: a row a reader cannot attribute is dropped rather than drawn
// unnamed, as an MCP row is.
func modelFromMeta(row map[string]any) (ModelStatus, bool) {
	key := stringFromMeta(row["key"])
	if key == "" {
		return ModelStatus{}, false
	}
	m := ModelStatus{
		Key:        key,
		Plan:       stringFromMeta(row["plan"]),
		SignIn:     stringFromMeta(row["sign_in"]),
		LastAnswer: instant(row["last_answer"]),
	}
	m.Resolves, _ = row["resolves"].(bool)
	if sit, ok := row["sitting_out"].(map[string]any); ok {
		m.SittingOut = &ModelSitOut{
			Until:    instant(sit["until"]),
			ResetsAt: instant(sit["resets_at"]),
			Since:    instant(sit["since"]),
			Window:   stringFromMeta(sit["window"]),
			Said:     stringFromMeta(sit["said"]),
		}
	}
	if f, ok := row["last_failure"].(map[string]any); ok {
		m.LastFailure = &ModelFailure{
			Kind: stringFromMeta(f["kind"]),
			At:   instant(f["at"]),
			Said: stringFromMeta(f["said"]),
		}
	}
	m.ParkedCount = intFromMeta(row["parked_count"])
	m.ParkedSince = instant(row["parked_since"])
	for _, p := range listFromMeta(row["parked"]) {
		seat, ok := p.(map[string]any)
		if !ok {
			continue
		}
		// A row naming no seat is a park nobody can be told about, and
		// is dropped for [seatFromMeta]'s reason.
		if id := seatFromMeta(seat["seat"]); id != uuid.Nil {
			m.Parked = append(m.Parked, ParkedSeat{Seat: id, Since: instant(seat["since"])})
		}
	}
	if h, ok := row["host_login"].(map[string]any); ok {
		m.HostLogin = &HostLogin{
			State:     HostLoginState(stringFromMeta(h["state"])),
			Method:    stringFromMeta(h["method"]),
			Account:   stringFromMeta(h["account"]),
			Detail:    stringFromMeta(h["detail"]),
			CheckedAt: instant(h["checked_at"]),
		}
	}
	return m, true
}

// cellFromMeta reads one runner cell; a cell the peer did not send reads with
// an empty state, which [RunnerState.Valid] tells apart from every answer.
func cellFromMeta(v any) RunnerCell {
	cell, ok := v.(map[string]any)
	if !ok {
		return RunnerCell{}
	}
	return RunnerCell{
		State:     RunnerState(stringFromMeta(cell["state"])),
		Detail:    stringFromMeta(cell["detail"]),
		Fix:       stringFromMeta(cell["fix"]),
		CheckedAt: instant(cell["checked_at"]),
	}
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
