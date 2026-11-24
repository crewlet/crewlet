// What each configured model is doing, as Settings › Models & Fleets reads it.

package queries

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/modelcatalog"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ModelHealth is ONE reading of a model's condition, decided here from every
// node's report and this node's sign-in, so a screen never re-derives it. The
// first that holds, in this order, is the answer.
type ModelHealth string

const (
	// HealthSittingOut is the reading: a live node sits the entry out on a vendor's limit
	// whose window has not ended.
	HealthSittingOut ModelHealth = "sitting_out"
	// HealthNoSignIn is the reading: nothing signs it in on the answering node: no route,
	// or a credential that does not resolve.
	HealthNoSignIn ModelHealth = "no_sign_in"
	// HealthRejected is the reading: the vendor refused its sign-in more recently than it
	// last answered, or its credential's computed expiry has passed.
	HealthRejected ModelHealth = "rejected"
	// HealthExpiring is the reading: its credential expires within
	// [statelog.CredentialWarningDays].
	HealthExpiring ModelHealth = "expiring"
	// HealthUntested is the reading: no answer, no failure and no call in the usage
	// window: nothing has said whether it works.
	HealthUntested ModelHealth = "untested"
	// HealthReady is the reading: none of the above.
	HealthReady ModelHealth = "ready"
)

// ModelHealths is every reading, for the gate holding the dashboard's copy.
var ModelHealths = []ModelHealth{HealthSittingOut, HealthNoSignIn, HealthRejected,
	HealthExpiring, HealthUntested, HealthReady}

// Valid reports whether h is a reading this build sends.
func (h ModelHealth) Valid() bool { return slices.Contains(ModelHealths, h) }

// LocalModel is what THIS node resolved for one entry of its active revision:
// how the entry signs in here, the variable that carries its credential and
// where that variable resolved from, and the facts derived from the value it
// resolved. The engine answers it from the providers it built
// ([ModelSource]); nothing in it is published or stored.
type LocalModel struct {
	Key string
	// Route is how the entry signs in on this node: token, credentials,
	// api-key, env, inherit-env, environment, device or none.
	Route string
	// Variable is the variable its credential is read from, "" for a
	// route that reads none (credentials, env, none). Where it resolved
	// from is the fleet's store when a row holds it, which the resolution
	// reads first, and the engine's environment otherwise.
	Variable string
	// Resolves is whether its credential resolved on this node.
	Resolves bool
	// Hint is the resolved credential's hint (harness's
	// CredentialHint), "" for none.
	Hint string
	// TokenLifetime is how long the harness's headless token lasts, zero when
	// unknown or not a token.
	TokenLifetime time.Duration
	// StoredAt and StoredBy are when and by whom the store wrote the value
	// this node resolves Variable from, as its snapshot holds it; zero when
	// the value came from elsewhere. The node's own expiry alarm judges the
	// same instant, so the page and the alarm above it agree.
	StoredAt time.Time
	StoredBy string
}

// ModelSource is the engine's half of the model questions: what this node
// resolved for each entry. Its alarms are the node's latest evaluation
// ([Sources.Alarms]).
type ModelSource interface {
	LocalModels() []LocalModel
}

// DeviceSignIns lists the fleet's device sign-ins in progress or lingering
// after their end.
type DeviceSignIns interface {
	ListDeviceSignIns(ctx context.Context) ([]coord.DeviceSignIn, error)
}

// ModelChecks reads each model entry's latest check, false where none still
// answers ([coord.ModelCheck.Lapsed]).
type ModelChecks interface {
	ModelCheck(ctx context.Context, entry string) (coord.ModelCheck, bool, error)
}

// ModelCatalogue is the model catalogue a model step suggests from
// (internal/modelcatalog).
type ModelCatalogue interface {
	Provider(ctx context.Context, provider string) (modelcatalog.Answer, error)
}

// ModelUsageDays is the usage window `llm_entries` reports per entry: a week,
// the span a person looking at a model's page asks "is it used" over.
const ModelUsageDays = 7

// llmAlarmKinds are the model alarms (statelog's per-entry kinds about one
// model entry), the ones `llm_entries` carries: read off the table, so a kind
// it adds is carried, and held to the page's union.
var llmAlarmKinds = statelog.PerModelKinds()

// ReadingAdmin and ReadingMember are which reading of a two-reading answer an
// answer is, named on the answer as `reading`: `llm_entries` ([Sources.llmEntries])
// and Settings › MCP & tools' ([MCPReading]).
const (
	ReadingAdmin  = "admin"
	ReadingMember = "member"
)

// ModelEntriesAnswer is `llm_entries`, the admin's reading.
type ModelEntriesAnswer struct {
	Reading string        `json:"reading"`
	Node    string        `json:"node"`
	Nodes   []ModelNode   `json:"nodes"`
	Entries []ModelEntry  `json:"entries"`
	Alarms  []ModelsAlarm `json:"alarms"`
	// ExecutorHarnesses is every harness this node's build runs a seat's
	// executor on, in the profiles' order ([config.ExecutorHarnesses]): the
	// page reads it here rather than compiling a copy, so a runner a build
	// adds reaches it with no dashboard rebuild.
	ExecutorHarnesses []config.Harness `json:"executor_harnesses"`
}

// ModelNode is one live node, and whether a status record of its live presence
// is counted, carrying its models.
type ModelNode struct {
	ID       string `json:"id"`
	Reported bool   `json:"reported"`
}

// ModelsAlarm is one of the five model alarms as this node last evaluated it.
type ModelsAlarm = statelog.Alarm

// ModelEntry is one entry, the admin's reading.
type ModelEntry struct {
	Key          string            `json:"key"`
	Harness      string            `json:"harness"`
	Model        string            `json:"model"`
	Health       ModelHealth       `json:"health"`
	Plan         string            `json:"plan"`
	SignIn       ModelSignIn       `json:"sign_in"`
	SittingOut   *ModelSittingOut  `json:"sitting_out"`
	LastSuccess  *time.Time        `json:"last_success"`
	LastFailure  *ModelLastFailure `json:"last_failure"`
	Waiting      *ModelWaiting     `json:"waiting"`
	Usage        ModelUsage        `json:"usage"`
	UsedBy       []ModelUse        `json:"used_by"`
	DeviceSignIn *ModelDevice      `json:"device_sign_in"`
	Limits       []ModelLimit      `json:"limits"`
	// HelperProblem is why no helper (Ask Crewlet, the Advisor) can run on
	// the entry, in a person's words, or null where one can: the one rule
	// ([config.Company.HelperModelProblem]), so a model picker never states
	// its own.
	HelperProblem *string `json:"helper_problem"`
}

// ModelSignIn is how an entry signs in, the admin's reading.
type ModelSignIn struct {
	Route       string           `json:"route"`
	Mode        string           `json:"mode"`
	Variable    *string          `json:"variable"`
	Source      string           `json:"source"`
	Resolves    bool             `json:"resolves"`
	Hint        *string          `json:"hint"`
	StoredAt    *time.Time       `json:"stored_at"`
	StoredBy    *string          `json:"stored_by"`
	ExpiresAt   *time.Time       `json:"expires_at"`
	ExpiryBasis *string          `json:"expiry_basis"`
	HostLogins  []ModelHostLogin `json:"host_logins"`
}

// ModelHostLogin is one node's harness login for an entry signing in with
// credential files.
type ModelHostLogin struct {
	Node      string     `json:"node"`
	State     string     `json:"state"`
	Method    *string    `json:"method"`
	Account   *string    `json:"account"`
	Detail    *string    `json:"detail,omitempty"`
	CheckedAt *time.Time `json:"checked_at"`
	// Command is what an operator runs on that node to sign it in.
	Command string `json:"command"`
}

// ModelSittingOut is the latest sit-out window across live nodes, and every
// node that sits it out.
type ModelSittingOut struct {
	Until    time.Time  `json:"until"`
	ResetsAt *time.Time `json:"resets_at"`
	Window   string     `json:"window"`
	Said     string     `json:"said,omitempty"`
	Since    time.Time  `json:"since"`
	Nodes    []string   `json:"nodes"`
}

// ModelLimit is one window of the entry's sign-in's vendor limit, as the
// newest report across live nodes left it ([modelLimits]).
type ModelLimit struct {
	// Window is session, weekly, model or unknown (llm.LimitWindow); Hours
	// its length as the vendor names it, 0 where unsaid; Model the model
	// family a model window is of.
	Window string `json:"window"`
	Hours  int    `json:"hours"`
	Model  string `json:"model,omitempty"`
	// State is ok, warning or reached (llm.LimitState), read at now
	// ([llm.LimitReading.At]).
	State string `json:"state"`
	// Used is the fraction used, null where the vendor did not say or the
	// window has turned over since.
	Used     *float64   `json:"used"`
	ResetsAt *time.Time `json:"resets_at"`
	// Until is when the entry is tried again, where it sits this window out.
	Until *time.Time `json:"until"`
	// Seen is when a call last heard it, and Stale whether that is past
	// [llm.LimitStaleAfter].
	Seen  time.Time `json:"seen"`
	Stale bool      `json:"stale"`
}

// ModelLastFailure is the latest failure across live nodes.
type ModelLastFailure struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	Said string    `json:"said,omitempty"`
	Node string    `json:"node"`
}

// ModelWaiting is the seats parked on a chain naming the entry, by handle,
// and since when the first was.
type ModelWaiting struct {
	Seats []string  `json:"seats"`
	Since time.Time `json:"since"`
}

// ModelUsage is the entry's use over [ModelUsageDays], in calls and tokens.
//
// NO MONEY. The dashboard renders tokens and never a price (its rule 19), and
// a price is quoted by one backend and not the rest, so a currency figure
// here would be a number the one screen that reads this answer may not show,
// and partial where it could. The spend rollup ([tokens.Bucket]) keeps its
// own cost fields for the readers that ask for them.
type ModelUsage struct {
	Days        int `json:"days"`
	Calls       int `json:"calls"`
	TotalTokens int `json:"total_tokens"`
}

// ModelUse is one job the entry runs, or is a fallback for: a seat's, by
// handle and job, Ask Crewlet's chat, or the Advisor's findings. Position is the
// entry's place in the resolved chain, 0 for the model that runs it.
type ModelUse struct {
	Seat      string `json:"seat,omitempty"`
	Job       string `json:"job,omitempty"`
	Assistant string `json:"assistant,omitempty"`
	// Advisor is "findings" where the Advisor explains its findings with the
	// entry (`advisor.model`).
	Advisor  string `json:"advisor,omitempty"`
	Position int    `json:"position"`
	// Written is whether the seat wrote the field that resolved the job;
	// Source is the level that decided it (phase.Source). Absent for the
	// assistant, whose fields are always written.
	Written *bool  `json:"written,omitempty"`
	Source  string `json:"source,omitempty"`
	// Fleet is the LLM fleet the use goes through, the entry one of its
	// members; absent for a pinned use.
	Fleet string `json:"fleet,omitempty"`
}

// ModelDevice is a device sign-in, as the routes and the query answer it.
type ModelDevice struct {
	ID        uuid.UUID     `json:"id"`
	Entry     string        `json:"entry"`
	Node      string        `json:"node"`
	URL       string        `json:"url,omitempty"`
	Code      string        `json:"code,omitempty"`
	Status    string        `json:"status"`
	Detail    string        `json:"detail,omitempty"`
	StartedAt time.Time     `json:"started_at"`
	ExpiresAt time.Time     `json:"expires_at"`
	Reload    *DeviceReload `json:"reload,omitempty"`
}

// DeviceReload is the re-activation an approved sign-in made.
type DeviceReload struct {
	RevisionID string `json:"revision_id"`
	Epoch      uint64 `json:"epoch"`
}

// DeviceOf is a device sign-in record as the answers render it.
func DeviceOf(d coord.DeviceSignIn) ModelDevice {
	out := ModelDevice{ID: d.ID, Entry: d.Entry, Node: d.Node, URL: d.URL, Code: d.Code,
		Status: string(d.Status), Detail: d.Detail, StartedAt: d.StartedAt, ExpiresAt: d.ExpiresAt}
	if d.RevisionID != "" {
		out.Reload = &DeviceReload{RevisionID: d.RevisionID, Epoch: d.Epoch}
	}
	return out
}

// ModelCheckState is where a model check is, as an answer names it.
type ModelCheckState string

const (
	// ModelCheckRunning is a check its node is still running.
	ModelCheckRunning ModelCheckState = "running"
	// ModelCheckEnded is a check that ended: with what it found, or why it
	// found nothing.
	ModelCheckEnded ModelCheckState = "ended"
	// ModelCheckLost is a check still running past its deadline: the node
	// running it stopped, and nothing will end it.
	ModelCheckLost ModelCheckState = "lost"
)

// ModelCheckStates is every state this build names, the closed set
// [ModelCheckState.Valid] reads and a client's copy of the words is held to.
var ModelCheckStates = []ModelCheckState{ModelCheckRunning, ModelCheckEnded, ModelCheckLost}

// Valid reports whether the state is one this build names.
func (s ModelCheckState) Valid() bool { return slices.Contains(ModelCheckStates, s) }

// ModelCheck is a model check, as `POST /llm/providers/{key}/check` and the
// `llm_check` question answer it.
type ModelCheck struct {
	ID        uuid.UUID       `json:"id"`
	Entry     string          `json:"entry"`
	Node      string          `json:"node"`
	State     ModelCheckState `json:"state"`
	Draft     bool            `json:"draft"`
	Smoke     bool            `json:"smoke"`
	StartedAt time.Time       `json:"started_at"`
	Deadline  time.Time       `json:"deadline"`
	// EndedAt is when it ended, null while it runs or once it is lost.
	EndedAt *time.Time `json:"ended_at"`
	By      string     `json:"by"`
	// Check is what an ended check found, as the check's lines; Failure is
	// why one ended, or was lost, without finding anything.
	Check   json.RawMessage `json:"check,omitempty"`
	Failure string          `json:"failure,omitempty"`
}

// ModelCheckOf is a check's record as the answers render it at now.
func ModelCheckOf(c coord.ModelCheck, now time.Time) ModelCheck {
	out := ModelCheck{ID: c.ID, Entry: c.Entry, Node: c.Node, State: ModelCheckRunning, Draft: c.Draft,
		Smoke: c.Smoke, StartedAt: c.StartedAt, Deadline: c.Deadline, By: c.By}
	switch {
	case !c.Running():
		ended := c.EndedAt
		out.State, out.EndedAt, out.Check, out.Failure = ModelCheckEnded, &ended, c.Answer, c.Failure
	case c.Lost(now):
		out.State = ModelCheckLost
		out.Failure = fmt.Sprintf("the check did not end on node %s by its deadline: the node may have "+
			"stopped; check again", c.Node)
	}
	return out
}

// MemberEntriesAnswer is `llm_entries`, a member's reading: every entry's
// condition, and nothing that names a credential, an account or a vendor's
// sentence (decision D7).
type MemberEntriesAnswer struct {
	Reading string        `json:"reading"`
	Node    string        `json:"node"`
	Nodes   []ModelNode   `json:"nodes"`
	Entries []MemberEntry `json:"entries"`
	Alarms  []ModelsAlarm `json:"alarms"`
	// ExecutorHarnesses is the admin reading's: which harness runs a seat's
	// executor is no secret.
	ExecutorHarnesses []config.Harness `json:"executor_harnesses"`
}

// MemberEntry is one entry, a member's reading.
type MemberEntry struct {
	Key         string           `json:"key"`
	Harness     string           `json:"harness"`
	Model       string           `json:"model"`
	Health      ModelHealth      `json:"health"`
	SignIn      MemberSignIn     `json:"sign_in"`
	SittingOut  *ModelSittingOut `json:"sitting_out"`
	LastSuccess *time.Time       `json:"last_success"`
	Waiting     *ModelWaiting    `json:"waiting"`
	UsedBy      []ModelUse       `json:"used_by"`
	Limits      []ModelLimit     `json:"limits"`
}

// MemberSignIn is how an entry signs in, a member's reading: the route and
// whether it resolves.
type MemberSignIn struct {
	Route    string `json:"route"`
	Resolves bool   `json:"resolves"`
}

// nodeModels is one live node's model rows.
type nodeModels struct {
	node     string
	reported bool
	models   map[string]coord.ModelStatus
}

// modelReports is every live node's model rows, ordered by node id. The lease
// table and the status records are the answer, so an unreadable one is an
// error rather than a fleet with no nodes.
func (s Sources) modelReports(ctx context.Context) ([]nodeModels, error) {
	leases, records, err := s.liveStatuses(ctx, coord.NodeStatusRead{Models: true})
	if err != nil {
		return nil, err
	}
	out := make([]nodeModels, 0, len(leases))
	for _, lease := range leases {
		r := nodeModels{node: nameIn(coord.ClassNode, lease.Resource)}
		if record, ok := records[r.node]; ok {
			r.reported = true
			r.models = make(map[string]coord.ModelStatus, len(record.Models))
			for _, m := range record.Models {
				r.models[m.Key] = m
			}
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b nodeModels) int { return cmp.Compare(a.node, b.node) })
	return out, nil
}

// llmEntries answers what each configured model is doing, across the fleet.
//
// FROM THE STATUS RECORDS, as `mcp_servers_status` is: every node writes its
// models on its record ([coord.NodeStatusRecord.Models]), so one listing
// against the lease table is every node's word, and a live node with no record
// is a column of its own rather than a row of zeros. What only THIS node can
// say (how it resolves each entry's credential, the hint of the value) comes
// from its own engine ([ModelSource]), and the answer names this node.
//
// TWO READINGS (decision D7): an admin reads the whole answer; anybody else
// reads each model's condition with nothing that names a credential, an
// account, a vendor's sentence or a cost.
func (s Sources) llmEntries(ctx context.Context, _ Params) (any, error) {
	reports, err := s.modelReports(ctx)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	nodes := make([]ModelNode, 0, len(reports))
	for _, r := range reports {
		nodes = append(nodes, ModelNode{ID: r.node, Reported: r.reported})
	}
	company, organization := s.Company()
	admin := readsConfig(ctx)
	entries := []ModelEntry{}
	if company != nil {
		local := map[string]LocalModel{}
		for _, m := range s.Models.LocalModels() {
			local[m.Key] = m
		}
		usage := s.modelUsage(ctx)
		devices, err := s.runningDevices(ctx, now)
		if err != nil {
			return nil, err
		}
		for _, key := range company.Providers.ProviderOrder() {
			entry := company.Providers.LLM[key]
			e := s.modelEntry(company, organization, key, &entry, local[key], reports,
				usage[key], now)
			if d, ok := devices[key]; ok {
				view := DeviceOf(d)
				e.DeviceSignIn = &view
			}
			entries = append(entries, e)
		}
	}
	if !admin {
		out := MemberEntriesAnswer{Reading: ReadingMember, Node: s.NodeID, Nodes: nodes,
			Entries: make([]MemberEntry, 0, len(entries)), ExecutorHarnesses: config.ExecutorHarnesses()}
		for _, e := range entries {
			out.Entries = append(out.Entries, memberEntry(e))
		}
		return out, nil
	}
	alarms := []ModelsAlarm{}
	for _, a := range s.latestAlarms() {
		if slices.Contains(llmAlarmKinds, a.Kind) {
			alarms = append(alarms, a)
		}
	}
	return ModelEntriesAnswer{Reading: ReadingAdmin, Node: s.NodeID, Nodes: nodes,
		Entries: entries, Alarms: alarms, ExecutorHarnesses: config.ExecutorHarnesses()}, nil
}

// memberEntry is an admin entry with everything a member does not read
// dropped: the plan, every sign-in field but the route and whether it
// resolves, the vendor's sentence, the last failure and the usage.
func memberEntry(e ModelEntry) MemberEntry {
	out := MemberEntry{Key: e.Key, Harness: e.Harness, Model: e.Model, Health: e.Health,
		SignIn:      MemberSignIn{Route: e.SignIn.Route, Resolves: e.SignIn.Resolves},
		LastSuccess: e.LastSuccess, Waiting: e.Waiting, UsedBy: e.UsedBy, Limits: e.Limits}
	if e.SittingOut != nil {
		sit := *e.SittingOut
		sit.Said = ""
		sit.Nodes = nil
		out.SittingOut = &sit
	}
	return out
}

// modelUsage is each entry's spend over [ModelUsageDays], through the same
// rollup the `tokens` question answers; nil when this node reads no usage.
func (s Sources) modelUsage(ctx context.Context) map[string]ModelUsage {
	out := map[string]ModelUsage{}
	if s.Usage == nil {
		return out
	}
	rollup, err := s.rollup(ctx, FromMap(map[string]any{"days": float64(ModelUsageDays)}))
	if err != nil {
		// A usage read that failed is no reason to withhold every model's
		// condition: the rest of the answer stands and usage reads empty.
		log.WarnContext(ctx, "llm_entries_usage_unread", "error", err)
		return out
	}
	for _, row := range rollup.ByProvider {
		out[row.ProviderKey] = ModelUsage{Days: ModelUsageDays, Calls: row.Calls,
			TotalTokens: row.TotalTokens}
	}
	return out
}

// runningDevices is each entry's device sign-in, the latest one whose record
// has not lapsed.
func (s Sources) runningDevices(ctx context.Context, now time.Time) (map[string]coord.DeviceSignIn, error) {
	out := map[string]coord.DeviceSignIn{}
	if s.DeviceSignIns == nil {
		return out, nil
	}
	all, err := s.DeviceSignIns.ListDeviceSignIns(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if d.Lapsed(now) {
			continue
		}
		if held, ok := out[d.Entry]; !ok || d.StartedAt.After(held.StartedAt) {
			out[d.Entry] = d
		}
	}
	return out, nil
}

// modelEntry is one entry's admin reading.
func (s Sources) modelEntry(company *config.Company, organization *org.Organization, key string,
	entry *config.LLMProvider, local LocalModel, reports []nodeModels,
	usage ModelUsage, now time.Time) ModelEntry {

	e := ModelEntry{Key: key, Harness: string(cmp.Or(entry.Harness, config.Harness("claude-code"))),
		Model: entry.Model, UsedBy: usedBy(company, key),
		Usage: usage, HelperProblem: helperProblem(company, key)}
	if e.Usage.Days == 0 {
		e.Usage = ModelUsage{Days: ModelUsageDays}
	}
	// THIS NODE'S SIGN-IN, and where this node has not applied the entry
	// yet, the first node that reports it: a route nobody can see is none.
	route, resolves := local.Route, local.Resolves
	if route == "" {
		route = "none"
		for _, r := range reports {
			if m, ok := r.models[key]; ok {
				route, resolves = cmp.Or(m.SignIn, "none"), m.Resolves
				break
			}
		}
	}
	// WHERE THE VALUE CAME FROM: the store, which resolution reads first,
	// when this node's snapshot holds the variable; else the environment it
	// resolved from.
	source := "none"
	switch {
	case local.Variable != "" && !local.StoredAt.IsZero():
		source = "store"
	case local.Variable != "" && resolves:
		source = "environment"
	}
	e.SignIn = ModelSignIn{Route: route, Mode: string(entry.ResolvedAuthMode()),
		Source: source, Resolves: resolves, HostLogins: []ModelHostLogin{}}
	if local.Variable != "" {
		e.SignIn.Variable = ptr(local.Variable)
	}
	if local.Hint != "" {
		e.SignIn.Hint = ptr(local.Hint)
	}
	if source == "store" {
		at := local.StoredAt
		e.SignIn.StoredAt = &at
		e.SignIn.StoredBy = ptr(local.StoredBy)
		if local.TokenLifetime > 0 && route == "token" {
			expires := at.Add(local.TokenLifetime)
			e.SignIn.ExpiresAt = &expires
			e.SignIn.ExpiryBasis = ptr("stored_at+" +
				strconv.Itoa(int(local.TokenLifetime/(24*time.Hour))) + "d")
		}
	}
	for _, r := range reports {
		m, ok := r.models[key]
		if !ok {
			continue
		}
		if e.Plan == "" {
			e.Plan = m.Plan
		}
		if m.HostLogin != nil {
			h := ModelHostLogin{Node: r.node, State: string(m.HostLogin.State),
				Method: optional(m.HostLogin.Method), Account: optional(m.HostLogin.Account),
				Detail: optional(m.HostLogin.Detail), Command: "crewlet llm login " + key}
			if !m.HostLogin.CheckedAt.IsZero() {
				at := m.HostLogin.CheckedAt
				h.CheckedAt = &at
			}
			e.SignIn.HostLogins = append(e.SignIn.HostLogins, h)
		}
		if out := m.SittingOut; out != nil && out.Until.After(now) {
			if e.SittingOut == nil || out.Until.After(e.SittingOut.Until) {
				nodes := []string{}
				if e.SittingOut != nil {
					nodes = e.SittingOut.Nodes
				}
				e.SittingOut = &ModelSittingOut{Until: out.Until, Window: out.Window,
					Said: out.Said, Since: out.Since, Nodes: nodes}
				if !out.ResetsAt.IsZero() {
					resets := out.ResetsAt
					e.SittingOut.ResetsAt = &resets
				}
			}
			e.SittingOut.Nodes = append(e.SittingOut.Nodes, r.node)
		}
		if !m.LastAnswer.IsZero() && (e.LastSuccess == nil || m.LastAnswer.After(*e.LastSuccess)) {
			at := m.LastAnswer
			e.LastSuccess = &at
		}
		if f := m.LastFailure; f != nil && (e.LastFailure == nil || f.At.After(e.LastFailure.At)) {
			e.LastFailure = &ModelLastFailure{Kind: f.Kind, At: f.At, Said: f.Said, Node: r.node}
		}
		for _, parked := range m.Parked {
			if e.Waiting == nil {
				e.Waiting = &ModelWaiting{Seats: []string{}, Since: parked.Since}
			}
			handle := organization.AgentHandleByID(parked.Seat)
			if !slices.Contains(e.Waiting.Seats, handle) {
				e.Waiting.Seats = append(e.Waiting.Seats, handle)
			}
			if parked.Since.Before(e.Waiting.Since) {
				e.Waiting.Since = parked.Since
			}
		}
	}
	if e.Waiting != nil {
		slices.Sort(e.Waiting.Seats)
	}
	e.Limits = modelLimits(key, entry.Model, reports, e.SittingOut, now)
	e.Health = modelHealth(e, now)
	return e
}

// modelLimits is the one reading of an entry's limit windows: each window's
// newest report across nodes, with the window it sits out marked reached.
func modelLimits(key, model string, reports []nodeModels, sitting *ModelSittingOut, now time.Time) []ModelLimit {
	newest := map[string]coord.ModelLimit{}
	for _, r := range reports {
		for _, l := range r.models[key].Limits {
			if !llm.LimitWindow(l.Window).Valid() || !llm.LimitState(l.State).Valid() {
				continue
			}
			k := l.Window + "/" + l.Model
			if held, ok := newest[k]; !ok || l.Seen.After(held.Seen) {
				newest[k] = l
			}
		}
	}
	out := make([]ModelLimit, 0, len(newest)+1)
	for _, l := range newest {
		out = append(out, limitOf(l, now))
	}
	if sitting != nil {
		until := sitting.Until
		i := slices.IndexFunc(out, func(l ModelLimit) bool { return sitsOut(l, sitting.Window, model) })
		if i < 0 {
			out = append(out, ModelLimit{Window: sitting.Window, Seen: sitting.Since})
			i = len(out) - 1
		}
		out[i].State, out[i].Until, out[i].Stale = string(llm.LimitReached), &until, false
		if sitting.ResetsAt != nil {
			out[i].ResetsAt = sitting.ResetsAt
		}
	}
	slices.SortFunc(out, func(a, b ModelLimit) int {
		return cmp.Or(cmp.Compare(slices.Index(llm.LimitWindows, llm.LimitWindow(a.Window)),
			slices.Index(llm.LimitWindows, llm.LimitWindow(b.Window))), cmp.Compare(a.Model, b.Model))
	})
	return out
}

// limitOf is one window's newest report as read at now ([llm.LimitReading.At]).
func limitOf(l coord.ModelLimit, now time.Time) ModelLimit {
	read := llm.LimitReading{State: llm.LimitState(l.State), Used: l.Used, ResetsAt: l.ResetsAt}.At(now)
	out := ModelLimit{Window: l.Window, Hours: l.Hours, Model: l.Model, State: string(read.State),
		Used: read.Used, Seen: l.Seen, Stale: now.Sub(l.Seen) > llm.LimitStaleAfter}
	if !l.ResetsAt.IsZero() {
		resets := l.ResetsAt
		out.ResetsAt = &resets
	}
	return out
}

// sitsOut reports whether the sit-out of window, on an entry running model,
// is the window l: the same kind, and for a model window, the family the
// entry's model names.
func sitsOut(l ModelLimit, window, model string) bool {
	if l.Window != window {
		return false
	}
	if window != string(llm.LimitModel) {
		return l.Model == ""
	}
	return l.Model != "" && strings.Contains(strings.ToLower(model), strings.ToLower(l.Model))
}

// modelHealth is [ModelHealth]'s one reading of an entry.
func modelHealth(e ModelEntry, now time.Time) ModelHealth {
	refused := e.LastFailure != nil && e.LastFailure.Kind == "auth" &&
		(e.LastSuccess == nil || e.LastFailure.At.After(*e.LastSuccess))
	expires := e.SignIn.ExpiresAt
	switch {
	case e.SittingOut != nil:
		return HealthSittingOut
	case e.SignIn.Route == "none" || !e.SignIn.Resolves:
		return HealthNoSignIn
	case refused || (expires != nil && !expires.After(now)):
		return HealthRejected
	case expires != nil && expires.Sub(now) <= statelog.CredentialWarningDays*24*time.Hour:
		return HealthExpiring
	case e.LastSuccess == nil && e.LastFailure == nil && e.Usage.Calls == 0:
		return HealthUntested
	}
	return HealthReady
}

// usedByPhases are the jobs `used_by` reports, each the phase it resolves:
// onboarding runs on its seat's executor chain and is reported as that.
var usedByPhases = []struct {
	phase phase.Phase
	job   config.ProviderJob
}{
	{phase.Execute, config.JobExecutor},
	{phase.Review, config.JobReview},
	{phase.Subagent, config.JobSubagent},
	{phase.Auxiliary, config.JobAuxiliary},
	{phase.Judge, config.JobJudge},
	{phase.Triage, config.JobTriage},
}

// usedBy is every job the entry key runs, or is a fallback in the chain of:
// each agent seat's phase, resolved by [phase.Resolve] (the one resolution
// the engine's chains use), and the two helpers' fields. A use through a
// fleet names it, at the entry's place in the fleet's list.
func usedBy(company *config.Company, key string) []ModelUse {
	out := []ModelUse{}
	models := company.Providers.PhaseModels()
	for role := range company.EachRole() {
		if role.Kind == org.KindHuman {
			continue
		}
		for _, pj := range usedByPhases {
			res := phase.Resolve(role.Seat(), pj.phase, models)
			position := slices.Index(res.Keys, key)
			if position < 0 {
				continue
			}
			written := res.Source == phase.SourceSeat
			out = append(out, ModelUse{Seat: role.IdentityKey(), Job: string(pj.job),
				Position: position, Written: &written, Source: string(res.Source), Fleet: res.Fleet})
		}
	}
	if use, ok := helperUse(models, company.Assistant.Model, key); ok {
		use.Assistant = "chat"
		out = append(out, use)
	}
	if use, ok := helperUse(models, company.Advisor.Model, key); ok {
		use.Advisor = "findings"
		out = append(out, use)
	}
	return out
}

// helperUse is a helper's use of the entry key through its field ref: the
// entry itself, or a fleet listing it.
func helperUse(models phase.Models, ref, key string) (ModelUse, bool) {
	if ref == key {
		return ModelUse{}, true
	}
	name, ok := config.FleetOf(ref)
	if !ok {
		return ModelUse{}, false
	}
	position := slices.Index(models.Fleets[name].Members, key)
	return ModelUse{Position: position, Fleet: name}, position >= 0
}

// helperProblem is why no helper can run on the entry key, in a person's
// words where the rule wrote them, or nil where one can.
func helperProblem(company *config.Company, key string) *string {
	problems := config.Problems(company.HelperModelProblem(key))
	if len(problems) == 0 {
		return nil
	}
	return ptr(cmp.Or(problems[0].Said, problems[0].Message))
}

func ptr[T any](v T) *T { return &v }

// optional is a text that may be empty, as a JSON null when it is.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// RunnersAnswer is `llm_runners`: whether each executor cell can start a box,
// on each live node.
type RunnersAnswer struct {
	DefaultRunIn string                `json:"default_run_in"`
	Cells        map[string]RunnerCell `json:"cells"`
	Nodes        []RunnerNode          `json:"nodes"`
}

// RunnerCell is one cell across the fleet: whether `providers.sandbox`
// declares it, and the nodes where it is ready and where it is not.
type RunnerCell struct {
	Declared   bool     `json:"declared"`
	ReadyOn    []string `json:"ready_on"`
	NotReadyOn []string `json:"not_ready_on"`
}

// RunnerNode is one live node's three cells.
type RunnerNode struct {
	ID        string          `json:"id"`
	Reported  bool            `json:"reported"`
	Direct    *RunnerNodeCell `json:"direct"`
	Container *RunnerNodeCell `json:"container"`
	E2B       *RunnerNodeCell `json:"e2b"`
}

// RunnerNodeCell is one cell's probe on one node.
type RunnerNodeCell struct {
	State     string     `json:"state"`
	Detail    string     `json:"detail"`
	Fix       *string    `json:"fix"`
	CheckedAt *time.Time `json:"checked_at"`
}

// llmRunners answers whether each executor cell can start a box on each node,
// from the runner probe every node publishes on its status record.
func (s Sources) llmRunners(ctx context.Context, _ Params) (any, error) {
	leases, records, err := s.liveStatuses(ctx, coord.NodeStatusRead{})
	if err != nil {
		return nil, err
	}
	out := RunnersAnswer{Cells: map[string]RunnerCell{}, Nodes: []RunnerNode{}}
	var sandbox *config.SandboxProvider
	if company, _ := s.Company(); company != nil {
		sandbox = company.Providers.Sandbox
		out.DefaultRunIn = string(sandbox.RunIn())
	}
	for _, p := range config.Placements {
		out.Cells[string(p)] = RunnerCell{Declared: sandbox.Configured(p), ReadyOn: []string{},
			NotReadyOn: []string{}}
	}
	for _, lease := range leases {
		node := RunnerNode{ID: nameIn(coord.ClassNode, lease.Resource)}
		if record, ok := records[node.ID]; ok && record.Runners != nil {
			node.Reported = true
			for p, cell := range map[config.Placement]coord.RunnerCell{
				config.PlacementDirect: record.Runners.Direct, config.PlacementContainer: record.Runners.Container,
				config.PlacementE2B: record.Runners.E2B,
			} {
				view := &RunnerNodeCell{State: string(cell.State), Detail: cell.Detail, Fix: optional(cell.Fix)}
				if !cell.CheckedAt.IsZero() {
					at := cell.CheckedAt
					view.CheckedAt = &at
				}
				switch p {
				case config.PlacementDirect:
					node.Direct = view
				case config.PlacementContainer:
					node.Container = view
				default:
					node.E2B = view
				}
				summary := out.Cells[string(p)]
				switch cell.State {
				case coord.RunnerReady:
					summary.ReadyOn = append(summary.ReadyOn, node.ID)
				case coord.RunnerNotReady:
					summary.NotReadyOn = append(summary.NotReadyOn, node.ID)
				}
				out.Cells[string(p)] = summary
			}
		}
		out.Nodes = append(out.Nodes, node)
	}
	slices.SortFunc(out.Nodes, func(a, b RunnerNode) int { return cmp.Compare(a.ID, b.ID) })
	for p, cell := range out.Cells {
		slices.Sort(cell.ReadyOn)
		slices.Sort(cell.NotReadyOn)
		out.Cells[p] = cell
	}
	return out, nil
}

// ModelsAnswer is `llm_models`: what a model step suggests for one harness.
type ModelsAnswer struct {
	Source    string               `json:"source"`
	FetchedAt *time.Time           `json:"fetched_at"`
	Provider  string               `json:"provider"`
	Aliases   []ModelAliasRow      `json:"aliases"`
	Models    []modelcatalog.Model `json:"models"`
	DocsURL   string               `json:"docs_url"`
	Reason    string               `json:"reason,omitempty"`
}

// ModelAliasRow is a model name the harness resolves itself.
type ModelAliasRow struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
	// Model and Version are what it resolves to on the vendor's own service
	// (the harness profile's), absent where the profile does not know.
	Model   string `json:"model,omitempty"`
	Version string `json:"version,omitempty"`
}

// catalogueProviders is the catalogue provider each harness reads, where it reads
// one fixed provider: the ids its vendor's harness takes as model arguments.
var catalogueProviders = map[string]string{"claude-code": "anthropic", "codex": "openai",
	"gemini-cli": "google", "grok": "xai"}

// namedProvider is the harnesses that name their provider in the model, so a
// caller names it: OpenCode and Pi as `<provider>/<model>`.
var namedProvider = []string{"opencode", "pi"}

// llmModels answers the models one harness can be given: the catalogue's list for
// the harness's provider, the aliases the harness resolves itself and its vendor's own
// page. No credential is read or sent (decision D1): the list is the public
// catalogue's, and the test call is what says whether a plan reaches a model. A
// harness with no one catalogue provider (its vendor's own names, or a provider
// its sign-in picks) answers its aliases and page alone.
func (s Sources) llmModels(ctx context.Context, p Params) (any, error) {
	harness := p.String("harness")
	profile, known := harnessprofile.Builtin(harness)
	if harness == "" || !known || harness == "custom" {
		return nil, fmt.Errorf("%w: harness must be a built-in harness other than custom", ErrBadParams)
	}
	provider := catalogueProviders[harness]
	named := slices.Contains(namedProvider, harness)
	if named {
		provider = p.String("provider")
		if provider == "" {
			return nil, fmt.Errorf("%w: %s names its provider in the model argument, so "+
				"provider is required (openrouter, anthropic, ...)", ErrBadParams, harness)
		}
	}
	answer := modelcatalog.Answer{Source: modelcatalog.SourceNone, Models: []modelcatalog.Model{},
		Reason: "the harness names its own models"}
	if provider != "" {
		fetched, err := s.Catalogue.Provider(ctx, provider)
		switch {
		case errors.Is(err, modelcatalog.ErrUnknownProvider) && named:
			return nil, &UnknownProviderError{Provider: provider}
		case errors.Is(err, modelcatalog.ErrUnknownProvider):
			answer.Reason = "the catalogue lists no provider " + strconv.Quote(provider)
		case err != nil:
			return nil, err
		default:
			answer = fetched
		}
	}
	out := ModelsAnswer{Source: answer.Source, Provider: provider, Aliases: []ModelAliasRow{},
		Models: answer.Models, DocsURL: profile.ModelsDocs, Reason: answer.Reason}
	if !answer.FetchedAt.IsZero() {
		at := answer.FetchedAt
		out.FetchedAt = &at
	}
	for _, a := range profile.ModelAliases {
		out.Aliases = append(out.Aliases, ModelAliasRow{ID: a.ID, Name: a.Name, Detail: a.Detail,
			Model: a.Model, Version: a.Version})
	}
	if named {
		// SUCH A HARNESS TAKES <provider>/<model>, so that is the id it is offered.
		models := make([]modelcatalog.Model, 0, len(out.Models))
		for _, m := range out.Models {
			m.ID = provider + "/" + m.ID
			models = append(models, m)
		}
		out.Models = models
	}
	return out, nil
}

// ErrUnknownProvider is a model catalogue question about a provider the
// catalogue lists nothing under: a request the caller has to change, answered
// 422 `unknown_provider` over REST and as a refused question on the socket.
var ErrUnknownProvider = errors.New("queries: the model catalogue lists no such provider")

// UnknownProviderError is [ErrUnknownProvider] naming the provider.
type UnknownProviderError struct{ Provider string }

func (e *UnknownProviderError) Error() string {
	return "the model catalogue lists no provider " + strconv.Quote(e.Provider)
}

// Unwrap makes it [ErrUnknownProvider] to errors.Is.
func (e *UnknownProviderError) Unwrap() error { return ErrUnknownProvider }

// llmDeviceSignIn answers one device sign-in by its id, while its record
// lingers.
func (s Sources) llmDeviceSignIn(ctx context.Context, p Params) (any, error) {
	id, err := uuid.Parse(p.String("id"))
	if err != nil {
		return nil, fmt.Errorf("%w: id must be a device sign-in's id", ErrBadParams)
	}
	all, err := s.DeviceSignIns.ListDeviceSignIns(ctx)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	for _, d := range all {
		if d.ID == id && !d.Lapsed(now) {
			return DeviceOf(d), nil
		}
	}
	return nil, fmt.Errorf("%w: no device sign-in %s is in progress or recently ended", ErrNotFound, id)
}

// llmCheck answers a model's latest check by its entry's key, while its record
// answers.
func (s Sources) llmCheck(ctx context.Context, p Params) (any, error) {
	key := p.String("key")
	if key == "" {
		return nil, fmt.Errorf("%w: key must name a model entry", ErrBadParams)
	}
	check, found, err := s.ModelChecks.ModelCheck(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: no check of %q is running or recently ended", ErrNotFound, key)
	}
	return ModelCheckOf(check, s.clock()), nil
}
