package analyst

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// SubmitTool is the call a run finishes with.
const SubmitTool = "submit_findings"

// maxSubmissions is how many submit_findings calls a run may make: the first,
// and one more after the engine said what was wrong with it.
const maxSubmissions = 2

// tool is one read: its description and its answer over the run's data, with
// the records the answer names.
type tool struct {
	description string
	answer      func(d Data) (any, []advisor.Record)
}

// tools is every read the analyst is offered. Each answers numbers, closed
// words, times, handles and record ids, never words somebody wrote.
var tools = map[string]tool{
	"company_outline": {"The company document's shape: every seat (agent or person) with the model " +
		"entries it runs on, its round cap and token budgets, and its schedules; the model entries; " +
		"the delegation rules.", outlineOf},
	"seat_turns": {"Each agent's turns in the last 7 days by what woke it and what the turn did " +
		"(moved, words, nothing), with tokens and auxiliary tokens.", seatTurnsOf},
	"executor_runs": {"Executor runs in the last 7 days that ended on their round cap or for " +
		"memory, with the turn, tokens and rounds.", runsOf},
	"budget_stops": {"Turns a token budget refused in the last 7 days: the seat, whether the " +
		"company's budget or the seat's own, the period, and when its window reset.", stopsOf},
	"open_work": {"Open work now: every holder's open items, and the open items other open items " +
		"wait on, with how many wait, who holds it, and the people asked on it.", openWorkOf},
	"decisions": {"Decisions in the last 7 days: each person's answers to each agent and how many " +
		"took the recommendation, answers that came too late, reviews idle a day, and merges a " +
		"delegate allowed that went to a person for want of a required check.", decisionsOf},
	"schedule_fires": {"Each schedule's fires in the last 7 days and how many moved any work.", firesOf},
	"rule_findings":  {"What the advisor's own rules already list this week. Never repeat one.", rulesOf},
	"past_answers":   {"How people answered earlier analyst findings: useful, wrong or already known.", answersOf},
}

// toolNames is every read, sorted.
func toolNames() []string { return slices.Sorted(maps.Keys(tools)) }

// surface serves the reads and the submission over one run's data.
type surface struct {
	data   Data
	served *advisor.Served

	mu        sync.Mutex
	submitted []advisor.Submitted
	accepted  bool
	attempts  int
}

var _ toolloop.Surface = (*surface)(nil)

func (s *surface) ToolDefs() []llm.ToolDef {
	out := make([]llm.ToolDef, 0, len(tools)+1)
	for _, name := range toolNames() {
		out = append(out, llm.ToolDef{Name: name, Description: tools[name].description,
			Parameters: map[string]any{"type": "object", "properties": map[string]any{},
				"additionalProperties": false}})
	}
	return append(out, llm.ToolDef{Name: SubmitTool, Description: submitDescription, Parameters: submitSchema})
}

func (s *surface) Revision() uint64 { return 0 }

func (s *surface) AnnotationsOf(name string) mcp.Annotations {
	if name == SubmitTool {
		return mcp.Annotations{}
	}
	return mcp.Annotations{ReadOnly: mcp.Yes}
}

func (s *surface) Phase() string { return "advisor_analysis" }

func (s *surface) Admit(name string, _ map[string]any) toolloop.Admitted {
	return func(_ context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
		if name == SubmitTool {
			return s.submit(call.Arguments), nil
		}
		t, ok := tools[name]
		if !ok {
			return toolloop.ToolResult{Output: fmt.Sprintf("there is no tool %q", name), Failed: true,
				Cause: mcp.RefusalNotFound}, nil
		}
		answer, records := t.answer(s.data)
		if err := s.served.Serve(name, answer, records); err != nil {
			return toolloop.ToolResult{}, err
		}
		raw, err := json.Marshal(answer)
		if err != nil {
			return toolloop.ToolResult{}, err
		}
		return toolloop.ToolResult{Output: string(raw)}, nil
	}
}

// submit takes the run's findings, or answers every reason it cannot.
func (s *surface) submit(args map[string]any) toolloop.ToolResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts++; s.attempts > maxSubmissions {
		return toolloop.ToolResult{Failed: true, Output: "this run's submissions are spent",
			Cause: mcp.RefusalNotRunning}
	}
	raw, err := json.Marshal(args)
	var sub struct {
		Findings []advisor.Submitted `json:"findings"`
	}
	if err == nil {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		err = dec.Decode(&sub)
	}
	if err != nil {
		return toolloop.ToolResult{Failed: true, Cause: mcp.Classify(mcp.RefusalInvalid, err),
			Output: "the findings do not match the schema: " + err.Error() +
				"; write them again, exactly as the schema says."}
	}
	s.submitted, s.accepted = sub.Findings, true
	return toolloop.ToolResult{Output: "received"}
}

// result is what the run submitted, and whether it did.
func (s *surface) result() ([]advisor.Submitted, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submitted, s.accepted
}

type outlineSeat struct {
	Handle    string         `json:"handle"`
	Kind      string         `json:"kind"`
	Executor  string         `json:"executor_entry,omitempty"`
	Auxiliary string         `json:"auxiliary_entry,omitempty"`
	RoundCap  int            `json:"round_cap"`
	Budget    map[string]int `json:"token_budget,omitempty"`
	Schedules []outlineFires `json:"schedules,omitempty"`
}

type outlineFires struct {
	Name      string `json:"name"`
	FiresADay int    `json:"fires_a_day"`
}

func outlineOf(d Data) (any, []advisor.Record) {
	c := d.Company
	var seats []outlineSeat
	for role := range c.EachRole() {
		seat := outlineSeat{Handle: role.Seat().Handle(), Kind: "agent"}
		if role.Kind == org.KindHuman {
			seat.Kind = "person"
			seats = append(seats, seat)
			continue
		}
		if chain := c.ExecutorChain(role); len(chain) > 0 {
			seat.Executor = chain[0]
		}
		if aux := role.LLMAuxiliary; len(aux) > 0 {
			seat.Auxiliary = aux[0]
		}
		seat.RoundCap = roundCap(c, role)
		seat.Budget = budgetOf(role.TokenBudget)
		for _, s := range role.Schedules {
			fires, _ := config.ScheduleFiresPerDay(s)
			seat.Schedules = append(seat.Schedules, outlineFires{Name: s.Name, FiresADay: fires})
		}
		seats = append(seats, seat)
	}
	type entry struct {
		Key, Model, Harness string
	}
	var entries []entry
	for _, key := range slices.Sorted(maps.Keys(c.Providers.LLM)) {
		e := c.Providers.LLM[key]
		entries = append(entries, entry{Key: key, Model: c.ModelName(key), Harness: e.HarnessName()})
	}
	type rule struct {
		Kinds     []string `json:"kinds"`
		Delegates []string `json:"delegates"`
		Checked   []string `json:"repositories_with_required_checks"`
	}
	var rules []rule
	for _, r := range c.DelegatedDecisions.Rules {
		out := rule{Delegates: slices.Clone(r.Delegates), Checked: []string{}}
		for _, k := range r.Kinds {
			out.Kinds = append(out.Kinds, string(k))
		}
		for _, rc := range r.RequiredChecks {
			out.Checked = append(out.Checked, rc.Repo+"@"+rc.Branch)
		}
		rules = append(rules, out)
	}
	return map[string]any{"seats": seats, "model_entries": entries, "delegation_rules": rules}, nil
}

func roundCap(c *config.Company, r *config.Role) int {
	if r.Sandbox != nil && r.Sandbox.MaxTurns != nil {
		return *r.Sandbox.MaxTurns
	}
	return c.Providers.Sandbox.MaxTurns()
}

func budgetOf(b config.TokenBudget) map[string]int {
	out := map[string]int{}
	for p, v := range map[string]*int{"day": b.Day, "week": b.Week, "month": b.Month} {
		if v != nil {
			out[p] = *v
		}
	}
	return out
}

func seatTurnsOf(d Data) (any, []advisor.Record) {
	type row struct {
		Seat, Wake, Disposition, Outcome string
		Turns, Tokens, AuxiliaryTokens   int64
	}
	rows := []row{}
	for _, r := range d.Evidence.Ledger {
		rows = append(rows, row{Seat: cmp.Or(r.Handle, r.AgentID), Wake: r.Wake, Disposition: r.Disposition,
			Outcome: r.Outcome, Turns: r.Turns, Tokens: r.Tokens(), AuxiliaryTokens: r.AuxInput + r.AuxOutput})
	}
	return map[string]any{"rows": rows}, nil
}

func runsOf(d Data) (any, []advisor.Record) {
	type run struct {
		Seat   string `json:"seat"`
		Kind   string `json:"kind"`
		Turn   string `json:"turn"`
		Tokens int64  `json:"tokens"`
		Rounds int64  `json:"rounds"`
	}
	runs := []run{}
	var records []advisor.Record
	for _, r := range d.Evidence.Runs {
		runs = append(runs, run{Seat: r.Seat, Kind: r.Kind, Turn: r.Turn, Tokens: r.Tokens, Rounds: r.Rounds})
		records = append(records, advisor.Record{Kind: advisor.RecordTurn, ID: r.Turn})
	}
	return map[string]any{"runs": runs}, records
}

func stopsOf(d Data) (any, []advisor.Record) {
	type stop struct {
		Seat    string    `json:"seat"`
		Company bool      `json:"company_budget"`
		Period  string    `json:"period"`
		Ceiling int64     `json:"ceiling"`
		At      time.Time `json:"at"`
		Resets  time.Time `json:"resets,omitzero"`
		Turn    string    `json:"turn"`
	}
	stops := []stop{}
	var records []advisor.Record
	for _, s := range d.Evidence.Stops {
		stops = append(stops, stop{Seat: s.Seat, Company: s.Company, Period: s.Period, Ceiling: s.Ceiling,
			At: s.At, Resets: s.Resets, Turn: s.Turn})
		records = append(records, advisor.Record{Kind: advisor.RecordTurn, ID: s.Turn})
	}
	return map[string]any{"stops": stops}, records
}

func openWorkOf(d Data) (any, []advisor.Record) {
	open := d.Evidence.Work.Open
	var records []advisor.Record
	items := func(keys ...string) {
		for _, k := range keys {
			records = append(records, advisor.Record{Kind: advisor.RecordItem, ID: k})
		}
	}
	type blocker struct {
		Key       string   `json:"key"`
		Holder    string   `json:"holder"`
		Status    string   `json:"status"`
		Blocks    int      `json:"blocks"`
		Ready     int      `json:"ready"`
		Waiting   []string `json:"waiting"`
		ReadyKeys []string `json:"ready_keys"`
		Asked     []string `json:"asked"`
		Questions int      `json:"questions"`
	}
	blockers := []blocker{}
	for _, b := range open.Blockers {
		blockers = append(blockers, blocker{Key: b.Key, Holder: b.Holder, Status: b.Status, Blocks: b.Blocks,
			Ready: b.Ready, Waiting: b.Waiting, ReadyKeys: b.ReadyKeys, Asked: b.Asked, Questions: len(b.Asks)})
		items(b.Key)
		items(b.Waiting...)
		items(b.ReadyKeys...)
	}
	for _, h := range open.Holders {
		items(h.Keys...)
	}
	return map[string]any{"holders": open.Holders, "blockers": blockers}, records
}

func decisionsOf(d Data) (any, []advisor.Record) {
	w := d.Evidence.Work
	var records []advisor.Record
	type review struct {
		Key     string   `json:"key"`
		Holder  string   `json:"holder"`
		WaitsOn []string `json:"waits_on"`
	}
	reviews := []review{}
	for _, r := range w.IdleReviews {
		reviews = append(reviews, review{Key: r.Key, Holder: r.Holder, WaitsOn: r.WaitsOn})
		records = append(records, advisor.Record{Kind: advisor.RecordItem, ID: r.Key})
	}
	type merge struct {
		Repo   string `json:"repo"`
		Change string `json:"change"`
	}
	merges := []merge{}
	for _, m := range d.Evidence.Merges {
		merges = append(merges, merge{Repo: m.Repo, Change: m.Change})
		records = append(records, advisor.Record{Kind: advisor.RecordChange, ID: m.Change})
	}
	return map[string]any{"answered": w.Decisions, "answers": w.Answers, "asked_again_first": w.Superseded,
		"answered_after_done": w.Moot, "idle_reviews": reviews, "merges_without_required_checks": merges}, records
}

func firesOf(d Data) (any, []advisor.Record) {
	type tally struct {
		Seat, Unit, Name string
		Fires, Moved     int
	}
	by := map[[3]string]*tally{}
	for _, f := range d.Evidence.Fires {
		k := [3]string{f.Seat, f.Unit, f.Name}
		t := by[k]
		if t == nil {
			t = &tally{Seat: f.Seat, Unit: f.Unit, Name: f.Name}
			by[k] = t
		}
		t.Fires++
		if f.Moved {
			t.Moved++
		}
	}
	out := []tally{}
	for _, k := range slices.SortedFunc(maps.Keys(by), func(a, b [3]string) int { return slices.Compare(a[:], b[:]) }) {
		out = append(out, *by[k])
	}
	return map[string]any{"schedules": out}, nil
}

func rulesOf(d Data) (any, []advisor.Record) {
	type said struct {
		Class   string           `json:"class"`
		Subject advisor.Subject  `json:"subject"`
		Item    string           `json:"item,omitempty"`
		Records []advisor.Record `json:"records,omitempty"`
	}
	out := []said{}
	var records []advisor.Record
	for _, f := range d.Rules {
		out = append(out, said{Class: string(f.Class), Subject: f.Subject, Item: f.Item, Records: f.Records})
		records = append(records, f.Records...)
	}
	return map[string]any{"findings": out}, records
}

func answersOf(d Data) (any, []advisor.Record) {
	type answer struct {
		Verdict advisor.Verdict `json:"verdict"`
		Subject advisor.Subject `json:"subject"`
		At      time.Time       `json:"at"`
	}
	out := []answer{}
	for _, a := range d.Answers {
		out = append(out, answer{Verdict: a.Verdict, Subject: a.Subject, At: a.At})
	}
	return map[string]any{"answers": out}, nil
}
