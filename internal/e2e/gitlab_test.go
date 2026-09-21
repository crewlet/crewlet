package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Gate G7's code-host half. The two things that only show up end to end are
// SEAT IDENTITY — a webhook names a username and nothing in the org model
// says which account a seat holds, so without the registration the whole
// integration is silently inert — and the PIPELINE EXCEPTION, which has to
// survive the spine's self-action guard to mean anything.

// forge is a fake GitLab answering GET /user, which is the whole of what
// the engine reads a seat's credential for. It counts the identity lookups
// so the token-keyed cache can be observed rather than asserted about.
type forge struct {
	url     string
	mu      sync.Mutex
	byToken map[string]string
	lookups int
}

func fakeGitLab(t *testing.T, accounts map[string]string) *forge {
	t.Helper()
	f := &forge{byToken: accounts}
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v4/user" {
				http.NotFound(w, r)
				return
			}
			f.mu.Lock()
			f.lookups++
			username := f.byToken[r.Header.Get("PRIVATE-TOKEN")]
			f.mu.Unlock()
			if username == "" {
				http.Error(w, `{"message":"401"}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprintf(w, `{"username":%q,"id":9}`, username)
		}))
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

func (f *forge) identityLookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

// testSigningSecret is a well-formed Standard-Webhooks key: whsec_ over 32
// bytes, the only shape GitLab signs with and the only shape the engine
// starts on. A placeholder rather than a credential — these tests publish
// onto the inbound queue directly, so nothing here ever computes an HMAC.
const testSigningSecret = "whsec_ZTJlLXRlc3Qtc2lnbmluZy1rZXktMzItYnl0ZXMhISE="

// gitlabCompany enables the code host with no seat holding a credential.
func gitlabCompany(url string) func(string) string {
	return func(doc string) string {
		return strings.Replace(doc, "roles:\n", `integrations:
  gitlab:
    enabled: true
    url: `+url+`
    signing_secret: `+testSigningSecret+`
roles:
`, 1)
	}
}

// withSeatCredential gives the CEO seat a code-host token. The engine
// resolves the ACCOUNT behind it rather than reading a declared username: a
// declaration that disagrees with the credential is a misroute nothing can
// detect.
func withSeatCredential(url string) func(string) string {
	return func(doc string) string {
		return strings.Replace(gitlabCompany(url)(doc),
			"    handle: ceo\n    llm: scripted\n",
			"    handle: ceo\n    llm: scripted\n    mcp_env:\n      gitlab:\n"+
				"        GITLAB_TOKEN: glpat-ceo\n", 1)
	}
}

func gitlabWebhook(t *testing.T, n *node, body map[string]any) {
	t.Helper()
	ev := events.New(types.RawWebhook{Body: body, Headers: map[string]string{}},
		events.NewTrace())
	ev.Source = gitlab.Backend
	if err := n.engine.Backends().Queue.Publish(t.Context(),
		topics.NotificationsInbound, ev); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func issueOpened(actor string, assignees ...string) map[string]any {
	users := make([]any, 0, len(assignees))
	for _, a := range assignees {
		users = append(users, map[string]any{"username": a})
	}
	return map[string]any{
		"object_kind": "issue",
		"user":        map[string]any{"username": actor},
		"project": map[string]any{
			"id": 7, "path_with_namespace": "nimbus/api"},
		"object_attributes": map[string]any{
			"action": "open", "iid": 42, "title": "Fix the login redirect",
			"description": "the redirect loops on staging",
			"url":         "https://gitlab.example.com/nimbus/api/-/issues/42",
		},
		"assignees": users,
	}
}

// THE SEAT IDENTITY IS THE WHOLE INTEGRATION. A webhook names people by
// username, and the org model does not say which account a seat holds — so
// the engine reads it from the same mcp_env block the seat's own tools use.
func TestACodeHostAssignmentWakesTheSeatThatOwnsTheAccount(t *testing.T) {
	n := startWith(t, withSeatCredential(fakeGitLab(t, map[string]string{"glpat-ceo": "ceo-bot"}).url))
	box := watchInbox(t, n, "ceo")

	gitlabWebhook(t, n, issueOpened("human-dev", "ceo-bot"))

	got := box.settled(t, 1)
	if len(got) != 1 {
		t.Fatalf("the assignee was woken %d times", len(got))
	}
	woken := got[0]
	seat, ok := n.engine.Registry().ByHandle("ceo")
	if !ok || woken.Agent != seat.AgentID.String() {
		t.Fatalf("the wake names agent %q, want %q", woken.Agent, seat.AgentID)
	}
	if got := woken.Metadata["event_type"]; got != gitlab.IssueAssigned {
		t.Fatalf("routed as %q", got)
	}
	if !strings.Contains(woken.Body, "been assigned an issue") {
		t.Fatalf("the trigger was not built for an assignment:\n%s", woken.Body)
	}
	// The keys are project-qualified: two repositories both have a #42.
	// BOTH OF THEM, and this is the end-to-end statement that a vendor
	// whose two keys coincide says so — GitLab's identity delegates to its
	// partition key, so the item reference is the merge unit and the
	// durable thread at once.
	if got := woken.Metadata[notify.PartitionField]; !strings.HasSuffix(got, "nimbus/api#42") {
		t.Fatalf("the partition key reads %q", got)
	}
	if got := woken.Metadata[notify.ConversationField]; got != woken.Metadata[notify.PartitionField] {
		t.Fatalf("the conversation identity reads %q and the partition key %q",
			got, woken.Metadata[notify.PartitionField])
	}
}

// WITHOUT A CREDENTIAL THERE IS NO IDENTITY, and without an identity the
// integration is inert. Worth pinning, because the failure is a company
// where every code-host event names a stranger and nothing says why.
func TestASeatWithNoCredentialIsUnreachableFromTheCodeHost(t *testing.T) {
	n := startWith(t, gitlabCompany(fakeGitLab(t, map[string]string{"glpat-ceo": "ceo-bot"}).url))
	box := watchInbox(t, n, "ceo")

	gitlabWebhook(t, n, issueOpened("human-dev", "ceo-bot"))
	box.quiet(t)
}

// A SIGNING SECRET THAT CANNOT VERIFY ANYTHING STOPS THE CODE HOST, LOUDLY.
//
// The route holds the other half of this — an unusable secret answers 503
// rather than accepting an unverifiable delivery — but by then the operator
// has a hook GitLab's own settings page reports as healthy, failing every
// delivery, with nothing naming the variable behind it. Config already
// refuses a bad LITERAL, so the only way to get here is a ${VAR} that
// resolved to nothing or to something else, which is exactly the case config
// cannot see. The company keeps running: the code host is what is
// unavailable, and saying so beats reporting it enabled and inert.
func TestACodeHostWithNoUsableSigningSecretDoesNotStart(t *testing.T) {
	forge := fakeGitLab(t, map[string]string{"glpat-ceo": "ceo-bot"})
	// Both cases are REFERENCES, because that is the only way to reach this
	// code: config refuses a literal that is not a usable key outright, so
	// what the engine has left to catch is a reference resolving to nothing
	// or to something else — precisely what config cannot see.
	const ref = "${CREWLET_TEST_GITLAB_SECRET}"
	for _, tc := range []struct{ name, resolvesTo string }{
		{"a reference nothing answers", ""},
		{"a value the vendor could not have produced", "not-a-whsec-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.resolvesTo != "" {
				t.Setenv("CREWLET_TEST_GITLAB_SECRET", tc.resolvesTo)
			}
			// THE SEAT HOLDS A CREDENTIAL, which is what makes the silence
			// mean something: this exact company with a usable signing
			// secret wakes the CEO on this exact delivery, as
			// TestACodeHostAssignmentWakesTheSeatThatOwnsTheAccount does.
			n := startWith(t, func(doc string) string {
				return strings.Replace(withSeatCredential(forge.url)(doc),
					testSigningSecret, ref, 1)
			})
			box := watchInbox(t, n, "ceo")

			// The company is up — the seat's mailbox exists and the rest of
			// the spine is running — and the code host is not.
			gitlabWebhook(t, n, issueOpened("human-dev", "ceo-bot"))
			box.quiet(t)
			if got := forge.identityLookups(); got != 0 {
				t.Errorf("the code host resolved %d identities on a config it "+
					"could not verify a delivery with", got)
			}
		})
	}
}

// THE ONE EXCEPTION TO THE SELF-ACTION RULE, through the whole spine. The
// guard drops an event whose actor is its recipient; the prompt's WakesActor
// is what lets a failed build past it, and this is the only path in the
// engine that exercises the override.
func TestAFailedPipelineReachesTheSeatThatBrokeIt(t *testing.T) {
	n := startWith(t, withSeatCredential(fakeGitLab(t, map[string]string{"glpat-ceo": "ceo-bot"}).url))
	box := watchInbox(t, n, "ceo")

	gitlabWebhook(t, n, map[string]any{
		"object_kind": "pipeline",
		"user":        map[string]any{"username": "ceo-bot"},
		"project": map[string]any{
			"id": 7, "path_with_namespace": "nimbus/api"},
		"object_attributes": map[string]any{"status": "failed"},
		"merge_request":     map[string]any{"iid": 9},
	})

	got := box.settled(t, 1)
	if len(got) != 1 {
		t.Fatalf("the seat that broke the build was woken %d times", len(got))
	}
	if body := got[0].Body; !strings.Contains(body, "has FAILED") ||
		!strings.Contains(body, "deliberately") {
		t.Fatalf("the trigger does not explain the exception:\n%s", body)
	}
}

// And the rule itself still holds for everything else: a seat's own comment
// must not come back to it, or a seat assigned to its own issue answers its
// own comment and loops.
func TestASeatIsNotWokenByItsOwnComment(t *testing.T) {
	n := startWith(t, withSeatCredential(fakeGitLab(t, map[string]string{"glpat-ceo": "ceo-bot"}).url))
	box := watchInbox(t, n, "ceo")

	gitlabWebhook(t, n, map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "ceo-bot"},
		"project": map[string]any{
			"id": 7, "path_with_namespace": "nimbus/api"},
		"object_attributes": map[string]any{
			"note": "@ceo-bot noting this for myself", "noteable_type": "Issue"},
		"issue": map[string]any{"iid": 42, "title": "Fix it",
			"assignees": []any{map[string]any{"username": "ceo-bot"}}},
	})
	box.quiet(t)
}

// A GREEN PIPELINE IS NOT NEWS. Routing every build would wake the seat that
// pushed on every commit, which is the loop the self-action rule exists to
// prevent, reintroduced through the one event allowed past it.
func TestAGreenPipelineWakesNobody(t *testing.T) {
	n := startWith(t, withSeatCredential(fakeGitLab(t, map[string]string{"glpat-ceo": "ceo-bot"}).url))
	box := watchInbox(t, n, "ceo")

	gitlabWebhook(t, n, map[string]any{
		"object_kind": "pipeline",
		"user":        map[string]any{"username": "ceo-bot"},
		"project": map[string]any{
			"id": 7, "path_with_namespace": "nimbus/api"},
		"object_attributes": map[string]any{"status": "success"},
	})
	box.quiet(t)
}

// AN APPLY MUST NOT LOSE THE IDENTITIES, and must not re-buy them either.
// They are config-derived and the registry is rebuilt whole on every apply,
// so a node that carried them across instead of rebuilding would make a seat
// added by a revision permanently unreachable — while one that re-resolved
// every seat would spend a request per seat on every reconcile.
func TestApplyingARevisionKeepsTheCodeHostIdentitiesWithoutReasking(t *testing.T) {
	instance := fakeGitLab(t, map[string]string{"glpat-ceo": "ceo-bot"})
	n := startWith(t, withSeatCredential(instance.url))
	box := watchInbox(t, n, "ceo")

	atBoot := instance.identityLookups()
	if atBoot != 1 {
		t.Fatalf("boot spent %d identity lookups for one credential", atBoot)
	}

	for range 3 {
		if _, _, err := n.engine.Apply(t.Context(), n.engine.Company().Config); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	if got := instance.identityLookups(); got != atBoot {
		t.Fatalf("three applies spent %d further lookups; identity is a "+
			"function of the credential and neither changed", got-atBoot)
	}
	if _, ok := n.engine.Registry().ByExternalID(gitlab.Backend, "ceo-bot"); !ok {
		t.Fatal("the applied revision lost the seat's code-host account")
	}

	gitlabWebhook(t, n, issueOpened("human-dev", "ceo-bot"))
	if got := box.settled(t, 1); len(got) != 1 {
		t.Fatalf("the assignee was woken %d times after the applies", len(got))
	}
}

// A ROTATED CREDENTIAL IS A CACHE MISS and costs exactly one request, which
// is right: it may well be a different account, and answering from the cache
// would keep routing that account's events to a seat that no longer holds it.
//
// # The rotation is a CHART write, and that is the half this pins
//
// A seat's `mcp_env` belongs to the SEAT, and a seat is an object on the org
// chart's own log — so rotating one is not a config activation at all. Driven
// through an apply it now changes nothing: the apply's company is composed
// with this node's chart rows, which still hold the old key, and the rotation
// is silently discarded.
//
// Which makes this a case about the convergence rather than about the cache.
// A chart write publishes a company, and everything derived from one has to
// follow it — the party registry among them, where a code-host identity is
// re-resolved from the seat's own credential. Following the apply alone, the
// engine would go on routing `ceo-bot`'s events to the seat and know nothing
// about the account it actually holds.
func TestARotatedCredentialIsReresolved(t *testing.T) {
	instance := fakeGitLab(t, map[string]string{
		"glpat-ceo": "ceo-bot", "glpat-rotated": "ceo-bot-v2"})
	n := startWith(t, withSeatCredential(instance.url))
	box := watchInbox(t, n, "ceo")
	atBoot := instance.identityLookups()

	rotateSeatToken(t, n, "ceo", "gitlab", "GITLAB_TOKEN", "glpat-rotated")

	// WAITED FOR RATHER THAN ASSERTED AT ONCE: the view publishes and the
	// convergence follows it, so the company carrying the new key is a
	// moment before the lookup it earns. The count is checked again after
	// the wait, which is what says the rotation cost ONE request rather
	// than one per pass.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, ok := n.engine.Registry().ByExternalID(gitlab.Backend, "ceo-bot-v2"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rotated account never reached the party registry")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := instance.identityLookups(); got != atBoot+1 {
		t.Fatalf("a rotation spent %d lookups, want exactly one", got-atBoot)
	}

	gitlabWebhook(t, n, issueOpened("human-dev", "ceo-bot-v2"))
	if got := box.settled(t, 1); len(got) != 1 {
		t.Fatalf("the new account was woken %d times", len(got))
	}
}

// rotateSeatToken writes one seat's tool credentials to the ORG CHART and
// waits for this node's published company to carry them.
//
// It publishes the seat's runtime document WHOLE, re-encoded from the seat the
// engine is running: a content write replaces the object, so anything left out
// would be a field the rotation silently cleared.
func rotateSeatToken(t *testing.T, n *node, handle, server, key, token string) {
	t.Helper()
	writer := n.engine.ChartWriter()
	if writer == nil {
		t.Fatal("this node runs no chart writer")
	}
	seat := n.engine.Company().Org.Role(handle)
	if seat == nil {
		t.Fatalf("no seat %q in the published company", handle)
	}
	content := *seat
	content.MCPEnv = org.MCPEnv{server: {key: token}}
	runtime, err := org.SeatRuntime(&content)
	if err != nil {
		t.Fatalf("encode %s's runtime: %v", handle, err)
	}
	if _, err := writer.WriteSeat(t.Context(), "test:rotate:"+handle+":"+token,
		chart.SeatContent{
			Handle: handle, Kind: chart.SeatAgent, Unit: seatRowUnit(t, n, handle),
			Name: seat.Name, Runtime: runtime,
		}); err != nil {
		t.Fatalf("rotate %s's credential: %v", handle, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := n.engine.Company().Org.Role(handle)
		if got != nil && got.MCPEnv[server][key] == token {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rotation never reached this node's published company")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// seatRowUnit is the unit a seat's chart ROW places it in, which is what a
// content write must state: the decide refuses a value that disagrees with
// the stored one.
func seatRowUnit(t *testing.T, n *node, handle string) string {
	t.Helper()
	rows, err := n.engine.Chart().Read(t.Context(),
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the chart: %v", err)
	}
	for _, row := range rows.Seats {
		if row.Handle == handle {
			return row.UnitKey
		}
	}
	t.Fatalf("no chart row for seat %q", handle)
	return ""
}

// AN INSTANCE THAT REFUSES A LOOKUP does not fail the boot: it may be
// briefly down, and the next apply retries. What it costs is that seat's
// inbound routing until then, which is reported per seat.
func TestAnUnresolvableCredentialDoesNotStopTheCompany(t *testing.T) {
	instance := fakeGitLab(t, map[string]string{})
	n := startWith(t, withSeatCredential(instance.url))
	box := watchInbox(t, n, "ceo")

	if n.engine.Company() == nil {
		t.Fatal("the company did not start")
	}
	gitlabWebhook(t, n, issueOpened("human-dev", "ceo-bot"))
	box.quiet(t)

	// The retry: the instance recovers and the next apply resolves it.
	instance.mu.Lock()
	instance.byToken["glpat-ceo"] = "ceo-bot"
	instance.mu.Unlock()
	if _, _, err := n.engine.Apply(t.Context(), n.engine.Company().Config); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	gitlabWebhook(t, n, issueOpened("human-dev", "ceo-bot"))
	if got := box.settled(t, 1); len(got) != 1 {
		t.Fatalf("the seat was woken %d times after the instance recovered", len(got))
	}
}
