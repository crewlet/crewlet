package mcp

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// A TOOL NO MODEL CAN BE HANDED IS REFUSED, NEVER FILED, AND SAID: a prefixed
// name holding a character a model's tool name cannot, or longer than the
// room the longest CLI prefix leaves, is not in the server's contribution, so
// it is neither offered nor callable, and never cut short to fit. The rest of
// the server's tools are filed as ever, and the refusal is the instance's
// record, with its reason, until a listing says otherwise.
//
// Mutation: file a refused tool anyway, cut a long name to the limit, or
// compare the bare name rather than the prefixed one, and this is red.
func TestAToolNoModelCanBeHandedIsRefusedAndNeverFiled(t *testing.T) {
	t.Parallel()
	log, rec := recorder()
	b := NewBridge(log, Supervision{})
	t.Cleanup(func() { _, _ = b.StopAll(context.Background()) })

	most := cliprofile.ToolNameMax()
	fits := strings.Repeat("f", most-len("docs_"))      // exactly the limit, prefixed
	tooLong := strings.Repeat("l", most-len("docs_")+1) // one over, prefixed: fits bare
	spec := helperSpec(t, "docs", "serve", map[string]string{helperToolsEnv: toolsJSON(
		[3]string{"search", "Search the handbook", ""},
		[3]string{"read.file", "Read one file", ""},
		[3]string{fits, "Fits exactly", ""},
		[3]string{tooLong, "One over", ""},
	)})
	spec.ToolPrefix = "docs_"
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	filed := addedNames(change)
	slices.Sort(filed)
	want := []string{"docs_" + fits, "docs_search"}
	slices.Sort(want)
	if !slices.Equal(filed, want) {
		t.Errorf("filed %v, want %v: a refused tool is never filed", filed, want)
	}
	for _, name := range []string{"docs_read.file", "docs_" + tooLong, ("docs_" + tooLong)[:most]} {
		if _, ok := b.Tool(name); ok {
			t.Errorf("%q is in the catalogue: a name no model can be handed is never offered, "+
				"and never cut short", name)
		}
	}
	if n := len(b.ServerTools("docs")); n != 2 {
		t.Errorf("the server contributes %d tools, want the 2 it can offer", n)
	}

	got := b.RefusedTools("docs")
	wantRefused := []RefusedTool{
		{Name: "docs_" + tooLong, Reason: NameTooLong},
		{Name: "docs_read.file", Reason: NameCharacters},
	}
	if !slices.Equal(got, wantRefused) {
		t.Errorf("RefusedTools = %v, want %v", got, wantRefused)
	}
	if !rec.hasAttr("mcp_tool_refused", "reason", string(NameTooLong)) ||
		!rec.hasAttr("mcp_tool_refused", "reason", string(NameCharacters)) {
		t.Error("a refused tool was not logged with its reason")
	}
	if b.RefusedTools("nobody") != nil {
		t.Error("a server the bridge does not hold has refusals")
	}
}

// A TOOL ANOTHER SERVER OFFERS UNDER THE SAME NAME IS SHADOWED, and the bridge
// says by which SERVER, by its configured name rather than an instance's: the
// first server by instance name keeps the name, and the one it shadows records
// it. The winner records nothing.
//
// Mutation: report the winner as shadowed, or name the winner's instance
// rather than its server, and this is red.
func TestAToolAnotherServerOffersIsShadowedAndSaysByWhom(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	tools := toolsJSON([3]string{"search", "Search", ""}, [3]string{"read", "Read", ""})
	first := InstanceName("handbook", "Engineer")
	if _, err := b.Add(t.Context(), helperSpec(t, first, "serve", map[string]string{helperToolsEnv: tools})); err != nil {
		t.Fatalf("Add %s: %v", first, err)
	}
	other := toolsJSON([3]string{"search", "Search the wiki", ""}, [3]string{"wiki_page", "A page", ""})
	if _, err := b.Add(t.Context(), helperSpec(t, "wiki", "serve", map[string]string{helperToolsEnv: other})); err != nil {
		t.Fatalf("Add wiki: %v", err)
	}
	if got := b.ShadowedTools(first); got != nil {
		t.Errorf("the server keeping the name records %v, want nothing", got)
	}
	want := []ShadowedTool{{Name: "search", By: "handbook"}}
	if got := b.ShadowedTools("wiki"); !slices.Equal(got, want) {
		t.Errorf("ShadowedTools(wiki) = %v, want %v", got, want)
	}

	// The shadow goes with the server that cast it.
	if _, err := b.Stop(t.Context(), first); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := b.ShadowedTools("wiki"); got != nil {
		t.Errorf("after the other server stopped, wiki records %v shadowed", got)
	}
}

// EVERY REFUSAL IS A KNOWN REASON WITH A SENTENCE OF ITS OWN: valid, listed
// once, and worded for a person, with the tool's name in it and what would
// offer it; no sentence is another's.
func TestEveryNameRefusalSaysWhyAndWhatOffersIt(t *testing.T) {
	t.Parallel()
	seen := map[string]NameRefusal{}
	for _, r := range NameRefusals {
		if !r.Valid() {
			t.Errorf("%q is listed and not valid", r)
		}
		sentence := r.Sentence("docs_search", "docs_")
		if !strings.Contains(sentence, "docs_search") {
			t.Errorf("%s: %q does not name the tool", r, sentence)
		}
		if strings.ContainsAny(sentence, "—–") {
			t.Errorf("%s: %q holds a dash a sentence is not written with", r, sentence)
		}
		if was, twice := seen[sentence]; twice {
			t.Errorf("%s says what %s says: %q", r, was, sentence)
		}
		seen[sentence] = r
	}
	if NameRefusal("").Valid() || NameRefusal("quota").Valid() {
		t.Error("a reason this build does not send reports Valid")
	}

	most := cliprofile.ToolNameMax()
	long := "pre_" + strings.Repeat("x", most-2)
	if s := NameTooLong.Sentence(long, "pre_"); !strings.Contains(s, `Shorten the tool_prefix "pre_" by 2`) {
		t.Errorf("a name the prefix pushes over says %q, want it to say how much to shorten the prefix", s)
	}
	if s := NameTooLong.Sentence("pre_"+strings.Repeat("x", most), "pre_"); !strings.Contains(s, `Take off the tool_prefix "pre_"`) {
		t.Errorf("a name that fits bare says %q, want it to say to take the prefix off", s)
	}
	if s := NameTooLong.Sentence("pre_"+strings.Repeat("x", most+1), "pre_"); strings.Contains(s, "tool_prefix") {
		t.Errorf("a name too long without its prefix says %q: no prefix can mend it", s)
	}
	if s := NameCharacters.Sentence("pre.search", "pre."); !strings.Contains(s, "tool_prefix") {
		t.Errorf("a prefix holding the character says %q, want it to name the prefix", s)
	}
	if s := ShadowSentence("search", "handbook"); !strings.Contains(s, "handbook") ||
		!strings.Contains(s, `"search"`) || strings.ContainsAny(s, "—–") {
		t.Errorf("ShadowSentence = %q", s)
	}
}
