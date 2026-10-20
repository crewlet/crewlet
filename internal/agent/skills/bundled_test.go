package skills_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/worklet"
)

// bundled is one of the example tool skills this repository ships, read
// through the parser every backend's pages go through.
func bundled(t *testing.T, name string) skills.Skill {
	t.Helper()
	path := filepath.Join(sourcetree.Root(t), "examples", "tool-skills", name)
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := skills.Parse(string(text), skills.Source{PageID: name})
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return s
}

// THE MENTIONS SKILL GATES EVERY NATIVE WRITE THAT CARRIES A BODY, and no
// read. It teaches the markup a mention takes, and the company's own chat,
// tracker and pages all take the same literal `@handle` resolved against the
// chart — so a surface offering any one of their writes, and nothing else,
// must catalogue it, or a seat on Chatlet posts a mention spelled for
// another surface and wakes nobody. The writes are read off the domains' own
// declarations rather than listed here, so a fourth way to post a message
// fails this until the skill's trigger names it.
func TestTheBundledMentionsSkillGatesEveryNativeWrite(t *testing.T) {
	t.Parallel()
	s := bundled(t, "platform-mentions.md")
	if s.Key != "skill:platform_mentions" {
		t.Fatalf("key = %q; the tool-skills doc names skill:platform_mentions", s.Key)
	}
	// ENFORCED: a mention spelled for the wrong surface posts as plain text
	// with nobody notified, which is exactly what a gate exists to stop.
	if !s.Required {
		t.Fatal("the mentions skill is advisory; it ships enforced")
	}
	catalogue := registry(t, s)

	writes := append([]string{}, chatlet.WriteTools()...)
	writes = append(writes, worklet.CreateWorkItemTool, worklet.UpdateWorkItemTool,
		worklet.CommentOnWorkTool)
	writes = append(writes, builtin.PageWrites()...)
	for _, tool := range writes {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			if len(catalogue.Matching(prompts.PhaseExecute, surface([]string{tool}))) != 1 {
				t.Fatalf("a seat holding only %s is never offered the mentions skill (trigger %+v)",
					tool, s.Trigger)
			}
		})
	}

	// READS NEVER WAIT on it: a read carries no body, so gating one would make
	// every look at a room or an item wait on markup it cannot get wrong.
	reads := []string{chatlet.ReadChannelTool, chatlet.SearchMessagesTool, chatlet.ListChannelsTool,
		worklet.GetWorkItemTool, builtin.GetPageTool}
	if got := catalogue.Matching(prompts.PhaseExecute, surface(reads)); len(got) != 0 {
		t.Fatalf("a seat holding only reads (%v) is gated on the mentions skill", reads)
	}
}
