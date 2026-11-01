package evals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// The case format. It is documented for operators in docs/reference/cli.md
// ("crewlet assistant eval"); the field tags below are its source.

// Case is one evaluation case, as a suite file states it.
type Case struct {
	// Name is the case's name, the file's own name when the file gives none.
	Name   string     `yaml:"name"`
	Person CasePerson `yaml:"person"`
	// Page is the page chip the question carries, in the wire's own member
	// names (`source_revision`), read into a [wire.PageContext] and
	// validated as the turn route validates one.
	Page     map[string]any `yaml:"page"`
	Question string         `yaml:"question"`
	// Mode is what the question asks for, `ask` when left out; a `brief`
	// needs the page.
	Mode    wire.TurnMode `yaml:"mode"`
	Answers []CaseAnswer  `yaml:"answers"`
	Script  []CaseRound   `yaml:"script"`
	Expect  Expect        `yaml:"expect"`

	page *wire.PageContext
}

// CasePerson is who asks: the permissions they hold and the writes the
// authority table would let them propose.
type CasePerson struct {
	Apps []iam.Grant `yaml:"apps"`
	Acts []string    `yaml:"acts"`
}

// CaseAnswer is what one route answers: a pattern in net/http's grammar
// ("GET /worklet/{id}"), a status (200 when left out) and a JSON body.
type CaseAnswer struct {
	Route  string `yaml:"route"`
	Status int    `yaml:"status"`
	Body   any    `yaml:"body"`
}

// CaseRound is one round the scripted model answers with: a tool call, or
// prose. Exactly one of Call and Say.
type CaseRound struct {
	Call string         `yaml:"call"`
	Args map[string]any `yaml:"args"`
	Say  string         `yaml:"say"`
}

// Expect is what the turn must do. Every field is optional, and a case
// expecting nothing is refused, since it could not fail.
type Expect struct {
	// Calls are tools the turn calls, in this order, others allowed
	// between them.
	Calls []string `yaml:"calls"`
	// Never are tools the turn must not call.
	Never []string `yaml:"never"`
	// Proposals is how many proposals the turn ends with.
	Proposals *int `yaml:"proposals"`
	// State is how the turn ends.
	State wire.TurnState `yaml:"state"`
	// Says and NeverSays are text the answer contains, and must not,
	// ignoring case.
	Says      []string `yaml:"says"`
	NeverSays []string `yaml:"never_says"`
	// Shows is text the turn's cards carry, ignoring case: a table's title,
	// columns and cells, an entity's facts, a proposal's rows and note. What
	// the person was shown, and what the model read of it, which the answer's
	// own words cannot stand for.
	Shows []string `yaml:"shows"`
	// Brief is what a brief turn's card holds.
	Brief *ExpectBrief `yaml:"brief"`
	// Refusals are what each refused call's answer names, in the order the
	// calls were refused: the i-th list holds text the i-th refusal must
	// contain, ignoring case (a path such as `needs[0].text`). So a case
	// holds the engine to naming EVERY fault of a refused call in one
	// answer (ADR-0059), which the turn's frames cannot show: a refusal's
	// sentence reaches the model and no card. Judged against a scripted
	// model, which hears every answer; a live model's answers travel inside
	// its own harness, and the expectation is not judged there.
	Refusals [][]string `yaml:"refusals"`
}

// ExpectBrief is what a brief's card must hold: the records its rows cite,
// in order (none is "Nothing needs you"), the tone each is drawn with, and
// the verdict the card says, where the case names them. The verdict is the
// engine's count of the rows it kept, so a case that names it holds the
// count to the rows that survived resolution, not to the rows the model
// wrote.
type ExpectBrief struct {
	Needs   []string         `yaml:"needs"`
	Tones   []wire.BriefTone `yaml:"tones"`
	Verdict string           `yaml:"verdict"`
}

// empty reports whether e expects nothing.
func (e Expect) empty() bool {
	return len(e.Calls) == 0 && len(e.Never) == 0 && e.Proposals == nil && e.State == "" &&
		len(e.Says) == 0 && len(e.NeverSays) == 0 && len(e.Shows) == 0 && e.Brief == nil &&
		len(e.Refusals) == 0
}

// LoadSuite reads every `*.yaml` case in dir, in name order, each checked.
func LoadSuite(dir string) ([]Case, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("assistant eval: %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("assistant eval: %s holds no *.yaml case", dir)
	}
	sort.Strings(paths)
	cases := make([]Case, 0, len(paths))
	names := map[string]string{}
	for _, path := range paths {
		c, err := loadCase(path)
		if err != nil {
			return nil, err
		}
		if other, seen := names[c.Name]; seen {
			return nil, fmt.Errorf("assistant eval: %s and %s are both named %q; give one "+
				"another name", other, path, c.Name)
		}
		names[c.Name] = path
		cases = append(cases, c)
	}
	return cases, nil
}

// loadCase reads one case file, STRICTLY and through the configuration's own
// reader ([config.ParseMember]): a misspelt member is refused with its path and
// line rather than an expectation silently not checked.
func loadCase(path string) (Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Case{}, fmt.Errorf("assistant eval: %w", err)
	}
	var c Case
	if err := config.ParseMember(raw, &c); err != nil {
		return Case{}, fmt.Errorf("assistant eval: %s: %w", path, err)
	}
	if c.Name == "" {
		c.Name = strings.TrimSuffix(filepath.Base(path), ".yaml")
	}
	if err := c.settle(); err != nil {
		return Case{}, fmt.Errorf("assistant eval: %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Case{}, fmt.Errorf("assistant eval: %s: %w", path, err)
	}
	return c, nil
}

// settle reads what YAML cannot type the way the turn does: the page chip
// into the wire's own shape, and each scripted call's arguments as JSON
// numbers, which is what a model's arguments carry.
func (c *Case) settle() error {
	if c.Page != nil {
		page, err := viaJSON[wire.PageContext](c.Page)
		if err != nil {
			return fmt.Errorf("page: %w", err)
		}
		c.page = &page
	}
	for i := range c.Script {
		if c.Script[i].Args == nil {
			continue
		}
		args, err := viaJSON[map[string]any](c.Script[i].Args)
		if err != nil {
			return fmt.Errorf("script[%d].args: %w", i, err)
		}
		c.Script[i].Args = args
	}
	return nil
}

// viaJSON is v read as a T through its JSON encoding, refusing a member T
// does not have.
func viaJSON[T any](v any) (T, error) {
	var out T
	raw, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err = dec.Decode(&out)
	return out, err
}

// Validate reports what is wrong with a case, naming the member.
func (c Case) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if strings.TrimSpace(c.Question) == "" {
		fail("question: the case asks nothing")
	}
	if c.Expect.empty() {
		fail("expect: the case expects nothing, so it cannot fail")
	}
	for i, app := range c.Person.Apps {
		if !app.Valid() {
			fail("person.apps[%d]: %q is not a permission (%s)", i, app, words.Or(iam.AllGrants))
		}
	}
	if c.page != nil {
		if err := c.page.Validate(); err != nil {
			fail("page: %w", err)
		}
	}
	for i, a := range c.Answers {
		method, path, ok := strings.Cut(a.Route, " ")
		if !ok || method == "" || !strings.HasPrefix(path, "/") {
			fail("answers[%d].route: %q is not METHOD /path", i, a.Route)
		}
		if a.Status != 0 && (a.Status < 100 || a.Status > 599) {
			fail("answers[%d].status: %d is not an HTTP status", i, a.Status)
		}
	}
	for i, r := range c.Script {
		switch {
		case (r.Call == "") == (r.Say == ""):
			fail("script[%d]: give exactly one of call and say", i)
		case r.Call != "" && !known(r.Call):
			fail("script[%d].call: %q is not a catalogue tool", i, r.Call)
		}
	}
	for field, tools := range map[string][]string{"calls": c.Expect.Calls, "never": c.Expect.Never} {
		for _, tool := range tools {
			if !known(tool) {
				fail("expect.%s: %q is not a catalogue tool", field, tool)
			}
		}
	}
	switch {
	case c.Mode != "" && !c.Mode.Valid():
		fail("mode: %q is not a turn mode (%s)", c.Mode, words.Or(wire.TurnModes))
	case c.Mode == wire.TurnBrief && c.page == nil:
		fail("mode: a brief needs the page it briefs")
	case c.Expect.Brief != nil && c.Mode != wire.TurnBrief:
		fail("expect.brief: only a brief turn writes a brief; set mode: brief")
	}
	if b := c.Expect.Brief; b != nil {
		if len(b.Tones) > 0 && len(b.Tones) != len(b.Needs) {
			fail("expect.brief.tones: %d tones for %d needs", len(b.Tones), len(b.Needs))
		}
		for i, tone := range b.Tones {
			if !tone.Valid() {
				fail("expect.brief.tones[%d]: %q is not a tone (%s)", i, tone,
					words.Or(wire.BriefTones))
			}
		}
	}
	for i, names := range c.Expect.Refusals {
		if len(names) == 0 {
			fail("expect.refusals[%d]: names nothing, so it cannot fail", i)
		}
		for j, n := range names {
			if strings.TrimSpace(n) == "" {
				fail("expect.refusals[%d][%d]: is empty", i, j)
			}
		}
	}
	if c.Expect.State != "" && !c.Expect.State.Valid() {
		fail("expect.state: %q is not a turn state (%s)", c.Expect.State,
			words.Or(wire.TurnStates))
	}
	return errors.Join(errs...)
}

// known reports whether the catalogue has a tool named name, the brief's
// answer among them.
func known(name string) bool {
	_, ok := catalog.Lookup(name)
	return ok || name == catalog.AnswerBriefTool
}
