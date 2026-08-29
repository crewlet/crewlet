package plane

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/notify"
)

// stubParties resolves the handful of ids a prompt test cares about.
type stubParties map[string]notify.Party

func (p stubParties) ByExternalID(transport, id string) (notify.Party, bool) {
	if transport != Backend {
		return notify.Party{}, false
	}
	party, ok := p[id]
	return party, ok
}

func inbound(via string, meta map[string]string) notify.Inbound {
	m := map[string]string{
		RoutedViaField:    via,
		"event_type":      "issue.updated",
		"issue_id":        "11111111-1111-1111-1111-111111111111",
		"work_item_key":   "ENG-42",
		"project":         "ENG",
		notify.ActorField: "actor-uuid",
		"actor_name":      "Ana Ruiz",
	}
	for k, v := range meta {
		if v == "" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return notify.Inbound{
		Source: Backend, EventType: m["event_type"],
		Sender: "Ana Ruiz", Subject: "Fix the login redirect",
		Body: "the redirect loops on staging", Metadata: m,
	}
}

func build(t *testing.T, n notify.Inbound, parties notify.Parties) string {
	t.Helper()
	text := Prompt{}.Build(n, parties)
	if strings.TrimSpace(text) == "" {
		t.Fatal("the prompt rendered nothing")
	}
	return text
}

func mustContain(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Fatalf("the prompt does not say %q:\n%s", w, text)
		}
	}
}

func mustNotContain(t *testing.T, text string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(text, w) {
			t.Fatalf("the prompt says %q and should not:\n%s", w, text)
		}
	}
}

// THE ROUTING REASON IS THE PROMPT. One payload reaches an assignee, a
// watcher and a lead, and each is asked for something different — which is
// the whole reason the parser stamps a reason rather than letting the prompt
// re-derive one.
func TestEachRoutingReasonAsksForSomethingDifferent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		via   string
		says  []string
		quiet []string
	}{
		{via: ViaAssignee,
			says:  []string{"work item you are assigned to", "Do the work it describes"},
			quiet: []string{"Why you received this", "@-mentioned"}},
		{via: ViaAssigneeAdded,
			says:  []string{"You have been assigned", "Do the work it describes"},
			quiet: []string{"Why you received this"}},
		{via: ViaMention,
			says:  []string{"@-mentioned", "Evaluate Before Acting"},
			quiet: []string{"Do the work it describes", "Why you received this"}},
		{via: ViaSubscriber,
			says:  []string{"you follow", "Evaluate Before Acting"},
			quiet: []string{"Why you received this", "Do the work it describes"}},
		{via: ViaLeadFallback,
			says:  []string{"Why you received this", "by FALLBACK", "Delegate"},
			quiet: []string{"@-mentioned"}},
	} {
		t.Run(tc.via, func(t *testing.T) {
			t.Parallel()
			text := build(t, inbound(tc.via, nil), nil)
			mustContain(t, text, tc.says...)
			mustNotContain(t, text, tc.quiet...)
		})
	}
}

// A reason nobody has taught this prompt still produces a usable trigger:
// the watching framing, which asks the seat to decide rather than to act.
func TestAnUnknownRoutingReasonFallsBackToWatching(t *testing.T) {
	t.Parallel()
	text := build(t, inbound("some_future_reason", nil), nil)
	mustContain(t, text, "Evaluate Before Acting", "ENG-42")
}

// The lead routings are the ONLY ones that explain themselves, and that is
// the point: a directed routing carries its own signal, while "you lead the
// team" left unexplained reads as "this is yours".
func TestOnlyTheLeadRoutingsExplainWhy(t *testing.T) {
	t.Parallel()
	for _, via := range []string{ViaLeadFallback, ViaIntake} {
		text := build(t, inbound(via, nil), nil)
		mustContain(t, text, "Why you received this", "Delegate",
			"Take it yourself", "Escalate", "the **ENG** Plane project")
	}
	for _, via := range []string{ViaAssignee, ViaAssigneeAdded, ViaMention, ViaSubscriber} {
		mustNotContain(t, build(t, inbound(via, nil), nil), "Why you received this")
	}
}

// Intake and lead-fallback are different situations and say so: intake has
// no owner YET, fallback has no owner AT ALL.
func TestIntakeAndFallbackDoNotShareTheirReason(t *testing.T) {
	t.Parallel()
	mustContain(t, build(t, inbound(ViaIntake, nil), nil),
		"intake work item needs triage", "have no owner until somebody triages")
	mustContain(t, build(t, inbound(ViaLeadFallback, nil), nil),
		"has no assignee", "nobody else is watching this item")
}

// A deletion has nothing to fetch and no work to do. Sending the seat off to
// read a work item that no longer exists is a guaranteed failed tool call.
func TestADeletedWorkItemTellsTheSeatToStop(t *testing.T) {
	t.Parallel()
	text := build(t, inbound(ViaAssignee, map[string]string{
		"event_type": "issue.deleted"}), nil)
	mustContain(t, text, "was deleted", "stop any in-flight work")
	mustNotContain(t, text, "Do the work it describes")
}

// The recon block and the recon FLAG answer on one condition. A flag without
// the block sends a seat looking with nothing to look for.
func TestTheReconFlagAndTheReconBlockAgree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"a work item", nil, true},
		{"a page", map[string]string{"issue_id": "", "page_id": "p-1"}, true},
		{"neither", map[string]string{"issue_id": "", "work_item_key": ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := inbound(ViaSubscriber, tc.meta)
			flag := Prompt{}.RequiresRecon(n)
			block := strings.Contains(build(t, n, nil), "## Get full context")
			if flag != tc.want || block != tc.want {
				t.Fatalf("flag=%v block=%v, want both %v", flag, block, tc.want)
			}
		})
	}
}

// The link rides the recon block where the parser could build one, because
// following a pointer by hand is what a person does when the fetch fails.
func TestTheReconBlockCarriesTheLinkWhenThereIsOne(t *testing.T) {
	t.Parallel()
	text := build(t, inbound(ViaAssignee, map[string]string{
		"url": "https://plane.example.com/w/projects/p/issues/i"}), nil)
	mustContain(t, text, "Link: https://plane.example.com/w/projects/p/issues/i")

	mustNotContain(t, build(t, inbound(ViaAssignee, nil), nil), "Link:")
}

// A page event is the one routing with no alternative recipient, so it says
// so — otherwise a lead reads a stream of page edits as unexplained noise.
func TestAPageEventExplainsThatPagesHaveNoWatchers(t *testing.T) {
	t.Parallel()
	text := build(t, inbound(ViaPageLead, map[string]string{
		"event_type": "page.created", "issue_id": "",
		"work_item_key": "", "page_id": "page-uuid"}), nil)
	mustContain(t, text, "A page was created", "no per-page watchers",
		"**Project:** ENG", "page `page-uuid`")
	// A page carries no work-item reference, and rendering the "?" the
	// display helper falls back to would be noise.
	mustNotContain(t, text, "**Work item:**")
}

// THE DISPLAY KEY IS NOT THE CONVERSATION KEY. The sequence number rides
// only the issue payload and the identifier in front of it needs a warm
// cache, so keying on it would split one item's comments from its updates
// into two partitions — silently, because each looks like a conversation.
func TestTheConversationKeyIsTheEntityNotTheDisplayKey(t *testing.T) {
	t.Parallel()
	issue := inbound(ViaAssignee, nil).Metadata
	comment := inbound(ViaMention, map[string]string{
		"work_item_key": "", "event_type": "issue_comment.created",
		"comment_id": "c-1"}).Metadata

	got, want := Prompt{}.ConversationKey(comment, ""), issue["issue_id"]
	if got != want {
		t.Fatalf("a comment keys on %q, want the work item %q", got, want)
	}
	if key := (Prompt{}).ConversationKey(issue, ""); key != want {
		t.Fatalf("the issue keys on %q, want %q", key, want)
	}
	if strings.Contains(got, "ENG-42") {
		t.Fatalf("the conversation key carries the display key: %q", got)
	}
}

// A page's conversation is the page. An event naming neither entity derives
// no key at all, so it is never merged with anything.
func TestAPageKeysOnItselfAndAnEntitylessEventKeysOnNothing(t *testing.T) {
	t.Parallel()
	page := map[string]string{"page_id": "page-uuid"}
	if got := (Prompt{}).ConversationKey(page, "subject"); got != "page-uuid" {
		t.Fatalf("a page keys on %q", got)
	}
	if got := (Prompt{}).ConversationKey(map[string]string{}, "subject"); got != "" {
		t.Fatalf("an entityless event keys on %q, want nothing", got)
	}
}

// THE SUPERSEDE RULE: a description snapshot is re-sent whole on every field
// change, so five in a digest is one paragraph five times. A comment is
// something a person actually said.
func TestOnlyCommentsSurviveADigest(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"issue_comment.created", "issue_comment.updated"} {
		if got := (Prompt{}).DigestBody(kind, "looks good to me"); got != "looks good to me" {
			t.Fatalf("%s collapsed to %q", kind, got)
		}
	}
	for _, kind := range []string{"issue.created", "issue.updated", "page.updated"} {
		if got := (Prompt{}).DigestBody(kind, "the whole description again"); got != "" {
			t.Fatalf("%s kept %q", kind, got)
		}
	}
}

// THE TRACKER NEVER WAKES ITS ACTOR. Every event here reports what somebody
// did, which they know; the exception is for a consequence they could not
// have seen coming, and a tracker emits none.
func TestNoTrackerEventWakesItsOwnActor(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{
		"issue.created", "issue.updated", "issue.deleted",
		"issue_comment.created", "page.updated", "intake_issue.created",
	} {
		if (Prompt{}).WakesActor(kind) {
			t.Fatalf("%s wakes its actor", kind)
		}
	}
}

// The actor is a workspace UUID. Only the registry can turn it into somebody
// the seat recognises — and a HUMAN one especially, since everything the
// seat can do next depends on whether the counterparty can be reached
// directly.
func TestAKnownActorRendersAsAColleague(t *testing.T) {
	t.Parallel()
	parties := stubParties{"actor-uuid": {
		Handle: "ana", Name: "Ana Ruiz", Human: true}}
	mustContain(t, build(t, inbound(ViaMention, nil), parties),
		"Ana Ruiz (ana, human colleague)")

	agent := stubParties{"actor-uuid": {
		Handle: "cto", Name: "CTO", AgentID: uuid.New()}}
	text := build(t, inbound(ViaMention, nil), agent)
	mustContain(t, text, "CTO (cto)")
	mustNotContain(t, text, "human colleague")
}

// Most people in a workspace are not seats here. The payload's display name
// is the answer for them — a real one, not a degradation.
func TestAnUnknownActorFallsBackThroughTheNamesItHas(t *testing.T) {
	t.Parallel()
	mustContain(t, build(t, inbound(ViaMention, nil), stubParties{}), "Ana Ruiz")

	nameless := inbound(ViaMention, map[string]string{"actor_name": ""})
	nameless.Sender = ""
	mustContain(t, build(t, nameless, nil), "**By:** someone")
}

// A work item whose sequence number never resolved still has to be
// followable: the UUID is what a seat pastes into a fetch, and an unusable
// reference is worse than an ugly one.
func TestAnUnresolvedDisplayKeyStillRendersTheUUID(t *testing.T) {
	t.Parallel()
	text := build(t, inbound(ViaAssignee, map[string]string{"work_item_key": ""}), nil)
	mustContain(t, text, "11111111-1111-1111-1111-111111111111")
	mustNotContain(t, text, "**Work item:** ? ")
}

// NO TOOL IS NAMED. The engine cannot know the deployed MCP server's tool
// names, so a prompt that named one would send the seat after a tool that
// does not exist.
func TestThePromptNamesNoTool(t *testing.T) {
	t.Parallel()
	for _, via := range []string{
		ViaAssignee, ViaAssigneeAdded, ViaMention, ViaSubscriber,
		ViaLeadFallback, ViaIntake, ViaPageLead,
	} {
		text := build(t, inbound(via, nil), nil)
		for _, name := range []string{
			"lookup_colleague", "create_issue", "add_comment",
			"get_issue", "plane_", "()`",
		} {
			if strings.Contains(text, name) {
				t.Fatalf("%s names the tool %q:\n%s", via, name, text)
			}
		}
	}
}

// The prompt is the vendor's entry in the registry, and the registry keys on
// the source name — a mismatch means every Plane event silently gets the
// generic fallback instead.
func TestThePromptAnswersForPlane(t *testing.T) {
	t.Parallel()
	if got := (Prompt{}).Source(); got != Backend {
		t.Fatalf("the prompt answers for %q, want %q", got, Backend)
	}
	if got := notify.NewPrompts(Prompt{}).For(Backend); got.Source() != Backend {
		t.Fatalf("the registry resolved %q", got.Source())
	}
}
