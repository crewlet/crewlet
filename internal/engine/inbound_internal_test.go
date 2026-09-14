package engine

import (
	"context"
	"strings"
	"testing"
)

// A FIRST START THAT FAILS LEAVES NOTHING BEHIND. The refused apply is retried,
// and the retry starts the edge from nothing: a chat transport left running by
// the failed attempt would hold every seat's socket beside the one the retry
// opens, and a service left registered would make the retry reconcile a
// subscription that does not exist.
func TestAFailedFirstInboundStartIsTakenDownForTheRetry(t *testing.T) {
	t.Parallel()
	e, q := engineOn(t)
	c := companyFor(t, `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    integrations:
      mattermost:
        bot_token: tok-ceo
integrations:
  mattermost:
    enabled: true
    url: http://127.0.0.1:1
    team: eng
`)
	// A broker that refuses the subscription is what fails a start.
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	err := e.startInbound(t.Context(), c)
	if err == nil {
		t.Fatal("an inbound edge started on a broker that refuses subscriptions")
	}
	if !strings.Contains(err.Error(), "inbound edge") {
		t.Errorf("the refusal %q does not say what failed to start", err)
	}
	if e.inboundStarted() {
		t.Error("a failed start left a service registered, so the retry would " +
			"reconcile a subscription that does not exist")
	}
	if e.Mattermost() != nil {
		t.Error("a failed start left its chat transport running beside the one " +
			"the retry opens")
	}

	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.startInbound(t.Context(), c); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if !e.inboundStarted() {
		t.Error("the retry on a healthy broker started no inbound edge")
	}
}

// THE CAP ANSWERS BEFORE THE EPOCH EXISTS. A node's first company starts the
// inbound edge before the epoch that carries it is published, and a delivery
// that reaches the edge in between still asks for the per-seat cap. Read off
// the epoch alone, that question dereferenced a company that did not exist
// yet; once the epoch is current it is the one that answers, so an apply that
// moves the cap moves it on the next notification.
func TestTheNotificationCapAnswersBeforeTheFirstEpoch(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	limit := e.notificationRateLimit(companyFor(t, `
name: Acme
notification_rate_limit: 7
roles:
  - name: CEO
    handle: ceo
`))
	if got := limit(); got != 7 {
		t.Errorf("cap before the first epoch = %d, want the starting company's 7", got)
	}
	setEpoch(e, companyFor(t, `
name: Acme
notification_rate_limit: 9
roles:
  - name: CEO
    handle: ceo
`))
	if got := limit(); got != 9 {
		t.Errorf("cap once an epoch is current = %d, want the epoch's 9", got)
	}
}
