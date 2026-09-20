package notify_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
)

// A ZERO-VALUE REGISTRY STILL ANSWERS, which is what makes [notify.Prompts.For]'s
// "never nil" promise a property of the type rather than a convention its
// constructor keeps.
//
// A service built with no prompts at all is an ordinary configuration —
// every node has one before an integration is wired, and [notify.New]
// validates the queue and the registry but has nothing to say about this —
// and its registry carried a nil fallback, so the first delivery of ANY
// source dereferenced it.
func TestAZeroPromptRegistryStillAnswers(t *testing.T) {
	var none notify.Prompts
	for _, source := range []string{"", "tracker", "a-source-nobody-wrote-a-prompt-for"} {
		p := none.For(source)
		if p == nil {
			t.Fatalf("%q resolved to no prompt", source)
		}
		// The conservative reading, which is what the fallback is for.
		if p.Addressed(notify.Inbound{Source: source}) {
			t.Errorf("%q addressed the seat without a prompt to say so", source)
		}
	}
	if got := none.Key(notify.Inbound{Source: "tracker"}); got != "" {
		t.Errorf("a source with no prompt derived the key %q", got)
	}
}

// And the whole delivery path runs on one: the seat is woken, with the
// generic body, rather than the process dying on the first webhook.
func TestAServiceWithNoPromptsDelivers(t *testing.T) {
	h := newService(t, func(o *notify.Options, _ *harness) {
		o.Prompts = notify.Prompts{}
	})
	h.parser.out = []notify.Routed{to(notify.Recipient{Handle: "engineering-lead"}, "please look")}

	if got := h.svc.Handle(t.Context(), delivery("tracker")); got.Outcome != queue.OutcomeAck {
		t.Fatalf("Handle = %+v, want an ack", got)
	}
	woken := h.inbox(t, "engineering-lead")
	if len(woken) != 1 {
		t.Fatalf("the seat was woken %d times", len(woken))
	}
	n, ok := events.DataAs[*types.ExternalNotification](woken[0])
	if !ok {
		t.Fatalf("the wake carried %T", woken[0].Data)
	}
	if !strings.Contains(n.Body, "please look") {
		t.Errorf("the generic fallback rendered:\n%s", n.Body)
	}
	if n.Addressed {
		t.Error("a source with no prompt claimed somebody is waiting")
	}
}
