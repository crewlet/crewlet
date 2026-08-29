package gitlab_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// The repository: three agent seats with GitLab usernames, and an outsider
// who contributes here and is not in the org chart.
const (
	agentLead     = "lead-bot"
	agentSWE      = "swe-bot"
	agentQA       = "qa-bot"
	humanReviewer = "outsider"
)

func registry(t *testing.T) *notify.Registry {
	t.Helper()
	o := &org.Organization{Name: "Nimbus", Units: []*org.Unit{{
		Name: "Engineering", Lead: "Tech Lead",
		Roles: []*org.Role{
			{Name: "Tech Lead", DeclaredHandle: "lead"},
			{Name: "Engineer", DeclaredHandle: "swe"},
			{Name: "QA", DeclaredHandle: "qa"},
		},
	}}}
	o.Normalize()
	reg := notify.NewRegistry(o)
	for username, handle := range map[string]string{
		agentLead: "lead", agentSWE: "swe", agentQA: "qa",
	} {
		if err := reg.Register(gitlab.Backend, username, handle); err != nil {
			t.Fatalf("register %s: %v", username, err)
		}
	}
	return reg
}

// participants is the thread fan-out seam.
type participants struct {
	people []string
	err    error
	calls  int
}

func (p *participants) Of(context.Context, int, string, int) ([]string, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return p.people, nil
}

func hook(kind string, body map[string]any) types.RawWebhook {
	full := map[string]any{
		"object_kind": kind,
		"user":        map[string]any{"username": "human-dev"},
		"project": map[string]any{
			"id": 7, "path_with_namespace": "nimbus/api",
		},
	}
	for k, v := range body {
		full[k] = v
	}
	return types.RawWebhook{Body: full}
}

func attrs(action string, extra map[string]any) map[string]any {
	a := map[string]any{
		"action": action, "iid": 42, "title": "Add the rate limiter",
		"description": "closes the gap", "url": "https://gitlab.example.com/nimbus/api/-/merge_requests/42",
	}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

func users(names ...string) []any {
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"username": n})
	}
	return out
}

func parse(t *testing.T, p *gitlab.Parser, w types.RawWebhook, reg *notify.Registry) []notify.Routed {
	t.Helper()
	got, err := p.Parse(t.Context(), w, reg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return got
}

// targets renders the routing outcome as "username:reason", which is the
// whole answer a routing test is asking for.
func targets(routed []notify.Routed) []string {
	out := make([]string, 0, len(routed))
	for _, r := range routed {
		out = append(out, fmt.Sprintf("%s:%s",
			r.To.ExternalIDs[0], r.Metadata["event_type"]))
	}
	return out
}

func parser(p gitlab.Participants) *gitlab.Parser {
	return gitlab.NewParser(gitlab.ParserOptions{Participants: p})
}

// ── The directed layer: no reads, and it must never depend on one ──

func TestAReviewRequestReachesTheNewReviewer(t *testing.T) {
	t.Parallel()
	routed := parse(t, parser(nil), hook("merge_request", map[string]any{
		"object_attributes": attrs("update", nil),
		"changes": map[string]any{"reviewers": map[string]any{
			"previous": users(agentQA), "current": users(agentQA, agentSWE),
		}},
	}), registry(t))

	if got := targets(routed); !slices.Equal(got, []string{agentSWE + ":" + gitlab.MRReview}) {
		t.Fatalf("targets = %v, want only the ADDED reviewer", got)
	}
}

// THE DIFF, NOT THE LIST. An update hook carries the whole assignee list
// every time any field moves, so routing to the list pings every assignee
// each time somebody touches a label.
func TestAnUnchangedListNotifiesNobody(t *testing.T) {
	t.Parallel()
	routed := parse(t, parser(nil), hook("merge_request", map[string]any{
		"object_attributes": attrs("update", nil),
		"changes": map[string]any{"assignees": map[string]any{
			"previous": users(agentSWE), "current": users(agentSWE),
		}},
	}), registry(t))
	if len(routed) != 0 {
		t.Fatalf("targets = %v, want none", targets(routed))
	}
}

// The same, on an issue: the two kinds read their diffs independently, so a
// fixture on one proves nothing about the other.
func TestAnUnchangedIssueAssigneeListNotifiesNobody(t *testing.T) {
	t.Parallel()
	routed := parse(t, parser(nil), hook("issue", map[string]any{
		"object_attributes": attrs("update", nil),
		"changes": map[string]any{"assignees": map[string]any{
			"previous": users(agentSWE, agentQA),
			"current":  users(agentSWE, agentQA),
		}},
	}), registry(t))
	if len(routed) != 0 {
		t.Fatalf("targets = %v, want none — nobody was added", targets(routed))
	}
}

// Being REMOVED is not a task.
func TestARemovedAssigneeIsNotNotified(t *testing.T) {
	t.Parallel()
	routed := parse(t, parser(nil), hook("issue", map[string]any{
		"object_attributes": attrs("update", nil),
		"changes": map[string]any{"assignees": map[string]any{
			"previous": users(agentSWE, agentQA), "current": users(agentQA),
		}},
	}), registry(t))
	if got := targets(routed); slices.Contains(got, agentSWE+":"+gitlab.IssueAssigned) {
		t.Fatalf("targets = %v, want no copy for the removed assignee", got)
	}
}

func TestANewIssueReachesItsAssignees(t *testing.T) {
	t.Parallel()
	routed := parse(t, parser(nil), hook("issue", map[string]any{
		"object_attributes": attrs("open", map[string]any{
			"description": "cc @" + agentQA}),
		"assignees": users(agentSWE),
	}), registry(t))

	want := []string{agentSWE + ":" + gitlab.IssueAssigned, agentQA + ":" + gitlab.IssueMention}
	if got := targets(routed); !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
}

// GITLAB'S OWN SEMANTICS: re-saving a description does not re-notify the
// people it already named. Without the diff, every typo fix pings everybody
// the description mentions.
func TestOnlyDescriptionMentionsTheEditAddedAreFresh(t *testing.T) {
	t.Parallel()
	routed := parse(t, parser(nil), hook("merge_request", map[string]any{
		"object_attributes": attrs("update", nil),
		"changes": map[string]any{"description": map[string]any{
			"previous": "cc @" + agentQA,
			"current":  "cc @" + agentQA + " and @" + agentSWE,
		}},
	}), registry(t))

	if got := targets(routed); !slices.Equal(got, []string{agentSWE + ":" + gitlab.MRMention}) {
		t.Fatalf("targets = %v, want only the newly named person", got)
	}
}

// THE FIRST REASON PER PERSON WINS, and the list is in priority order: a
// mentioned assignee is woken once, as a mention, which is the stronger
// claim on their attention and the one the prompt renders differently.
func TestAMentionedAssigneeIsWokenOnceAsAMention(t *testing.T) {
	t.Parallel()
	routed := parse(t, parser(&participants{people: []string{agentSWE}}),
		hook("note", map[string]any{
			"object_attributes": map[string]any{
				"note":          "@" + agentSWE + " can you take this",
				"noteable_type": "Issue",
			},
			"issue": map[string]any{"iid": 42, "title": "Fix it",
				"assignees": users(agentSWE)},
		}), registry(t))

	if got := targets(routed); !slices.Equal(got, []string{agentSWE + ":" + gitlab.NoteMention}) {
		t.Fatalf("targets = %v, want one copy as a mention", got)
	}
}

// ── The watching layer: additive, and never load-bearing ──

func TestACommentReachesTheThreadsParticipants(t *testing.T) {
	t.Parallel()
	people := &participants{people: []string{agentLead, agentQA}}
	routed := parse(t, parser(people), hook("note", map[string]any{
		"object_attributes": map[string]any{
			"note": "looks good to me", "noteable_type": "MergeRequest",
		},
		"merge_request": map[string]any{"iid": 42, "title": "Add the rate limiter",
			"assignees": users(agentSWE)},
	}), registry(t))

	want := []string{
		agentSWE + ":" + gitlab.NoteComment,
		agentLead + ":" + gitlab.NoteComment,
		agentQA + ":" + gitlab.NoteComment,
	}
	if got := targets(routed); !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	if people.calls != 1 {
		t.Fatalf("the fan-out spent %d lookups, want exactly one", people.calls)
	}
}

// A FAILED LOOKUP COSTS REACH, NEVER CORRECTNESS. The directed targets are
// already in the payload, so a company whose read credential lapsed keeps
// routing everything that names somebody.
func TestAFailedParticipantLookupStillReachesTheNamedParties(t *testing.T) {
	t.Parallel()
	routed := parse(t, parser(&participants{err: errors.New("401")}),
		hook("note", map[string]any{
			"object_attributes": map[string]any{
				"note": "ping @" + agentQA, "noteable_type": "Issue",
			},
			"issue": map[string]any{"iid": 42, "assignees": users(agentSWE)},
		}), registry(t))

	want := []string{agentQA + ":" + gitlab.NoteMention, agentSWE + ":" + gitlab.NoteComment}
	if got := targets(routed); !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want the payload-named parties", got)
	}
}

// A FRESH ISSUE SPENDS NO LOOKUP. Its participants are its author (the
// actor), its assignees and its description mentions — all already targeted,
// so a request here would buy what the payload just said.
func TestOpeningAnIssueSpendsNoParticipantLookup(t *testing.T) {
	t.Parallel()
	people := &participants{people: []string{agentLead}}
	parse(t, parser(people), hook("issue", map[string]any{
		"object_attributes": attrs("open", nil), "assignees": users(agentSWE),
	}), registry(t))
	if people.calls != 0 {
		t.Fatalf("opening an issue spent %d lookups, want none", people.calls)
	}
}

// Closing an issue an agent is working on has to reach that agent — the
// message is stop — so it is the one issue event that earns its request.
func TestClosingAnIssueWakesTheThread(t *testing.T) {
	t.Parallel()
	people := &participants{people: []string{agentLead}}
	routed := parse(t, parser(people), hook("issue", map[string]any{
		"object_attributes": attrs("close", nil), "assignees": users(agentSWE),
	}), registry(t))

	want := []string{agentSWE + ":" + gitlab.IssueClosed, agentLead + ":" + gitlab.IssueClosed}
	if got := targets(routed); !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
}

// ── Gating and the actor ──

// THE GATE ON EVERY FAN-OUT. A repository has contributors who are not seats
// here; a comment naming three of them would otherwise produce three
// undeliverable notifications, by the dozen on a busy project.
func TestOutsidersAreNeverTargeted(t *testing.T) {
	t.Parallel()
	routed := parse(t, parser(&participants{people: []string{humanReviewer}}),
		hook("note", map[string]any{
			"object_attributes": map[string]any{
				"note":          "@" + humanReviewer + " and @nobody-at-all",
				"noteable_type": "Issue",
			},
			"issue": map[string]any{"iid": 42, "assignees": users(humanReviewer)},
		}), registry(t))
	if len(routed) != 0 {
		t.Fatalf("targets = %v, want none — nobody named is a seat here", targets(routed))
	}
}

// THE GATE IS ONE PLACE, and it holds for both layers: an outsider reached
// through a mention and an outsider reached through the participants lookup
// are dropped by the same check, so neither layer can quietly stop gating.
func TestBothLayersMeetTheSameGate(t *testing.T) {
	t.Parallel()
	routed := parse(t, parser(&participants{people: []string{humanReviewer, agentQA}}),
		hook("note", map[string]any{
			"object_attributes": map[string]any{
				"note":          "@" + humanReviewer + " @" + agentSWE,
				"noteable_type": "Issue",
			},
			"issue": map[string]any{"iid": 42, "assignees": users(humanReviewer)},
		}), registry(t))

	want := []string{agentSWE + ":" + gitlab.NoteMention, agentQA + ":" + gitlab.NoteComment}
	if got := targets(routed); !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
}

// THE ACTOR IS NOT FILTERED BY THE PARSER. It is stamped under the one key
// every vendor writes and suppressed by the spine, which knows the
// pipeline.failed exception and can resolve an actor across identity
// namespaces — neither of which a username comparison here could do.
func TestTheActorIsStampedRatherThanFiltered(t *testing.T) {
	t.Parallel()
	body := hook("issue", map[string]any{
		"object_attributes": attrs("open", nil), "assignees": users(agentSWE),
	})
	body.Body["user"] = map[string]any{"username": "SWE-Bot"}

	routed := parse(t, parser(nil), body, registry(t))
	if len(routed) != 1 {
		t.Fatalf("targets = %v, want the assignee's copy", targets(routed))
	}
	// LOWERCASED, because GitLab preserves the case a username was
	// created with and echoes whatever case a mention was typed in — so
	// the spine's comparison sees one person rather than two.
	if got := routed[0].Metadata[notify.ActorField]; got != agentSWE {
		t.Fatalf("the actor stamp reads %q, want %q", got, agentSWE)
	}
}

// ── Pipelines ──

func TestOnlyAFailedPipelineIsRouted(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"success", "running", "pending", "canceled"} {
		body := hook("pipeline", map[string]any{
			"object_attributes": map[string]any{"status": status},
		})
		body.Body["user"] = map[string]any{"username": agentSWE}
		if routed := parse(t, parser(nil), body, registry(t)); len(routed) != 0 {
			t.Fatalf("a %s pipeline routed %v", status, targets(routed))
		}
	}
}

func TestAFailedPipelineReachesWhoeverTriggeredIt(t *testing.T) {
	t.Parallel()
	body := hook("pipeline", map[string]any{
		"object_attributes": map[string]any{"status": "failed"},
		"merge_request":     map[string]any{"iid": 42},
	})
	body.Body["user"] = map[string]any{"username": agentSWE}

	routed := parse(t, parser(nil), body, registry(t))
	if got := targets(routed); !slices.Equal(got, []string{agentSWE + ":" + gitlab.PipelineFailed}) {
		t.Fatalf("targets = %v", got)
	}
	if got := routed[0].Metadata["mr_iid"]; got != "42" {
		t.Fatalf("the merge request reference reads %q", got)
	}
}

// ── Everything else ──

// Push, tag, wiki, deployment, release, emoji: none names a party. An emoji
// award is the near miss — it carries the awarder and an awardable with no
// username, so there is nobody to tell even though something happened.
func TestNonRoutableHooksReachNobody(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"push", "tag_push", "wiki_page",
		"deployment", "release", "emoji", "build"} {
		routed := parse(t, parser(&participants{people: []string{agentSWE}}),
			hook(kind, map[string]any{
				"object_attributes": attrs("open", nil),
				"assignees":         users(agentSWE),
			}), registry(t))
		if len(routed) != 0 {
			t.Fatalf("a %s hook routed %v", kind, targets(routed))
		}
	}
}

// An action a later release adds reaches nobody rather than everybody: a
// draft toggle spelled differently must not fan out as a state change.
func TestAnUnknownMergeRequestActionReachesNobody(t *testing.T) {
	t.Parallel()
	people := &participants{people: []string{agentLead}}
	routed := parse(t, parser(people), hook("merge_request", map[string]any{
		"object_attributes": attrs("auto_merge_enabled", nil),
		"assignees":         users(agentSWE),
	}), registry(t))
	if len(routed) != 0 || people.calls != 0 {
		t.Fatalf("targets = %v, lookups = %d, want none of either",
			targets(routed), people.calls)
	}
}

// THE RAW BYTES WIN over the decoded map, because they are the ones the
// signature was checked against and the only faithful copy: a payload that
// made a round trip through a map has had every number turned into a float.
func TestTheRawBytesAreParsedWhenPresent(t *testing.T) {
	t.Parallel()
	w := hook("issue", map[string]any{
		"object_attributes": attrs("open", nil), "assignees": users(agentSWE),
	})
	w.BodyRaw = []byte(`{"object_kind":"issue","user":{"username":"human-dev"},
		"project":{"id":7,"path_with_namespace":"nimbus/api"},
		"object_attributes":{"action":"open","iid":99,"title":"From the wire"},
		"assignees":[{"username":"` + agentQA + `"}]}`)

	routed := parse(t, parser(nil), w, registry(t))
	if got := targets(routed); !slices.Equal(got, []string{agentQA + ":" + gitlab.IssueAssigned}) {
		t.Fatalf("targets = %v, want the wire's assignee", got)
	}
	if got := routed[0].Metadata["issue_iid"]; got != "99" {
		t.Fatalf("iid = %q, want the wire's 99", got)
	}
}

// The project id appears in two places and older releases carried it in only
// one. A lookup keyed on the wrong one silently returns nobody.
func TestTheProjectIdIsFoundInEitherPlace(t *testing.T) {
	t.Parallel()
	people := &participants{people: []string{agentLead}}
	w := hook("note", map[string]any{
		"object_attributes": map[string]any{"note": "hi", "noteable_type": "Issue"},
		"issue":             map[string]any{"iid": 42},
		"project_id":        7,
	})
	delete(w.Body, "project")

	routed := parse(t, parser(people), w, registry(t))
	if people.calls != 1 {
		t.Fatalf("the fan-out spent %d lookups", people.calls)
	}
	if got := targets(routed); !slices.Equal(got, []string{agentLead + ":" + gitlab.NoteComment}) {
		t.Fatalf("targets = %v", got)
	}
}
