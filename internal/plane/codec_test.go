package plane_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/plane"
)

const frontmatter = `key: chat-mentions
title: Mentioning people
summary: How to write an @-mention
phases: [plan, execute]
trigger:
  mcp_server: mattermost`

// A PAGE AND A FILE MEAN THE SAME THING. The decoder produces exactly what
// the skill parser reads from a file, so the two cannot diverge in what a
// skill is.
func TestASkillPageDecodesToWhatTheParserReads(t *testing.T) {
	t.Parallel()
	page := plane.EncodeSkillPage(frontmatter,
		"<p>Write <code>@username</code>, not an id.</p><ul><li>one</li><li>two</li></ul>")

	text := plane.DecodeSkillPage(page)
	if text == "" {
		t.Fatal("the page decoded to nothing")
	}
	skill, err := skills.Parse(text, skills.Source{PageID: "page-1"})
	if err != nil {
		t.Fatalf("Parse: %v\n---\n%s", err, text)
	}
	if skill.Key != "chat-mentions" || skill.Trigger.MCPServer != "mattermost" {
		t.Fatalf("skill = %+v", skill)
	}
	if !strings.Contains(skill.Body, "@username") {
		t.Fatalf("body = %q", skill.Body)
	}
	// BLOCK ELEMENTS BECOME NEWLINES: a list whose items ran together
	// reads as one sentence, and a skill body is mostly lists of steps.
	if !strings.Contains(skill.Body, "one\n\ntwo") {
		t.Fatalf("the list ran together:\n%q", skill.Body)
	}
}

// TOLERANT OF THE EDITOR, which rewrites the tags every time somebody opens
// the page: a strict matcher would decode on import and fail on the same
// page afterwards.
func TestTheDecoderToleratesEditorReserialisation(t *testing.T) {
	t.Parallel()
	for _, form := range []string{
		`<pre><code class="language-yaml">%s</code></pre><p>body</p>`,
		`<PRE><CODE>%s</CODE></PRE><p>body</p>`,
		"\n  <pre spellcheck=\"false\"><code class=\"language-yaml\" spellcheck=\"false\">%s</code></pre><p>body</p>",
		`<pre><code>%s</code>
</pre><p>body</p>`,
	} {
		page := strings.Replace(form, "%s", frontmatter, 1)
		if got := plane.DecodeSkillPage(page); !strings.Contains(got, "chat-mentions") {
			t.Fatalf("this form did not decode:\n%s\n---\n%q", page, got)
		}
	}
}

// AN ORDINARY PAGE IN THE SAME CONTAINER is not a broken skill: a project
// home page or an operator's notes decode to nothing, which is how the sync
// tells them apart without a decode failure on every walk.
func TestAnOrdinaryPageDecodesToNothing(t *testing.T) {
	t.Parallel()
	for _, page := range []string{
		"<p>Just some notes about the project.</p>",
		"", "<h1>Onboarding</h1><p>Read this first.</p>",
		// A code block that is not FIRST is body content, not
		// frontmatter — a skill's own example, most likely.
		"<p>Here is an example:</p><pre><code>key: x</code></pre>",
		// An EMPTY leading block is not frontmatter either.
		"<pre><code>   </code></pre><p>body</p>",
	} {
		if got := plane.DecodeSkillPage(page); got != "" {
			t.Fatalf("%q decoded to %q", page, got)
		}
	}
}

// The YAML round-trips through html escaping, which matters because a
// trigger or a summary legitimately contains the characters that need it.
func TestFrontmatterSurvivesEscaping(t *testing.T) {
	t.Parallel()
	tricky := `key: k
title: 'Quotes "and" <brackets> & ampersands'
summary: a > b && c < d
phases: [plan]
trigger:
  tool: post_message`
	page := plane.EncodeSkillPage(tricky, "<p>body</p>")

	skill, err := skills.Parse(plane.DecodeSkillPage(page), skills.Source{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if skill.Title != `Quotes "and" <brackets> & ampersands` {
		t.Fatalf("title = %q", skill.Title)
	}
	if skill.Summary != "a > b && c < d" {
		t.Fatalf("summary = %q", skill.Summary)
	}
}

func TestHTMLToTextFlattensForAModelRatherThanABrowser(t *testing.T) {
	t.Parallel()
	got := plane.HTMLToText(
		"<h2>Steps</h2><p>First do this.<br>Then that.</p>" +
			"<ul><li>one</li><li>two</li></ul><p>&amp; done</p>")
	want := "Steps\n\nFirst do this.\nThen that.\n\none\n\ntwo\n\n& done"
	if got != want {
		t.Fatalf("HTMLToText =\n%q\nwant\n%q", got, want)
	}
}
