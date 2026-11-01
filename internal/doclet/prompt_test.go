package doclet_test

import (
	"regexp"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/notify"
)

// The registry's contract, asserted where the type lives: a source that
// forgets a method is a compile error rather than a silent default.
var _ notify.Prompt = doclet.Prompt{}

// A PAGE IS BOTH KEYS, AND THE ID IS BOTH OF THEM.
//
// The merge unit and the durable thread are one object here — a comment on a
// page is activity ON the page, not a thread of its own — so the identity
// delegates rather than deriving the address a second time. And it is the ID
// rather than the title on both sides: a rename would otherwise split the
// conversation in half, silently, each half looking ordinary.
func TestAPageIsBothThePartitionAndTheConversation(t *testing.T) {
	t.Parallel()
	renamed := map[string]string{
		doclet.MetaPageID: "p-1", doclet.MetaTitle: "Deploy runbook (2026)",
	}
	original := map[string]string{
		doclet.MetaPageID: "p-1", doclet.MetaTitle: "Deploy runbook",
	}
	for _, meta := range []map[string]string{original, renamed} {
		if got := (doclet.Prompt{}).PartitionKey(meta, ""); got != "p-1" {
			t.Errorf("partition key = %q, want the page id", got)
		}
		if got := (doclet.Prompt{}).ConversationIdentity(meta, ""); got != "p-1" {
			t.Errorf("conversation identity = %q, want the page id", got)
		}
	}
	// A wake naming no page derives NEITHER key, and both must agree about
	// that: one deriving a key where the other does not would let a turn be
	// recorded under an address no later change can reproduce.
	empty := map[string]string{doclet.MetaTitle: "Deploy runbook"}
	if got := (doclet.Prompt{}).PartitionKey(empty, "subject"); got != "" {
		t.Errorf("a page-less wake keyed on %q", got)
	}
	if got := (doclet.Prompt{}).ConversationIdentity(empty, "subject"); got != "" {
		t.Errorf("a page-less wake resolved to %q", got)
	}
}

// THE PROMPT NAMES ONLY PAGE TOOLS THIS BUILD REGISTERS.
//
// It names its tools — they are shipped by this build, under names this build
// chose — and a seat acts on what it is shown, so a name that drifted from the
// registered one is a seat told to call something that is not there. The
// names are literals in the prompt because the builtin surface that registers
// them imports this package, so this is the one place the two are held
// together.
func TestThePromptNamesOnlyRegisteredPageTools(t *testing.T) {
	t.Parallel()
	registered := map[string]bool{}
	for _, name := range builtin.PageTools() {
		registered[name] = true
	}
	tool := regexp.MustCompile("`([a-z]+(?:_[a-z]+)*_page)`")
	named := map[string]bool{}
	for _, via := range []string{doclet.ViaMention, doclet.ViaWatcher, doclet.ViaLeadFallback} {
		for _, kind := range []doclet.ChangeKind{
			doclet.ChangeCreated, doclet.ChangeSaved, doclet.ChangeComment, doclet.ChangeRemoved,
		} {
			text := (doclet.Prompt{}).Build(notify.Inbound{
				Source: doclet.Source, EventType: string(kind), Subject: "Deploy runbook",
				Body: "@ana is this current?",
				Metadata: map[string]string{
					doclet.RoutedViaField: via, doclet.MetaPageID: "p-1",
					doclet.MetaChangeKind: string(kind), doclet.MetaVersion: "3",
				},
			}, nil)
			for _, m := range tool.FindAllStringSubmatch(text, -1) {
				named[m[1]] = true
				if !registered[m[1]] {
					t.Errorf("%s/%s names `%s`, which this build does not register:\n%s",
						via, kind, m[1], text)
				}
			}
		}
	}
	// AND THE GUARD SEES SOMETHING: a pattern that matched nothing would
	// pass every prompt, renamed tools included.
	for _, want := range []string{builtin.GetPageTool, builtin.CommentOnPageTool, builtin.SavePageTool} {
		if !named[want] {
			t.Errorf("no prompt names `%s`, so this guard is not reading what it guards", want)
		}
	}
}
