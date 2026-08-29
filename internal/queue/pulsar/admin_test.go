package pulsar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// settleQuick bounds a call that must fail fast — reaching an endpoint that
// is not there. Long enough not to be flaky on a loaded box, short enough
// that a suite does not wait out a dial timeout.
const settleQuick = 2 * time.Second

// adminServer is a stand-in for the broker's admin v2 REST API.
//
// It records every request and answers from a table the test writes, so the
// assertions here are about the REQUEST — the method, the path, the body, the
// credential — and about how each STATUS is interpreted. That is the whole
// contract between this backend and the broker for subscription lifecycle,
// and it is the half that has no consumer to observe it: a wrong path or a
// mis-read 409 shows up much later as a seat whose first publish vanished.
type adminServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []recorded
	answers  map[string]answer // "METHOD /path" -> answer
}

type recorded struct {
	method string
	path   string
	query  string
	body   string
	auth   string
	ctype  string
}

type answer struct {
	status int
	body   string
}

func newAdminServer(t *testing.T) *adminServer {
	t.Helper()
	a := &adminServer{t: t, answers: map[string]answer{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.requests = append(a.requests, recorded{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			body: string(body), auth: r.Header.Get("Authorization"),
			ctype: r.Header.Get("Content-Type"),
		})
		ans, ok := a.answers[r.Method+" "+r.URL.Path]
		a.mu.Unlock()
		if !ok {
			// Unrouted is a test bug, not a broker behaviour: answering
			// something plausible would let a wrong path pass.
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("no answer registered for " + r.Method + " " + r.URL.Path))
			return
		}
		w.WriteHeader(ans.status)
		_, _ = w.Write([]byte(ans.body))
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *adminServer) answer(method, path string, status int, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answers[method+" "+path] = answer{status: status, body: body}
}

func (a *adminServer) seen() []recorded {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recorded(nil), a.requests...)
}

func (a *adminServer) last() recorded {
	seen := a.seen()
	if len(seen) == 0 {
		a.t.Fatal("no admin request was made")
	}
	return seen[len(seen)-1]
}

func (a *adminServer) admin(t *testing.T, mutate ...func(*Config)) *restAdmin {
	t.Helper()
	cfg := Config{URL: "pulsar://broker:6650", Tenant: "acme", Namespace: "prod", AdminURL: a.srv.URL}
	for _, m := range mutate {
		m(&cfg)
	}
	admin, err := newRESTAdmin(cfg)
	if err != nil {
		t.Fatalf("newRESTAdmin: %v", err)
	}
	t.Cleanup(admin.Close)
	return admin
}

const inboxPath = "/admin/v2/persistent/acme/prod/crewlet.agent.alice.inbox"

func aliceInbox() (string, string) {
	return topics.AgentInbox("alice"), topics.AgentInboxGroup("alice")
}

// TestEnsureSubscriptionAsksForEarliest is the assertion the whole
// subscription-existence invariant rests on. A subscription created at the
// latest message EXISTS and still discards everything published before its
// first consumer attached — which is the precise failure EnsureSubscription
// is for, and it was measured on the first real run of the Python harness.
func TestEnsureSubscriptionAsksForEarliest(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	topic, group := aliceInbox()
	srv.answer(http.MethodPut, inboxPath+"/subscription/"+group, http.StatusNoContent, "")

	created, err := srv.admin(t).EnsureSubscription(context.Background(), topic, group)
	if err != nil || !created {
		t.Fatalf("EnsureSubscription = (%v, %v), want (true, nil)", created, err)
	}
	got := srv.last()
	if got.method != http.MethodPut {
		t.Errorf("method = %s, want PUT", got.method)
	}
	if got.path != inboxPath+"/subscription/"+group {
		t.Errorf("path = %s, want %s", got.path, inboxPath+"/subscription/"+group)
	}
	if got.body != `"earliest"` {
		t.Errorf("body = %s, want \"earliest\"", got.body)
	}
	if got.ctype != "application/json" {
		t.Errorf("content-type = %q, want application/json", got.ctype)
	}
}

// TestSubscriptionLifecycleToleratesTheEndStateItWanted: creating one that
// exists answers 409 and deleting an absent one answers 404. Both are the
// desired end state, so each is a false rather than an error — a boot that
// re-declares every seat's inbox has to be a no-op.
func TestSubscriptionLifecycleToleratesTheEndStateItWanted(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	topic, group := aliceInbox()
	subPath := inboxPath + "/subscription/" + group
	srv.answer(http.MethodPut, subPath, http.StatusConflict, "Subscription already exists")
	srv.answer(http.MethodDelete, subPath, http.StatusNotFound, "Subscription not found")
	admin := srv.admin(t)

	created, err := admin.EnsureSubscription(context.Background(), topic, group)
	if err != nil || created {
		t.Fatalf("EnsureSubscription on an existing one = (%v, %v), want (false, nil)", created, err)
	}
	deleted, err := admin.DeleteSubscription(context.Background(), topic, group)
	if err != nil || deleted {
		t.Fatalf("DeleteSubscription of an absent one = (%v, %v), want (false, nil)", deleted, err)
	}
}

// TestAdminFailuresAreNeverSilent: a caller that read a failure as "done"
// would report the subscription-existence invariant established when it is
// not, and the symptom is a seat whose first publish vanishes with nothing to
// alert on.
func TestAdminFailuresAreNeverSilent(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	topic, group := aliceInbox()
	subPath := inboxPath + "/subscription/" + group
	// 412 is the one an operator actually meets: the broker refuses to
	// delete a subscription that still has a connected consumer.
	srv.answer(http.MethodPut, subPath, http.StatusForbidden, "Don't have permission")
	srv.answer(http.MethodDelete, subPath, http.StatusPreconditionFailed, "Subscription has active consumers")
	srv.answer(http.MethodGet, inboxPath+"/subscriptions", http.StatusInternalServerError, "boom")
	admin := srv.admin(t)

	if _, err := admin.EnsureSubscription(context.Background(), topic, group); !errors.Is(err, ErrAdmin) {
		t.Errorf("EnsureSubscription on a 403 = %v, want an ErrAdmin", err)
	} else if !strings.Contains(err.Error(), "permission") {
		t.Errorf("EnsureSubscription error %v does not quote the broker", err)
	}
	if _, err := admin.DeleteSubscription(context.Background(), topic, group); !errors.Is(err, ErrAdmin) {
		t.Errorf("DeleteSubscription on a 412 = %v, want an ErrAdmin", err)
	} else if !strings.Contains(err.Error(), "active consumers") {
		t.Errorf("DeleteSubscription error %v does not quote the broker", err)
	}
	if _, err := admin.Subscriptions(context.Background(), topic); !errors.Is(err, ErrAdmin) {
		t.Errorf("Subscriptions on a 500 = %v, want an ErrAdmin", err)
	}
}

// TestAnUnreachableAdminEndpointNamesTheConfigField, because the failure an
// operator meets is "the engine will not start" and the fix is one setting.
func TestAnUnreachableAdminEndpointNamesTheConfigField(t *testing.T) {
	t.Parallel()
	admin, err := newRESTAdmin(Config{
		URL: "pulsar://broker:6650", Tenant: "acme", Namespace: "prod",
		// A port nothing is listening on, in the reserved-for-testing
		// documentation range so a stray success is impossible.
		AdminURL: "http://192.0.2.1:1",
	})
	if err != nil {
		t.Fatalf("newRESTAdmin: %v", err)
	}
	t.Cleanup(admin.Close)
	ctx, cancel := context.WithTimeout(context.Background(), settleQuick)
	defer cancel()
	_, err = admin.EnsureSubscription(ctx, topics.AgentInbox("alice"), "agent-alice")
	if !errors.Is(err, ErrAdmin) {
		t.Fatalf("EnsureSubscription against nothing = %v, want an ErrAdmin", err)
	}
	if !strings.Contains(err.Error(), "admin_url") {
		t.Errorf("error %v does not name the setting that fixes it", err)
	}
}

// TestSubscriptionsOnAMissingTopicIsAnAnswer: a seat that has never been
// published to is the normal state of a new company, not a failure.
func TestSubscriptionsOnAMissingTopicIsAnAnswer(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	topic, _ := aliceInbox()
	srv.answer(http.MethodGet, inboxPath+"/subscriptions", http.StatusNotFound, "Topic not found")

	names, err := srv.admin(t).Subscriptions(context.Background(), topic)
	if err != nil || len(names) != 0 {
		t.Fatalf("Subscriptions on a missing topic = (%v, %v), want (nil, nil)", names, err)
	}
}

func TestSubscriptionsListsWhatTheBrokerReports(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	topic, _ := aliceInbox()
	srv.answer(http.MethodGet, inboxPath+"/subscriptions", http.StatusOK, `["agent-alice","agent-alice-control"]`)

	names, err := srv.admin(t).Subscriptions(context.Background(), topic)
	if err != nil {
		t.Fatalf("Subscriptions: %v", err)
	}
	if len(names) != 2 || names[0] != "agent-alice" || names[1] != "agent-alice-control" {
		t.Fatalf("Subscriptions = %v, want both seat subscriptions", names)
	}
}

// TestTheAdminCallCarriesTheSameCredentialAsTheBroker: the admin API is a
// second door onto the same authorization, not an unguarded one.
func TestTheAdminCallCarriesTheSameCredentialAsTheBroker(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	topic, group := aliceInbox()
	srv.answer(http.MethodPut, inboxPath+"/subscription/"+group, http.StatusNoContent, "")

	admin := srv.admin(t, func(c *Config) { c.Token = "a-jwt" })
	if _, err := admin.EnsureSubscription(context.Background(), topic, group); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	if got := srv.last().auth; got != "Bearer a-jwt" {
		t.Fatalf("Authorization = %q, want the bearer token", got)
	}
}

// TestSubjectsAreEscapedIntoTheURL. A subject is engine data; one that
// reached the path unescaped could address a different resource entirely.
func TestSubjectsAreEscapedIntoTheURL(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	// A dead-letter subject is the realistic case for an unusual name: it
	// is built from a topic AND a group, so it carries both.
	subject := topics.DeadLetter("crewlet.events.task_created", "agent-alice")
	path := "/admin/v2/persistent/acme/prod/" + subject
	srv.answer(http.MethodGet, path+"/subscriptions", http.StatusOK, `[]`)

	if _, err := srv.admin(t).Subscriptions(context.Background(), subject); err != nil {
		t.Fatalf("Subscriptions: %v", err)
	}
	if got := srv.last().path; got != path+"/subscriptions" {
		t.Fatalf("path = %s, want %s", got, path+"/subscriptions")
	}
}

// TestAdminRefusesASubjectThatCannotBeATopic — before any request goes out.
// A subject with a '/' would be read as a namespace separator and address
// another company's namespace on a shared estate.
func TestAdminRefusesASubjectThatCannotBeATopic(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	admin := srv.admin(t)
	for _, subject := range []string{"", "acme/other/topic", "crewlet.agent..inbox"} {
		if _, err := admin.EnsureSubscription(context.Background(), subject, "g"); !errors.Is(err, ErrSubject) {
			t.Errorf("EnsureSubscription(%q) = %v, want an ErrSubject", subject, err)
		}
	}
	if _, err := admin.EnsureSubscription(context.Background(), topics.AgentInbox("alice"), ""); !errors.Is(err, ErrSubject) {
		t.Errorf("EnsureSubscription with no group = %v, want an ErrSubject", err)
	}
	if got := srv.seen(); len(got) != 0 {
		t.Fatalf("a refused subject still reached the broker: %v", got)
	}
}

// TestPeekWalksTheBacklogWithoutConsumingIt. Peek is the only route that does
// not change what it inspects: a throwaway consumer would join the Shared
// subscription and take a share of that seat's live traffic, which is the
// same hazard EnsureSubscription exists to avoid.
func TestPeekWalksTheBacklogWithoutConsumingIt(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	topic, group := aliceInbox()
	subPath := inboxPath + "/subscription/" + group
	srv.answer(http.MethodGet, inboxPath+"/stats", http.StatusOK,
		`{"subscriptions":{"`+group+`":{"msgBacklog":2,"unackedMessages":0},"other":{"msgBacklog":9}}}`)
	srv.answer(http.MethodGet, subPath+"/position/1", http.StatusOK, eventJSON(t, "e0"))
	srv.answer(http.MethodGet, subPath+"/position/2", http.StatusOK, eventJSON(t, "e1"))

	payloads, err := srv.admin(t).PeekBacklog(context.Background(), topic, group)
	if err != nil {
		t.Fatalf("PeekBacklog: %v", err)
	}
	if got := labelsOf(t, payloads); strings.Join(got, ",") != "e0,e1" {
		t.Fatalf("PeekBacklog = %v, want [e0 e1]", got)
	}
	for _, r := range srv.seen() {
		if r.method != http.MethodGet {
			t.Fatalf("inspecting a mailbox issued a %s", r.method)
		}
	}
}

// TestPeekStopsAtTheEndTheBrokerWillServe. msgBacklog is the broker's own
// estimate; a position it declines is the end of what it will show, not a
// failure worth losing the rows already read over.
func TestPeekStopsAtTheEndTheBrokerWillServe(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	topic, group := aliceInbox()
	subPath := inboxPath + "/subscription/" + group
	srv.answer(http.MethodGet, inboxPath+"/stats", http.StatusOK,
		`{"subscriptions":{"`+group+`":{"msgBacklog":3,"unackedMessages":0}}}`)
	srv.answer(http.MethodGet, subPath+"/position/1", http.StatusOK, eventJSON(t, "e0"))
	srv.answer(http.MethodGet, subPath+"/position/2", http.StatusNotFound, "no such position")
	srv.answer(http.MethodGet, subPath+"/position/3", http.StatusOK, eventJSON(t, "never asked for"))

	payloads, err := srv.admin(t).PeekBacklog(context.Background(), topic, group)
	if err != nil {
		t.Fatalf("PeekBacklog: %v", err)
	}
	if got := labelsOf(t, payloads); strings.Join(got, ",") != "e0" {
		t.Fatalf("PeekBacklog = %v, want [e0]", got)
	}
}

// TestPeekOnAnAbsentSubscriptionReadsEmpty covers the two shapes of "there is
// no mailbox here": a topic nothing was ever published to, and a topic with
// other groups but not this one. Both must read as an empty backlog rather
// than an error, because "publishing to no subscription retains nothing" is
// the contract's own stated behaviour.
func TestPeekOnAnAbsentSubscriptionReadsEmpty(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	topic, group := aliceInbox()
	srv.answer(http.MethodGet, inboxPath+"/stats", http.StatusNotFound, "Topic not found")
	admin := srv.admin(t)

	got, err := admin.PeekBacklog(context.Background(), topic, group)
	if err != nil || len(got) != 0 {
		t.Fatalf("PeekBacklog on a missing topic = (%d rows, %v), want (0, nil)", len(got), err)
	}

	srv.answer(http.MethodGet, inboxPath+"/stats", http.StatusOK, `{"subscriptions":{"someone-else":{"msgBacklog":4}}}`)
	got, err = admin.PeekBacklog(context.Background(), topic, group)
	if err != nil || len(got) != 0 {
		t.Fatalf("PeekBacklog on another group's topic = (%d rows, %v), want (0, nil)", len(got), err)
	}
}

// TestBacklogIsWhatASuccessorWouldFindWaiting.
//
// "Backlog" means the mail an unowned seat has waiting. Two kinds of message
// are NOT that, and both must be excluded:
//
//   - one a consumer already holds (unackedMessages) — work in progress;
//   - one the broker is about to send it, because a connected consumer has an
//     outstanding flow permit covering it — in flight, and nobody else can
//     have it.
//
// Counting either is not a cosmetic error. It makes "the mailbox is filling
// up" and "the seat is busy" the same reading, and it makes a deferral's
// effect unobservable: the message is in msgBacklog from the instant the
// publish is acked — before any handler has seen it — so anything watching
// the backlog to learn that a deferral happened is told so immediately and
// wrongly. Measured against the conformance suite, counting them turned
// NegativePaths/a_deferral_spends_no_dead_letter_budget into a 70% flake.
//
// A Shared subscription dispatches in order, so the spoken-for messages are
// the oldest ones behind the mark-delete point — which is why the skip is a
// position offset rather than a filter.
func TestBacklogIsWhatASuccessorWouldFindWaiting(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	topic, group := aliceInbox()
	subPath := inboxPath + "/subscription/" + group
	for position, label := range map[int]string{1: "held-0", 2: "held-1", 3: "in-flight", 4: "waiting"} {
		srv.answer(http.MethodGet, fmt.Sprintf("%s/position/%d", subPath, position),
			http.StatusOK, eventJSON(t, label))
	}

	for _, tc := range []struct {
		name  string
		stats string
		want  string
	}{
		{
			// Nothing attached: everything retained is waiting.
			name:  "an unowned seat",
			stats: `{"msgBacklog":2,"unackedMessages":0,"consumers":[]}`,
			want:  "held-0,held-1",
		},
		{
			// Two held, one credited to a consumer with a spare permit,
			// one genuinely waiting behind them.
			name:  "a working seat with a queue behind it",
			stats: `{"msgBacklog":4,"unackedMessages":2,"consumers":[{"availablePermits":1}]}`,
			want:  "waiting",
		},
		{
			// The window that made the deferral case flake: published,
			// acked by the broker, not yet pushed — and covered by a
			// permit, so it is on its way to a consumer, not waiting.
			name:  "published but not yet dispatched",
			stats: `{"msgBacklog":1,"unackedMessages":0,"consumers":[{"availablePermits":2}]}`,
			want:  "",
		},
		{
			// A consumer working through a full prefetch has NO spare
			// permits, so a real backlog behind it is still reported —
			// the property that stops this exclusion hiding a busy seat.
			name:  "a saturated consumer",
			stats: `{"msgBacklog":3,"unackedMessages":2,"consumers":[{"availablePermits":0}]}`,
			want:  "in-flight",
		},
		{
			// Credit is summed across every member of the group.
			name:  "several members sharing the subscription",
			stats: `{"msgBacklog":4,"unackedMessages":0,"consumers":[{"availablePermits":1},{"availablePermits":2}]}`,
			want:  "waiting",
		},
	} {
		srv.answer(http.MethodGet, inboxPath+"/stats", http.StatusOK,
			`{"subscriptions":{"`+group+`":`+tc.stats+`}}`)
		payloads, err := srv.admin(t).PeekBacklog(context.Background(), topic, group)
		if err != nil {
			t.Errorf("%s: PeekBacklog: %v", tc.name, err)
			continue
		}
		if got := strings.Join(labelsOf(t, payloads), ","); got != tc.want {
			t.Errorf("%s: PeekBacklog = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func eventJSON(t *testing.T, label string) string {
	t.Helper()
	raw, err := json.Marshal(&events.Event{Type: label})
	if err != nil {
		t.Fatalf("marshal probe event: %v", err)
	}
	return string(raw)
}

func labelsOf(t *testing.T, payloads [][]byte) []string {
	t.Helper()
	out := make([]string, 0, len(payloads))
	for _, raw := range payloads {
		var ev events.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode peeked payload %s: %v", raw, err)
		}
		out = append(out, ev.Type)
	}
	return out
}
