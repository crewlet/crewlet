package docindex_test

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/docindex"
)

// files is a version as the format reads it: path to bytes, each typed by its
// extension as the media table would.
type files map[string]string

func (f files) Paths() []string {
	out := make([]string, 0, len(f))
	for p := range f {
		out = append(out, p)
	}
	return out
}

func (f files) Essence(p string) (string, bool) {
	if _, ok := f[p]; !ok {
		return "", false
	}
	switch path.Ext(p) {
	case ".md":
		return "text/markdown", true
	case ".json":
		return "application/json", true
	case ".png":
		return "image/png", true
	}
	return "text/plain", true
}

func (f files) Read(p string) ([]byte, error) {
	body, ok := f[p]
	if !ok {
		return nil, fmt.Errorf("no %s", p)
	}
	return []byte(body), nil
}

func refusal(t *testing.T, err error) []docindex.Finding {
	t.Helper()
	var refused *docindex.Refused
	if !errors.As(err, &refused) || !errors.Is(err, docindex.ErrRefused) {
		t.Fatalf("got %v, want a refusal", err)
	}
	return refused.Findings
}

func mentions(findings []docindex.Finding, rule docindex.Rule, words string) bool {
	for _, f := range findings {
		if f.Rule == rule && strings.Contains(f.Message, words) {
			return true
		}
	}
	return false
}

// A DOC OF ONE FILE is one tab, titled by its file's name, with no index.
func TestADocOfOneFileIsOneTab(t *testing.T) {
	t.Parallel()
	doc, err := docindex.Read(files{"index.md": "# Runbook\n\nRestart it.\n", "chart.png": "png"},
		"index.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Indexed || len(doc.Tabs) != 1 || doc.Tabs[0].Title != "index" ||
		doc.Tabs[0].Text != "# Runbook\n\nRestart it.\n" {
		t.Fatalf("got %+v", doc)
	}
	if got := doc.Text(); got != "# Runbook\n\nRestart it.\n" {
		t.Errorf("a doc of one file searches as %q, not its text", got)
	}
}

// AN INDEX ORDERS THE TABS, and a Markdown file it does not name is a file of
// the version and not a tab.
func TestAnIndexOrdersTheTabs(t *testing.T) {
	t.Parallel()
	doc, err := docindex.Read(files{
		"doc.json":      `{"v":1,"tabs":[{"path":"tabs/plan.md","title":"Plan"},{"path":"tabs/risks.md","title":"Risks"}]}`,
		"tabs/risks.md": "# Risks\n",
		"tabs/plan.md":  "# Plan\n",
		"appendix.md":   "linked, not a tab",
		"images/a.png":  "png",
	}, docindex.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Indexed || len(doc.Tabs) != 2 || doc.Tabs[0].Path != "tabs/plan.md" ||
		doc.Tabs[1].Title != "Risks" {
		t.Fatalf("got %+v", doc)
	}
	if _, ok := doc.Tab("appendix.md"); ok {
		t.Error("a Markdown file the index does not name is a tab")
	}
	if got := doc.Text(); got != "Plan\n# Plan\n\nRisks\n# Risks\n" {
		t.Errorf("a tabbed doc searches as %q", got)
	}
}

// EVERY WAY AN INDEX FAILS TO RESOLVE IS REFUSED BY NAME, in one pass.
func TestAnIndexThatDoesNotResolveIsRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		index string
		extra files
		rule  docindex.Rule
		words string
	}{
		{"not JSON", `{`, nil, docindex.RuleIndex, "doc.json is not JSON"},
		{"an unknown field", `{"v":1,"tabs":[{"path":"a.md","title":"A"}],"theme":"x"}`, nil,
			docindex.RuleIndex, "theme is not a key the doc index carries: the top level of doc.json takes v, tabs"},
		{"two values", `{"v":1,"tabs":[{"path":"a.md","title":"A"}]} {}`, nil, docindex.RuleIndex,
			"more after its value"},
		{"another version", `{"v":2,"tabs":[{"path":"a.md","title":"A"}]}`, nil, docindex.RuleIndex,
			"v is 2"},
		{"no tabs", `{"v":1,"tabs":[]}`, nil, docindex.RuleIndex, "at least one tab"},
		{"a path twice", `{"v":1,"tabs":[{"path":"a.md","title":"A"},{"path":"a.md","title":"B"}]}`,
			nil, docindex.RuleIndex, "already names"},
		{"no title", `{"v":1,"tabs":[{"path":"a.md","title":"  "}]}`, nil, docindex.RuleIndex,
			"no title"},
		{"a long title", `{"v":1,"tabs":[{"path":"a.md","title":"` + strings.Repeat("x", 121) + `"}]}`,
			nil, docindex.RuleIndex, "121 bytes"},
		{"a control character", `{"v":1,"tabs":[{"path":"a.md","title":"A\u0007"}]}`, nil,
			docindex.RuleIndex, "control character"},
		{"a missing file", `{"v":1,"tabs":[{"path":"gone.md","title":"Gone"}]}`, nil,
			docindex.RuleReference, "not one of the version's files"},
		{"a tab that is not Markdown", `{"v":1,"tabs":[{"path":"a.png","title":"A"}]}`,
			files{"a.png": "png"}, docindex.RuleReference, "a tab is Markdown"},
		{"a tab that is not UTF-8", `{"v":1,"tabs":[{"path":"b.md","title":"B"}]}`,
			files{"b.md": "\xff\xfe"}, docindex.RuleEncoding, "UTF-8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			src := files{"doc.json": c.index, "a.md": "# A\n"}
			for p, b := range c.extra {
				src[p] = b
			}
			_, err := docindex.Read(src, docindex.IndexPath)
			if found := refusal(t, err); !mentions(found, c.rule, c.words) {
				t.Errorf("findings %v do not say %s: %q", found, c.rule, c.words)
			}
		})
	}
}

// A DOC IS BOUNDED: at most 32 tabs, and its tabs' text together at most one
// entry's bound, so the dashboard reads it whole in one answer.
func TestADocIsBounded(t *testing.T) {
	t.Parallel()
	src := files{}
	var tabs []string
	for i := range docindex.MaxTabs + 1 {
		p := fmt.Sprintf("t%d.md", i)
		src[p] = "x"
		tabs = append(tabs, fmt.Sprintf(`{"path":%q,"title":"T"}`, p))
	}
	src["doc.json"] = `{"v":1,"tabs":[` + strings.Join(tabs, ",") + `]}`
	_, err := docindex.Read(src, docindex.IndexPath)
	if !mentions(refusal(t, err), docindex.RuleIndex, "at most 32") {
		t.Errorf("33 tabs are not refused for their count: %v", err)
	}

	half := strings.Repeat("x", docindex.MaxTextBytes/2+1)
	big := files{"doc.json": `{"v":1,"tabs":[{"path":"a.md","title":"A"},{"path":"b.md","title":"B"}]}`,
		"a.md": half, "b.md": half}
	_, err = docindex.Read(big, docindex.IndexPath)
	if !mentions(refusal(t, err), docindex.RuleBound, "at most") {
		t.Errorf("tabs past the text bound are not refused: %v", err)
	}
}

// A DOC OF ONE FILE THAT IS NOT MARKDOWN is refused as a doc, not drawn.
func TestADocOfOneFileIsMarkdown(t *testing.T) {
	t.Parallel()
	_, err := docindex.Read(files{"notes.txt": "plain"}, "notes.txt")
	if !mentions(refusal(t, err), docindex.RuleReference, "a tab is Markdown") {
		t.Errorf("a plain text entry is not refused: %v", err)
	}
}

// THE OUTLINE CARRIES NO TEXT beyond the first tab's opening, cut at a line
// boundary within the cover's bound, so a gallery of covers stays small.
func TestTheOutlineIsSmall(t *testing.T) {
	t.Parallel()
	long := "# Plan\n\n" + strings.Repeat("A line of the plan that goes on.\n", 100)
	doc, err := docindex.Read(files{
		"doc.json": `{"v":1,"tabs":[{"path":"plan.md","title":"Plan"},{"path":"next.md","title":"Next"}]}`,
		"plan.md":  long,
		"next.md":  "# Next\n",
	}, docindex.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	o := doc.Outline()
	if len(o.Tabs) != 2 || o.Tabs[0].Bytes != len(long) || o.Tabs[1].Title != "Next" {
		t.Fatalf("tabs %+v", o.Tabs)
	}
	if o.Cover.Tab != "plan.md" || !strings.HasPrefix(o.Cover.Markdown, "# Plan\n") ||
		len(o.Cover.Markdown) > docindex.CoverBytes || strings.HasSuffix(o.Cover.Markdown, "\n") ||
		strings.Count(o.Cover.Markdown, "\n") >= 12 {
		t.Errorf("cover %q", o.Cover.Markdown)
	}

	one, _ := docindex.Read(files{"a.md": strings.Repeat("é", 2000)}, "a.md")
	cut := one.Outline().Cover.Markdown
	if len(cut) > docindex.CoverBytes || !strings.HasPrefix(strings.Repeat("é", 2000), cut) {
		t.Errorf("a long first line is cut to %d bytes, splitting a rune or past the bound", len(cut))
	}
}

// THE STARTER IS A DOC: one tab, held to the format, its title escaped so it
// reads as the words somebody typed.
func TestTheStarterIsADoc(t *testing.T) {
	t.Parallel()
	src := files{}
	for p, b := range docindex.Starter("*Q3* plan #1") {
		src[p] = string(b)
	}
	doc, err := docindex.Read(src, docindex.StarterEntry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.Tabs[0].Text, `# \*Q3\* plan \#1`+"\n") {
		t.Errorf("the starter opens %q", doc.Tabs[0].Text)
	}
}

// A DOC BEGUN WITH ITS TEXT keeps the text as written under the escaped
// title, and a text of nothing but space leaves the heading alone.
func TestAWrittenStarterKeepsItsText(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ body, want string }{
		{"Some *bold* text.\n\n- one\n", "# \\*Q3\\* plan\n\nSome *bold* text.\n\n- one\n"},
		{"\n\n  indented code\n\n\n", "# \\*Q3\\* plan\n\n  indented code\n"},
		{" \n\t\n", "# \\*Q3\\* plan\n"},
	} {
		got := docindex.Written("*Q3* plan", c.body)
		src := files{}
		for p, b := range got {
			src[p] = string(b)
		}
		doc, err := docindex.Read(src, docindex.StarterEntry)
		if err != nil {
			t.Fatal(err)
		}
		if doc.Tabs[0].Text != c.want {
			t.Errorf("Written(%q) = %q, want %q", c.body, doc.Tabs[0].Text, c.want)
		}
	}
}
