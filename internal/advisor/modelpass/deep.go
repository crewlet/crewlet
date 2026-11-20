package modelpass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE DEEP RUN (ADR-0166): an investigation in rounds, on the Advisor's model,
// read-only. It reads the company's snapshot, then any read on the engine's
// allowlist of read questions (the answers the dashboard reads), follows
// leads and tests them against the data, and submits findings the engine
// grounds as it grounds the daily pass's: every number cited at a path into
// an answer it read, and every number its headline says one of them. It is
// bounded by tokens, minutes and rounds; as it nears any bound its reads
// answer that time is up, so it ends by submitting what it has rather than
// being cut off mid-read.

// Reads is the engine's read questions as the deep run reads them.
type Reads interface {
	// Questions is every question the run may ask, with its parameters and
	// what it answers.
	Questions() []Question
	// Answer asks one, as the Advisor.
	Answer(ctx context.Context, name string, params map[string]any) (any, error)
}

// Question is one read the deep run may ask.
type Question struct {
	Name   string `json:"name"`
	Params string `json:"params"`
	About  string `json:"about"`
}

// DeepData is what one deep run reads.
type DeepData struct {
	Data
	Snapshot advisor.Snapshot
	Reads    Reads
}

// DeepBounds are what a deep run may spend: tokens, wall minutes and rounds.
type DeepBounds struct {
	Tokens  int
	Minutes int
	Rounds  int
}

// DefaultDeepBounds are a deep run's bounds where the company sets none.
var DefaultDeepBounds = DeepBounds{Tokens: 2_000_000, Minutes: 20, Rounds: 80}

// windDown is the share of a bound at which the reads tell the run to submit.
const windDown = 85

// maxReadBytes is the most one read answers whole: a larger answer is
// refused with how to narrow it, so what is cited is what was read.
const maxReadBytes = 48 << 10

// Progress is one line of what a deep run is doing.
type Progress struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// The stops a deep run ends at.
const (
	StoppedDone    = "done"
	StoppedTokens  = "tokens"
	StoppedMinutes = "minutes"
	StoppedRounds  = "rounds"
)

// DeepRun is how one deep run ended.
type DeepRun struct {
	advisor.Pass
	Progress  []Progress `json:"progress"`
	StoppedBy string     `json:"stopped_by"`
	// Minutes is the wall time it took.
	Minutes float64 `json:"minutes"`
	Bounds  struct {
		Tokens  int `json:"tokens"`
		Minutes int `json:"minutes"`
		Rounds  int `json:"rounds"`
	} `json:"bounds"`
}

// The deep run's own tools.
const (
	SnapshotTool  = "snapshot"
	ListReadsTool = "list_reads"
	ReadTool      = "read"
	ProgressTool  = "progress"
)

// RunDeep is one deep run over d within b, telling progress each line as it
// is written.
func RunDeep(ctx context.Context, deps Deps, d DeepData, b DeepBounds, progress func(Progress)) DeepRun {
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	start := now()
	out := DeepRun{Pass: advisor.Pass{Status: advisor.PassDone, Model: deps.ProviderKey, Ceiling: b.Tokens,
		Findings: []advisor.Finding{}, Dropped: map[advisor.Drop]int{}, Unread: append([]string{}, d.Unread...)},
		Progress: []Progress{}}
	out.Bounds.Tokens, out.Bounds.Minutes, out.Bounds.Rounds = b.Tokens, b.Minutes, b.Rounds
	meter := &ceilingMeter{inner: deps.Budget, ceiling: b.Tokens}
	surf := &deepSurface{data: d, served: advisor.NewServed(), meter: meter, bounds: b, start: start, now: now,
		progress: progress}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(b.Minutes)*time.Minute)
	defer cancel()
	res, err := toolloop.Run(runCtx, toolloop.Config{
		Provider: deps.Provider, ProviderKey: deps.ProviderKey, Surface: surf, Host: deps.Host,
		ServerTitle: "advisor", Messages: deepOpening(b), MaxRounds: b.Rounds, OwnPrompt: true,
		TerminateAfter: []string{SubmitTool}, Budget: meter, Now: deps.Now,
		LastRoundNote: "This is your last round: call submit_findings now with what you have found.",
		// NO SEAT TO FENCE: it reads only through Reads, as the Advisor, and
		// one run at a time is the duty's lease.
		Fence: nil,
	})
	roundsSpent := false
	if res != nil {
		out.InputTokens, out.OutputTokens = res.InputTokens, res.OutputTokens
		roundsSpent = res.RoundsUsed >= b.Rounds
	}
	out.Minutes = now().Sub(start).Minutes()
	out.Progress = surf.lines()
	submitted, accepted := surf.result()
	var budget *toolloop.BudgetError
	switch {
	case errors.As(err, &budget) && budget.Scope == CeilingScope:
		out.StoppedBy = StoppedTokens
	case errors.As(err, &budget):
		out.Status, out.Reason = advisor.PassFailed, "The company's token budget is spent for now, so the run stopped."
		return out
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded):
		out.StoppedBy = StoppedMinutes
	case err != nil && !accepted:
		out.Status, out.Reason = advisor.PassFailed, "The Advisor's model did not answer: "+err.Error()
		return out
	case surf.woundDown() != "":
		out.StoppedBy = surf.woundDown()
	case roundsSpent:
		out.StoppedBy = StoppedRounds
	default:
		out.StoppedBy = StoppedDone
	}
	if !accepted {
		if out.StoppedBy == StoppedDone {
			out.Status = advisor.PassDeclined
			out.Reason = "The model did not submit findings the engine could read, so nothing it found is listed."
			return out
		}
		out.Status = advisor.PassCeiling
		out.Reason = fmt.Sprintf("The run reached its %s bound before it submitted, so it lists nothing.", out.StoppedBy)
		return out
	}
	if out.StoppedBy != StoppedDone {
		out.Status = advisor.PassCeiling
		out.Reason = fmt.Sprintf("The run reached its %s bound and submitted what it had, so this list may be short.",
			out.StoppedBy)
	}
	findings, dropped := advisor.Ground(submitted, surf.served, advisor.Grounding{
		Company: d.Company, Rules: d.Rules, DryRun: deps.DryRun, Answered: d.Answers.Holding(now()),
		RecordsOptional: true})
	kept := findings[:0]
	for _, f := range findings {
		if f.Headline != "" && !headlineGrounded(f) {
			dropped[advisor.DropWords]++
			continue
		}
		kept = append(kept, f)
	}
	out.Findings, out.Dropped, out.Listed = kept, dropped, len(kept)
	return out
}

// headlineGrounded reports whether every number a headline says is one of
// its finding's cited numbers, a day of a month aside.
func headlineGrounded(f advisor.Finding) bool {
	allowed := map[int64]bool{}
	for _, fact := range f.Evidence {
		allowed[fact.Value] = true
	}
	for _, m := range headlineNumber().FindAllStringSubmatchIndex(f.Headline, -1) {
		raw := strings.ReplaceAll(f.Headline[m[2]:m[3]], ",", "")
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return false
		}
		rest := strings.TrimSpace(f.Headline[m[1]:])
		if n >= 1 && n <= 31 && monthAhead(rest) {
			continue
		}
		if !allowed[n] {
			return false
		}
	}
	return true
}

// headlineNumber is a number as a headline says it, thousands grouped or not.
// Compiled at first use, not at init: Gitlet's push hook is this binary.
var headlineNumber = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\b(\d[\d,]*)\b`)
})

// monthAhead reports whether s starts with a month's name or its first three letters.
func monthAhead(s string) bool {
	for _, m := range []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"} {
		if strings.HasPrefix(s, m) {
			return true
		}
	}
	return false
}

// deepSurface serves the deep run's reads, its progress and its submission.
type deepSurface struct {
	data     DeepData
	served   *advisor.Served
	meter    *ceilingMeter
	bounds   DeepBounds
	start    time.Time
	now      func() time.Time
	progress func(Progress)

	mu        sync.Mutex
	reads     int
	lineList  []Progress
	submitted []advisor.Submitted
	accepted  bool
	attempts  int
	stopping  string
}

var _ toolloop.Surface = (*deepSurface)(nil)

func (s *deepSurface) ToolDefs() []llm.ToolDef {
	object := func(required []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": required,
			"properties": props}
	}
	return []llm.ToolDef{
		{Name: SnapshotTool, Description: "The company's numbers over the last 7 days: stats beside the 7 " +
			"days before, and the charts' series (where each seat's work stood, open items by holder, hours " +
			"each model entry was out on its limit per day, what each kind of wake led to, tokens per turn). " +
			"Cite as snapshot.<path>.", Parameters: object([]string{}, map[string]any{})},
		{Name: ListReadsTool, Description: "Every read you may ask: its name, its parameters and what it " +
			"answers.", Parameters: object([]string{}, map[string]any{})},
		{Name: ReadTool, Description: "Ask one read by name with its parameters. The answer says how to " +
			"cite it (read_N), and a number you cite must be at a path into an answer you read.",
			Parameters: object([]string{"name"}, map[string]any{
				"name":   map[string]any{"type": "string"},
				"params": map[string]any{"type": "object"}})},
		{Name: ProgressTool, Description: "Say in one short line what you are doing now, such as " +
			"\"reading flow hours\" or \"testing: releases pile up behind one seat\". Shown to the administrator.",
			Parameters: object([]string{"text"}, map[string]any{
				"text": map[string]any{"type": "string", "maxLength": maxProgressRunes}})},
		{Name: SubmitTool, Description: submitDescription, Parameters: submitSchema()},
	}
}

// maxProgressRunes bounds one progress line, and maxProgressLines a run's.
const (
	maxProgressRunes = 120
	maxProgressLines = 60
)

func (s *deepSurface) Revision() uint64 { return 0 }

func (s *deepSurface) AnnotationsOf(name string) mcp.Annotations {
	if name == SubmitTool || name == ProgressTool {
		return mcp.Annotations{}
	}
	return mcp.Annotations{ReadOnly: mcp.Yes}
}

func (s *deepSurface) Phase() string { return "advisor_deep" }

func (s *deepSurface) Admit(name string, _ map[string]any) toolloop.Admitted {
	return func(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
		switch name {
		case SubmitTool:
			return s.submit(call.Arguments), nil
		case ProgressTool:
			return s.say(call.Arguments), nil
		}
		if why := s.winding(); why != "" {
			return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalNotRunning,
				Output: "The investigation's " + why + " is nearly spent: call submit_findings now with what you " +
					"have found."}, nil
		}
		switch name {
		case SnapshotTool:
			return s.serve(SnapshotTool, s.data.Snapshot)
		case ListReadsTool:
			return s.serve("", map[string]any{"reads": s.data.Reads.Questions()})
		case ReadTool:
			return s.read(ctx, call.Arguments)
		}
		return toolloop.ToolResult{Output: fmt.Sprintf("there is no tool %q", name), Failed: true,
			Cause: mcp.RefusalNotFound}, nil
	}
}

// winding is the bound the run is near, "" while it has room: its tokens or
// its minutes. Its rounds are the loop's own, whose last round is told to
// submit.
func (s *deepSurface) winding() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping != "" {
		return s.stopping
	}
	s.meter.mu.Lock()
	spent := s.meter.spent
	s.meter.mu.Unlock()
	switch {
	case spent*100 >= windDown*s.bounds.Tokens:
		s.stopping = StoppedTokens
	case s.now().Sub(s.start)*100 >= windDown*time.Duration(s.bounds.Minutes)*time.Minute:
		s.stopping = StoppedMinutes
	}
	return s.stopping
}

// woundDown is the bound the run wound down at, "" where it did not.
func (s *deepSurface) woundDown() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// serve answers with answer, recorded under as for citing; "" serves nothing.
func (s *deepSurface) serve(as string, answer any) (toolloop.ToolResult, error) {
	raw, err := json.Marshal(answer)
	if err != nil {
		return toolloop.ToolResult{}, err
	}
	if as != "" {
		if err := s.served.Serve(as, answer, recordsIn(raw)); err != nil {
			return toolloop.ToolResult{}, err
		}
	}
	return toolloop.ToolResult{Output: string(raw)}, nil
}

// read asks one read, served as read_N.
func (s *deepSurface) read(ctx context.Context, args map[string]any) (toolloop.ToolResult, error) {
	name, _ := args["name"].(string)
	params, _ := args["params"].(map[string]any)
	if !slices.ContainsFunc(s.data.Reads.Questions(), func(q Question) bool { return q.Name == name }) {
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalNotFound,
			Output: fmt.Sprintf("%q is not a read you may ask; list_reads lists them", name)}, nil
	}
	answer, err := s.data.Reads.Answer(ctx, name, params)
	if err != nil {
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalInvalid,
			Output: fmt.Sprintf("%s could not be read: %v", name, err)}, nil
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return toolloop.ToolResult{}, err
	}
	if len(raw) > maxReadBytes {
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalInvalid,
			Output: fmt.Sprintf("%s answered %d KB, past the %d KB one read takes: narrow it with its parameters "+
				"(a seat, a range, a limit)", name, len(raw)>>10, maxReadBytes>>10)}, nil
	}
	s.mu.Lock()
	s.reads++
	as := fmt.Sprintf("read_%d", s.reads)
	s.mu.Unlock()
	if err := s.served.ServeRead(as, advisor.ReadSource{Read: name, Params: params}, answer, recordsIn(raw)); err != nil {
		return toolloop.ToolResult{}, err
	}
	return toolloop.ToolResult{Output: fmt.Sprintf(`{"cite_as":%q,"answer":%s}`, as, raw)}, nil
}

// say keeps one progress line and tells it.
func (s *deepSurface) say(args map[string]any) toolloop.ToolResult {
	text, _ := args["text"].(string)
	text = strings.TrimSpace(text)
	s.mu.Lock()
	full := len(s.lineList) >= maxProgressLines
	s.mu.Unlock()
	if full {
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalNotRunning,
			Output: "this run has said enough progress lines; go on without them"}
	}
	if text == "" || len([]rune(text)) > maxProgressRunes || strings.Contains(text, "://") ||
		strings.ContainsAny(text, "<>`\n") {
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalInvalid,
			Output: fmt.Sprintf("a progress line is one plain line of at most %d characters", maxProgressRunes)}
	}
	line := Progress{At: s.now().UTC(), Text: text}
	s.mu.Lock()
	s.lineList = append(s.lineList, line)
	s.mu.Unlock()
	if s.progress != nil {
		s.progress(line)
	}
	return toolloop.ToolResult{Output: "shown"}
}

// lines is every progress line the run wrote.
func (s *deepSurface) lines() []Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.lineList)
}

// submit takes the run's findings, or answers why it cannot.
func (s *deepSurface) submit(args map[string]any) toolloop.ToolResult {
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
func (s *deepSurface) result() ([]advisor.Submitted, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.submitted), s.accepted
}

// recordsIn is the records an answer names: each item key and turn id in it.
func recordsIn(raw []byte) []advisor.Record {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []advisor.Record
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				walk(k, child)
			}
		case []any:
			for _, child := range x {
				walk(key, child)
			}
		case string:
			switch {
			case key == "turn_id" && x != "":
				out = append(out, advisor.Record{Kind: advisor.RecordTurn, ID: x})
			case (key == "key" || key == "keys" || key == "item") && itemKey().MatchString(x):
				out = append(out, advisor.Record{Kind: advisor.RecordItem, ID: x})
			}
		}
	}
	walk("", v)
	return out
}

// itemKey is a work item's key, PROJECT-123. Compiled at first use.
var itemKey = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Z][A-Z0-9]*-\d+$`) })

// deepSystem is what the deep run is told.
const deepSystem = `You are the Advisor of a company of AI agents and the people who direct them: its analyst. Your job is
to find what is costing the company time, tokens or waiting, with the numbers that prove it, and say it so the
administrator can act in one step.

How to work:
- Start with snapshot, then list_reads. Form hypotheses from the numbers (a model entry out on its limit for hours,
  work piling up behind one seat, a kind of wake that changes nothing, a seat whose turns cost far more than the
  rest, items blocked with no reason, people whose items block others), and test each against the data with read.
  Drop what the data does not support.
- Call progress with one short line whenever you start on something new, such as "reading capacity" or "testing:
  releases pile up behind one seat".
- When you know enough, or when a read tells you your time is nearly spent, call submit_findings once.

How each finding is checked before anyone sees it:
- Every number in facts is cited with "from" naming where you read it: snapshot.<path> or read_N.<path>, such as
  read_3.entries[0].hits. A number no answer holds at that path drops the finding.
- The headline is one line of at most 120 characters that leads with the number, such as "4 seats sat out 11 h on
  19 Nov: main hit its usage limit 3 times". Every number it says must be one of its facts (a day of a month aside).
- Cite records (item keys, turn ids) where an answer named them. Never repeat what rule_findings lists.
- Give each finding a premise: the one number it rests on, cited from a read (not the snapshot) as
  {"from": "read_N.<path>", "op": "ge", "value": 3} (op is eq, ne, ge, gt, le or lt), which must hold now. The
  engine asks that read again every hour and resolves the finding once its premise no longer holds.
- One finding per cause: put every affected seat in one finding rather than one finding per seat.
- Offer only a fix the engine builds: round_cap or token_budget for an agent seat, auxiliary_model with a
  providers.llm entry, open_item with an item key you cite, open_seat with an agent's handle, or none.
- Title at most 120 characters, why at most 600, plain sentences, no links or markup. Severity is costs or note.`

// deepOpening is a deep run's first messages.
func deepOpening(b DeepBounds) []llm.Message {
	return []llm.Message{
		{Role: llm.RoleSystem, Content: deepSystem},
		{Role: llm.RoleUser, Content: fmt.Sprintf("Investigate the company now. You have up to %d minutes, %d "+
			"rounds and %d tokens.", b.Minutes, b.Rounds, b.Tokens)},
	}
}
