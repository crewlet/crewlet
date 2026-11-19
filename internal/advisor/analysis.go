package advisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// THE ANALYST'S FINDINGS (ADR-0157): nothing a model submits is listed until every
// record and number it cites is one the run's own tools answered and the engine
// has built its fix. No model and no I/O here; internal/advisor/analyst runs it.

// SourceModel is a finding the Advisor's model submitted and the engine grounded.
const SourceModel Source = "model"

// ClassModel is every model finding's class: what it is about is its words,
// and its identity its [Fingerprint].
const ClassModel Class = "model.finding"

// PassStatus is how a model pass ended.
type PassStatus string

// The ways a pass ends.
const (
	// PassOff is a company that assigns the Advisor no model.
	PassOff PassStatus = "off"
	// PassDone read every tool it asked for and submitted, its findings checked.
	PassDone PassStatus = "done"
	// PassCeiling stopped at its token ceiling: what it grounded so far is listed.
	PassCeiling PassStatus = "ceiling"
	// PassDeclined is a model that declined, or answered outside the schema twice.
	PassDeclined PassStatus = "declined"
	// PassFailed could not run: no model answered, or the company's budget was spent.
	PassFailed PassStatus = "failed"
)

// Pass is how one model pass ended, as its run records it.
type Pass struct {
	Status PassStatus `json:"status"`
	// Model is the providers.llm entry the pass ran on, and Ceiling its token ceiling.
	Model   string `json:"model,omitempty"`
	Ceiling int    `json:"ceiling,omitempty"`
	// InputTokens and OutputTokens are what every round together spent.
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Findings are the grounded ones, Listed how many, Dropped how many were not and why.
	Findings []Finding    `json:"-"`
	Listed   int          `json:"listed"`
	Dropped  map[Drop]int `json:"dropped"`
	Unread   []string     `json:"unread"`
	Reason   string       `json:"reason,omitempty"`
}

// Drop is why a submitted finding was not listed.
type Drop string

// The reasons.
const (
	DropNoEvidence Drop = "no_evidence"
	DropUnread     Drop = "unread_record"
	DropWrongFact  Drop = "wrong_number"
	DropRepeat     Drop = "repeats_a_rule"
	DropFix        Drop = "fix_not_built"
	DropWords      Drop = "words"
	DropAnswered   Drop = "answered"
)

// Submitted is one finding as the model submits it.
type Submitted struct {
	Title    string       `json:"title"`
	Why      string       `json:"why"`
	Severity Severity     `json:"severity"`
	Subject  Subject      `json:"subject"`
	Item     string       `json:"item"`
	Facts    []CitedFact  `json:"facts"`
	Records  []Record     `json:"records"`
	Fix      SubmittedFix `json:"fix"`
}

// CitedFact is one number and where in the run's own tool answers it was read:
// a tool's name, then the path into its answer (open_work.blockers[0].blocks).
type CitedFact struct {
	Key   string `json:"key"`
	Value int64  `json:"value"`
	From  string `json:"from"`
}

// FixBuilder names a change the engine builds for an analyst finding.
type FixBuilder string

// The builders: the advisor's own changes, and the pages a fix may open.
const (
	BuildRoundCap       FixBuilder = "round_cap"
	BuildTokenBudget    FixBuilder = "token_budget"
	BuildAuxiliaryModel FixBuilder = "auxiliary_model"
	BuildOpenItem       FixBuilder = "open_item"
	BuildOpenSeat       FixBuilder = "open_seat"
	BuildNone           FixBuilder = "none"
)

// FixBuilders is every builder.
func FixBuilders() []FixBuilder {
	return []FixBuilder{BuildRoundCap, BuildTokenBudget, BuildAuxiliaryModel, BuildOpenItem, BuildOpenSeat, BuildNone}
}

// SubmittedFix is the change the model asks the engine to build.
type SubmittedFix struct {
	Builder FixBuilder `json:"builder"`
	Seat    string     `json:"seat,omitempty"`
	Period  string     `json:"period,omitempty"`
	Value   int        `json:"value,omitempty"`
	Entry   string     `json:"entry,omitempty"`
	Target  string     `json:"target,omitempty"`
}

// Served is everything the run's tools answered, by tool: what a cited record
// or number is checked against.
type Served struct {
	answers map[string]any
	records map[Record]bool
}

// NewServed is an empty record of what a run's tools answered.
func NewServed() *Served {
	return &Served{answers: map[string]any{}, records: map[Record]bool{}}
}

// Serve records one tool's answer and the records in it.
func (s *Served) Serve(tool string, answer any, records []Record) error {
	raw, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	s.answers[tool] = decoded
	for _, r := range records {
		s.records[r] = true
	}
	return nil
}

// read is the number at from in the answers, false where none is.
func (s *Served) read(from string) (int64, bool) {
	tool, path, _ := strings.Cut(from, ".")
	v, ok := s.answers[tool]
	if !ok {
		return 0, false
	}
	for path != "" {
		var step string
		step, path, _ = strings.Cut(path, ".")
		name, index, indexed := strings.Cut(strings.TrimSuffix(step, "]"), "[")
		if name != "" {
			var m map[string]any
			if m, ok = v.(map[string]any); !ok {
				return 0, false
			}
			if v, ok = m[name]; !ok {
				return 0, false
			}
		}
		if indexed {
			var list []any
			list, ok = v.([]any)
			i, err := strconv.Atoi(index)
			if !ok || err != nil || i < 0 || i >= len(list) {
				return 0, false
			}
			v = list[i]
		}
	}
	n, ok := v.(float64)
	if !ok || n != float64(int64(n)) {
		return 0, false
	}
	return int64(n), true
}

// Grounding is what a submitted finding is checked against beside the answers.
type Grounding struct {
	Company *config.Company
	// Rules are the rule-based findings of the same answer, which the analyst never repeats.
	Rules []Finding
	// DryRun is the entity route's own check of a change, in-process.
	DryRun func(Change) error
	// Answered are the fingerprints people answered wrong or already known, still in force.
	Answered map[string]bool
}

// Bounds of an analyst finding's words.
const (
	MaxAnalystTitle = 120
	MaxAnalystWhy   = 600
	// MaxAnalystFindings is the most findings one run lists.
	MaxAnalystFindings = 10
)

// Ground is the submitted findings the served answers and g support, as
// findings, and how many of the rest were dropped and why.
func Ground(submitted []Submitted, served *Served, g Grounding) ([]Finding, map[Drop]int) {
	out := []Finding{}
	dropped := map[Drop]int{}
	listed := map[string]bool{}
	for _, s := range submitted {
		f, drop := ground(s, served, g)
		if drop == "" && listed[Fingerprint(f)] {
			drop = DropRepeat
		}
		switch {
		case drop != "":
			dropped[drop]++
		case len(out) == MaxAnalystFindings:
			dropped[DropWords]++
		default:
			listed[Fingerprint(f)] = true
			out = append(out, f)
		}
	}
	sortFindings(out)
	return out, dropped
}

func ground(s Submitted, served *Served, g Grounding) (Finding, Drop) {
	if len(s.Records) == 0 || len(s.Facts) == 0 {
		return Finding{}, DropNoEvidence
	}
	records := []Record{}
	for _, r := range s.Records {
		if !served.records[r] {
			return Finding{}, DropUnread
		}
		if len(records) < MaxAnalystRecords && !slices.Contains(records, r) {
			records = append(records, r)
		}
	}
	facts := []Fact{}
	for _, c := range s.Facts {
		if got, ok := served.read(c.From); !ok || got != c.Value || c.Key == "" {
			return Finding{}, DropWrongFact
		}
		facts = append(facts, Fact{Key: c.Key, Value: c.Value})
	}
	if !plainWords(s.Title, MaxAnalystTitle) || !plainWords(s.Why, MaxAnalystWhy) ||
		!plainWords(s.Item, MaxAnalystTitle) || (s.Severity != SeverityCosts && s.Severity != SeverityNote) {
		return Finding{}, DropWords
	}
	subject, ok := subjectIn(g.Company, s.Subject)
	if !ok {
		return Finding{}, DropWords
	}
	fix, ok := buildFix(s.Fix, served, g)
	if !ok {
		return Finding{}, DropFix
	}
	f := Finding{Class: ClassModel, Severity: s.Severity, Actor: ActorOwner, Source: SourceModel,
		Area: AreaWork, Subject: subject, Title: s.Title, Why: s.Why, Item: s.Item,
		Evidence: facts, Records: records, Fix: fix}
	f.Focus = Fingerprint(f)
	if repeats(f, g.Rules) {
		return Finding{}, DropRepeat
	}
	if g.Answered[Fingerprint(f)] {
		return Finding{}, DropAnswered
	}
	return f, ""
}

// MaxAnalystRecords bounds the records one analyst finding names.
const MaxAnalystRecords = 5

// plainWords holds a line of words to its bound: no address, no markup, no
// control character, so it is read as text wherever it is drawn.
func plainWords(s string, limit int) bool {
	if utf8.RuneCountInString(s) > limit || strings.Contains(s, "://") || strings.ContainsAny(s, "<>`[]") {
		return false
	}
	for _, r := range s {
		if r < ' ' {
			return false
		}
	}
	return true
}

// subjectIn is the submitted subject as the chart names it, false where it
// names a seat the chart does not hold.
func subjectIn(c *config.Company, s Subject) (Subject, bool) {
	switch s.Kind {
	case SubjectCompany:
		return Subject{Kind: SubjectCompany}, true
	case SubjectSeat:
		if role, _ := roleOf(c, s.Seat); role != nil {
			return Subject{Kind: SubjectSeat, Seat: s.Seat, Name: role.Name}, true
		}
	case SubjectRepository:
		if plainWords(s.Name, MaxAnalystTitle) && s.Name != "" {
			return Subject{Kind: SubjectRepository, Name: s.Name}, true
		}
	}
	return Subject{}, false
}

// buildFix is the change the engine builds for f, checked as the entity
// route checks it; false where it cannot be built or does not check.
func buildFix(f SubmittedFix, served *Served, g Grounding) (Fix, bool) {
	none := Fix{Kind: FixNone, Options: []Option{}, Sentence: "Nothing to send: open the records to look."}
	switch f.Builder {
	case BuildNone, "":
		return none, true
	case BuildOpenItem:
		if !served.records[Record{Kind: RecordItem, ID: f.Target}] {
			return Fix{}, false
		}
		return Fix{Kind: FixOpenPage, Page: PageItem, Target: f.Target, Options: []Option{},
			Sentence: "Open the item."}, true
	case BuildOpenSeat:
		if role, _ := roleOf(g.Company, f.Target); role == nil || role.Kind == org.KindHuman {
			return Fix{}, false
		}
		return Fix{Kind: FixOpenPage, Page: PageSeat, Target: f.Target, Options: []Option{},
			Sentence: "Open the agent."}, true
	}
	role, _ := roleOf(g.Company, f.Seat)
	if role == nil || role.Kind == org.KindHuman {
		return Fix{}, false
	}
	var fix Fix
	switch f.Builder {
	case BuildRoundCap:
		if f.Value < 0 {
			return Fix{}, false
		}
		fix = Fix{Kind: FixConfigChange, Sentence: fmt.Sprintf("Change %s's round cap.", role.Name),
			Options: []Option{{Label: fmt.Sprintf("Set the cap to %d rounds", f.Value),
				Change: Change{Entity: EntityRoles, ID: f.Seat, Set: map[string]any{},
					Merge:   map[string]any{"sandbox": map[string]any{"max_turns": f.Value}},
					Summary: fmt.Sprintf("Set %s's round cap to %d", f.Seat, f.Value)}}}}
	case BuildTokenBudget:
		if f.Value <= 0 || !slices.Contains([]string{"day", "week", "month"}, f.Period) {
			return Fix{}, false
		}
		fix = Fix{Kind: FixConfigChange, Sentence: fmt.Sprintf("Change %s's %s budget.", role.Name, periodWord(f.Period)),
			Options: []Option{{Label: fmt.Sprintf("Set its %s budget to %s tokens", periodWord(f.Period), tokensSaid(int64(f.Value))),
				Change: Change{Entity: EntityRoles, ID: f.Seat, Set: map[string]any{},
					Merge:   map[string]any{"token_budget": map[string]any{f.Period: f.Value}},
					Summary: fmt.Sprintf("Set %s's %s token budget to %d", f.Seat, periodWord(f.Period), f.Value)}}}}
	case BuildAuxiliaryModel:
		if _, ok := g.Company.Providers.LLM[f.Entry]; !ok {
			return Fix{}, false
		}
		fix = Fix{Kind: FixConfigChange, Options: []Option{{Label: fmt.Sprintf("Learn on %s", g.Company.ModelName(f.Entry)),
			Change: Change{Entity: EntityRoles, ID: f.Seat, Set: map[string]any{"llm_auxiliary": org.ProviderKeys{f.Entry}},
				Summary: fmt.Sprintf("Run %s's learning on providers.llm.%s", f.Seat, f.Entry)}}},
			Sentence: fmt.Sprintf("Run %s's learning on another model.", role.Name)}
	default:
		return Fix{}, false
	}
	if g.DryRun == nil || g.DryRun(fix.Options[0].Change) != nil {
		return Fix{}, false
	}
	return fix, true
}

// repeats reports whether f says what a rule finding already says: every record
// it cites is cited by one rule finding about the same subject, or its change
// is one a rule finding offers.
func repeats(f Finding, rules []Finding) bool {
	for _, r := range rules {
		if r.Subject.Kind == f.Subject.Kind && r.Subject.Seat == f.Subject.Seat && r.Subject.Name == f.Subject.Name &&
			len(f.Records) > 0 && !slices.ContainsFunc(f.Records, func(x Record) bool { return !slices.Contains(r.Records, x) }) {
			return true
		}
		for _, o := range r.Fix.Options {
			for _, mine := range f.Fix.Options {
				if reflect.DeepEqual(o.Change.Merge, mine.Change.Merge) && reflect.DeepEqual(o.Change.Set, mine.Change.Set) &&
					o.Change.Entity == mine.Change.Entity && o.Change.ID == mine.Change.ID {
					return true
				}
			}
		}
	}
	return false
}

// Fingerprint names an analyst finding across runs: its subject, the records
// it cites and its kind of fix, never its words, which a model rephrases.
func Fingerprint(f Finding) string {
	ids := make([]string, 0, len(f.Records))
	for _, r := range f.Records {
		ids = append(ids, string(r.Kind)+":"+r.ID)
	}
	slices.Sort(ids)
	sum := sha256.Sum256([]byte(strings.Join(append([]string{string(f.Subject.Kind), f.Subject.Seat,
		f.Subject.Name, string(f.Fix.Kind)}, ids...), "\x00")))
	return hex.EncodeToString(sum[:8])
}

// Verdict is why a person dismissed a model finding, as its next pass is told.
type Verdict string

// The two verdicts a pass is told: a finding that was wrong, and one already known.
const (
	VerdictWrong Verdict = "wrong"
	VerdictKnown Verdict = "known"
)

// AnswerState is one person's dismissal of one model finding, by fingerprint,
// with what it said so the next pass can be told.
type AnswerState struct {
	Fingerprint string    `json:"fingerprint"`
	Verdict     Verdict   `json:"verdict"`
	By          string    `json:"by"`
	At          time.Time `json:"at"`
	Title       string    `json:"title"`
	Subject     Subject   `json:"subject"`
}

// Answers is every dismissal a pass is told of.
type Answers []AnswerState

// Holding is the fingerprints a pass leaves off: every one dismissed as wrong
// or known, whose key [HoldFor] still holds.
func (a Answers) Holding(now time.Time) map[string]bool {
	out := map[string]bool{}
	for _, x := range a {
		reason := ReasonDismissedKnown
		if x.Verdict == VerdictWrong {
			reason = ReasonDismissedWrong
		}
		if span, forGood := HoldFor(reason); forGood || now.Sub(x.At) < span {
			out[x.Fingerprint] = true
		}
	}
	return out
}
