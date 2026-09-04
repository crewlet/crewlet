package gitlab_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/notify"
)

type stubParties map[string]notify.Party

func (p stubParties) ByExternalID(transport, id string) (notify.Party, bool) {
	if transport != gitlab.Backend {
		return notify.Party{}, false
	}
	party, ok := p[id]
	return party, ok
}

// ByHandle answers nothing: a GitLab prompt must resolve its actor through
// the transport-scoped id its payload carries, and a stub that answered here
// would let a lookup by the wrong identity pass.
func (stubParties) ByHandle(string) (notify.Party, bool) { return notify.Party{}, false }

func note(reason string, meta map[string]string) notify.Inbound {
	m := map[string]string{
		"event_type": reason, "project": "nimbus/api", "mr_iid": "42",
		"url":             "https://gitlab.example.com/nimbus/api/-/merge_requests/42",
		notify.ActorField: "human-dev", "actor_name": "human-dev",
	}
	for k, v := range meta {
		if v == "" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return notify.Inbound{
		Source: gitlab.Backend, EventType: reason, Sender: "human-dev",
		Subject: "Add the rate limiter", Body: "closes the gap", Metadata: m,
	}
}

func build(t *testing.T, n notify.Inbound, parties notify.Parties) string {
	t.Helper()
	text := gitlab.Prompt{}.Build(n, parties)
	if strings.TrimSpace(text) == "" {
		t.Fatal("the prompt rendered nothing")
	}
	return text
}

func says(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Fatalf("the prompt does not say %q:\n%s", w, text)
		}
	}
}

func silentOn(t *testing.T, text string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(text, w) {
			t.Fatalf("the prompt says %q and should not:\n%s", w, text)
		}
	}
}

// THE ROUTING REASON IS THE PROMPT. One merge request event reaches a
// reviewer, an assignee and a watcher, and each is asked for something
// different.
func TestEachReasonAsksForSomethingDifferent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason string
		says   []string
		quiet  []string
	}{
		{gitlab.MRReview,
			[]string{"requested to review", "Read the diff", "not to review"},
			[]string{"deliberately", "Evaluate Before Acting"}},
		{gitlab.MRAssigned,
			[]string{"been assigned a merge request", "push and update"},
			[]string{"requested to review", "coding runtime"}},
		{gitlab.IssueAssigned,
			[]string{"been assigned an issue", "coding runtime"},
			[]string{"push and update the merge request"}},
		{gitlab.NoteMention,
			[]string{"mentioned in a comment", "Evaluate Before Acting"},
			[]string{"been assigned", "requested to review"}},
		{gitlab.MRMention,
			[]string{"mentioned in a issue or merge-request description"},
			[]string{"been assigned"}},
		{gitlab.PipelineFailed,
			[]string{"has FAILED", "Read the job log", "deliberately"},
			[]string{"Evaluate Before Acting"}},
		{gitlab.IssueClosed,
			[]string{"was CLOSED", "Stop any in-flight work"},
			[]string{"Evaluate Before Acting"}},
		{"merge_request.merge",
			[]string{"take part in this thread", "Evaluate Before Acting"},
			[]string{"been assigned", "deliberately"}},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			text := build(t, note(tc.reason, nil), nil)
			says(t, text, tc.says...)
			silentOn(t, text, tc.quiet...)
		})
	}
}

// An approval rule a later release adds produces a reason nobody has taught
// this prompt, and it still yields a usable trigger.
func TestAnUnknownReasonFallsBackToWatching(t *testing.T) {
	t.Parallel()
	says(t, build(t, note("merge_request.some_future_action", nil), nil),
		"take part in this thread", "nimbus/api!42")
}

// THE ONE EXCEPTION TO THE SELF-ACTION RULE, and the only one in the engine.
// A build runs minutes after the push and reports something nobody could
// have predicted; every other event here reports what somebody DID.
func TestOnlyAFailedPipelineWakesItsActor(t *testing.T) {
	t.Parallel()
	if !(gitlab.Prompt{}).WakesActor(gitlab.PipelineFailed) {
		t.Fatal("a failed pipeline does not reach the person who broke it")
	}
	for _, reason := range []string{
		gitlab.MRReview, gitlab.MRAssigned, gitlab.IssueAssigned,
		gitlab.NoteMention, gitlab.NoteComment, gitlab.IssueClosed,
		gitlab.MRMention, gitlab.IssueMention, "merge_request.merge",
		"pipeline.success", "",
	} {
		if (gitlab.Prompt{}).WakesActor(reason) {
			t.Fatalf("%q wakes its own actor", reason)
		}
	}
}

// The exception is stated IN the prompt, because a seat that has learned "I
// am not told about my own actions" reads its own name here as a routing
// mistake otherwise.
func TestThePipelinePromptExplainsWhyItReachedItsOwnActor(t *testing.T) {
	t.Parallel()
	says(t, build(t, note(gitlab.PipelineFailed, nil), nil),
		"You are being told about your own action deliberately",
		"you are the one who can fix it")
}

// THE RECON FLAG SPLITS POINTERS FROM CONTENT. A review request carries a
// description while the thing to read is the diff; a comment's body IS what
// was said, so the trigger is the context.
func TestOnlyPointerEventsRequireRecon(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{
		gitlab.MRReview, gitlab.MRAssigned, gitlab.IssueAssigned, gitlab.PipelineFailed,
	} {
		if !(gitlab.Prompt{}).RequiresRecon(note(reason, nil)) {
			t.Fatalf("%q does not require recon", reason)
		}
	}
	for _, reason := range []string{
		gitlab.NoteMention, gitlab.NoteComment, gitlab.MRMention,
		gitlab.IssueMention, gitlab.IssueClosed, "merge_request.merge",
	} {
		if (gitlab.Prompt{}).RequiresRecon(note(reason, nil)) {
			t.Fatalf("%q requires recon and carries its own content", reason)
		}
	}
}

// THE ADDRESSED FLAG SPLITS AN ASK FROM NEWS. Leaving an assignment or a
// mention unanswered looks to the person who wrote it exactly like the
// webhook never arrived; a seat obliged to reply to every state change of
// every merge request it has ever touched is noise.
func TestOnlyAnAskAddressesTheSeat(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{
		gitlab.IssueAssigned, gitlab.IssueMention, gitlab.MRAssigned,
		gitlab.MRReview, gitlab.MRMention, gitlab.NoteMention,
	} {
		if !(gitlab.Prompt{}).Addressed(note(reason, nil)) {
			t.Errorf("%q does not address the seat", reason)
		}
	}
	for _, reason := range []string{
		gitlab.NoteComment, gitlab.IssueClosed, gitlab.PipelineFailed,
		"merge_request.merge",
	} {
		if (gitlab.Prompt{}).Addressed(note(reason, nil)) {
			t.Errorf("%q addresses the seat and is news about a thread it follows", reason)
		}
	}
}

// AN IID IS UNIQUE ONLY WITHIN ITS PROJECT. Two repositories both have a !1,
// so a key that was just the number would merge a comment on one with a
// review request on the other.
func TestTheConversationKeyIsProjectQualified(t *testing.T) {
	t.Parallel()
	key := gitlab.Prompt{}.ConversationKey
	if got := key(map[string]string{"project": "nimbus/api", "mr_iid": "42"}, ""); got != "nimbus/api!42" {
		t.Fatalf("a merge request keys on %q", got)
	}
	if got := key(map[string]string{"project": "nimbus/api", "issue_iid": "42"}, ""); got != "nimbus/api#42" {
		t.Fatalf("an issue keys on %q", got)
	}
	// A merge request and an issue numbered the same in one project are
	// two conversations; the separator is what keeps them apart.
	if a, b := key(map[string]string{"project": "p", "mr_iid": "1"}, ""),
		key(map[string]string{"project": "p", "issue_iid": "1"}, ""); a == b {
		t.Fatalf("!1 and #1 share the key %q", a)
	}
	// A pipeline on a branch push names no item: never merged with
	// anything, which is right — two unrelated builds failing are two
	// problems.
	if got := key(map[string]string{"project": "nimbus/api"}, "subject"); got != "" {
		t.Fatalf("an itemless event keys on %q, want nothing", got)
	}
	// And a bare iid with no project would collide across every
	// repository in the instance.
	if got := key(map[string]string{"mr_iid": "42"}, ""); got != "" {
		t.Fatalf("a project-less event keys on %q, want nothing", got)
	}
}

// THE SUPERSEDE RULE: an item hook carries its description as it was on
// every event, so five in a digest is one paragraph five times. A note is a
// message.
func TestOnlyCommentsSurviveADigest(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{gitlab.NoteComment, gitlab.NoteMention} {
		if got := (gitlab.Prompt{}).DigestBody(reason, "looks good"); got != "looks good" {
			t.Fatalf("%s collapsed to %q", reason, got)
		}
	}
	for _, reason := range []string{
		gitlab.MRAssigned, gitlab.IssueAssigned, gitlab.MRReview,
		gitlab.MRMention, gitlab.IssueMention, gitlab.PipelineFailed,
	} {
		if got := (gitlab.Prompt{}).DigestBody(reason, "the description again"); got != "" {
			t.Fatalf("%s kept %q", reason, got)
		}
	}
}

// THE RAW USERNAME IS KEPT ALONGSIDE the colleague label, unlike the
// tracker's: a code host's actor is the handle the seat types to mention
// them back, so dropping it costs the reply.
func TestAKnownActorKeepsTheUsernameBesideTheLabel(t *testing.T) {
	t.Parallel()
	parties := stubParties{"human-dev": {
		Handle: "ana", Name: "Ana Ruiz", Human: true}}
	says(t, build(t, note(gitlab.MRReview, nil), parties),
		"Ana Ruiz (ana, human colleague) — `human-dev`")
}

// A repository has contributors who are not seats here, and the username is
// a real answer for them rather than a degraded one.
func TestAnUnknownActorRendersItsUsername(t *testing.T) {
	t.Parallel()
	says(t, build(t, note(gitlab.MRReview, nil), stubParties{}), "human-dev")

	nameless := note(gitlab.MRReview, nil)
	nameless.Sender = ""
	says(t, build(t, nameless, nil), "someone")
}

// THE REFERENCE IS THE ONE A PERSON WOULD PASTE, because it is both what the
// model fetches with and what a human in the same conversation recognises.
func TestTheItemReferenceReadsAsGitLabWritesIt(t *testing.T) {
	t.Parallel()
	says(t, build(t, note(gitlab.MRReview, nil), nil), "nimbus/api!42",
		"**Link:** https://gitlab.example.com/nimbus/api/-/merge_requests/42")
	says(t, build(t, note(gitlab.IssueAssigned,
		map[string]string{"mr_iid": "", "issue_iid": "7"}), nil), "nimbus/api#7")

	// An event that names no item still renders something followable —
	// the project — rather than a bare "?" the model cannot use.
	says(t, build(t, note(gitlab.PipelineFailed,
		map[string]string{"mr_iid": ""}), nil), "nimbus/api")
	// And with no link there is no empty Link line.
	silentOn(t, build(t, note(gitlab.MRReview,
		map[string]string{"url": ""}), nil), "**Link:**")
}

// NO TOOL IS NAMED. The engine cannot know the deployed MCP server's tool
// names, so a prompt naming one sends the seat after a tool that does not
// exist.
func TestThePromptNamesNoTool(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{
		gitlab.MRReview, gitlab.MRAssigned, gitlab.IssueAssigned,
		gitlab.NoteMention, gitlab.NoteComment, gitlab.MRMention,
		gitlab.IssueMention, gitlab.IssueClosed, gitlab.PipelineFailed,
		"merge_request.merge",
	} {
		text := build(t, note(reason, nil), nil)
		for _, name := range []string{
			"create_merge_request", "approve_merge_request", "get_diff",
			"add_comment", "gitlab_", "()`",
		} {
			if strings.Contains(text, name) {
				t.Fatalf("%s names the tool %q:\n%s", reason, name, text)
			}
		}
	}
}

// The prompt is the vendor's entry in the registry, which keys on the source
// name — a mismatch means every GitLab event silently gets the generic
// fallback, and with it the self-action rule with no exception.
func TestThePromptAnswersForGitLab(t *testing.T) {
	t.Parallel()
	if got := (gitlab.Prompt{}).Source(); got != gitlab.Backend {
		t.Fatalf("the prompt answers for %q", got)
	}
	prompts := notify.NewPrompts(gitlab.Prompt{})
	if !notify.WakesActor(prompts, gitlab.Backend, gitlab.PipelineFailed) {
		t.Fatal("the spine does not reach the actor through the registry")
	}
	if notify.WakesActor(prompts, gitlab.Backend, gitlab.NoteComment) {
		t.Fatal("the spine reaches the actor for their own comment")
	}
}
