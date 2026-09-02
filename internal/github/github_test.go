package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// What these tests protect.
//
// The routing rules are the whole integration, and every one of their
// failures is SILENT: a delivery that names nobody is indistinguishable from
// a quiet repository, and one that names the wrong person is a turn nobody
// asked for. So the cases below are written as the invariant rather than as
// the function — "a review request reaches the reviewer named on it", not
// "routePullRequest returns one target".

// logs captures what the package writes, because two of the rules below are
// DIAGNOSTIC rather than behavioural: a delivery that names nobody routable
// reaches nobody whether or not the code noticed why, so the log line is the
// only thing that separates "the review request found no seat" from "no
// review was requested". Without an assertion on it, removing the branch
// that says so changes nothing any other test can see.
var logs = &syncBuffer{}

// syncBuffer is a writer the package's own logger and the parallel tests
// below can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) contains(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(b.buf.String(), s)
}

func TestMain(m *testing.M) {
	// DEBUG, because the lines these tests read are debug ones: a routing
	// decision nobody is woken by is not a warning. The handler resolves
	// the root logger per record, so a logger this package took at init
	// still lands here.
	logging.Configure(slog.LevelDebug, logging.FormatText, logs)
	code := m.Run()
	logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard)
	os.Exit(code)
}

// delivery builds one webhook envelope the way the API edge does: the event
// in a LOWERCASED header, the body as raw bytes.
func delivery(event, body string) types.RawWebhook {
	return types.RawWebhook{
		Headers: map[string]string{"x-github-event": event},
		BodyRaw: []byte(body),
	}
}

// route runs the parser with no registry, which lets every login through —
// the question here is which logins a payload implies.
func route(t *testing.T, event, body string) []notify.Routed {
	t.Helper()
	out, err := github.NewParser(github.ParserOptions{}).
		Parse(context.Background(), delivery(event, body), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return out
}

// reasons maps each routed login to the reason it was routed for.
func reasons(routed []notify.Routed) map[string]string {
	out := map[string]string{}
	for _, r := range routed {
		for _, id := range r.To.ExternalIDs {
			out[id] = r.Inbound.Metadata["event_type"]
		}
	}
	return out
}

func TestAReviewRequestReachesTheReviewerNamedOnIt(t *testing.T) {
	t.Parallel()
	got := reasons(route(t, "pull_request", `{
		"action": "review_requested",
		"requested_reviewer": {"login": "Reviewer"},
		"pull_request": {"number": 7, "title": "Add the thing",
			"user": {"login": "author"}, "html_url": "https://github.com/acme/api/pull/7"},
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "requester"}
	}`))

	// LOWERCASED. GitHub preserves the case an account was created with
	// and echoes whatever case a mention was typed in, so a login compared
	// raw is one person the engine sees as two.
	if got["reviewer"] != github.PRReviewRequested {
		t.Fatalf("the reviewer was not asked to review: %v", got)
	}
	// THE AUTHOR IS NOT WOKEN. Somebody requesting a review from a third
	// party is not news to the person who opened the pull request.
	if _, woken := got["author"]; woken {
		t.Errorf("the author was woken by a review request aimed at somebody else: %v", got)
	}
}

// A TEAM REVIEW REQUEST NAMES NO PERSON, and the org half of `@acme/team` is
// not one either. Reading it as a login wakes whichever seat happens to share
// the organization's name, on every team ping.
func TestATeamReviewRequestWakesNobody(t *testing.T) {
	t.Parallel()
	got := route(t, "pull_request", `{
		"action": "review_requested",
		"requested_team": {"slug": "reviewers", "name": "Reviewers"},
		"pull_request": {"number": 7, "user": {"login": "author"}},
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "requester"}
	}`)
	if len(got) != 0 {
		t.Fatalf("a team review request routed to %v", reasons(got))
	}
	// AND IT SAYS SO. Reaching nobody is also what a repository with no
	// seats in it looks like, so an operator whose team review requests
	// are silent has nothing else to go on.
	if !logs.contains("github_team_review_request_not_routed") {
		t.Error("a team review request reached nobody silently")
	}
}

// AN ASSIGNMENT NAMES ONE PARTY, so it routes to one.
//
// The self-hosted code host beside this one sends the whole assignee list on
// every update and leaves the reader to diff it. Reading GitHub's list the
// same way would ping every existing assignee each time anybody was added.
func TestAnAssignmentReachesOnlyTheNewAssignee(t *testing.T) {
	t.Parallel()
	got := reasons(route(t, "issues", `{
		"action": "assigned",
		"assignee": {"login": "newcomer"},
		"issue": {"number": 3, "title": "Fix it", "user": {"login": "author"},
			"assignees": [{"login": "existing"}, {"login": "newcomer"}]},
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "manager"}
	}`))
	if got["newcomer"] != github.IssueAssigned {
		t.Fatalf("the new assignee was not told: %v", got)
	}
	if _, woken := got["existing"]; woken {
		t.Errorf("an existing assignee was re-notified by somebody else's "+
			"assignment: %v", got)
	}
}

// A MERGE AND AN ABANDONMENT ARRIVE AS ONE ACTION, and to the author they
// are opposite outcomes. GitHub distinguishes them with a boolean and this
// is the only point that can still tell them apart.
func TestAClosedPullRequestSaysWhetherItLanded(t *testing.T) {
	t.Parallel()
	body := func(merged string) string {
		return `{
			"action": "closed",
			"pull_request": {"number": 7, "merged": ` + merged + `,
				"user": {"login": "author"}, "title": "Add the thing"},
			"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
			"sender": {"login": "maintainer"}
		}`
	}
	if got := reasons(route(t, "pull_request", body("true"))); got["author"] != github.PRMerged {
		t.Errorf("a merged pull request reached its author as %q", got["author"])
	}
	if got := reasons(route(t, "pull_request", body("false"))); got["author"] != github.PRClosed {
		t.Errorf("an abandoned pull request reached its author as %q", got["author"])
	}
}

// A CHANGES-REQUESTED REVIEW IS A DIRECT ASK, and it is the strongest one a
// code host makes. Rendered as ordinary thread activity an agent reads a
// blocking review as news and leaves the pull request open.
func TestAReviewCarriesItsVerdictToTheAuthor(t *testing.T) {
	t.Parallel()
	body := func(state string) string {
		return `{
			"action": "submitted",
			"review": {"state": "` + state + `", "body": "Needs a test",
				"user": {"login": "reviewer"}},
			"pull_request": {"number": 7, "user": {"login": "author"}, "title": "Add"},
			"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
			"sender": {"login": "reviewer"}
		}`
	}
	for state, want := range map[string]string{
		"changes_requested": github.PRChangesRequested,
		"approved":          github.PRApproved,
		"commented":         github.PRReviewed,
		// A verdict a later API version adds is thread activity with a
		// body rather than an unknown that reaches nobody.
		"dismissed": github.PRReviewed,
	} {
		if got := reasons(route(t, "pull_request_review", body(state))); got["author"] != want {
			t.Errorf("a %q review reached the author as %q, want %q",
				state, got["author"], want)
		}
	}
}

// ONLY A FAILED RUN, and only its actor.
//
// A cancel is somebody deciding the run was unnecessary; waking the pusher to
// fix a build that was deliberately stopped is noise. A green run is not news
// at all.
func TestOnlyAFailedWorkflowRunWakesItsActor(t *testing.T) {
	t.Parallel()
	body := func(conclusion string) string {
		return `{
			"action": "completed",
			"workflow_run": {"name": "CI", "conclusion": "` + conclusion + `",
				"head_branch": "topic", "actor": {"login": "pusher"},
				"html_url": "https://github.com/acme/api/actions/runs/1",
				"pull_requests": [{"number": 7}]},
			"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
			"sender": {"login": "pusher"}
		}`
	}
	if got := reasons(route(t, "workflow_run", body("failure"))); got["pusher"] != github.WorkflowFailed {
		t.Fatalf("a red run did not reach the person who caused it: %v", got)
	}
	for _, conclusion := range []string{"success", "cancelled", "timed_out", "skipped"} {
		if got := route(t, "workflow_run", body(conclusion)); len(got) != 0 {
			t.Errorf("a %q run woke %v", conclusion, reasons(got))
		}
	}
	// AND THE SPINE IS TOLD IT MAY. Every other event reports what
	// somebody did, which that somebody already knows — so the actor is
	// suppressed unless the prompt says otherwise, and this is the one
	// exception in the engine.
	if !(github.Prompt{}).WakesActor(github.WorkflowFailed) {
		t.Error("the one self-action exception is not declared, so the only " +
			"person who can fix a red build is never told")
	}
	if (github.Prompt{}).WakesActor(github.CommentAdded) {
		t.Error("a comment wakes its own author")
	}
}

// AN EDIT ONLY PINGS THE NAMES IT ADDED, which is GitHub's own rule.
// Ignoring the diff pings every named person on every typo fix.
func TestAnEditPingsOnlyTheNewlyMentioned(t *testing.T) {
	t.Parallel()
	got := reasons(route(t, "issue_comment", `{
		"action": "edited",
		"changes": {"body": {"from": "cc @ana"}},
		"comment": {"body": "cc @ana @ben", "user": {"login": "writer"}},
		"issue": {"number": 3, "user": {"login": "author"},
			"assignees": [{"login": "worker"}]},
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "writer"}
	}`))
	if got["ben"] != github.CommentMention {
		t.Fatalf("the newly named person was not pinged: %v", got)
	}
	if _, again := got["ana"]; again {
		t.Errorf("somebody already named was re-pinged by an edit: %v", got)
	}
	// AND NOTHING ELSE ABOUT AN EDIT REACHES ANYBODY. An edited comment is
	// not a new comment: the author and the assignees already read it.
	if _, woken := got["author"]; woken {
		t.Errorf("an edit woke the item's author: %v", got)
	}
	if _, woken := got["worker"]; woken {
		t.Errorf("an edit woke an assignee: %v", got)
	}
}

// A DELETED COMMENT NAMES NOBODY: whatever it said is gone, and a
// notification pointing at it sends the recipient to a 404.
func TestADeletedCommentRoutesNothing(t *testing.T) {
	t.Parallel()
	got := route(t, "issue_comment", `{
		"action": "deleted",
		"comment": {"body": "cc @ana", "user": {"login": "writer"}},
		"issue": {"number": 3, "user": {"login": "author"}},
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "writer"}
	}`)
	if len(got) != 0 {
		t.Fatalf("a deleted comment routed to %v", reasons(got))
	}
}

// A MENTION OUTRANKS A FAN-OUT REASON, because it is the stronger claim on
// the recipient's attention and the prompt renders it differently.
func TestTheStrongerReasonWinsPerPerson(t *testing.T) {
	t.Parallel()
	got := reasons(route(t, "issue_comment", `{
		"action": "created",
		"comment": {"body": "@author what do you think?", "user": {"login": "writer"}},
		"issue": {"number": 3, "title": "Fix it", "user": {"login": "author"}},
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "writer"}
	}`))
	if got["author"] != github.CommentMention {
		t.Fatalf("the author was woken as a watcher rather than as the person "+
			"who was asked a question: %v", got)
	}
	if len(got) != 1 {
		t.Errorf("one person was woken twice: %v", got)
	}
}

// AN EVENT WITH NO HEADER, AND ONE THIS BUILD DOES NOT ROUTE, BOTH GO QUIET.
//
// Neither is an error: naking a delivery would have GitHub redeliver a
// payload that will be just as unroutable next time.
func TestAnUnroutableDeliveryIsQuietRatherThanFailed(t *testing.T) {
	t.Parallel()
	parser := github.NewParser(github.ParserOptions{})
	for _, tc := range []struct{ name, event string }{
		{"no header at all", ""},
		{"a push", "push"},
		// check_run reports the SAME failing Actions run as workflow_run,
		// once per job — routing both wakes one seat as many times as the
		// workflow has jobs.
		{"a check run", "check_run"},
		{"a star", "star"},
	} {
		out, err := parser.Parse(context.Background(),
			delivery(tc.event, `{"action": "completed",
				"repository": {"full_name": "acme/api"}, "sender": {"login": "x"}}`), nil)
		if err != nil {
			t.Errorf("%s: parse = %v", tc.name, err)
		}
		if len(out) != 0 {
			t.Errorf("%s routed to %v", tc.name, reasons(out))
		}
	}
}

// A COMMENT ON A PULL REQUEST IS AN `issue_comment`, because GitHub models a
// pull request as an issue with a diff. A reader taking the payload at face
// value files every pull-request comment under the wrong kind — and then
// looks its participants up on the issues collection, which has no reviews.
func TestAPullRequestCommentIsRecognisedAsOne(t *testing.T) {
	t.Parallel()
	var asked struct{ kind string }
	parser := github.NewParser(github.ParserOptions{
		Participants: participantsFunc(func(_ context.Context, _, _, kind string, _ int) ([]string, error) {
			asked.kind = kind
			return []string{"commenter"}, nil
		}),
	})
	out, err := parser.Parse(context.Background(), delivery("issue_comment", `{
		"action": "created",
		"comment": {"body": "looks good", "user": {"login": "writer"}},
		"issue": {"number": 7, "title": "Add the thing", "user": {"login": "author"},
			"pull_request": {"url": "https://api.github.com/repos/acme/api/pulls/7"}},
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "writer"}
	}`), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if asked.kind != "pull_request" {
		t.Fatalf("participants were looked up as %q, so a pull request's "+
			"reviewers were never asked for", asked.kind)
	}
	got := reasons(out)
	if got["commenter"] != github.CommentAdded {
		t.Errorf("the fan-out did not reach a previous commenter: %v", got)
	}
	// AND THE REFERENCE IS A PULL REQUEST'S. It decides which conversation
	// the notification coalesces into.
	for _, r := range out {
		if r.Inbound.Metadata["pr_number"] != "7" {
			t.Errorf("the copy names no pull request: %v", r.Inbound.Metadata)
		}
	}
}

// participantsFunc adapts a function to the parser's seam.
type participantsFunc func(ctx context.Context, owner, repo, kind string, number int) ([]string, error)

func (f participantsFunc) Of(ctx context.Context, owner, repo, kind string, number int) ([]string, error) {
	return f(ctx, owner, repo, kind, number)
}

// A FAILED LOOKUP COSTS REACH ON THE WATCHING LAYER AND NOTHING ELSE.
//
// Losing a fan-out is a smaller harm than a delivery that raises: the
// directed targets are in the payload and route with no credential at all.
func TestAFailedParticipantLookupKeepsTheDirectedTargets(t *testing.T) {
	t.Parallel()
	parser := github.NewParser(github.ParserOptions{
		Participants: participantsFunc(func(context.Context, string, string, string, int) ([]string, error) {
			return nil, errors.New("rate limited")
		}),
	})
	out, err := parser.Parse(context.Background(), delivery("issue_comment", `{
		"action": "created",
		"comment": {"body": "@ana take a look", "user": {"login": "writer"}},
		"issue": {"number": 3, "title": "Fix it", "user": {"login": "author"}},
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "writer"}
	}`), nil)
	if err != nil {
		t.Fatalf("a failed lookup failed the delivery: %v", err)
	}
	got := reasons(out)
	if got["ana"] != github.CommentMention || got["author"] != github.CommentAdded {
		t.Fatalf("the payload's own targets were lost with the lookup: %v", got)
	}
}

// THE REGISTRY IS THE ONE GATE. A repository has contributors who are not
// seats here, and a comment naming three of them would otherwise produce
// three notifications the service cannot deliver.
func TestOnlyRoutableLoginsProduceNotifications(t *testing.T) {
	t.Parallel()
	organization := &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{{Name: "Engineer", DeclaredHandle: "eng"}},
	}
	reg := notify.NewRegistry(organization)
	if err := reg.Register(github.Backend, "eng-bot", "eng"); err != nil {
		t.Fatal(err)
	}
	out, err := github.NewParser(github.ParserOptions{}).Parse(context.Background(),
		delivery("issue_comment", `{
			"action": "created",
			"comment": {"body": "@eng-bot and @outsider please look", "user": {"login": "writer"}},
			"issue": {"number": 3, "title": "Fix it", "user": {"login": "stranger"}},
			"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
			"sender": {"login": "writer"}
		}`), reg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := reasons(out)
	if len(got) != 1 || got["eng-bot"] != github.CommentMention {
		t.Fatalf("the gate let a stranger through, or dropped a seat: %v", got)
	}
}

// # Mentions

func TestMentionsReadGitHubsOwnLoginGrammar(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"a plain mention", "@ana please look", []string{"ana"}},
		{"case is normalised", "@Ana and @ANA", []string{"ana"}},
		{"hyphens are part of a login", "@ana-dev shipped it", []string{"ana-dev"}},
		{"a trailing hyphen is not", "@ana- is a typo", []string{"ana"}},
		// An underscore is a valid GitLab username byte and not a valid
		// GitHub one. Reading the other host's grammar here turns
		// `@ana_bot` into one account instead of two — a different person.
		{"an underscore ends a login", "@ana_bot", []string{"ana"}},
		{"an email address mentions nobody", "mail deploy@example.com", nil},
		{"a path fragment mentions nobody", "see docs/@internal/readme", nil},
		{"a team mention names nobody routable", "cc @acme/reviewers", nil},
		{"a team mention does not eat a real one", "@acme/reviewers and @ana",
			[]string{"ana"}},
		{"a trailing slash is not a team", "@ana/ ", []string{"ana"}},
		{"order is kept and duplicates dropped", "@ben @ana @ben",
			[]string{"ben", "ana"}},
		{"a mention at the very start", "@ana", []string{"ana"}},
		{"nothing at all", "no names here", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := github.Mentions(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("Mentions(%q) = %v, want %v", tc.text, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Mentions(%q) = %v, want %v", tc.text, got, tc.want)
				}
			}
		})
	}
}

// # The prompt

// THE CONVERSATION KEY IS REPOSITORY-QUALIFIED, because a number is unique
// only within its repository: two repositories both have a #1, and a bare
// number would merge a comment on one with a review request on the other.
func TestTheConversationKeyNamesTheRepository(t *testing.T) {
	t.Parallel()
	key := func(meta map[string]string) string {
		return (github.Prompt{}).ConversationKey(meta, "")
	}
	if got := key(map[string]string{"repo": "acme/api", "pr_number": "7"}); got != "acme/api#7" {
		t.Errorf("pull request key = %q", got)
	}
	// ONE SEPARATOR FOR BOTH KINDS, which is where this differs from the
	// self-hosted host: GitHub numbers issues and pull requests in one
	// sequence per repository and writes both as #42.
	if got := key(map[string]string{"repo": "acme/api", "issue_number": "3"}); got != "acme/api#3" {
		t.Errorf("issue key = %q", got)
	}
	// A RUN ON A BRANCH PUSH NAMES NO ITEM, so it derives no key and is
	// never merged with anything — two unrelated builds failing are two
	// problems.
	if got := key(map[string]string{"repo": "acme/api"}); got != "" {
		t.Errorf("a workflow run on a branch got the key %q", got)
	}
}

// EACH DIRECTED REASON GETS ITS OWN FRAMING, and the differences are the
// point: an approval says merge it, a change request says the work is not
// finished, a merge says go and tell whoever asked.
func TestTheDirectedPromptsSayDifferentThings(t *testing.T) {
	t.Parallel()
	build := func(reason, body string) string {
		return (github.Prompt{}).Build(notify.Inbound{
			Source: github.Backend, Subject: "Add the thing", Body: body,
			Sender: "reviewer",
			Metadata: map[string]string{
				"event_type": reason, "repo": "acme/api", "pr_number": "7",
				"url": "https://github.com/acme/api/pull/7",
			},
		}, nil)
	}
	for _, tc := range []struct{ reason, want string }{
		{github.PRChangesRequested, "REQUESTED CHANGES"},
		{github.PRApproved, "APPROVED"},
		{github.PRMerged, "MERGED"},
		{github.PRReviewRequested, "requested to review"},
		{github.WorkflowFailed, "FAILED"},
	} {
		got := build(tc.reason, "Needs a test")
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s does not say %q:\n%s", tc.reason, tc.want, got)
		}
		// EVERY ONE NAMES THE ITEM the way a person would paste it, which
		// is simultaneously what the model fetches with.
		if !strings.Contains(got, "acme/api#7") {
			t.Errorf("%s names no item:\n%s", tc.reason, got)
		}
	}

	// A CHANGES-REQUESTED REVIEW SENDS THE READER TO THE LINE COMMENTS.
	// The summary says what is wrong; the lines say where, and a change
	// answering only the summary comes back for a second round.
	if got := build(github.PRChangesRequested, "Needs a test"); !strings.Contains(got, "line comment") {
		t.Errorf("the change request never mentions the line comments:\n%s", got)
	}
	// AND AN APPROVAL SAYS TO MERGE IT. An approved pull request nobody
	// merges is work that never shipped.
	if got := build(github.PRApproved, ""); !strings.Contains(got, "Merge it") {
		t.Errorf("the approval does not say to merge:\n%s", got)
	}
}

// THE FAILED-RUN PROMPT SAYS WHY IT IS ADDRESSED TO ITS OWN ACTOR.
//
// A seat that has learned "I am not told about my own actions" reads its own
// name as a routing mistake otherwise.
func TestTheFailedRunPromptExplainsTheException(t *testing.T) {
	t.Parallel()
	got := (github.Prompt{}).Build(notify.Inbound{
		Source: github.Backend, Subject: "CI on topic", Sender: "pusher",
		Metadata: map[string]string{
			"event_type": github.WorkflowFailed, "repo": "acme/api",
			"url": "https://github.com/acme/api/actions/runs/1",
		},
	}, nil)
	if !strings.Contains(got, "your own action deliberately") {
		t.Errorf("the one self-addressed prompt does not say so:\n%s", got)
	}
}

// A REVIEW COMMENT SAYS WHICH LINE IT IS ABOUT.
//
// "Somebody said this about your code" without saying which code is a
// message the recipient has to go and reconstruct.
func TestAReviewCommentCarriesItsDiffLocation(t *testing.T) {
	t.Parallel()
	out := route(t, "pull_request_review_comment", `{
		"action": "created",
		"comment": {"body": "@ana this leaks", "path": "internal/api/client.go",
			"line": 42, "user": {"login": "reviewer"},
			"html_url": "https://github.com/acme/api/pull/7#discussion_r1"},
		"pull_request": {"number": 7, "title": "Add", "user": {"login": "author"}},
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "reviewer"}
	}`)
	if len(out) == 0 {
		t.Fatal("a review comment routed to nobody")
	}
	meta := out[0].Inbound.Metadata
	if meta["path"] != "internal/api/client.go" || meta["line"] != "42" {
		t.Fatalf("the diff location was lost: %v", meta)
	}
	// AND THE LINK IS THE COMMENT'S OWN. Sending a recipient to the top of
	// a thread with two hundred messages is a link that technically works.
	if meta["url"] != "https://github.com/acme/api/pull/7#discussion_r1" {
		t.Errorf("the link points at the thread rather than the comment: %v", meta)
	}
	if got := (github.Prompt{}).Build(out[0].Inbound, nil); !strings.Contains(got,
		"internal/api/client.go:42") {
		t.Errorf("the prompt never says which line:\n%s", got)
	}
}

// A DIGEST KEEPS WHAT SOMEBODY SAID AND DROPS WHAT IS A SNAPSHOT.
//
// The issue and pull request payloads carry the description as it was on
// every event, so five of them coalesced is one paragraph five times.
func TestADigestKeepsOnlyTheBodiesSomebodyWrote(t *testing.T) {
	t.Parallel()
	p := github.Prompt{}
	if p.DigestBody(github.CommentAdded, "what somebody said") == "" {
		t.Error("a comment lost its body in a digest")
	}
	if p.DigestBody(github.PRChangesRequested, "why it is blocked") == "" {
		t.Error("a review lost its body in a digest")
	}
	if p.DigestBody(github.PRAssigned, "the description again") != "" {
		t.Error("an item snapshot repeated its description in a digest")
	}
}

// # The client

// stub is the record of what a GitHub stand-in was asked for.
//
// SYNCHRONIZED, and not out of caution: a handler runs on the HTTP server's
// OWN goroutines and more than one can be in flight at once. ParticipantsOf
// reads a pull request's comments and its reviews concurrently, and a
// reconcile resolves every seat's credential in parallel — so a map or slice
// captured straight by a handler closure is two goroutines writing one
// value, which is the data race `-race` exists to find. Recording through
// one type is what stops the next stub below from reintroducing it.
type stub struct {
	mu     sync.Mutex
	paths  map[string]int
	posts  []string
	bodies map[string][][]byte
}

// record notes one request and hands the handler back a body it can still
// read: consuming r.Body here without restoring it would leave every
// handler decoding an empty request.
func (s *stub) record(r *http.Request) error {
	var body []byte
	if r.Body != nil {
		var err error
		if body, err = io.ReadAll(r.Body); err != nil {
			return err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths[r.URL.Path]++
	if r.Method == http.MethodPost {
		s.posts = append(s.posts, r.URL.Path)
		s.bodies[r.URL.Path] = append(s.bodies[r.URL.Path], body)
	}
	return nil
}

// saw reports whether a path was asked for.
func (s *stub) saw(path string) bool { return s.count(path) > 0 }

// count is how many times a path was asked for. A round trip that was NOT
// made is as much a property as one that was: a seat with no credential is
// reported without a lookup, and asking anyway would be a request per seat
// against a deployment that has nothing to answer it with.
func (s *stub) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paths[path]
}

// seen is every path asked for, sorted, for a failure message.
func (s *stub) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Sorted(maps.Keys(s.paths))
	return out
}

// forget drops the record, so a second call can be asserted on without the
// first call's paths still in it.
func (s *stub) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.paths)
	s.posts = nil
	clear(s.bodies)
}

// posted is the paths POSTed to, in arrival order.
func (s *stub) posted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.posts)
}

// postedTo decodes the body of the one POST to a path.
//
// DECODED HERE, on the test's own goroutine, rather than in the handler: a
// t.Fatal from a server goroutine ends that goroutine instead of the test,
// so the response is never written and the real failure reaches the test as
// a client-side EOF naming nothing.
func (s *stub) postedTo(t *testing.T, path string) map[string]any {
	t.Helper()
	s.mu.Lock()
	raw := s.bodies[path]
	s.mu.Unlock()
	switch len(raw) {
	case 1:
	case 0:
		t.Fatalf("nothing was POSTed to %s; the posts were %v", path, s.posted())
	default:
		// REFUSED RATHER THAN "the last one": a stub is written to from
		// the server's goroutines, so which body arrived last is not a
		// fact about anything, and a test asserting on it would pass or
		// fail on scheduling.
		t.Fatalf("%d bodies were POSTed to %s, so there is no one body to "+
			"assert on", len(raw), path)
	}
	var body map[string]any
	if err := json.Unmarshal(raw[0], &body); err != nil {
		t.Fatalf("the body POSTed to %s is not an object: %v", path, err)
	}
	return body
}

// newStub is an empty record.
func newStub() *stub {
	return &stub{paths: map[string]int{}, bodies: map[string][][]byte{}}
}

// A hook past the first page read as ABSENT, and every caller uses this
// listing to decide whether Crewlet's own hook already exists — so the
// reconcile created a duplicate on every run, each delivering the same event
// again. GitHub's documented 20-hooks limit is per EVENT per target, so a full
// page is not evidence the listing is complete.
func TestWebhooksAreWalkedToExhaustion(t *testing.T) {
	t.Parallel()
	var pages []string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		// A full page, then a short one: the walk must ask twice.
		if r.URL.Query().Get("page") == "1" {
			rows := make([]string, 0, 100)
			for i := range 100 {
				rows = append(rows, fmt.Sprintf(
					`{"id":%d,"active":true,"events":["push"],"config":{"url":"https://e.example.com/%d"}}`,
					i, i))
			}
			_, _ = fmt.Fprintf(w, "[%s]", strings.Join(rows, ","))
			return
		}
		_, _ = w.Write([]byte(
			`[{"id":999,"active":true,"events":["push"],"config":{"url":"https://e.example.com/last"}}]`))
	})

	got, err := client.OrgWebhooks(context.Background(), "acme")
	if err != nil {
		t.Fatalf("OrgWebhooks: %v", err)
	}
	if len(got) != 101 {
		t.Errorf("hooks = %d, want both pages (101)", len(got))
	}
	if len(pages) < 2 || pages[0] != "1" || pages[1] != "2" {
		t.Errorf("pages requested = %v, want the walk to continue past a full page", pages)
	}
	// The hook only the SECOND page carries is the one a first-page-only
	// listing reported as absent.
	var found bool
	for _, h := range got {
		if h.URL == "https://e.example.com/last" {
			found = true
		}
	}
	if !found {
		t.Error("the hook on the second page was not returned")
	}
}

// The ceiling is a NON-CONVERGENCE guard, so it has to let a converging walk
// through. Compared with >= it refuses a target holding EXACTLY the ceiling —
// with an error saying the target has "more than" that many hooks, when the
// next page is empty and the walk had finished. A refused listing is not a
// cosmetic failure: every caller uses it to decide whether Crewlet's own hook
// exists, so provisioning stops dead on that target.
//
// The limit is read back out of the error rather than hardcoded, so this
// tracks the constant instead of pinning a copy of it.
func TestAWalkStopsPastItsCeilingAndNotAtIt(t *testing.T) {
	t.Parallel()
	full := func(w http.ResponseWriter, page int) {
		rows := make([]string, 0, 100)
		for i := range 100 {
			rows = append(rows, fmt.Sprintf(
				`{"id":%d,"active":true,"events":["push"],"config":{"url":"https://e.example.com/%d-%d"}}`,
				page*100+i, page, i))
		}
		_, _ = fmt.Fprintf(w, "[%s]", strings.Join(rows, ","))
	}

	// A target that never converges must be refused rather than walked for
	// ever, and the refusal must name the limit it hit.
	endless, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		full(w, page)
	})
	_, err := endless.OrgWebhooks(context.Background(), "acme")
	if err == nil {
		t.Fatal("a walk that never converges was not refused")
	}
	m := regexp.MustCompile(`more than (\d+)`).FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("the refusal does not name the limit it hit: %v", err)
	}
	ceiling, _ := strconv.Atoi(m[1])

	// And a target holding EXACTLY that many is listed: full pages up to the
	// ceiling, then the empty page that ends the walk.
	pages := ceiling / 100
	exact, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page > pages {
			_, _ = w.Write([]byte("[]"))
			return
		}
		full(w, page)
	})
	got, err := exact.OrgWebhooks(context.Background(), "acme")
	if err != nil {
		t.Fatalf("a target holding exactly the %d-hook ceiling was refused: %v", ceiling, err)
	}
	if len(got) != ceiling {
		t.Errorf("hooks = %d, want the whole %d", len(got), ceiling)
	}
}

// newTestClient points a client at a stub and asserts the request shape
// every call has to carry. The returned [stub] is what the client asked
// for, recorded under a lock no handler can forget to take.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*github.Client, *stub) {
	t.Helper()
	rec := newStub()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := rec.record(r); err != nil {
			// Errorf, NOT Fatal: this runs on the server's goroutine,
			// where Fatal ends the goroutine rather than the test and
			// the caller sees an unexplained EOF instead of the reason.
			t.Errorf("reading the %s %s body: %v", r.Method, r.URL.Path, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// THE CREDENTIAL PROBE IS ANSWERED FOR EVERY STUB. A reconcile
		// reads /user before it writes anything, because a run that
		// registered webhooks with a dead credential would report
		// nothing trustworthy — so a stub that did not answer it would
		// fail every provisioning test for the wrong reason.
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login": "crewlet-engine"}`))
			return
		}
		// A BEARER, WHICH COVERS ALL THREE TOKEN KINDS. The older `token`
		// scheme works for a classic PAT and not for an installation
		// token: the same credential accepted or refused on which word
		// carried it.
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		// PINNED. GitHub serves its newest API version to a client that
		// names none, so an unpinned client's behaviour changes under a
		// deployment that did not.
		if got := r.Header.Get("X-GitHub-Api-Version"); got != github.APIVersion {
			t.Errorf("X-GitHub-Api-Version = %q", got)
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := github.NewClient(github.ClientOptions{
		APIBase: server.URL, WebBase: server.URL, Token: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, rec
}

// PARTICIPANTS ARE COMPUTED, because GitHub has no endpoint for them — and a
// pull request needs BOTH collections: a reviewer who approved without
// writing anything appears in neither the other.
func TestParticipantsReadCommentsAndReviews(t *testing.T) {
	t.Parallel()
	client, rec := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			_, _ = w.Write([]byte(`[{"user": {"login": "Commenter"}}]`))
		case strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			_, _ = w.Write([]byte(`[{"user": {"login": "Approver"}}]`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})

	got, err := client.ParticipantsOf(context.Background(), "acme", "api", "pull_request", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.saw("/repos/acme/api/issues/7/comments") || !rec.saw("/repos/acme/api/pulls/7/reviews") {
		t.Fatalf("a pull request's participants came from one collection: %v", rec.seen())
	}
	want := map[string]bool{"commenter": true, "approver": true}
	if len(got) != 2 {
		t.Fatalf("participants = %v", got)
	}
	for _, login := range got {
		// LOWERCASED AT THE BOUNDARY, like every other login: an author
		// compared raw against a lowercased mention is one person the
		// engine sees as two.
		if !want[login] {
			t.Errorf("participants = %v, want %v", got, want)
		}
	}

	// AN ISSUE HAS NO REVIEWS, so asking for them would 404 on every
	// comment in the company.
	rec.forget()
	if _, err := client.ParticipantsOf(context.Background(), "acme", "api", "issue", 7); err != nil {
		t.Fatal(err)
	}
	if rec.saw("/repos/acme/api/pulls/7/reviews") {
		t.Error("an issue's participants were looked for in the reviews collection")
	}
}

// A REFUSAL IS TYPED, so a caller deciding what one MEANS does not
// substring-match a message GitHub changes freely.
func TestARefusalCarriesItsStatus(t *testing.T) {
	t.Parallel()
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "Not Found"}`))
	})
	_, err := client.RepoOf(context.Background(), "acme", "missing")
	var apiErr *github.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if !apiErr.NotFound() {
		t.Errorf("a 404 does not report itself as one: %+v", apiErr)
	}
}

// A CREDENTIAL THAT ANSWERS WITH NO LOGIN HOLDS NO SEAT.
//
// An installation token authenticates as an app rather than a person.
// Registering a seat under "" would make it the routing target for every
// event whose login field was missing.
func TestAnAccountWithNoLoginIsRefused(t *testing.T) {
	t.Parallel()
	// NOT THROUGH newTestClient: that stub answers the credential probe
	// for every other test, and this one is about the probe itself.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	client, err := github.NewClient(github.ClientOptions{
		APIBase: server.URL, Token: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Me(context.Background()); err == nil {
		t.Fatal("a login-less account was accepted as a seat's identity")
	}
}

// # Webhooks

// A HOOK IS CREATED AS JSON AND VERIFIED. GitHub's DEFAULT content type is
// form-encoded, which delivers the payload as a urlencoded `payload=` field
// that no verifier or parser here can decode.
func TestAHookIsCreatedWithTheShapeTheEdgeReads(t *testing.T) {
	t.Parallel()
	client, rec := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id": 1, "config": {"url": "https://x/webhooks/github"}}`))
	})
	if _, err := client.CreateRepoWebhook(context.Background(), "acme", "api",
		"https://x/webhooks/github", "s3cret"); err != nil {
		t.Fatal(err)
	}
	body := rec.postedTo(t, "/repos/acme/api/hooks")
	cfg, _ := body["config"].(map[string]any)
	if cfg["content_type"] != "json" {
		t.Errorf("content_type = %v, so every delivery arrives form-encoded", cfg["content_type"])
	}
	if cfg["insecure_ssl"] != "0" {
		t.Errorf("insecure_ssl = %v, which carries the secret to whoever answers "+
			"on that address", cfg["insecure_ssl"])
	}
	if cfg["secret"] != "s3cret" {
		t.Errorf("the hook was registered without its secret: %v", cfg)
	}

	// THE EVENT LIST IS EXACTLY WHAT THE PARSER READS. A wildcard hook
	// delivers every push, star and fork — thousands a day on a busy
	// repository, each verified, stored, deduped and routed to nobody.
	events, _ := body["events"].([]any)
	if len(events) != len(github.WebhookEvents) {
		t.Fatalf("subscribed events = %v, want %v", events, github.WebhookEvents)
	}
}

// # Provisioning

// A SEAT'S CREDENTIAL IS READ UNDER THE KEYS ITS OWN TOOLS USE.
//
// Inventing CREWLET_GITHUB_TOKEN_<seat> would be a variable nothing reads,
// and a provisioner writing to a key this lookup does not read would hand
// every seat a credential nothing authenticates with.
func TestASeatsCredentialIsFoundUnderEveryToolsSpelling(t *testing.T) {
	t.Parallel()
	literal := func(v string) string { return v }
	for _, key := range github.CredentialKeys {
		seat := &org.Role{Name: "Engineer", DeclaredHandle: "eng",
			MCPEnv: map[string]map[string]string{
				github.SeatEnv: {key: "ghp_token"},
			}}
		if got := github.CredentialOf(seat, literal); got != "ghp_token" {
			t.Errorf("%s: credential = %q", key, got)
		}
	}
	// A SCHEME IS STRIPPED, which is what lets one config shape work
	// through both an HTTP MCP server and this lookup. GitHub's older
	// `token` scheme is the same credential wearing the word the API used
	// to want.
	for _, value := range []string{"Bearer ghp_token", "token ghp_token"} {
		seat := &org.Role{Name: "Engineer", DeclaredHandle: "eng",
			MCPEnv: map[string]map[string]string{
				github.SeatEnv: {"Authorization": value},
			}}
		if got := github.CredentialOf(seat, literal); got != "ghp_token" {
			t.Errorf("%q: credential = %q", value, got)
		}
	}
	// A HUMAN SEAT HOLDS NONE and must never be looked up as though it
	// did: it is addressable through its own contact block.
	human := &org.Role{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
		MCPEnv: map[string]map[string]string{github.SeatEnv: {"GITHUB_TOKEN": "ghp"}}}
	if got := github.CredentialOf(human, literal); got != "" {
		t.Errorf("a human seat was given a tool credential: %q", got)
	}
}

// EVERY SEAT IS RESOLVED TO THE ACCOUNT ITS OWN CREDENTIAL HOLDS.
//
// That mapping is the whole of a seat's inbound routing and nothing in the
// org chart declares it, so the two findings this walk exists to surface are
// the seats it could NOT resolve: one with no credential receives no GitHub
// events at all, and one whose credential is dead receives none either. Both
// are reported rather than raised — failing the run over one seat would hide
// the state of every other.
//
// The walk is CONCURRENT, which is part of the contract rather than a
// tuning choice: it is one round trip per seat on a command an operator sits
// and waits for. It also means several requests land on the stub at once,
// which is the shape that produced this package's last -race failure — so
// this test is where the recording stub has to hold under real parallelism.
func TestEachSeatIsResolvedToTheAccountItsOwnCredentialHolds(t *testing.T) {
	t.Parallel()
	rec := newStub()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := rec.record(r); err != nil {
			// Errorf, NOT Fatal: a server goroutine is not the test's.
			t.Errorf("reading the %s %s body: %v", r.Method, r.URL.Path, err)
			return
		}
		// EACH CREDENTIAL AUTHENTICATES AS ITS OWN ACCOUNT, which is what
		// a stub answering one login for every token could not tell apart
		// from a walk that resolved one seat and copied it to the rest.
		switch token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); token {
		case "ghp_revoked":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message": "Bad credentials"}`))
		default:
			_, _ = w.Write([]byte(`{"login": "` + strings.TrimPrefix(token, "ghp_") + `"}`))
		}
	}))
	t.Cleanup(server.Close)

	client, err := github.NewClient(github.ClientOptions{
		APIBase: server.URL, WebBase: server.URL, Token: "ghp_engine",
	})
	if err != nil {
		t.Fatal(err)
	}

	seat := func(name, handle, token string) *org.Role {
		role := &org.Role{Name: name, DeclaredHandle: handle}
		if token != "" {
			role.MCPEnv = map[string]map[string]string{
				github.SeatEnv: {"GITHUB_TOKEN": token},
			}
		}
		return role
	}
	founder := seat("Founder", "founder", "ghp_founder")
	founder.Kind = org.KindHuman

	res, err := github.Reconcile(context.Background(), github.Options{
		Client: client,
		Config: &config.GitHub{Enabled: true},
		Org: &org.Organization{
			Name:  "Acme",
			Roles: []*org.Role{seat("Engineer", "eng", "${ENG_TOKEN}"), founder},
			Units: []*org.Unit{{
				Name: "Platform",
				Roles: []*org.Role{
					seat("Designer", "design", ""),
					seat("Analyst", "analyst", "ghp_revoked"),
					// A SECOND SEAT THAT RESOLVES, so a walk that
					// wrote every answer into one slot would be
					// caught: with one resolving seat, "everyone got
					// seat zero's login" and "everyone got their own"
					// are the same result.
					seat("Reviewer", "review", "ghp_review-bot"),
				},
			}},
		},
		Value: func(v string) string {
			if v == "${ENG_TOKEN}" {
				return "ghp_eng-bot"
			}
			return v
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	byHandle := map[string]github.SeatIdentity{}
	for _, identity := range res.Seats {
		byHandle[identity.Handle] = identity
	}
	if len(res.Seats) != 4 {
		t.Fatalf("seats = %+v, want the four agent seats", res.Seats)
	}
	// A HUMAN SEAT IS NOT WALKED AT ALL: it holds no tool credential and is
	// addressable through its own contact block, so reporting it unresolved
	// would be a finding about nothing.
	if _, walked := byHandle["founder"]; walked {
		t.Errorf("a human seat was looked up as though it held a credential: %+v", res.Seats)
	}
	// A ROOT ROLE AND A ROLE IN A UNIT ARE BOTH WALKED, because the walk is
	// the whole org chart — one that read only the root roles would report
	// a two-tier company as having no seats below the top.
	for handle, want := range map[string]string{"eng": "eng-bot", "review": "review-bot"} {
		if got := byHandle[handle].Login; got != want {
			t.Errorf("%s resolved to %q, want the account its own token holds (%q)",
				handle, got, want)
		}
	}
	if byHandle["design"].Routes() ||
		!strings.Contains(byHandle["design"].Reason, "mcp_env."+github.SeatEnv) {
		t.Errorf("a seat with no credential was reported as %+v — the reason has "+
			"to name the block an operator fixes", byHandle["design"])
	}
	if byHandle["analyst"].Routes() || byHandle["analyst"].Reason == "" {
		t.Errorf("a refused credential was reported as %+v", byHandle["analyst"])
	}
	if res.Routing() != 2 {
		t.Errorf("Routing() = %d, want the two seats whose events can reach them",
			res.Routing())
	}
	// FOUR PROBES, NOT FIVE: the org credential and the three seats that
	// carry one. The seat with no credential is answered from the config
	// alone, so a company where most seats are unconfigured does not pay a
	// round trip per seat to be told so.
	if got := rec.count("/user"); got != 4 {
		t.Errorf("/user was asked %d times, want the org credential and the three "+
			"seats that hold one", got)
	}
}

// AN ORG HOOK COVERS EVERY REPOSITORY, so the repos are not hooked again:
// two hooks on one repository deliver every event twice.
func TestOneOrgHookReplacesThePerRepositoryOnes(t *testing.T) {
	t.Parallel()
	client, rec := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id": 1, "config": {"url": "https://x/webhooks/github"}}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})

	res, err := github.Reconcile(context.Background(), github.Options{
		Client: client,
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "s3cret",
			Provisioning: &config.GitHubProvisioning{
				Org: "acme", Repos: []string{"acme/api", "acme/web"},
			},
		},
		Value:       func(v string) string { return v },
		WebhookBase: "https://x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created := rec.posted(); len(created) != 1 || created[0] != "/orgs/acme/hooks" {
		t.Fatalf("hooks were created at %v, want one organization hook", created)
	}
	if len(res.Hooks) != 1 || !res.Hooks[0].Hooked() {
		t.Fatalf("hooks = %+v", res.Hooks)
	}
}

// WITHOUT admin:org_hook, `auto` FALLS BACK rather than failing.
//
// A fine-grained token cannot carry that scope at all, so failing would make
// the most common credential the one this command refuses.
func TestAnOrgHookRefusalFallsBackToRepositories(t *testing.T) {
	t.Parallel()
	client, rec := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/orgs/") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message": "Resource not accessible"}`))
			return
		}
		switch {
		case r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id": 1, "config": {"url": "https://x/webhooks/github"}}`))
		case strings.HasSuffix(r.URL.Path, "/hooks"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{"full_name": "acme/api", "permissions": {"admin": true}}`))
		}
	})

	cfg := &config.GitHub{
		Enabled: true, WebhookSecret: "s3cret",
		Provisioning: &config.GitHubProvisioning{
			Org: "acme", Repos: []string{"acme/api"},
		},
	}
	res, err := github.Reconcile(context.Background(), github.Options{
		Client: client, Config: cfg,
		Value: func(v string) string { return v }, WebhookBase: "https://x",
	})
	if err != nil {
		t.Fatalf("auto failed instead of falling back: %v", err)
	}
	if created := rec.posted(); len(created) != 1 || created[0] != "/repos/acme/api/hooks" {
		t.Fatalf("hooks were created at %v, want the repository's", created)
	}
	// AND THE FALLBACK IS SAID OUT LOUD, because per-repository hooks do
	// not cover a repository created later — an operator who thinks they
	// have an org hook will not know why a new repository is silent.
	var told bool
	for _, note := range res.Notes {
		if strings.Contains(note, "per repository") {
			told = true
		}
	}
	if !told {
		t.Errorf("the run fell back to repository hooks silently: %v", res.Notes)
	}

	// AND `true` DOES FAIL, because it is an operator saying the org hook
	// is the arrangement — falling back silently would leave every
	// repository created later unhooked with nothing saying so.
	cfg.Provisioning.OrgWebhook = config.ContainerWebhookRequire
	if _, err := github.Reconcile(context.Background(), github.Options{
		Client: client, Config: cfg,
		Value: func(v string) string { return v }, WebhookBase: "https://x",
	}); err == nil {
		t.Fatal("org_webhook: true accepted a deployment with no org hook")
	}
}

// A REPOSITORY THAT CANNOT BE HOOKED IS REPORTED, NOT RAISED.
//
// A company's repository list will contain one that was renamed, archived or
// made private to a team this credential is not in. Failing the whole run
// over it leaves every other repository unhooked to punish one typo.
func TestOneUnhookableRepositoryDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repos/acme/gone"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message": "Not Found"}`))
		case strings.Contains(r.URL.Path, "/repos/acme/readonly"):
			_, _ = w.Write([]byte(`{"full_name": "acme/readonly", "permissions": {"admin": false}}`))
		case strings.Contains(r.URL.Path, "/repos/acme/old"):
			_, _ = w.Write([]byte(`{"full_name": "acme/old", "archived": true, "permissions": {"admin": true}}`))
		case r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id": 1, "config": {"url": "https://x/webhooks/github"}}`))
		case strings.HasSuffix(r.URL.Path, "/hooks"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{"full_name": "acme/api", "permissions": {"admin": true}}`))
		}
	})

	res, err := github.Reconcile(context.Background(), github.Options{
		Client: client,
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "s3cret",
			Provisioning: &config.GitHubProvisioning{
				Repos: []string{"acme/gone", "acme/readonly", "acme/old", "acme/api"},
			},
		},
		Value: func(v string) string { return v }, WebhookBase: "https://x",
	})
	if err != nil {
		t.Fatalf("one bad repository failed the whole run: %v", err)
	}
	if len(res.Hooks) != 4 {
		t.Fatalf("hooks = %+v", res.Hooks)
	}
	byTarget := map[string]github.HookState{}
	for _, hook := range res.Hooks {
		byTarget[hook.Target.String()] = hook
	}
	if !byTarget["acme/api"].Hooked() {
		t.Errorf("the working repository was not hooked: %+v", byTarget["acme/api"])
	}
	// GITHUB CONFLATES "ABSENT" AND "INVISIBLE" and says so nowhere. An
	// operator reading "not found" about a repository they are looking at
	// needs to be told the other half.
	if !strings.Contains(byTarget["acme/gone"].Detail, "visible") {
		t.Errorf("a 404 was reported as absence alone: %q", byTarget["acme/gone"].Detail)
	}
	if !strings.Contains(byTarget["acme/readonly"].Detail, "admin") {
		t.Errorf("a permission refusal did not name the permission: %q",
			byTarget["acme/readonly"].Detail)
	}
	if !strings.Contains(byTarget["acme/old"].Detail, "archived") {
		t.Errorf("an archived repository was not reported as one: %q",
			byTarget["acme/old"].Detail)
	}
}

// A SECRET THAT ALREADY RESOLVES IS REUSED.
//
// The tempting shape is to mint every run, and it is an outage: the engine is
// running with the OLD secret, so re-registering with a fresh one has GitHub
// sign every delivery with a key the running engine does not hold.
func TestAWorkingSecretIsNotReminted(t *testing.T) {
	t.Parallel()
	client, rec := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id": 1, "config": {"url": "https://x/webhooks/github"}}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})

	sink := &recordingSink{}
	if _, err := github.Reconcile(context.Background(), github.Options{
		Client: client,
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "${GH_SECRET}",
			Provisioning: &config.GitHubProvisioning{Org: "acme"},
		},
		Value: func(v string) string {
			if v == "${GH_SECRET}" {
				return "the-live-secret"
			}
			return v
		},
		Sink: sink, WebhookBase: "https://x",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := rec.postedTo(t, "/orgs/acme/hooks")["config"].(map[string]any)
	if registered, _ := cfg["secret"].(string); registered != "the-live-secret" {
		t.Errorf("the hook was registered with %q rather than the live secret", registered)
	}
	if len(sink.recorded) != 0 {
		t.Errorf("a working secret was reminted, invalidating every running "+
			"engine's copy: %v", sink.recorded)
	}
}

// AND ONE THAT RESOLVES TO NOTHING IS MINTED, into the variable the config
// already points at.
func TestAnUnsetSecretIsMintedIntoItsOwnVariable(t *testing.T) {
	t.Parallel()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id": 1, "config": {"url": "https://x/webhooks/github"}}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})

	sink := &recordingSink{}
	res, err := github.Reconcile(context.Background(), github.Options{
		Client: client,
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "${GH_SECRET}",
			Provisioning: &config.GitHubProvisioning{Org: "acme"},
		},
		// Resolves to nothing, which is the mid-setup state: the config
		// names a variable and nobody has set it.
		Value: func(string) string { return "" },
		Sink:  sink, WebhookBase: "https://x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sink.recorded["GH_SECRET"] == "" {
		t.Fatalf("nothing was minted into the variable the config names: %v", sink.recorded)
	}
	if !sink.flushed {
		t.Error("the minted secret was never flushed, so it exists at GitHub " +
			"and nowhere else")
	}
	// AND THE OPERATOR IS TOLD, because a secret they cannot find is a
	// deployment that will not verify a single delivery.
	var told bool
	for _, note := range res.Notes {
		if strings.Contains(note, "GH_SECRET") {
			told = true
		}
	}
	if !told {
		t.Errorf("the run minted a secret and never said so: %v", res.Notes)
	}
}

// A LITERAL SECRET THAT RESOLVES TO NOTHING HAS NOWHERE TO MINT INTO, and
// saying so beats registering a hook nothing can verify.
func TestALiteralSecretWithNoValueIsRefused(t *testing.T) {
	t.Parallel()
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	_, err := github.Reconcile(context.Background(), github.Options{
		Client: client,
		Config: &config.GitHub{
			Enabled: true, WebhookSecret: "wh-${GH}-suffix",
			Provisioning: &config.GitHubProvisioning{Org: "acme"},
		},
		Value: func(string) string { return "" },
		Sink:  &recordingSink{}, WebhookBase: "https://x",
	})
	if err == nil {
		t.Fatal("an unmintable secret registered a hook anyway")
	}
	if !strings.Contains(err.Error(), "webhook_secret") {
		t.Errorf("the refusal does not name the field to fix: %v", err)
	}
}

// recordingSink is a [provision.TokenSink] that remembers what it was given.
type recordingSink struct {
	recorded map[string]string
	flushed  bool
}

func (s *recordingSink) Record(_ context.Context, name, value string) error {
	if s.recorded == nil {
		s.recorded = map[string]string{}
	}
	s.recorded[name] = value
	return nil
}

func (s *recordingSink) Value(_ context.Context, name string) (string, bool, error) {
	value, held := s.recorded[name]
	return value, held, nil
}

func (s *recordingSink) Discard(context.Context) error { clear(s.recorded); return nil }
func (s *recordingSink) Flush(context.Context) error   { s.flushed = true; return nil }
func (s *recordingSink) Describe() string              { return "a test sink" }
func (s *recordingSink) NextStep() string              { return "export it" }

var _ provision.TokenSink = (*recordingSink)(nil)

// # The config model

// THE API BASE IS DERIVED, because the two forms disagree in a way an
// operator has no reason to know: github.com serves its API from a different
// HOST, and an Enterprise Server serves it from /api/v3 on the instance.
func TestTheAPIBaseIsDerivedFromHowTheDeploymentIsNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ url, api, web string }{
		{"", "https://api.github.com", "https://github.com"},
		{"https://github.example.com", "https://github.example.com/api/v3",
			"https://github.example.com"},
		{"https://github.example.com/", "https://github.example.com/api/v3",
			"https://github.example.com"},
		// A copy-paste from GitHub's own docs writes the API base. Taking
		// it at face value puts /api/v3 in the path twice.
		{"https://github.example.com/api/v3", "https://github.example.com/api/v3",
			"https://github.example.com"},
	} {
		cfg := config.GitHub{URL: tc.url}
		if got := cfg.APIBase(); got != tc.api {
			t.Errorf("APIBase(%q) = %q, want %q", tc.url, got, tc.api)
		}
		if got := cfg.WebURL(); got != tc.web {
			t.Errorf("WebURL(%q) = %q, want %q", tc.url, got, tc.web)
		}
	}
}

// THE ADDRESSED FLAG SPLITS AN ASK FROM NEWS. Leaving an assignment, a review
// request or a mention unanswered looks to the person who wrote it exactly
// like the webhook never arrived; a seat obliged to reply to every state
// change of every pull request it has ever touched is noise.
func TestOnlyAnAskAddressesTheSeat(t *testing.T) {
	t.Parallel()
	addressed := func(reason string) bool {
		return (github.Prompt{}).Addressed(notify.Inbound{
			Source:   github.Backend,
			Metadata: map[string]string{"event_type": reason},
		})
	}
	for _, reason := range []string{
		github.IssueAssigned, github.IssueMention, github.PRAssigned,
		github.PRReviewRequested, github.PRMention, github.PRChangesRequested,
		github.CommentMention,
	} {
		if !addressed(reason) {
			t.Errorf("%q does not address the seat", reason)
		}
	}
	for _, reason := range []string{
		github.CommentAdded, github.IssueClosed, github.PRMerged, github.PRClosed,
		github.PRApproved, github.PRReviewed, github.WorkflowFailed,
	} {
		if addressed(reason) {
			t.Errorf("%q addresses the seat and is news about a thread it follows", reason)
		}
	}
}
