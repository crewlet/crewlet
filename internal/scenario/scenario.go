// Package scenario holds synthetic timelines that reproduce the shape of a
// measured operating pattern in a closed vocabulary ([CheckTree]), for the
// seams they exercise to drive through their real writers. Only tests import it.
package scenario

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// Timelines is every timeline this package ships, one JSON file each.
//
//go:embed timelines/*.json
var Timelines embed.FS

// Timeline is one synthetic sequence of steps on a company's items.
type Timeline struct {
	// Name is the file's stem.
	Name string `json:"name"`

	// Pattern is what the timeline reproduces, from [Patterns].
	Pattern Pattern `json:"pattern"`

	// Seats are the handles the timeline speaks as and about.
	Seats []Seat `json:"seats"`

	// Steps run in order; At never goes backwards.
	Steps []Step `json:"steps"`
}

// Pattern names what a timeline reproduces.
type Pattern string

// The patterns a timeline may declare. A new one joins this list with the
// timeline that reproduces it.
const (
	// PatternHealthyCompany is the control: hand-offs, asks and answers that
	// must keep waking the seats they concern whatever routing rule changes.
	PatternHealthyCompany Pattern = "healthy_company"
	// PatternAgentHandoffs is agents handing work to each other in comments
	// that say they ask: every one wakes, and no detector may count it.
	PatternAgentHandoffs Pattern = "agent_handoffs"
	// PatternAgentMentionsWithoutAsk is agents naming each other in comments
	// that ask nothing, each one waking the other.
	PatternAgentMentionsWithoutAsk Pattern = "agent_mentions_without_ask"
	// PatternPersonRequestsWithoutAsk is an agent naming a person again and
	// again with no question, so nothing reaches their Inbox as a decision.
	PatternPersonRequestsWithoutAsk Pattern = "person_requests_without_ask"
	// PatternAskOvertaken is a person answering questions that were asked
	// again since, or whose item had already finished.
	PatternAskOvertaken Pattern = "ask_overtaken"
	// PatternReviewIdle is an item in review its agent holder never takes a
	// turn on.
	PatternReviewIdle Pattern = "review_idle"

	// PatternApprovalRubberStamp is a person choosing one agent's
	// recommended option on almost every decision it asks of them.
	PatternApprovalRubberStamp Pattern = "approval_rubber_stamp"
)

// Patterns is every [Pattern] a timeline may declare.
var Patterns = []Pattern{PatternHealthyCompany, PatternAgentHandoffs, PatternAgentMentionsWithoutAsk,
	PatternPersonRequestsWithoutAsk, PatternAskOvertaken, PatternReviewIdle, PatternApprovalRubberStamp}

// Valid reports whether p is a declared pattern.
func (p Pattern) Valid() bool { return slices.Contains(Patterns, p) }

// SeatKind says whether a seat is an agent or a person.
type SeatKind string

// The two kinds of seat.
const (
	SeatAgent  SeatKind = "agent"
	SeatPerson SeatKind = "person"
)

// Valid reports whether k is one of the two kinds.
func (k SeatKind) Valid() bool { return k == SeatAgent || k == SeatPerson }

// Seat is one participant.
type Seat struct {
	Handle string   `json:"handle"`
	Kind   SeatKind `json:"kind"`
}

// Verb is what a step does.
type Verb string

// The verbs a step may take.
const (
	// VerbFile creates the item, assigned to Assignee, reported by Actor.
	VerbFile Verb = "file"
	// VerbComment posts a comment saying Say, naming Mentions, asking Ask,
	// answering the comment of step Answers.
	VerbComment Verb = "comment"
	// VerbAssign hands the item to Assignee.
	VerbAssign Verb = "assign"
	// VerbStatus moves the item to Status, handing it to Assignee in the same
	// change when one is named.
	VerbStatus Verb = "status"
	// VerbWatch adds Watcher to the item's watchers.
	VerbWatch Verb = "watch"
	// VerbTurn is a turn Actor takes on the item, charged to it.
	VerbTurn Verb = "turn"
)

// Verbs is every [Verb].
var Verbs = []Verb{VerbFile, VerbComment, VerbAssign, VerbStatus, VerbWatch, VerbTurn}

// Valid reports whether v is a declared verb.
func (v Verb) Valid() bool { return slices.Contains(Verbs, v) }

// Step is one action by one seat on one item.
type Step struct {
	// At orders the step, in seconds from the timeline's start. A driver stamps
	// records with the clock the broker stamps them by, so At says only what
	// comes first, never how long passed.
	At int `json:"at"`

	Actor string `json:"actor"`
	Verb  Verb   `json:"verb"`
	Item  string `json:"item"`

	// Say is the class of words a comment carries; [Phrase] renders it.
	Say PhraseClass `json:"say,omitempty"`

	Mentions []string `json:"mentions,omitempty"`
	Ask      string   `json:"ask,omitempty"`
	// Asks is an agent author's statement that the comment asks the seats it
	// names, as the comment tool requires one; absent is an older build's record.
	Asks *bool `json:"asks,omitempty"`

	// Answers is the index of the earlier comment step this comment answers.
	Answers *int `json:"answers,omitempty"`

	// Recommend makes a question to a person a decision between [Choices],
	// recommending this one; Choose is the option an answer to one chose.
	Recommend string `json:"recommend,omitempty"`
	Choose    string `json:"choose,omitempty"`

	Assignee string `json:"assignee,omitempty"`
	Status   string `json:"status,omitempty"`
	Watcher  string `json:"watcher,omitempty"`

	// Expect is what the step must cause, read from the records it wrote.
	Expect Expect `json:"expect"`
}

// Expect is a step's declared outcome.
type Expect struct {
	// Woken is every agent seat the step's notice starts a turn for, sorted.
	// Empty means the step wakes no agent.
	Woken []string `json:"woken"`
}

// Named loads one shipped timeline by its stem.
func Named(name string) (Timeline, error) {
	return Load(Timelines, path.Join("timelines", name+".json"))
}

// All loads every shipped timeline, in file order.
func All() ([]Timeline, error) {
	entries, err := fs.ReadDir(Timelines, "timelines")
	if err != nil {
		return nil, fmt.Errorf("scenario: list the timelines: %w", err)
	}
	out := make([]Timeline, 0, len(entries))
	for _, e := range entries {
		tl, err := Load(Timelines, path.Join("timelines", e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, tl)
	}
	return out, nil
}

// Load reads and checks one timeline. An unknown field, a trailing value or
// any rule [Timeline.Check] names is an error naming the file.
func Load(fsys fs.FS, name string) (Timeline, error) {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Timeline{}, fmt.Errorf("scenario: read %s: %w", name, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var tl Timeline
	if err := dec.Decode(&tl); err != nil {
		return Timeline{}, fmt.Errorf("scenario: decode %s: %w", name, err)
	}
	if dec.More() {
		return Timeline{}, fmt.Errorf("scenario: %s holds more than one timeline", name)
	}
	if stem := strings.TrimSuffix(path.Base(name), ".json"); tl.Name != stem {
		return Timeline{}, fmt.Errorf("scenario: %s is named %q, want its stem %q", name, tl.Name, stem)
	}
	if err := tl.Check(); err != nil {
		return Timeline{}, fmt.Errorf("scenario: %s: %w", name, err)
	}
	return tl, nil
}

// Kind is the kind of the seat holding handle, or "" when no seat does.
func (tl Timeline) Kind(handle string) SeatKind {
	for _, s := range tl.Seats {
		if s.Handle == handle {
			return s.Kind
		}
	}
	return ""
}

// Check enforces the shape every consumer relies on: declared seats only,
// time never going backwards, each verb's own fields and no others, and an
// item filed before anything else touches it.
func (tl Timeline) Check() error {
	if !tl.Pattern.Valid() {
		return fmt.Errorf("pattern %q is not one of %s", tl.Pattern, words.Or(Patterns))
	}
	if len(tl.Steps) == 0 {
		return errors.New("a timeline with no steps asserts nothing")
	}
	seen := map[string]bool{}
	for _, s := range tl.Seats {
		if !s.Kind.Valid() {
			return fmt.Errorf("seat %q has kind %q, want agent or person", s.Handle, s.Kind)
		}
		if seen[s.Handle] {
			return fmt.Errorf("seat %q is declared twice", s.Handle)
		}
		seen[s.Handle] = true
	}
	filed := map[string]bool{}
	for i, st := range tl.Steps {
		if err := tl.checkStep(i, st, filed); err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		if st.Verb == VerbFile {
			filed[st.Item] = true
		}
	}
	return nil
}

func (tl Timeline) checkStep(i int, st Step, filed map[string]bool) error {
	if i > 0 && st.At < tl.Steps[i-1].At {
		return fmt.Errorf("at %d is before the previous step's %d", st.At, tl.Steps[i-1].At)
	}
	if !st.Verb.Valid() {
		return fmt.Errorf("verb %q is not one of %s", st.Verb, words.Or(Verbs))
	}
	if tl.Kind(st.Actor) == "" {
		return fmt.Errorf("actor %q is not a declared seat", st.Actor)
	}
	if !itemKey.MatchString(st.Item) {
		return fmt.Errorf("item %q is not a W-<n> key", st.Item)
	}
	if st.Verb == VerbFile && filed[st.Item] {
		return fmt.Errorf("%s is filed twice", st.Item)
	}
	if st.Verb != VerbFile && !filed[st.Item] {
		return fmt.Errorf("%s is %s before it is filed", st.Item, st.Verb)
	}
	for _, h := range slices.Concat(st.Mentions, []string{st.Ask, st.Assignee, st.Watcher}, st.Expect.Woken) {
		if h != "" && tl.Kind(h) == "" {
			return fmt.Errorf("%q is not a declared seat", h)
		}
	}
	for _, h := range st.Expect.Woken {
		if tl.Kind(h) != SeatAgent {
			return fmt.Errorf("expects %q woken, and only an agent seat takes a turn", h)
		}
	}
	if !slices.IsSorted(st.Expect.Woken) {
		return fmt.Errorf("expect.woken %s is not sorted", words.And(st.Expect.Woken))
	}
	return checkVerbFields(tl, i, st)
}

// checkVerbFields refuses a field the verb does not read, so a step cannot
// declare something its driver silently ignores.
func checkVerbFields(tl Timeline, i int, st Step) error {
	has := map[string]bool{
		"say": st.Say != "", "mentions": len(st.Mentions) > 0, "ask": st.Ask != "",
		"asks":    st.Asks != nil,
		"answers": st.Answers != nil, "assignee": st.Assignee != "",
		"recommend": st.Recommend != "", "choose": st.Choose != "",
		"status": st.Status != "", "watcher": st.Watcher != "",
	}
	allowed := map[Verb][]string{
		VerbFile:    {"assignee"},
		VerbComment: {"say", "mentions", "ask", "asks", "answers", "recommend", "choose"},
		VerbAssign:  {"assignee"},
		VerbStatus:  {"status", "assignee"},
		VerbWatch:   {"watcher"},
		VerbTurn:    {},
	}[st.Verb]
	required := map[Verb]string{
		VerbComment: "say", VerbAssign: "assignee", VerbStatus: "status", VerbWatch: "watcher",
	}[st.Verb]
	for field, set := range has {
		if set && !slices.Contains(allowed, field) {
			return fmt.Errorf("a %s step does not take %s", st.Verb, field)
		}
	}
	if required != "" && !has[required] {
		return fmt.Errorf("a %s step needs %s", st.Verb, required)
	}
	if st.Asks != nil && tl.Kind(st.Actor) != SeatAgent {
		return fmt.Errorf("only an agent states asks; %q is a person", st.Actor)
	}
	if st.Say != "" && !st.Say.Valid() {
		return fmt.Errorf("say %q is not one of %s", st.Say, words.Or(PhraseClasses))
	}
	if st.Status != "" && !slices.Contains(Statuses, st.Status) {
		return fmt.Errorf("status %q is not one of %s", st.Status, words.Or(Statuses))
	}
	if st.Recommend != "" && (!slices.Contains(Choices, st.Recommend) || st.Ask == "" ||
		tl.Kind(st.Ask) != SeatPerson) {
		return fmt.Errorf("recommend %q needs a question to a person and one of %s",
			st.Recommend, words.Or(Choices))
	}
	if st.Answers != nil {
		a := *st.Answers
		if a < 0 || a >= i || tl.Steps[a].Verb != VerbComment || tl.Steps[a].Item != st.Item {
			return fmt.Errorf("answers %d is not an earlier comment on %s", a, st.Item)
		}
		if tl.Steps[a].Ask != st.Actor {
			return fmt.Errorf("answers step %d, which asked %q, not %q", a, tl.Steps[a].Ask, st.Actor)
		}
	}
	if st.Choose != "" && (!slices.Contains(Choices, st.Choose) || st.Answers == nil ||
		tl.Steps[*st.Answers].Recommend == "") {
		return fmt.Errorf("choose %q needs an answer to a decision and one of %s",
			st.Choose, words.Or(Choices))
	}
	return nil
}
