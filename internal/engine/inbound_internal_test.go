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
