package setupapi_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// maxIntroWords is the most words a form's opening sentence says: what
// connecting the tool does, read once above its fields.
const maxIntroWords = 28

// cautioned is every tool whose form warns of its credential's risk, and the
// risk: a warning is added here on purpose, or not at all.
var cautioned = map[integration.Kind]string{
	integration.KindScaleway:   "the provisioner key administers IAM for the whole Organization",
	integration.KindCloudflare: "the provisioner token holds every permission an agent can be given",
	integration.KindAtlassian:  "a scoped organization key cannot create the agents' accounts",
}

// dataCenterDoc is the seeded company on Atlassian Data Center, whose forms
// ask different questions and so carry different words.
const dataCenterDoc = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {"atlassian": {"deployment": "data_center"}},
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"},
    {"name": "CTO", "handle": "cto", "llm": "zulu"}
  ]
}`

// EVERY VENDOR'S WORDS KEEP ONE SET OF RULES, read off the live listing for
// every tool on both Atlassian deployments, so a wall of text cannot return
// one vendor at a time (the rules are listed in the PR and in each check).
func TestEveryVendorsCopyKeepsTheRules(t *testing.T) {
	t.Parallel()
	for name, document := range map[string]string{"cloud": companyDoc, "data center": dataCenterDoc} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seedDocument(t, document)
			res := s.do(t, http.MethodGet, "/setup/integrations", "", nil)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", res.Code, res.Body)
			}
			var listing struct {
				Tools []setupapi.ToolState `json:"tools"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &listing); err != nil {
				t.Fatal(err)
			}
			byKind := map[integration.Kind]setupapi.ToolState{}
			for _, tool := range listing.Tools {
				byKind[tool.Kind] = tool
			}
			for _, tool := range integration.Tools {
				checkTool(t, tool, byKind)
			}
		})
	}
}

// AND THE CAUTION IS WHERE THE RISK IS: a tool cautioned names keeps its
// warning, so the list cannot outlive the copy it describes.
func TestACautionedToolWarns(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	for kind, risk := range cautioned {
		res := s.do(t, http.MethodGet, "/setup/integrations/"+string(kind), "", nil)
		var state setupapi.ToolState
		if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		if state.Caution == nil {
			t.Errorf("%s carries no caution, and its credential's risk is that %s", kind, risk)
		}
	}
}

// checkTool holds one tool's surfaces, as one form draws them, to the rules.
func checkTool(t *testing.T, tool integration.Tool, byKind map[integration.Kind]setupapi.ToolState) {
	t.Helper()
	fields := map[string]setup.Requirement{}
	var lists [][]setup.Requirement
	var guides []setup.Guide
	var cautioner integration.Kind
	for _, surface := range tool.Surfaces {
		state, ok := byKind[integration.Kind(surface.Key)]
		if !ok {
			continue
		}
		lists = append(lists, state.Requirements)
		for _, seat := range state.Seats {
			lists = append(lists, seat.Requirements)
		}
		if state.NeedsOperator != nil {
			lists = append(lists, []setup.Requirement{*state.NeedsOperator})
		}
		if !emptyGuide(state.Guide) {
			guides = append(guides, state.Guide)
		}
		if state.Caution != nil {
			cautioner = state.Kind
		}
	}
	if len(lists) == 0 {
		t.Fatalf("%s: no surface of the tool is on the listing", tool.Key)
	}
	// WHERE EACH FIELD STANDS on the form, first declaration first.
	order := map[string]int{}
	for _, list := range lists {
		for _, r := range list {
			fields[r.Field] = r
			if _, seen := order[r.Field]; !seen {
				order[r.Field] = len(order)
			}
		}
	}
	// ONE PAGE, ONE NAME: a form calling one vendor page two things reads as two pages.
	pages := map[string]string{}
	switch len(guides) {
	case 0:
		t.Errorf("%s: no surface says what connecting it does", tool.Key)
	case 1:
		checkGuide(t, tool.Key, guides[0], fields)
		checkStepLinks(t, tool.Key, guides[0], fields)
		for i, step := range guides[0].Prepare {
			samePage(t, tool.Key+" step "+string(rune('1'+i)), step.Text, pages)
		}
	default:
		t.Errorf("%s: %d surfaces carry a guide, and one form opens on one", tool.Key, len(guides))
	}
	if _, ok := cautioned[cautioner]; cautioner != "" && !ok {
		t.Errorf("%s warns of a risk cautioned does not name: decide it there, with the risk", cautioner)
	}
	for _, list := range lists {
		for _, r := range list {
			checkField(t, tool.Key, r)
			checkFieldLink(t, tool.Key, r, fields, order)
			samePage(t, tool.Key+"."+r.Field+" hint", r.Hint, pages)
			samePage(t, tool.Key+"."+r.Field+" more", r.More, pages)
		}
	}
}

// samePage fails t where text links a page by other words than the form
// already gave it, recording the words of each page it links first.
func samePage(t *testing.T, label string, text vendortext.Text, pages map[string]string) {
	t.Helper()
	for _, p := range text {
		if p.URL == "" {
			continue
		}
		words := vendortext.Text{p}.String()
		if named, ok := pages[p.URL]; !ok {
			pages[p.URL] = words
		} else if named != words {
			t.Errorf("%s: calls %s %q, which the form also calls %q", label, p.URL, words, named)
		}
	}
}

func emptyGuide(g setup.Guide) bool {
	return g.Summary == "" && len(g.Prepare) == 0 && g.Caution == nil
}

// checkGuide holds a tool's intro, steps and caution to the rules.
func checkGuide(t *testing.T, tool string, g setup.Guide, fields map[string]setup.Requirement) {
	t.Helper()
	line(t, tool+" intro", g.Summary, ".")
	if n := setuptest.Words(g.Summary); n > maxIntroWords {
		t.Errorf("%s: the intro runs to %d words, past %d: %q", tool, n, maxIntroWords, g.Summary)
	}
	for i, step := range g.Prepare {
		label := tool + " step " + string(rune('1'+i))
		end := "."
		if len(step.Grants) > 0 {
			end = ":"
		}
		said(t, label, step.Text, end)
		if _, ok := fields[step.Yields]; step.Yields != "" && !ok {
			t.Errorf("%s: yields %q, which is no field of %s", label, step.Yields, tool)
		}
		for _, grant := range step.Grants {
			if grant != strings.TrimSpace(grant) || grant == "" || strings.ContainsAny(grant, "\"\n\u2014") {
				t.Errorf("%s: grant %q is not a name a person can find at the vendor", label, grant)
			}
		}
	}
	if c := g.Caution; c != nil {
		if c.Title == "" || strings.HasSuffix(c.Title, ".") || strings.ContainsAny(c.Title, "\n\u2014") {
			t.Errorf("%s: the caution's title %q is not a heading", tool, c.Title)
		}
		line(t, tool+" caution", c.Body, ".")
	}
}

// checkField holds one requirement's words to the rules.
func checkField(t *testing.T, tool string, r setup.Requirement) {
	t.Helper()
	label := tool + "." + r.Field
	if !r.Hidden {
		said(t, label+" hint", r.Hint, ".")
		if n := setuptest.TextWords(r.Hint); n > setuptest.MaxHintWords {
			t.Errorf("%s: the hint runs to %d words, past %d: %q", label, n, setuptest.MaxHintWords, r.Hint)
		}
	}
	if len(r.More) > 0 {
		said(t, label+" more", r.More, ".")
	}
	if r.CheckNote != "" {
		line(t, label+" check note", r.CheckNote, ".")
	}
	for what, text := range map[string]string{"label": r.Label, "format": r.Format} {
		if problem := setuptest.LineProblem(text+".", "."); text != "" && problem != "" {
			t.Errorf("%s: the %s %q: %s", label, what, text, problem)
		}
	}
	for _, c := range r.Choices {
		if problem := setuptest.LineProblem(c.Label+".", "."); problem != "" {
			t.Errorf("%s: the choice %q: %s", label, c.Label, problem)
		}
		if c.Hint != "" {
			line(t, label+" choice "+c.Value, c.Hint, ".")
		}
	}
	// THE FORM MARKS A FIELD OPTIONAL FROM required, so a label saying so is read twice.
	if strings.Contains(strings.ToLower(r.Label), "optional") {
		t.Errorf("%s: the label %q says optional, which the form marks from required", label, r.Label)
	}
	// A FIELD THE INTEGRATION CANNOT WORK WITHOUT NEVER CALLS ITSELF OPTIONAL.
	if r.Required && strings.HasPrefix(strings.ToLower(r.Hint.String()), "optional") {
		t.Errorf("%s: is required and its hint %q calls it optional", label, r.Hint)
	}
}

// placeholder is a `{field}` in a vendor address, filled from the form.
var placeholder = regexp.MustCompile(`\{([a-z_.]+)\}`)

// waitsOn is the fields the vendor addresses texts link cannot be opened without.
func waitsOn(texts ...vendortext.Text) []string {
	var out []string
	for _, text := range texts {
		for _, url := range text.URLs() {
			for _, m := range placeholder.FindAllStringSubmatch(url, -1) {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// checkStepLinks holds each step's link to a page it can open: a value its
// address waits on is one an earlier step produces, or has a default.
func checkStepLinks(t *testing.T, tool string, g setup.Guide, fields map[string]setup.Requirement) {
	t.Helper()
	yielded := map[string]bool{}
	for i, step := range g.Prepare {
		for _, field := range waitsOn(step.Text) {
			if !yielded[field] && fields[field].Default == "" {
				t.Errorf("%s step %d: links a page that waits on %s, and no earlier step "+
					"produces it, so the step names a page it cannot open", tool, i+1, field)
			}
		}
		if step.Yields != "" {
			yielded[step.Yields] = true
		}
	}
}

// checkFieldLink holds a field's own link to fields above it: its address
// waits only on a value the form asks before it, or one with a default.
func checkFieldLink(t *testing.T, tool string, r setup.Requirement, fields map[string]setup.Requirement,
	order map[string]int,
) {
	t.Helper()
	for _, field := range waitsOn(r.Hint, r.More) {
		at, ok := order[field]
		if !ok || (at >= order[r.Field] && fields[field].Default == "") {
			t.Errorf("%s.%s: its link waits on %s, which the form does not ask before it", tool, r.Field, field)
		}
	}
}

// said fails t when text is not one finished line ending on end, or carries
// by hand what a vendortext part says (vendortext.Text.Check).
func said(t *testing.T, label string, text vendortext.Text, end string) {
	t.Helper()
	line(t, label, text.String(), end)
	if err := text.Check(); err != nil {
		t.Errorf("%s: %q: %v", label, text, err)
	}
}

// line fails t when text is not one finished line ending on end.
func line(t *testing.T, label, text, end string) {
	t.Helper()
	if problem := setuptest.LineProblem(text, end); problem != "" {
		t.Errorf("%s: %q: %s", label, text, problem)
	}
}
