package modelpass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools/toolschema"
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

// Question is one read the deep run may ask. Words is what it reads as its
// step says it to the administrator ("token spend"); the model is not told it.
type Question struct {
	Name   string `json:"name"`
	Params string `json:"params"`
	About  string `json:"about"`
	Words  string `json:"-"`
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

// maxReadBytes is the most one read is kept whole: a larger answer is
// refused with how to narrow it, so what is cited is what was read.
const maxReadBytes = 256 << 10

// A RUN'S CONTEXT IS KEPT SMALL, since every round sends it again: an answer
// past outlineBytes reaches the model as its outline (each list's first
// outlineRows rows, objects to outlineDepth levels, each cut naming where the
// rest is) while the engine keeps it whole to cite and to look into.
const (
	outlineBytes = 6 << 10
	outlineRows  = 3
	outlineDepth = 4
	// lookRows is the rows a look shows of a list before it outlines.
	lookRows = 12
)

// Progress is one line of what a deep run is doing: what step it is, whether
// it failed and why, in the engine's words, and what it asked (a read's call,
// a verdict's item and question).
type Progress struct {
	At     time.Time                  `json:"at"`
	Text   string                     `json:"text"`
	Step   advisordomain.ProgressStep `json:"step,omitempty"`
	Failed bool                       `json:"failed,omitempty"`
	Detail string                     `json:"detail,omitempty"`
	Why    string                     `json:"why,omitempty"`
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
	// Agenda is what the run had to answer and its verdict on each, as the
	// run answers it; its record keeps it as a tally ([DeepRun.Kept]).
	Agenda []AgendaItem `json:"agenda,omitempty"`
	// Tally is the agenda as a run's record keeps it, nil on the run itself.
	Tally *AgendaTally `json:"tally,omitempty"`
	// Resumed is how many agenda items came answered from a run taken over,
	// 0 for a run that started fresh.
	Resumed int `json:"resumed"`
	// Skipped is why a press's deep part did not run, as the duty records it
	// (duty.DeepKept, duty.DeepNoModel), "" where it ran; KeptFrom is when
	// the investigation a kept part keeps ran.
	Skipped  string     `json:"skipped,omitempty"`
	KeptFrom *time.Time `json:"kept_from,omitempty"`
}

// The deep run's own tools.
const (
	SnapshotTool  = "snapshot"
	ListReadsTool = "list_reads"
	ReadTool      = "read"
	ProgressTool  = "progress"
	LookTool      = "look"
	AgendaTool    = "agenda"
	VerdictTool   = "verdict"
	AddTool       = "add_to_agenda"
)

// RunDeep is one deep run over d within b, telling progress each line as it
// is written.
func RunDeep(ctx context.Context, deps Deps, d DeepData, b DeepBounds, progress func(Progress)) DeepRun {
	return RunDeepFrom(ctx, deps, d, b, progress, nil, nil)
}

// RunDeepFrom is [RunDeep] resuming from resume, a checkpoint a run taken
// over wrote (nil starts fresh), telling checkpoint where it stands each time
// that moves.
func RunDeepFrom(ctx context.Context, deps Deps, d DeepData, b DeepBounds, progress func(Progress),
	resume *Checkpoint, checkpoint func(Checkpoint)) DeepRun {
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
		progress: progress, agenda: agendaOf(d), asked: map[string]string{}, failed: map[string]bool{},
		checkpoint: checkpoint, kept: []KeptRead{}}
	if resume != nil {
		surf.agenda, out.Resumed = resumedAgenda(surf.agenda, resume.Agenda)
		surf.reads = lastRead(resume.Reads)
		surf.kept = slices.Clone(resume.Reads)
		surf.line(Progress{Step: advisordomain.ProgressNote, Text: fmt.Sprintf(
			"Picked up where it stopped: %d of %d questions already answered", out.Resumed, len(surf.agenda))})
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(b.Minutes)*time.Minute)
	defer cancel()
	res, err := toolloop.Run(runCtx, toolloop.Config{
		Provider: deps.Provider, ProviderKey: deps.ProviderKey, Surface: surf, Host: deps.Host,
		ServerTitle: "advisor", Messages: deepOpening(b, surf.agenda, resumedSaid(resume, out.Resumed)),
		MaxRounds: b.Rounds, OwnPrompt: true,
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
	out.Agenda = surf.agendaKept()
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
		out.Status, out.Reason = advisor.PassFailed, "The Advisor's model did not answer: "+stopped(ctx, err).Error()
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
	checked, refused := stated(submitted)
	grounded := advisor.Ground(checked, surf.served, deepGrounding(d, deps.DryRun, d.Answers.Holding(now())))
	for why, n := range refused {
		grounded.Dropped[why] += n
	}
	// WHAT THE MODEL REWROTE once told why counts as its own repair.
	if n := surf.reworded(); n > 0 {
		grounded.Repaired[advisor.RepairReworded] += n
	}
	kept := make([]advisor.Finding, 0, len(grounded.Findings))
	for _, f := range grounded.Findings {
		kept = append(kept, advisor.SayNumbers(f))
	}
	out.Findings, out.Dropped, out.Listed = kept, grounded.Dropped, len(kept)
	out.Repaired = grounded.Repaired
	return out
}

// deepGrounding is how a deep run's findings are checked: on cited numbers
// alone where its reads name no record, each number saying what it counts,
// and every number a headline says one of its facts.
func deepGrounding(d DeepData, dryRun func(advisor.Change) error, answered map[string]bool) advisor.Grounding {
	return advisor.Grounding{Company: d.Company, Rules: d.Rules, DryRun: dryRun, Answered: answered,
		RecordsOptional: true, Units: true, Headlines: true}
}

// stated is the submissions whose token totals say how much of them was cache
// reads, and how many of the rest were dropped and why. A JUMP IN A SEAT'S
// TOKENS IS MOST OFTEN THE CACHE: a finding about tokens cites the cache reads
// beside the total, so the split was read before the seat was blamed. A number
// that says no unit is not dropped: grounding reads its unit off where it was
// cited ([advisor.Grounding.Units]).
func stated(submitted []advisor.Submitted) ([]advisor.Submitted, map[advisor.Drop]int) {
	out := []advisor.Submitted{}
	dropped := map[advisor.Drop]int{}
	for _, sub := range submitted {
		switch {
		case tokensUnsplit(sub):
			dropped[advisor.DropTokensUnsplit]++
		default:
			out = append(out, sub)
		}
	}
	return out, dropped
}

// tokensUnsplit reports a submission citing a token number and no cache reads.
func tokensUnsplit(sub advisor.Submitted) bool {
	tokens, cache := false, false
	for _, c := range sub.Facts {
		last := c.From[strings.LastIndex(c.From, ".")+1:]
		switch {
		case strings.Contains(last, "cache_read"):
			cache = true
		case c.Unit == advisor.UnitTokens || strings.Contains(last, "token"):
			tokens = true
		}
	}
	return tokens && !cache
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
	said      int
	submitted []advisor.Submitted
	accepted  bool
	attempts  int
	// rewrites is how many submissions were sent back for their words,
	// flagged how many findings the first held that broke the rule, and
	// remaining how many the one taken still did.
	rewrites, flagged, remaining int
	stopping                     string
	agenda                       []AgendaItem
	added                        int
	// asked is each read asked, by its name and parameters, as it is cited;
	// failed is each read name that could not be read.
	asked  map[string]string
	failed map[string]bool
	// kept is every read asked, the run's taken over first, for the
	// checkpoint; checkpoint is told where the run stands, nil where nobody
	// keeps it.
	kept       []KeptRead
	checkpoint func(Checkpoint)
	// servedMu guards served, which calls side by side write and read.
	servedMu sync.Mutex
}

var _ toolloop.Surface = (*deepSurface)(nil)

func (s *deepSurface) ToolDefs() []llm.ToolDef {
	object := func(required []string, props map[string]any) map[string]any {
		return toolschema.Object(props, required...)
	}
	return []llm.ToolDef{
		{Name: SnapshotTool, Description: "The company's numbers over the last 7 days: stats beside the 7 " +
			"days before, and the charts' series (where each seat's work stood, open items by holder, hours " +
			"each model entry was out on its limit per day, what each kind of wake led to, tokens per turn). " +
			"Read it once. Cite as snapshot.<path>.", Parameters: object([]string{}, map[string]any{})},
		{Name: ListReadsTool, Description: "Every read you may ask: its name, its parameters and what it " +
			"answers.", Parameters: object([]string{}, map[string]any{})},
		{Name: ReadTool, Description: "Ask one read by name with its parameters. The answer says how to " +
			"cite it (read_N), and a number you cite must be at a path into an answer you read. A large answer " +
			"comes as its outline, kept whole as read_N: look into it rather than reading it again.",
			Parameters: object([]string{"name"}, map[string]any{
				"name":   map[string]any{"type": "string"},
				"params": map[string]any{"type": "object"}})},
		{Name: ProgressTool, Description: "Say in one short plain line what you are doing now, as the " +
			"administrator reads it, naming no tool, read or parameter, such as \"Checking why work waits\" or " +
			"\"Testing: releases pile up behind one seat\". Shown to the administrator.",
			Parameters: object([]string{"text"}, map[string]any{
				"text": map[string]any{"type": "string", "maxLength": maxProgressRunes}})},
		{Name: LookTool, Description: "Look at one part of an answer you already read, by its path, such as " +
			"read_3.entries or snapshot.charts[2].series[0]. Costs no read.",
			Parameters: object([]string{"at"}, map[string]any{"at": map[string]any{"type": "string"}})},
		{Name: AgendaTool, Description: "Your agenda as it stands: every item, and the verdict of each answered.",
			Parameters: object([]string{}, map[string]any{})},
		{Name: VerdictTool, Description: "Give one agenda item its verdict: confirmed or refuted, with the " +
			"numbers you read that show it (each cited at a path), or unreadable, naming the read that failed. " +
			"The note says the verdict in one plain line. Shown to the administrator.",
			Parameters: object([]string{"id", "verdict", "note"}, map[string]any{
				"id":      map[string]any{"type": "string"},
				"verdict": map[string]any{"enum": []string{string(VerdictConfirmed), string(VerdictRefuted), string(VerdictUnreadable)}},
				"note":    map[string]any{"type": "string", "maxLength": maxVerdictNote},
				"read":    map[string]any{"type": "string"},
				"numbers": map[string]any{"type": "array", "items": object([]string{"label", "from"}, map[string]any{
					"label": map[string]any{"type": "string"}, "from": map[string]any{"type": "string"}})}})},
		{Name: AddTool, Description: "Add a question to your agenda: a lead the numbers showed. It must then " +
			"get a verdict too.",
			Parameters: object([]string{"question"}, map[string]any{
				"question": map[string]any{"type": "string", "maxLength": maxVerdictNote}})},
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
	if name == SubmitTool || name == ProgressTool || name == VerdictTool || name == AddTool {
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
		case VerdictTool:
			return s.verdict(call.Arguments), nil
		case AddTool:
			return s.add(call.Arguments), nil
		case AgendaTool:
			return s.serve("", map[string]any{"agenda": s.agendaNow()})
		}
		if why := s.winding(); why != "" {
			return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalNotRunning,
				Output: "The investigation's " + why + " is nearly spent: call submit_findings now with what you " +
					"have found."}, nil
		}
		switch name {
		case SnapshotTool:
			return s.snapshot()
		case LookTool:
			return s.look(call.Arguments)
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
		s.servedMu.Lock()
		err = s.served.Serve(as, answer, recordsIn(raw))
		s.servedMu.Unlock()
		if err != nil {
			return toolloop.ToolResult{}, err
		}
	}
	return toolloop.ToolResult{Output: string(raw)}, nil
}

// snapshot serves the snapshot once, as its outline where it is large.
func (s *deepSurface) snapshot() (toolloop.ToolResult, error) {
	s.servedMu.Lock()
	again := s.served.Has(SnapshotTool)
	s.servedMu.Unlock()
	if again {
		return toolloop.ToolResult{Output: "You read the snapshot already: look at snapshot.<path> for any part of it."}, nil
	}
	s.line(Progress{Step: advisordomain.ProgressRead, Text: "Reading the company's numbers", Detail: SnapshotTool})
	raw, err := json.Marshal(s.data.Snapshot)
	if err != nil {
		return toolloop.ToolResult{}, err
	}
	s.servedMu.Lock()
	err = s.served.Serve(SnapshotTool, s.data.Snapshot, recordsIn(raw))
	whole, _ := s.served.Part(SnapshotTool)
	s.servedMu.Unlock()
	if err != nil {
		return toolloop.ToolResult{}, err
	}
	return s.shown(SnapshotTool, whole, len(raw))
}

// shown is answer as the model is told it, cited as as: whole where it is
// small, else its outline and where the rest is.
func (s *deepSurface) shown(as string, decoded any, size int) (toolloop.ToolResult, error) {
	if size <= outlineBytes {
		raw, err := json.Marshal(decoded)
		if err != nil {
			return toolloop.ToolResult{}, err
		}
		return toolloop.ToolResult{Output: fmt.Sprintf(`{"cite_as":%q,"answer":%s}`, as, raw)}, nil
	}
	raw, err := json.Marshal(map[string]any{"cite_as": as, "kb": size >> 10,
		"note": fmt.Sprintf("This answer is kept whole as %s; what is cut here is there. Look at %s.<path> to see "+
			"a part, rather than reading it again.", as, as),
		"outline": outline(decoded, as, outlineDepth, outlineRows)})
	if err != nil {
		return toolloop.ToolResult{}, err
	}
	return toolloop.ToolResult{Output: string(raw)}, nil
}

// look shows one part of an answer already served.
func (s *deepSurface) look(args map[string]any) (toolloop.ToolResult, error) {
	at, _ := args["at"].(string)
	s.servedMu.Lock()
	part, ok := s.served.Part(strings.TrimSpace(at))
	s.servedMu.Unlock()
	if !ok {
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalNotFound,
			Output: fmt.Sprintf("nothing you read is at %q: a path starts with snapshot or read_N", at)}, nil
	}
	raw, err := json.Marshal(part)
	if err != nil {
		return toolloop.ToolResult{}, err
	}
	if len(raw) > outlineBytes {
		for _, rows := range []int{lookRows, outlineRows} {
			if raw, err = json.Marshal(outline(part, at, outlineDepth, rows)); err != nil {
				return toolloop.ToolResult{}, err
			}
			if len(raw) <= outlineBytes {
				break
			}
		}
	}
	return toolloop.ToolResult{Output: fmt.Sprintf(`{"at":%q,"part":%s}`, at, raw)}, nil
}

// outline is v cut so a reader finds the way in it: each list's first rows
// rows and a note naming where the rest is, each object to depth levels and
// past that a note naming where it is. Every path it shows is the answer's.
func outline(v any, at string, depth, rows int) any {
	switch x := v.(type) {
	case map[string]any:
		if depth == 0 {
			return fmt.Sprintf("(%d keys: look at %s)", len(x), at)
		}
		out := make(map[string]any, len(x))
		for k, child := range x {
			out[k] = outline(child, at+"."+k, depth-1, rows)
		}
		return out
	case []any:
		if depth == 0 {
			return fmt.Sprintf("(a list of %d: look at %s)", len(x), at)
		}
		n := min(len(x), rows)
		out := make([]any, 0, n+1)
		for i := range n {
			out = append(out, outline(x[i], fmt.Sprintf("%s[%d]", at, i), depth-1, rows))
		}
		if len(x) > n {
			out = append(out, fmt.Sprintf("(%d more: look at %s[%d] and on)", len(x)-n, at, n))
		}
		return out
	}
	return v
}

// read asks one read, served as read_N.
func (s *deepSurface) read(ctx context.Context, args map[string]any) (toolloop.ToolResult, error) {
	name, _ := args["name"].(string)
	params, _ := args["params"].(map[string]any)
	if !slices.ContainsFunc(s.data.Reads.Questions(), func(q Question) bool { return q.Name == name }) {
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalNotFound,
			Output: fmt.Sprintf("%q is not a read you may ask; list_reads lists them", name)}, nil
	}
	asking := askedAs(name, params)
	s.mu.Lock()
	before, again := s.asked[asking]
	s.mu.Unlock()
	if again {
		return toolloop.ToolResult{Output: fmt.Sprintf("You read this already as %s: look at %s.<path> for any "+
			"part of it.", before, before)}, nil
	}
	said := s.readSaid(name, params)
	s.line(Progress{Step: advisordomain.ProgressRead, Text: "Reading " + said, Detail: asking})
	answer, err := s.data.Reads.Answer(ctx, name, params)
	if err != nil {
		s.mu.Lock()
		s.failed[name] = true
		s.mu.Unlock()
		s.line(Progress{Step: advisordomain.ProgressRead, Failed: true, Text: "Could not read " + said,
			Detail: asking, Why: err.Error()})
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
	s.asked[asking] = as
	s.kept = append(s.kept, KeptRead{As: as, Name: name, Params: params})
	s.mu.Unlock()
	s.keep()
	s.servedMu.Lock()
	err = s.served.ServeRead(as, advisor.ReadSource{Read: name, Params: params}, answer, recordsIn(raw))
	whole, _ := s.served.Part(as)
	s.servedMu.Unlock()
	if err != nil {
		return toolloop.ToolResult{}, err
	}
	return s.shown(as, whole, len(raw))
}

// askedAs is one read as the run keeps it asked: its name and its
// parameters in order ("work_items assignee=sre status_group=active").
func askedAs(name string, params map[string]any) string {
	parts := []string{name}
	for _, k := range slices.Sorted(maps.Keys(params)) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, params[k]))
	}
	return strings.Join(parts, " ")
}

// readSaid is one read as its step says it: what it reads, then whose and
// over when where its parameters say ("token spend for sre, last 7 days").
// Paging, limits and ids say nothing to a reader and are left out.
func (s *deepSurface) readSaid(name string, params map[string]any) string {
	words := name
	for _, q := range s.data.Reads.Questions() {
		if q.Name == name && q.Words != "" {
			words = q.Words
		}
	}
	arg := func(k string) string {
		v, ok := params[k]
		if !ok || v == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	for _, k := range []string{"seat", "assignee", "name"} {
		if v := arg(k); v != "" {
			words += " for " + v
			break
		}
	}
	var scope []string
	if v := arg("status_group"); v != "" {
		scope = append(scope, strings.ReplaceAll(v, "_", " "))
	}
	if v := arg("week"); v != "" {
		scope = append(scope, v)
	}
	days := arg("days")
	if r := strings.TrimSuffix(arg("range"), "d"); days == "" && r != arg("range") {
		days = r
	}
	switch since, until := arg("since"), arg("until"); {
	case since != "" && until != "":
		scope = append(scope, since+" to "+until)
	case days != "" && arg("previous") == "true":
		scope = append(scope, "the "+days+" days before")
	case days != "":
		scope = append(scope, "last "+days+" days")
	}
	if len(scope) == 0 {
		return words
	}
	return words + ", " + strings.Join(scope, ", ")
}

// say keeps one progress line and tells it.
func (s *deepSurface) say(args map[string]any) toolloop.ToolResult {
	text, _ := args["text"].(string)
	text = strings.TrimSpace(text)
	s.mu.Lock()
	full := s.said >= maxProgressLines
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
	s.mu.Lock()
	s.said++
	s.mu.Unlock()
	s.line(Progress{Step: advisordomain.ProgressNote, Text: text})
	return toolloop.ToolResult{Output: "shown"}
}

// maxRunLines bounds every progress line of a run, the engine's included: as
// many as one read of its progress answers, so its whole trail is read.
const maxRunLines = advisordomain.MaxProgressLines

// line keeps one step and tells it, its text cut to one plain short line and
// its detail and why to one line within the log's bound.
func (s *deepSurface) line(p Progress) {
	text := strings.Join(strings.Fields(p.Text), " ")
	if r := []rune(text); len(r) > maxProgressRunes {
		text = string(r[:maxProgressRunes-1]) + "…"
	}
	line := Progress{At: s.now().UTC(), Text: text, Step: p.Step, Failed: p.Failed,
		Detail: cut(strings.Join(strings.Fields(p.Detail), " "), advisordomain.MaxProgressBytes),
		Why:    cut(strings.Join(strings.Fields(p.Why), " "), advisordomain.MaxProgressBytes)}
	s.mu.Lock()
	if len(s.lineList) >= maxRunLines {
		s.mu.Unlock()
		return
	}
	s.lineList = append(s.lineList, line)
	s.mu.Unlock()
	if s.progress != nil {
		s.progress(line)
	}
}

// plainLine reports one plain line of at most n characters: no link, no
// markup, no line break.
func plainLine(text string, n int) bool {
	return text != "" && len([]rune(text)) <= n && !strings.Contains(text, "://") && !strings.ContainsAny(text, "<>`\n")
}

// verdict records one agenda item's verdict, or answers why it cannot.
func (s *deepSurface) verdict(args map[string]any) toolloop.ToolResult {
	refuse := func(why string) toolloop.ToolResult {
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalInvalid, Output: why}
	}
	id, _ := args["id"].(string)
	v, _ := args["verdict"].(string)
	note, _ := args["note"].(string)
	read, _ := args["read"].(string)
	note, read = strings.TrimSpace(note), strings.TrimSpace(read)
	if !plainLine(note, maxVerdictNote) {
		return refuse(fmt.Sprintf("the note is one plain line of at most %d characters", maxVerdictNote))
	}
	numbers := []AgendaNumber{}
	list, _ := args["numbers"].([]any)
	for _, n := range list {
		m, _ := n.(map[string]any)
		label, _ := m["label"].(string)
		from, _ := m["from"].(string)
		s.servedMu.Lock()
		value, ok := s.served.Number(from)
		s.servedMu.Unlock()
		if !ok || strings.TrimSpace(label) == "" {
			return refuse(fmt.Sprintf("%q holds no whole number you read: cite a path into an answer, such as "+
				"read_2.rows[0].open, with a label", from))
		}
		numbers = append(numbers, AgendaNumber{Label: strings.TrimSpace(label), Value: value, From: from})
	}
	s.mu.Lock()
	at := slices.IndexFunc(s.agenda, func(it AgendaItem) bool { return it.ID == id })
	failed := s.failed[read]
	s.mu.Unlock()
	switch {
	case at < 0:
		return refuse(fmt.Sprintf("there is no agenda item %q: the agenda tool lists them", id))
	case (Verdict(v) == VerdictConfirmed || Verdict(v) == VerdictRefuted) && len(numbers) == 0:
		return refuse("a verdict of " + v + " rests on numbers you read: cite at least one")
	case Verdict(v) == VerdictUnreadable && !failed:
		return refuse("a verdict of unreadable names, as read, a read you asked that failed")
	case Verdict(v) != VerdictConfirmed && Verdict(v) != VerdictRefuted && Verdict(v) != VerdictUnreadable:
		return refuse("a verdict is confirmed, refuted or unreadable")
	}
	if Verdict(v) != VerdictUnreadable {
		read = ""
	}
	s.mu.Lock()
	s.agenda[at].Verdict, s.agenda[at].Note, s.agenda[at].Numbers, s.agenda[at].Read = Verdict(v), note, numbers, read
	asked := s.agenda[at].ID + " " + s.agenda[at].Question
	left := open(s.agenda)
	s.mu.Unlock()
	s.keep()
	s.line(Progress{Step: advisordomain.ProgressVerdict, Failed: Verdict(v) == VerdictUnreadable,
		Text: verdictWords[Verdict(v)] + ": " + note, Detail: asked})
	return toolloop.ToolResult{Output: fmt.Sprintf("recorded; %d items still have no verdict", len(left))}
}

// verdictWords is each verdict as its step says it.
var verdictWords = map[Verdict]string{
	VerdictConfirmed:  "Confirmed",
	VerdictRefuted:    "Ruled out",
	VerdictUnreadable: "Could not check",
}

// add puts one of the run's own questions on its agenda.
func (s *deepSurface) add(args map[string]any) toolloop.ToolResult {
	question, _ := args["question"].(string)
	question = strings.TrimSpace(question)
	if !plainLine(question, maxVerdictNote) {
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalInvalid,
			Output: fmt.Sprintf("a question is one plain line of at most %d characters", maxVerdictNote)}
	}
	s.mu.Lock()
	if s.added >= maxAgendaAdded {
		s.mu.Unlock()
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalNotRunning,
			Output: "this run has added enough items; answer the agenda you have"}
	}
	s.added++
	id := fmt.Sprintf("A%d", len(s.agenda)+1)
	s.agenda = append(s.agenda, AgendaItem{ID: id, Source: AgendaModel, Question: question, Numbers: []AgendaNumber{}})
	s.mu.Unlock()
	s.keep()
	s.line(Progress{Step: advisordomain.ProgressQuestion, Text: "New question: " + question, Detail: id})
	return toolloop.ToolResult{Output: fmt.Sprintf("added as %s; it needs a verdict too", id)}
}

// keep tells the checkpoint where the run stands now.
func (s *deepSurface) keep() {
	if s.checkpoint == nil {
		return
	}
	s.mu.Lock()
	c := Checkpoint{Agenda: slices.Clone(s.agenda), Reads: slices.Clone(s.kept)}
	s.mu.Unlock()
	s.checkpoint(c)
}

// agendaNow is the agenda as it stands.
func (s *deepSurface) agendaNow() []AgendaItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.agenda)
}

// agendaKept is the agenda as the run keeps it: an item still without a
// verdict is open, the run's bounds having run out on it.
func (s *deepSurface) agendaKept() []AgendaItem {
	out := s.agendaNow()
	for i := range out {
		if out[i].Verdict == "" {
			out[i].Verdict = VerdictOpen
		}
	}
	return out
}

// lines is every progress line the run wrote.
func (s *deepSurface) lines() []Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.lineList)
}

// submit takes the run's findings, or answers why it cannot.
func (s *deepSurface) submit(args map[string]any) toolloop.ToolResult {
	winding := s.winding()
	s.mu.Lock()
	defer s.mu.Unlock()
	if left := open(s.agenda); len(left) > 0 && winding == "" {
		ids := make([]string, len(left))
		for i, it := range left {
			ids[i] = it.ID
		}
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalNotRunning,
			Output: fmt.Sprintf("These agenda items have no verdict yet: %s. Give each one a verdict (confirmed or "+
				"refuted with the numbers you read, or unreadable with the read that failed), then submit. You may "+
				"add items; you may not leave one out.", strings.Join(ids, ", "))}
	}
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
	// WORDS THE RULE REFUSES GO BACK WITH ITS REASONS, a bounded number of
	// times, so the model rewrites them rather than lose what it found; what
	// still breaks the rule after that is said by the engine from its facts.
	said, flagged := s.precheck(sub.Findings)
	if flagged > 0 && s.rewrites < maxRewrites && winding == "" {
		s.rewrites++
		s.attempts--
		if s.flagged == 0 {
			s.flagged = flagged
		}
		return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalInvalid,
			Output: "Rewrite these findings and submit all of them again; every number stands as you cited it. " +
				said}
	}
	s.remaining = flagged
	s.submitted, s.accepted = sub.Findings, true
	return toolloop.ToolResult{Output: "received"}
}

// maxRewrites is how many times a submission is sent back for its words.
const maxRewrites = 2

// precheck is what in findings breaks the wording rule, in words a model
// rewrites them by, and how many findings it is in.
func (s *deepSurface) precheck(findings []advisor.Submitted) (string, int) {
	g := deepGrounding(s.data, nil, nil)
	var said []string
	flagged := 0
	for i, f := range findings {
		problems := advisor.Precheck(f, g)
		if tokensUnsplit(f) {
			problems = append(problems, "it cites tokens without the cache reads: read the split and cite "+
				"cache_read_tokens beside the total")
		}
		if len(problems) == 0 {
			continue
		}
		flagged++
		title := []rune(strings.Join(strings.Fields(f.Title), " "))
		said = append(said, fmt.Sprintf("Finding %d (%q): %s.", i+1, string(title[:min(len(title), 60)]),
			strings.Join(problems, "; ")))
	}
	return strings.Join(said, " "), flagged
}

// reworded is how many findings the model rewrote once told why: those sent
// back for their words, less those still breaking the rule when taken.
func (s *deepSurface) reworded() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return max(0, s.flagged-s.remaining)
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
- You are given an agenda: the numbers that moved the wrong way most, what every open rule finding says, and what
  the rules could not read. Answer every item with the verdict tool: confirmed or refuted with the numbers you read
  that show it, or unreadable naming the read that failed. Add an item with add_to_agenda for a lead the numbers
  show; never leave one out. submit_findings is refused while an item has no verdict, unless your time is nearly
  spent.
- Start with snapshot (once), then list_reads. Test each item against the data with read. A large answer comes as
  its outline: look into it with look rather than reading it again, and narrow a read with its parameters.
- Before you blame a seat's tokens, read the split: tokens answers input_tokens (which includes the cache),
  cache_read_tokens and output_tokens. A jump in a seat's tokens is most often cache reads, which cost a tenth as
  much; a finding about tokens cites cache_read_tokens beside the total, or it is dropped. Offer a budget or a
  cap that cuts a seat's tokens only when fresh input (input_tokens less cache_read_tokens) or output drove them;
  where cache reads did, say so, and the engine offers no budget.
- Call progress with one short line whenever you start on something new, such as "testing: releases pile up behind
  one seat". Each read and each verdict is shown on its own.
- When every item has a verdict, or when a read tells you your time is nearly spent, call submit_findings once.

How each finding is checked before anyone sees it:
- Every number in facts is cited with "from" naming where you read it: snapshot.<path> or read_N.<path>, such as
  read_3.entries[0].hits. A number no answer holds at that path drops the finding. Each says its unit (tokens,
  turns, hours, minutes, items, count, percent, bytes), or the finding is dropped, and cites the same number over
  the range before as was_from where you read it (a read's previous range).
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

// deepOpening is a deep run's first messages: its bounds and its agenda,
// said once, and where a run taken over stands.
func deepOpening(b DeepBounds, agenda []AgendaItem, resumed string) []llm.Message {
	return []llm.Message{
		{Role: llm.RoleSystem, Content: deepSystem},
		{Role: llm.RoleUser, Content: fmt.Sprintf("Investigate the company now. You have up to %d minutes, %d "+
			"rounds and %d tokens.\n\n%s%s", b.Minutes, b.Rounds, b.Tokens, agendaSaid(agenda), resumed)},
	}
}
