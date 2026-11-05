package setupapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

// maxIntroWords is the most words a form's opening sentence says: what
// connecting the tool does, read once above its fields.
const maxIntroWords = 28

// cautioned is every tool whose credential carries a risk its form warns of,
// and the risk. A warning on every form is a warning nobody reads, so one is
// added here, on purpose, or not at all.
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

// EVERY VENDOR'S WORDS ARE HELD TO ONE SET OF RULES, read off the listing the
// dashboard draws every form from. A form once opened on a wall of text: one
// help string per field carrying the value, the steps at the vendor, the
// permissions and the risk, run together, and it grew one vendor at a time.
// So the rules are checked here for every tool this build serves, on both
// Atlassian deployments, and a vendor that adds a long hint, a second risk
// warning or a link to nowhere fails here rather than on a screen:
//
//   - a shown field has a hint of one line, at most setuptest.MaxHintWords;
//   - More, a check note, a step and an intro are finished lines;
//   - no copy carries a dash where punctuation belongs;
//   - every vendor link has words and a place, and every step's link and
//     yielded field names a field of the same tool;
//   - each tool opens on one intro, said by one of its surfaces;
//   - a caution only where cautioned names the risk.
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
	for _, list := range lists {
		for _, r := range list {
			fields[r.Field] = r
		}
	}
	linked := map[string]bool{}
	switch len(guides) {
	case 0:
		t.Errorf("%s: no surface says what connecting it does", tool.Key)
	case 1:
		checkGuide(t, tool.Key, guides[0], fields, linked)
	default:
		t.Errorf("%s: %d surfaces carry a guide, and one form opens on one", tool.Key, len(guides))
	}
	if _, ok := cautioned[cautioner]; cautioner != "" && !ok {
		t.Errorf("%s warns of a risk cautioned does not name: decide it there, with the risk", cautioner)
	}
	for _, list := range lists {
		for _, r := range list {
			checkField(t, tool.Key, r, linked)
		}
	}
}

func emptyGuide(g setup.Guide) bool {
	return g.Summary == "" && len(g.Prepare) == 0 && g.Caution == nil
}

// checkGuide holds a tool's intro, steps and caution to the rules, and
// records which fields a step links.
func checkGuide(t *testing.T, tool string, g setup.Guide, fields map[string]setup.Requirement, linked map[string]bool) {
	t.Helper()
	line(t, tool+" intro", g.Summary, ".")
	if n := setuptest.Words(g.Summary, ""); n > maxIntroWords {
		t.Errorf("%s: the intro runs to %d words, past %d: %q", tool, n, maxIntroWords, g.Summary)
	}
	for i, step := range g.Prepare {
		label := tool + " step " + string(rune('1'+i))
		end := "."
		if len(step.Grants) > 0 {
			end = ":"
		}
		line(t, label, step.Text, end)
		marked := strings.Contains(step.Text, setup.LinkMark)
		if marked != (step.Link != "") {
			t.Errorf("%s: places a link %v and names a field to link %q: the two go together",
				label, marked, step.Link)
		}
		if step.Link != "" {
			if r, ok := fields[step.Link]; !ok || r.VendorURL == "" || r.LinkText == "" {
				t.Errorf("%s: links %q, which is no field of %s with a vendor link", label, step.Link, tool)
			}
			linked[step.Link] = true
		}
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
func checkField(t *testing.T, tool string, r setup.Requirement, linked map[string]bool) {
	t.Helper()
	label := tool + "." + r.Field
	if !r.Hidden {
		line(t, label+" hint", r.Hint, ".")
		if n := setuptest.Words(r.Hint, r.LinkText); n > setuptest.MaxHintWords {
			t.Errorf("%s: the hint runs to %d words, past %d: %q", label, n, setuptest.MaxHintWords, r.Hint)
		}
	}
	for what, text := range map[string]string{"more": r.More, "check note": r.CheckNote} {
		if text != "" {
			line(t, label+" "+what, text, ".")
		}
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
	placed := strings.Contains(r.Hint, setup.LinkMark) || strings.Contains(r.More, setup.LinkMark)
	switch {
	case placed && (r.VendorURL == "" || r.LinkText == ""):
		t.Errorf("%s: places a link and has no address or words for it", label)
	case r.VendorURL != "" && r.LinkText == "":
		t.Errorf("%s: has a vendor link with no words to carry it", label)
	case r.VendorURL != "" && !placed && !linked[r.Field] && !r.Hidden:
		t.Errorf("%s: has a vendor link that neither its words nor a step place", label)
	}
}

// line fails t when text is not one finished line ending on end.
func line(t *testing.T, label, text, end string) {
	t.Helper()
	if problem := setuptest.LineProblem(text, end); problem != "" {
		t.Errorf("%s: %q: %s", label, text, problem)
	}
}
