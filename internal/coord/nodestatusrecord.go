package coord

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/textcut"
)

// A node's STATUS RECORD: what it says of its MCP servers, models, runners and
// infra, read by the whole fleet (company-wide, current value only: a model's
// sit-out on one node decides another node's retry).
//
// Not on the presence lease, which is rewritten every beat and held to
// [MaxLeaseMetaBytes]; these rows grow with the configuration. A loop of its
// own writes them, each a key of its own (nodestatusrows.go), on change and
// at least every [ReconcileInterval]. The bucket's age is [StatusFreshness],
// so a dead node's rows age out with no sweep (ADR-0142). A record counts
// only at its writer's presence epoch ([LiveNodeStatuses], like
// [LiveMCPListings]), so an earlier incarnation is never drawn.

// NodeStatusRecord is one node's status record: the rows it reports of itself
// beside its presence, under the presence epoch it wrote them at.
//
// It carries no struct tags: the wire shape is [NodeStatusRecord.Rows] and
// [AssembleNodeStatusRecords], written by hand over the row encoders, so a key
// this build does not know is ignored rather than refused, for the reason
// [StatusFromMeta] gives. A listing fills only the parts its reader asked for
// ([NodeStatusRead]): MCP, Models and Growth are nil where not read.
type NodeStatusRecord struct {
	// Node is the node the record describes. A store files every row under
	// it and stamps it from the key, so a node can describe only itself.
	Node string

	// Epoch is the presence epoch the node held when it wrote its own row,
	// which a reader counts it at ([LiveNodeStatuses]) and every other row
	// only at ([AssembleNodeStatusRecords]).
	Epoch int64

	// At is when the node wrote it, on its own clock.
	At time.Time

	// MCP is what this node's MCP servers did when it last started them,
	// one row per configured server. Nil is "this node started none".
	MCP []MCPServerStatus

	// Models is what this node knows about each configured model entry,
	// one row per entry of its ACTIVE revision ([ModelStatus]). Nil is
	// "this node runs none".
	//
	// Each node's own: a model's sit-out, its last answer and its last
	// refusal are its node's (a node's process meets the vendor's limit,
	// and its memory keeps the window), and the Models page reading one
	// listing of the records answers for the fleet without asking each
	// node in turn. A node with no record counted is a column of its own
	// on that page, never a row of zeros.
	Models []ModelStatus

	// Runners is whether each executor cell can start a box on this node,
	// as its runner probe last found it. Nil is a node that has not probed
	// (one that runs no executor, or one still booting).
	Runners *RunnerStatus

	// Infra is what this node says of what it runs on ([NodeInfra]), read
	// once from its own file at boot. The zero value says nothing.
	Infra NodeInfra

	// ResumesBlocked is the runs of seats this node holds whose executor's
	// job finished and whose last resume here failed on a condition only an
	// operator clears ([ResumesBlocked]). Nil is a node holding no such run,
	// or one that does not say: one running no executor, or a build from
	// before it. ON THE RECORD, since a satellite holding the seat evaluates
	// no alarm; the count and the longest-waiting run, never how long it has
	// waited (the package doc's "A node's runs waiting for an operator").
	ResumesBlocked *ResumesBlocked

	// Growth is what this node's growth products met at their vendors that an
	// administrator must act on (internal/growth's Signals), read by every
	// node's platform notices (internal/notice). Nil is a node that met
	// nothing, or one that does not say. ON THE RECORD because only the node
	// whose process met the vendor knows, and its clock is its own.
	Growth *GrowthSignals
}

// GrowthSignals is a node's growth signals as its record carries them, each
// list at most [MaxGrowthSignals] rows.
type GrowthSignals struct {
	ClockSkews   []GrowthClockSkew
	Deprecations []GrowthDeprecation
}

// GrowthClockSkew is a vendor refusing this node's freshly signed token while
// its clock is OffsetSeconds behind the node's (negative where it is ahead).
type GrowthClockSkew struct {
	Product, Vendor string
	OffsetSeconds   int
	Since           time.Time
}

// GrowthDeprecation is a vendor saying the API version a product calls is
// deprecated, retiring on Retires (zero where it gave no date).
type GrowthDeprecation struct {
	Product, Version string
	Retires, Since   time.Time
}

// MaxGrowthSignals bounds each list of [GrowthSignals] on the wire, and
// MaxGrowthSignalBytes each name in it: internal/growth's own bounds. Two
// full lists store 15,230 bytes at their widest, inside the growth row's
// [MaxNodeStatusRowBytes]; 35 of each would not fit.
const (
	MaxGrowthSignals     = 32
	MaxGrowthSignalBytes = 64
)

// ResumesBlocked is what a node says of the runs of seats it holds whose
// executor's job finished and whose last resume there failed on a condition
// only an operator clears ([NodeStatusRecord.ResumesBlocked]):
// internal/sandbox's Coordinator.ResumesBlocked, as the record carries it.
type ResumesBlocked struct {
	// Runs is how many there are; at least one wherever the row says any.
	Runs int

	// Seat is the handle of the seat whose run has waited longest, bounded
	// to [iam.MaxLogin], the longest handle a chart may name; Kind the
	// condition its last resume failed on, one of internal/agent/builtin's
	// kinds, bounded to [MaxResumeBlockedKindBytes]; and Since when the first
	// such failure in a row was met, on the node's own clock.
	Seat  string
	Kind  string
	Since time.Time
}

// MaxResumeBlockedKindBytes bounds [ResumesBlocked.Kind] on the wire: 64
// bytes, past the longest kind internal/agent/builtin names (24), so a build
// that adds one travels whole and a value nothing names cannot grow the record.
const MaxResumeBlockedKindBytes = 64

// NodeStatusRecords is the fleet's status records, each node's filed as rows
// (nodestatusrows.go).
type NodeStatusRecords interface {
	// PutNodeStatusRow writes one row, replacing it, and refreshes its age,
	// UNLESS the key holds a row at a later epoch, which stands and is no
	// error: a node's keys are written by its incarnations, and an earlier
	// one's write still in flight never lands over a later one's. A row past
	// [MaxNodeStatusRowBytes] is refused as [ErrValueTooLarge].
	PutNodeStatusRow(ctx context.Context, row NodeStatusRow) error

	// RemoveNodeStatusRow removes one row where it was written at an epoch
	// no later than through, for [NodeStatusRecords.DeleteNodeStatus]'s
	// reason: a server or entry no longer configured, or growth signals
	// gone. A row that is not there is no error.
	RemoveNodeStatusRow(ctx context.Context, key NodeStatusRowKey, through int64) error

	// NodeStatusRecords is every node's record the store still holds,
	// ordered by node, each counted whole or not at all
	// ([AssembleNodeStatusRecords]), from its own row and the classes read
	// names, one certified pass each: an unreadable store is an error,
	// never a fleet that reports nothing. Readers count them through
	// [LiveNodeStatuses].
	NodeStatusRecords(ctx context.Context, read NodeStatusRead) ([]NodeStatusRecord, error)

	// DeleteNodeStatus removes every row of a node written at an epoch no
	// later than through, its own row first: a node that stops cleanly says
	// so rather than leaving its rows for the age to remove. A row that is
	// not there is no error, and neither is one written at a LATER epoch,
	// which is left as it stands.
	//
	// UP TO AN EPOCH, never whatever the key holds: presence epochs only
	// grow, so a row past through is a later incarnation's of the same
	// node id — one started before this one finished stopping — and removing
	// it would read that live node as not saying until its writer next
	// wrote. Through rather than at, so a stopping node also removes a row
	// whose write it saw fail that landed after all.
	DeleteNodeStatus(ctx context.Context, node string, through int64) error
}

// ErrNoNodeStatus is a status record a store cannot file: one naming no node,
// or no presence epoch it was written under.
var ErrNoNodeStatus = errors.New("coord: a node status record needs its node and its presence epoch")

// Check refuses a record a store cannot file ([ErrNoNodeStatus]).
func (r NodeStatusRecord) Check() error {
	if r.Node == "" || r.Epoch <= 0 {
		return fmt.Errorf("%w (node %q, epoch %d)", ErrNoNodeStatus, r.Node, r.Epoch)
	}
	return nil
}

// meta is the signals' row, each list bounded and each name fitted to
// [MaxGrowthSignalBytes] as stored, nil where they say nothing: absent reads
// as none.
func (g *GrowthSignals) meta() map[string]any {
	if g == nil || (len(g.ClockSkews) == 0 && len(g.Deprecations) == 0) {
		return nil
	}
	skews := make([]map[string]any, 0, min(len(g.ClockSkews), MaxGrowthSignals))
	for _, c := range g.ClockSkews[:min(len(g.ClockSkews), MaxGrowthSignals)] {
		skews = append(skews, map[string]any{"product": fitWithin(c.Product, MaxGrowthSignalBytes),
			"vendor": fitWithin(c.Vendor, MaxGrowthSignalBytes), "offset_seconds": c.OffsetSeconds,
			"since": stamp(c.Since)})
	}
	deprecations := make([]map[string]any, 0, min(len(g.Deprecations), MaxGrowthSignals))
	for _, d := range g.Deprecations[:min(len(g.Deprecations), MaxGrowthSignals)] {
		row := map[string]any{"product": fitWithin(d.Product, MaxGrowthSignalBytes),
			"version": fitWithin(d.Version, MaxGrowthSignalBytes), "since": stamp(d.Since)}
		if !d.Retires.IsZero() {
			row["retires"] = stamp(d.Retires)
		}
		deprecations = append(deprecations, row)
	}
	return map[string]any{"clock_skews": skews, "deprecations": deprecations}
}

// growthFromMeta reads a node's growth row, nil where it carries none: a
// signal naming no product, or a deprecation naming no version, is dropped,
// and each list is read to its bound.
func growthFromMeta(row map[string]any) *GrowthSignals {
	g := &GrowthSignals{}
	for _, item := range listFromMeta(row["clock_skews"]) {
		m, ok := item.(map[string]any)
		product := textcut.Within(stringFromMeta(m["product"]), MaxGrowthSignalBytes)
		if !ok || product == "" || len(g.ClockSkews) == MaxGrowthSignals {
			continue
		}
		g.ClockSkews = append(g.ClockSkews, GrowthClockSkew{Product: product,
			Vendor:        textcut.Within(stringFromMeta(m["vendor"]), MaxGrowthSignalBytes),
			OffsetSeconds: intFromMeta(m["offset_seconds"]), Since: instant(m["since"])})
	}
	for _, item := range listFromMeta(row["deprecations"]) {
		m, ok := item.(map[string]any)
		product := textcut.Within(stringFromMeta(m["product"]), MaxGrowthSignalBytes)
		version := textcut.Within(stringFromMeta(m["version"]), MaxGrowthSignalBytes)
		if !ok || product == "" || version == "" || len(g.Deprecations) == MaxGrowthSignals {
			continue
		}
		g.Deprecations = append(g.Deprecations, GrowthDeprecation{Product: product, Version: version,
			Retires: instant(m["retires"]), Since: instant(m["since"])})
	}
	if len(g.ClockSkews) == 0 && len(g.Deprecations) == 0 {
		return nil
	}
	return g
}

// resumesBlockedFromMeta reads a record's runs waiting for an operator, nil
// where it names none: no row, one that is not a map, or one counting no run,
// which no build writes.
func resumesBlockedFromMeta(v any) *ResumesBlocked {
	row, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	runs := intFromMeta(row["runs"])
	if runs <= 0 {
		return nil
	}
	return &ResumesBlocked{Runs: runs, Seat: stringFromMeta(row["seat"]), Kind: stringFromMeta(row["kind"]),
		Since: instant(row["since"])}
}

// SortNodeStatusRecords orders records by node, the order every listing of
// them answers in.
func SortNodeStatusRecords(records []NodeStatusRecord) {
	slices.SortFunc(records, func(a, b NodeStatusRecord) int { return cmp.Compare(a.Node, b.Node) })
}

// LiveNodeStatuses is the records readers count, by node: each one whose node
// holds its presence now AT THE EPOCH the record was written under, presence
// being [Backend.ListLive] of [ClassNode].
//
// THE ONE FILTER, for every reader, as [LiveMCPListings] is for the listings:
// a node that died, drained or restarted stops counting the moment its
// presence lapses or moves to a new epoch, and a reader that filtered on the
// node alone would draw a restarted node's previous incarnation, its sit-outs
// and its failures, until the new one wrote over it. A live node with no
// record counted is absent from the answer: a node not saying, never one with
// nothing to say.
func LiveNodeStatuses(records []NodeStatusRecord, presence []Lease) map[string]NodeStatusRecord {
	live := make(map[string]int64, len(presence))
	for _, lease := range presence {
		if node, ok := NodeID(lease.Resource); ok {
			live[node] = lease.Epoch
		}
	}
	out := make(map[string]NodeStatusRecord, len(records))
	for _, r := range records {
		if epoch, ok := live[r.Node]; ok && epoch == r.Epoch {
			out[r.Node] = r
		}
	}
	return out
}

// LiveNodeStatusesKept is [LiveNodeStatuses] for a reader that judges the fleet
// over and over, an alarm evaluation above all: last is what it counted on its
// previous read, and a live node whose record is not counted now keeps the
// record last holds for it while its presence stands at the epoch that record
// was written under. A live node whose record is not counted is a node not
// saying, never one whose rows went away; a node whose presence is gone or
// moved to a new epoch keeps nothing (the package doc's "A live node not
// saying keeps what it said").
func LiveNodeStatusesKept(records []NodeStatusRecord, presence []Lease,
	last map[string]NodeStatusRecord) map[string]NodeStatusRecord {
	out := LiveNodeStatuses(records, presence)
	for _, lease := range presence {
		node, ok := NodeID(lease.Resource)
		if !ok {
			continue
		}
		if _, counted := out[node]; counted {
			continue
		}
		if kept, ok := last[node]; ok && kept.Epoch == lease.Epoch {
			out[node] = kept
		}
	}
	return out
}

// ModelStatus is one configured model entry, as ONE node holds it when it
// writes its status record.
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
	// share one sign-in", which is all a reader of the record needs, and the
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

	// Limits are the windows of the entry's sign-in as this node's calls last
	// heard the vendor report them, at most [MaxModelLimits].
	Limits []ModelLimit

	// Homes are the seat jobs whose last piece of work on an LLM fleet this
	// node gave the entry, the [MaxModelFleetHomes] most recent.
	Homes []FleetHome
}

// FleetHome is one seat job's home on an LLM fleet: the fleet, the seat's
// handle (or a helper's name), the job, and when the entry took its work.
type FleetHome struct {
	Fleet, Subject, Job string
	At                  time.Time
}

// MaxModelFleetHomes bounds the homes a model row names: 32, the most recently
// taken first. A home only keeps a seat job on its member within the fleet's
// threshold, so one left off reads as none, and the row does not grow with
// the company.
const MaxModelFleetHomes = 32

// MaxFleetHomeNameBytes bounds a home's fleet, subject and job on the wire:
// 64, past a fleet key (63) and a seat's handle.
const MaxFleetHomeNameBytes = 64

// ModelLimit is one of a sign-in's limit windows on the wire, in harness's
// words (llm.LimitReading), with when a call last heard it.
type ModelLimit struct {
	Window         string
	Hours          int
	Model          string
	State          string
	Used           *float64
	ResetsAt, Seen time.Time
}

// MaxModelLimits bounds the windows a model row carries: a plan has a handful
// (Claude Code names four), and a profile's map is what names them.
const MaxModelLimits = 8

// MaxModelLimitModelBytes bounds a window's model family ("opus") on the wire.
const MaxModelLimitModelBytes = 32

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
	Direct, Container RunnerCell
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

// ModelPlanChars is how much of a sign-in identity a status record carries: twelve
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
// record rewritten every [ReconcileInterval]; the whole sentence is in the node's
// own `harness_limit_reported` line.
const MaxModelSaidBytes = 240

// MaxModelParkedSeats bounds the parked seats a model row names on the wire:
// EIGHT, the longest-waiting first. A row's size must not grow with the
// company: every seat of a hundred-seat company parked on one plan would put
// a hundred ids on a record rewritten every [ReconcileInterval]. Eight is what a
// person reads before "and N more", and the count and the earliest instant
// travel whole beside them.
const MaxModelParkedSeats = 8

// MaxModelAccountBytes bounds a host login's account (an email or an org
// name): 120 bytes, past any real address, so a harness that printed something
// else there cannot grow the record.
const MaxModelAccountBytes = 120

// MCPServerStatus is one configured MCP server, as ONE node runs it when it
// writes its status record.
//
// AGGREGATED PER SERVER, NOT PER CHILD. A per-role server is a template with
// one child per seat this node holds, and a row per child would put a
// company's seat count times its per-role servers on a record every node
// rewrites — to answer a question ("is the jira server working on this
// node?") the counts answer as well.
type MCPServerStatus struct {
	// Server is the config's own name for it — `mcp_servers[].name`, never
	// a per-seat instance name, since the row stands for every instance.
	Server string

	// Shared is true for the one company-wide child, false for a per-role
	// template.
	Shared bool

	// Started is how many instances are serving: started, listed their
	// tools, and still up as of this record. A child that died since is not
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

	// Error is ONE failed instance's reason, and ErrorSeat its seat's agent
	// id (uuid.Nil for a shared server). One, for the size argument above:
	// the instance DownSince dates, the longest down (ties to the lower agent
	// id), so reason, seat, class and outage start are one instance's facts
	// and equal failures publish the same row.
	//
	// The id, not the handle: a peer reads this row against its OWN org and
	// resolves the id (derived from the handle, ADR-0013) itself.
	Error     string
	ErrorSeat uuid.UUID

	// ErrorClass is why that instance failed, in internal/mcp's FailureClass
	// words (kept a string here, as a record carries it, for a reader to
	// judge), "" where no class fits.
	ErrorClass string

	// DownSince is when the longest-failed instance on this node stopped
	// serving, or first failed to start; zero when none has failed. What an
	// alarm measures a server's outage against, and the same instant in
	// every record written during one outage, so a record carrying the same
	// facts is the same bytes and its writer does not write it again.
	DownSince time.Time

	// Restarts is how many times this node brought the server's instances
	// back since each was started from its spec, summed over them: a count
	// that climbs while the server stays up is a server that flaps.
	Restarts int

	// Wait is ONE waiting instance's sentence (the variables it waits for
	// and the integration that mints them), and WaitSeat its seat: the one
	// WaitingSince dates, chosen and bounded as Error and ErrorSeat are.
	Wait     string
	WaitSeat uuid.UUID

	// WaitingSince is when the longest-waiting instance on this node was
	// first held back for its credential; zero when none waits. What an alarm
	// measures a wait against, kept across the passes that hold it back
	// again, as DownSince is across retries.
	WaitingSince time.Time

	// Unlisted are the entry's tool_annotations keys that NO instance this
	// node started lists a tool for, sorted. Such a key overrides nothing
	// here. Only the row can say it: one instance's listing is not the
	// server's, because a per-role child serves what its seat's credentials
	// reach, and a key one seat's child lacks may be exactly right for the
	// next one's. Across nodes the same intersection is the status answer's.
	//
	// Whole keys: one cut short would name a tool nobody wrote. All of them
	// but past what the row holds ([MaxNodeStatusRowBytes]), then the first
	// by sort, UnlistedCount saying how many in all (zero where whole), so
	// an intersection across nodes under-reports, never names one listed.
	Unlisted      []string
	UnlistedCount int

	// Refused are tools an instance on this node lists that are not offered
	// for their name: one no model can be handed (characters, length) or one
	// a built-in holds. Shadowed are tools another server's tool of the same
	// name is offered in place of, each with that server's configured name.
	// Each over every instance, once per tool and reason (or server), sorted
	// by tool, at most [MaxMCPToolNotes] of them; RefusedCount and
	// ShadowedCount are how many there are in all, the ones listed included.
	//
	// BOUNDED, unlike Unlisted: these are bounded by what the server lists,
	// not by the entry, and a server with a long prefix can refuse every
	// tool it has. The count still says how many.
	Refused       []MCPToolRefusal
	RefusedCount  int
	Shadowed      []MCPToolShadow
	ShadowedCount int
}

// MCPToolRefusal is one tool not offered for its name, by the name it would
// have been filed under, and the reason (internal/mcp's NameRefusal: kept a
// string here, as a record carries it, for a reader to judge).
type MCPToolRefusal struct {
	Tool   string
	Reason string
}

// MCPToolShadow is one tool another server's tool of the same name is offered
// in place of, and that server's configured name.
type MCPToolShadow struct {
	Tool string
	By   string
}

// MaxMCPToolNotes bounds [MCPServerStatus.Refused] and
// [MCPServerStatus.Shadowed] on the wire: EIGHT, the first by tool name, as a
// model row names its parked seats ([MaxModelParkedSeats]). What a person
// reads before "and N more", and what a record rewritten every
// [ReconcileInterval] can carry for each server however many tools it
// refuses; the counts travel whole beside them.
const MaxMCPToolNotes = 8

// MaxMCPToolNameBytes bounds a tool's name on the wire: 128 bytes, the
// longest name the MCP specification says a server's tool should have, so a
// name any conforming server lists travels whole and one that is not cannot
// grow the record.
const MaxMCPToolNameBytes = 128

// MaxMCPErrorBytes bounds [MCPServerStatus.Error] and [MCPServerStatus.Wait]
// on the wire.
//
// A start failure's text is a handshake error or a child's last stderr line,
// useful at its head ("command not found", "401"). The engine's own sentence
// for a held-back server is useful whole: the variable, then each remedy, the
// last naming the key to declare optional. Those run about 300 bytes (the
// tools guide's `NPM_CONFIG_REGISTRY` case) to 380 (a per-role child awaiting
// its token), which 240 cut short; 512 carries both. Only a failed or
// waiting row has text, so a healthy row pays nothing; the full text is in the
// node's log (`mcp_server_failed`, `mcp_server_credential_missing`).
const MaxMCPErrorBytes = 512

// meta is one MCP row on the wire, filed under its server's name. Every
// optional part is OMITTED when unset, so a healthy server's row is its
// counts.
func (m MCPServerStatus) meta() map[string]any {
	row := map[string]any{
		"shared":  m.Shared,
		"started": m.Started,
		"failed":  m.Failed,
		"tools":   m.Tools,
	}
	if m.Error != "" {
		row["error"] = fitWithin(m.Error, MaxMCPErrorBytes)
	}
	if m.ErrorSeat != uuid.Nil {
		row["error_seat"] = m.ErrorSeat.String()
	}
	// OMITTED WHEN UNSET, which is every healthy row: absent reads
	// as no class, no outage and no restart.
	if m.ErrorClass != "" {
		row["error_class"] = fitWithin(m.ErrorClass, MaxMCPToolNameBytes)
	}
	if !m.DownSince.IsZero() {
		row["down_since"] = m.DownSince.UTC().Format(time.RFC3339)
	}
	if m.Restarts > 0 {
		row["restarts"] = m.Restarts
	}
	// OMITTED WHEN NOTHING WAITS, which is every row but a
	// seat's during provisioning: absent reads as zero.
	if m.Waiting > 0 {
		row["waiting"] = m.Waiting
	}
	if m.Wait != "" {
		row["wait"] = fitWithin(m.Wait, MaxMCPErrorBytes)
	}
	if m.WaitSeat != uuid.Nil {
		row["wait_seat"] = m.WaitSeat.String()
	}
	if !m.WaitingSince.IsZero() {
		row["waiting_since"] = m.WaitingSince.UTC().Format(time.RFC3339)
	}
	// OMITTED WHEN NOTHING IS REFUSED OR SHADOWED, which is every
	// row of a company whose names are in order.
	if m.RefusedCount > 0 {
		notes := make([]map[string]any, 0, min(len(m.Refused), MaxMCPToolNotes))
		for _, r := range m.Refused[:min(len(m.Refused), MaxMCPToolNotes)] {
			notes = append(notes, map[string]any{
				"tool":   fitWithin(r.Tool, MaxMCPToolNameBytes),
				"reason": fitWithin(r.Reason, MaxMCPToolNameBytes),
			})
		}
		row["refused"], row["refused_count"] = notes, m.RefusedCount
	}
	if m.ShadowedCount > 0 {
		notes := make([]map[string]any, 0, min(len(m.Shadowed), MaxMCPToolNotes))
		for _, s := range m.Shadowed[:min(len(m.Shadowed), MaxMCPToolNotes)] {
			notes = append(notes, map[string]any{
				"tool": fitWithin(s.Tool, MaxMCPToolNameBytes),
				"by":   fitWithin(s.By, MaxMCPToolNameBytes),
			})
		}
		row["shadowed"], row["shadowed_count"] = notes, m.ShadowedCount
	}
	if len(m.Unlisted) > 0 {
		fitUnlisted(row, m.Unlisted)
	}
	return row
}

// fitUnlisted sets a row's unlisted keys LAST, after every other part: each
// whole, sorted, as many as fit [MaxNodeStatusRowBytes] beside the rest with
// the epoch at its widest, and their count where not all do.
func fitUnlisted(row map[string]any, keys []string) {
	keys = slices.Sorted(slices.Values(keys))
	row["epoch"], row["unlisted"], row["unlisted_count"] = int64(math.MaxInt64), []string{}, len(keys)
	size, kept := len(mustMarshal(row)), 0
	for _, key := range keys {
		grows := jsonTextBytes(key) + len(`""`) + min(kept, 1)
		if size+grows > MaxNodeStatusRowBytes {
			break
		}
		size, kept = size+grows, kept+1
	}
	delete(row, "epoch")
	row["unlisted"] = keys[:kept]
	if kept == len(keys) {
		delete(row, "unlisted_count")
	}
}

// mustMarshal is a row's JSON, which a map of strings, numbers and lists
// always has.
func mustMarshal(row map[string]any) []byte {
	raw, _ := json.Marshal(row)
	return raw
}

// meta is one model row on the wire, filed under its entry's key. Every
// optional part is OMITTED when unset, so a healthy entry's row is its plan
// and its route.
func (m ModelStatus) meta() map[string]any {
	row := map[string]any{"sign_in": fitWithin(m.SignIn, MaxModelWordBytes), "resolves": m.Resolves}
	if m.Plan != "" {
		row["plan"] = fitBytes(m.Plan, ModelPlanChars)
	}
	if o := m.SittingOut; o != nil {
		sit := map[string]any{
			"until":  stamp(o.Until),
			"since":  stamp(o.Since),
			"window": fitWithin(o.Window, MaxModelWordBytes),
			"said":   fitWithin(o.Said, MaxModelSaidBytes),
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
			"kind": fitWithin(f.Kind, MaxModelWordBytes), "at": stamp(f.At),
			"said": fitWithin(f.Said, MaxModelSaidBytes),
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
		login := map[string]any{"state": fitWithin(string(h.State), MaxModelWordBytes),
			"checked_at": stamp(h.CheckedAt)}
		if h.Method != "" {
			login["method"] = fitWithin(h.Method, MaxModelSaidBytes)
		}
		if h.Account != "" {
			login["account"] = fitWithin(h.Account, MaxModelAccountBytes)
		}
		if h.Detail != "" {
			login["detail"] = fitWithin(h.Detail, MaxModelSaidBytes)
		}
		row["host_login"] = login
	}
	if len(m.Limits) > 0 {
		limits := make([]map[string]any, 0, min(len(m.Limits), MaxModelLimits))
		for _, l := range m.Limits[:min(len(m.Limits), MaxModelLimits)] {
			limits = append(limits, l.meta())
		}
		row["limits"] = limits
	}
	if len(m.Homes) > 0 {
		named := slices.Clone(m.Homes)
		slices.SortStableFunc(named, func(a, b FleetHome) int { return b.At.Compare(a.At) })
		homes := make([]map[string]any, 0, min(len(named), MaxModelFleetHomes))
		for _, h := range named[:min(len(named), MaxModelFleetHomes)] {
			homes = append(homes, map[string]any{
				"fleet":   textcut.Bytes(h.Fleet, MaxFleetHomeNameBytes),
				"subject": textcut.Bytes(h.Subject, MaxFleetHomeNameBytes),
				"job":     textcut.Bytes(h.Job, MaxFleetHomeNameBytes),
				"at":      stamp(h.At),
			})
		}
		row["homes"] = homes
	}
	return row
}

// meta is one limit window on the wire, its unsaid parts omitted.
func (l ModelLimit) meta() map[string]any {
	out := map[string]any{"window": fitWithin(l.Window, MaxModelWordBytes),
		"state": fitWithin(l.State, MaxModelWordBytes), "seen": stamp(l.Seen)}
	if l.Hours > 0 {
		out["hours"] = l.Hours
	}
	if l.Model != "" {
		out["model"] = fitBytes(l.Model, MaxModelLimitModelBytes)
	}
	if l.Used != nil {
		out["used"] = *l.Used
	}
	if !l.ResetsAt.IsZero() {
		out["resets_at"] = stamp(l.ResetsAt)
	}
	return out
}

// meta is one runner cell on the wire.
func (c RunnerCell) meta() map[string]any {
	cell := map[string]any{"state": fitWithin(string(c.State), MaxModelWordBytes),
		"checked_at": stamp(c.CheckedAt)}
	if c.Detail != "" {
		cell["detail"] = fitWithin(c.Detail, MaxModelSaidBytes)
	}
	if c.Fix != "" {
		cell["fix"] = fitWithin(c.Fix, MaxModelSaidBytes)
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

// mcpFromMeta reads one MCP row filed under server.
func mcpFromMeta(server string, row map[string]any) MCPServerStatus {
	m := MCPServerStatus{
		Server:       server,
		Started:      intFromMeta(row["started"]),
		Failed:       intFromMeta(row["failed"]),
		Tools:        intFromMeta(row["tools"]),
		Error:        stringFromMeta(row["error"]),
		ErrorSeat:    seatFromMeta(row["error_seat"]),
		ErrorClass:   stringFromMeta(row["error_class"]),
		DownSince:    instant(row["down_since"]),
		Restarts:     max(intFromMeta(row["restarts"]), 0),
		Waiting:      intFromMeta(row["waiting"]),
		Wait:         stringFromMeta(row["wait"]),
		WaitSeat:     seatFromMeta(row["wait_seat"]),
		Unlisted:     stringsFromMeta(row["unlisted"]),
		WaitingSince: instant(row["waiting_since"]),
	}
	m.Shared, _ = row["shared"].(bool)
	for _, n := range listFromMeta(row["refused"]) {
		if note, ok := n.(map[string]any); ok && stringFromMeta(note["tool"]) != "" {
			m.Refused = append(m.Refused, MCPToolRefusal{
				Tool: stringFromMeta(note["tool"]), Reason: stringFromMeta(note["reason"]),
			})
		}
	}
	for _, n := range listFromMeta(row["shadowed"]) {
		if note, ok := n.(map[string]any); ok && stringFromMeta(note["tool"]) != "" {
			m.Shadowed = append(m.Shadowed, MCPToolShadow{
				Tool: stringFromMeta(note["tool"]), By: stringFromMeta(note["by"]),
			})
		}
	}
	// A COUNT IS AT LEAST WHAT IT LISTS, whatever a peer wrote.
	m.RefusedCount = max(intFromMeta(row["refused_count"]), len(m.Refused))
	m.ShadowedCount = max(intFromMeta(row["shadowed_count"]), len(m.Shadowed))
	if count := intFromMeta(row["unlisted_count"]); count > len(m.Unlisted) {
		m.UnlistedCount = count
	}
	return m
}

// modelFromMeta reads one model row filed under key.
func modelFromMeta(key string, row map[string]any) ModelStatus {
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
	for _, v := range listFromMeta(row["limits"]) {
		if l, ok := limitFromMeta(v); ok && len(m.Limits) < MaxModelLimits {
			m.Limits = append(m.Limits, l)
		}
	}
	for _, v := range listFromMeta(row["homes"]) {
		h, ok := v.(map[string]any)
		if !ok || len(m.Homes) == MaxModelFleetHomes {
			continue
		}
		// A home naming no fleet or subject steers no pick, and is dropped.
		home := FleetHome{Fleet: stringFromMeta(h["fleet"]), Subject: stringFromMeta(h["subject"]),
			Job: stringFromMeta(h["job"]), At: instant(h["at"])}
		if home.Fleet != "" && home.Subject != "" {
			m.Homes = append(m.Homes, home)
		}
	}
	return m
}

// limitFromMeta reads one limit window, false for one naming no window or
// state, which a reader could not draw.
func limitFromMeta(v any) (ModelLimit, bool) {
	row, ok := v.(map[string]any)
	if !ok {
		return ModelLimit{}, false
	}
	l := ModelLimit{Window: stringFromMeta(row["window"]), Hours: intFromMeta(row["hours"]),
		Model: stringFromMeta(row["model"]), State: stringFromMeta(row["state"]),
		ResetsAt: instant(row["resets_at"]), Seen: instant(row["seen"])}
	if l.Window == "" || l.State == "" {
		return ModelLimit{}, false
	}
	if used, ok := floatFromMeta(row["used"]); ok {
		l.Used = &used
	}
	return l, true
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
