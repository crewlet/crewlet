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

// THE CHAT PRACTICE SKILLS REACH A SEAT ON THE COMPANY'S OWN CHAT. Each was
// written for Mattermost and triggered on its tools alone, so a company on
// Chatlet — the default when no vendor block is declared — posted,
// replied, opened a DM, listed its rooms and joined one without ever being
// offered how to hand a problem to its manager, where to share a standing
// rule or which room to choose. Each must be catalogued on a surface holding
// one Chatlet tool and nothing else.
func TestTheBundledChatPracticeSkillsFireOnChatlet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file, key string
		tools     []string
	}{
		// A handoff is said where the problem lives, and on chat that is
		// any of the three ways a seat says something.
		{"getting-unstuck.md", "skill:getting_unstuck", chatlet.WriteTools()},
		// A standing rule is shared by saying it, wherever it is said.
		{"observed-directives.md", "skill:observed_directives", chatlet.WriteTools()},
		// Choosing a room is listing the rooms, joining one, or posting in
		// one; a DM or a reply has already chosen its conversation.
		{"channel-discovery.md", "skill:channel_discovery",
			[]string{chatlet.ListChannelsTool, chatlet.JoinChannelTool, chatlet.PostMessageTool}},
	} {
		s := bundled(t, tc.file)
		if s.Key != tc.key {
			t.Fatalf("%s: key = %q, want %q", tc.file, s.Key, tc.key)
		}
		catalogue := registry(t, s)
		for _, tool := range tc.tools {
			t.Run(tc.key+"/"+tool, func(t *testing.T) {
				t.Parallel()
				if len(catalogue.Matching(prompts.PhaseExecute, surface([]string{tool}))) != 1 {
					t.Fatalf("a seat holding only %s is never offered %s (trigger %+v)",
						tool, tc.key, s.Trigger)
				}
			})
		}
	}
	// ADVISORY, as the tool-skills doc says: practice prose on the
	// commonest calls a seat makes, which enforced would make every one of
	// them wait on judgement rather than on markup a call can get wrong.
	if bundled(t, "getting-unstuck.md").Required {
		t.Fatal("the getting-unstuck skill is enforced; it ships advisory")
	}
}

// THE CHAT PRACTICE SKILLS REACH A SEAT ON SLACK TOO. None of them named a
// Slack tool, so a seat posting through slack-mcp-server was never offered how
// a mention reaches somebody there (`<@member-id>`, where a typed `@name`
// notifies nobody), how to answer in a thread, which channel to choose or how
// to hand a problem to its manager — the gap Chatlet had until its own
// tools were added. The names are slack-mcp-server's at the release
// internal/config's pinnedToolServers reads it at, under the `slack_` prefix
// docs/integrations/slack.md gives its entry; the pin test there is what
// sends a release that renames them back through every page that names them.
func TestTheBundledChatPracticeSkillsFireOnSlack(t *testing.T) {
	t.Parallel()
	const (
		post     = "slack_conversations_add_message"
		mine     = "slack_channels_me"
		channels = "slack_channels_list"
		users    = "slack_users_search"
	)
	for _, tc := range []struct {
		file, key string
		tools     []string
	}{
		// A mention is spelled in the post, so the post waits on it.
		{"platform-mentions.md", "skill:platform_mentions", []string{post}},
		{"getting-unstuck.md", "skill:getting_unstuck", []string{post}},
		{"observed-directives.md", "skill:observed_directives", []string{post}},
		// Choosing a channel is listing them, finding somebody's DM, or
		// posting in one.
		{"channel-discovery.md", "skill:channel_discovery", []string{mine, channels, users, post}},
	} {
		s := bundled(t, tc.file)
		if s.Key != tc.key {
			t.Fatalf("%s: key = %q, want %q", tc.file, s.Key, tc.key)
		}
		catalogue := registry(t, s)
		for _, tool := range tc.tools {
			t.Run(tc.key+"/"+tool, func(t *testing.T) {
				t.Parallel()
				if len(catalogue.Matching(prompts.PhaseExecute, surface([]string{tool}))) != 1 {
					t.Fatalf("a seat holding only %s is never offered %s (trigger %+v)",
						tool, tc.key, s.Trigger)
				}
			})
		}
	}
	// READS NEVER WAIT on the mentions skill, on Slack as on Chatlet.
	mentions := registry(t, bundled(t, "platform-mentions.md"))
	reads := []string{"slack_conversations_history", "slack_conversations_replies", channels, mine}
	if got := mentions.Matching(prompts.PhaseExecute, surface(reads)); len(got) != 0 {
		t.Fatalf("a seat holding only Slack's reads (%v) is gated on the mentions skill", reads)
	}
}
