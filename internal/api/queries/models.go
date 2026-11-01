// What each configured model is doing, as Settings › Models reads it.

package queries

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/modelcatalog"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/secrets"
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
	// TokenLifetime is how long the CLI's headless token lasts, zero when
	// unknown or not a token.
	TokenLifetime time.Duration
}

// ModelSource is the engine's half of the model questions: what this node
// resolved for each entry, and its latest alarm evaluation.
type ModelSource interface {
	LocalModels() []LocalModel
	LatestAlarms() []statelog.Alarm
}

// SecretRows lists the fleet's secret rows, metadata only: when each was
// stored and by whom, which a model's credential is dated by.
type SecretRows interface {
	List(ctx context.Context) ([]secrets.Record, error)
}

// DeviceSignIns lists the fleet's device sign-ins in progress or lingering
// after their end.
type DeviceSignIns interface {
	ListDeviceSignIns(ctx context.Context) ([]coord.DeviceSignIn, error)
}

// ModelCatalogue is the model catalogue a model step suggests from
// (internal/modelcatalog).
type ModelCatalogue interface {
	Provider(ctx context.Context, provider string) (modelcatalog.Answer, error)
}

// ModelUsageDays is the usage window `llm_entries` reports per entry: a week,
// the span a person looking at a model's page asks "is it used" over.
const ModelUsageDays = 7

// llmAlarmKinds are the five model alarms (statelog), the ones `llm_entries`
// carries.
var llmAlarmKinds = []statelog.Kind{statelog.KindLLMCredentialExpiring,
	statelog.KindLLMCredentialExpiresSoon, statelog.KindLLMSignInRefused,
	statelog.KindLLMLimit, statelog.KindLLMLimitWaiting}

// ModelReading is which reading of `llm_entries` an answer is.
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
}

// ModelNode is one live node, and whether its latest beat carried its models.
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

// ModelHostLogin is one node's CLI login for an entry signing in with
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
// handle and job, or the assistant's chat or deep work. Position is the
// entry's place in the resolved chain, 0 for the model that runs it.
type ModelUse struct {
	Seat      string `json:"seat,omitempty"`
	Job       string `json:"job,omitempty"`
	Assistant string `json:"assistant,omitempty"`
	Position  int    `json:"position"`
	// Written is whether the seat wrote the field that resolved the job;
	// Source is the level that decided it (phase.Source). Absent for the
	// assistant, whose fields are always written.
	Written *bool  `json:"written,omitempty"`
	Source  string `json:"source,omitempty"`
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

// MemberEntriesAnswer is `llm_entries`, a member's reading: every entry's
// condition, and nothing that names a credential, an account or a vendor's
// sentence (decision D7).
type MemberEntriesAnswer struct {
	Reading string        `json:"reading"`
	Node    string        `json:"node"`
	Nodes   []ModelNode   `json:"nodes"`
	Entries []MemberEntry `json:"entries"`
	Alarms  []ModelsAlarm `json:"alarms"`
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
// table is the answer, so an unreadable one is an error rather than a fleet
// with no nodes.
func (s Sources) modelReports(ctx context.Context) ([]nodeModels, []coord.NodeStatus, error) {
	leases, err := s.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, nil, err
	}
	out := make([]nodeModels, 0, len(leases))
	statuses := make([]coord.NodeStatus, 0, len(leases))
	for _, lease := range leases {
		r := nodeModels{node: nameIn(coord.ClassNode, lease.Resource)}
		status, ok := coord.StatusFromMeta(lease.Meta)
		if ok {
			r.reported = true
			r.models = make(map[string]coord.ModelStatus, len(status.Models))
			for _, m := range status.Models {
				r.models[m.Key] = m
			}
		}
		out = append(out, r)
		statuses = append(statuses, status)
	}
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return cmp.Compare(out[a].node, out[b].node) })
	sorted := make([]nodeModels, len(out))
	sortedStatus := make([]coord.NodeStatus, len(out))
	for i, j := range order {
		sorted[i], sortedStatus[i] = out[j], statuses[j]
	}
	return sorted, sortedStatus, nil
}

// llmEntries answers what each configured model is doing, across the fleet.
//
// FROM THE HEARTBEATS, as `mcp_servers_status` is: every node re-sends its
// models on its presence lease ([coord.NodeStatus.Models]), so one listing of
// the lease table is every node's word at once, and a node whose last beat
// carried no report is a column of its own rather than a row of zeros. What
// only THIS node can say (how it resolves each entry's credential, the hint of
// the value it resolved) comes from its own engine ([ModelSource]), and the
// answer names this node.
//
// TWO READINGS (decision D7): an admin reads the whole answer; anybody else
// reads each model's condition with nothing that names a credential, an
// account, a vendor's sentence or a cost.
func (s Sources) llmEntries(ctx context.Context, _ Params) (any, error) {
	reports, _, err := s.modelReports(ctx)
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
		stored, err := s.storedRows(ctx)
		if err != nil {
			return nil, err
		}
		usage := s.modelUsage(ctx)
		devices, err := s.runningDevices(ctx, now)
		if err != nil {
			return nil, err
		}
		for _, key := range company.Providers.ProviderOrder() {
			entry := company.Providers.LLM[key]
			e := s.modelEntry(company, organization, key, &entry, local[key], reports, stored,
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
			Entries: make([]MemberEntry, 0, len(entries))}
		for _, e := range entries {
			out.Entries = append(out.Entries, memberEntry(e))
		}
		return out, nil
	}
	alarms := []ModelsAlarm{}
	for _, a := range s.Models.LatestAlarms() {
		if slices.Contains(llmAlarmKinds, a.Kind) {
			alarms = append(alarms, a)
		}
	}
	return ModelEntriesAnswer{Reading: ReadingAdmin, Node: s.NodeID, Nodes: nodes,
		Entries: entries, Alarms: alarms}, nil
}

// memberEntry is an admin entry with everything a member does not read
// dropped: the plan, every sign-in field but the route and whether it
// resolves, the vendor's sentence, the last failure and the usage.
func memberEntry(e ModelEntry) MemberEntry {
	out := MemberEntry{Key: e.Key, Harness: e.Harness, Model: e.Model, Health: e.Health,
		SignIn:      MemberSignIn{Route: e.SignIn.Route, Resolves: e.SignIn.Resolves},
		LastSuccess: e.LastSuccess, Waiting: e.Waiting, UsedBy: e.UsedBy}
	if e.SittingOut != nil {
		sit := *e.SittingOut
		sit.Said = ""
		sit.Nodes = nil
		out.SittingOut = &sit
	}
	return out
}

// storedRows is the fleet's secret rows by name.
func (s Sources) storedRows(ctx context.Context) (map[string]secrets.Record, error) {
	out := map[string]secrets.Record{}
	if s.Secrets == nil {
		return out, nil
	}
	rows, err := s.Secrets.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the secret store: %w", err)
	}
	for _, row := range rows {
		out[row.Name] = row
	}
	return out, nil
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
	stored map[string]secrets.Record, usage ModelUsage, now time.Time) ModelEntry {

	e := ModelEntry{Key: key, Harness: string(cmp.Or(entry.Harness, config.Harness("claude-code"))),
		Model: entry.Model, UsedBy: usedBy(company, key),
		Usage: usage}
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
	// when a row holds the variable; else the environment it resolved from.
	source := "none"
	row, inStore := stored[local.Variable]
	switch {
	case local.Variable != "" && inStore:
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
		at := row.UpdatedAt
		e.SignIn.StoredAt = &at
		e.SignIn.StoredBy = ptr(row.UpdatedBy)
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
			handle := instanceSeat(organization, parked.Seat)
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
	e.Health = modelHealth(e, now)
	return e
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
}

// usedBy is every job the entry key runs, or is a fallback in the chain of:
// each agent seat's phase, resolved by [phase.ResolveWithSource] (the one
// resolution the engine's chains use), and the assistant's two fields.
func usedBy(company *config.Company, key string) []ModelUse {
	out := []ModelUse{}
	order := company.Providers.ProviderOrder()
	for role := range company.EachRole() {
		if role.Kind == org.KindHuman {
			continue
		}
		for _, pj := range usedByPhases {
			chain, source := phase.ResolveWithSource(role.Seat(), pj.phase, order)
			position := slices.Index(chain, key)
			if position < 0 {
				continue
			}
			written := source == phase.SourceSeat
			out = append(out, ModelUse{Seat: role.IdentityKey(), Job: string(pj.job),
				Position: position, Written: &written, Source: string(source)})
		}
	}
	if position := slices.Index(company.Assistant.LLM, key); position >= 0 {
		out = append(out, ModelUse{Assistant: "chat", Position: position})
	}
	if deep := company.Assistant.DeepWork; deep != nil && deep.LLM == key {
		out = append(out, ModelUse{Assistant: "deep_work", Position: 0})
	}
	return out
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
// from the runner probe every node publishes on its heartbeat.
func (s Sources) llmRunners(ctx context.Context, _ Params) (any, error) {
	leases, err := s.Coord.ListLive(ctx, coord.ClassNode)
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
		if status, ok := coord.StatusFromMeta(lease.Meta); ok && status.Runners != nil {
			node.Reported = true
			for p, cell := range map[config.Placement]coord.RunnerCell{
				config.PlacementDirect: status.Runners.Direct, config.PlacementContainer: status.Runners.Container,
				config.PlacementE2B: status.Runners.E2B,
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

// ModelsAnswer is `llm_models`: what a model step suggests for one CLI.
type ModelsAnswer struct {
	Source    string               `json:"source"`
	FetchedAt *time.Time           `json:"fetched_at"`
	Provider  string               `json:"provider"`
	Aliases   []ModelAliasRow      `json:"aliases"`
	Models    []modelcatalog.Model `json:"models"`
	DocsURL   string               `json:"docs_url"`
	Reason    string               `json:"reason,omitempty"`
}

// ModelAliasRow is a model name the CLI resolves itself.
type ModelAliasRow struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// catalogueProviders is the catalogue provider each CLI reads, where it reads
// one fixed provider: the ids its vendor's CLI takes as model arguments.
// OpenCode names its provider in the model argument itself, so a caller
// names it.
var catalogueProviders = map[string]string{"claude-code": "anthropic", "codex": "openai"}

// llmModels answers the models one CLI can be given: the catalogue's list for
// the CLI's provider, the aliases the CLI resolves itself and its vendor's own
// page. No credential is read or sent (decision D1): the list is the public
// catalogue's, and the test call is what says whether a plan reaches a model.
func (s Sources) llmModels(ctx context.Context, p Params) (any, error) {
	harness := p.String("harness")
	profile, known := harnessprofile.Builtin(harness)
	if harness == "" || !known || !slices.Contains([]string{"claude-code", "codex", "opencode"}, harness) {
		return nil, fmt.Errorf("%w: harness must be claude-code, codex or opencode", ErrBadParams)
	}
	provider := catalogueProviders[harness]
	if harness == "opencode" {
		provider = p.String("provider")
		if provider == "" {
			return nil, fmt.Errorf("%w: opencode names its provider in the model argument, so "+
				"provider is required (openrouter, anthropic, ...)", ErrBadParams)
		}
	}
	answer, err := s.Catalogue.Provider(ctx, provider)
	if err != nil {
		if errors.Is(err, modelcatalog.ErrUnknownProvider) {
			return nil, &UnknownProviderError{Provider: provider}
		}
		return nil, err
	}
	out := ModelsAnswer{Source: answer.Source, Provider: provider, Aliases: []ModelAliasRow{},
		Models: answer.Models, DocsURL: profile.ModelsDocs, Reason: answer.Reason}
	if !answer.FetchedAt.IsZero() {
		at := answer.FetchedAt
		out.FetchedAt = &at
	}
	for _, a := range profile.ModelAliases {
		out.Aliases = append(out.Aliases, ModelAliasRow{ID: a.ID, Name: a.Name, Detail: a.Detail})
	}
	if harness == "opencode" {
		// OPENCODE TAKES <provider>/<model>, so that is the id it is offered.
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
