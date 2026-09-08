package datadog

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// alert builds a delivery from the fields a template fills.
func alert(fields map[string]any) types.RawWebhook {
	return types.RawWebhook{Body: fields}
}

func handlesOf(routed []notify.Routed) []string {
	out := make([]string, 0, len(routed))
	for _, r := range routed {
		out = append(out, r.To.Handle)
	}
	return out
}

func parse(t *testing.T, p *Parser, w types.RawWebhook) []notify.Routed {
	t.Helper()
	routed, err := p.Parse(context.Background(), w, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return routed
}

// A monitor tagged with a seat wakes that seat. This is the whole primary
// routing tier, and before this package existed every Datadog delivery was
// verified, stored, counted and then dropped with "no parser for this source".
func TestATaggedMonitorWakesTheSeatItNames(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	routed := parse(t, p, alert(map[string]any{
		"title": "[Triggered] API latency", "tags": "env:prod,crewlet:backend-lead",
		"alert_transition": "Triggered",
	}))

	if got := handlesOf(routed); !slices.Equal(got, []string{"backend-lead"}) {
		t.Fatalf("woke %v, want the tagged seat", got)
	}
	if via := routed[0].Metadata[RoutedViaField]; via != RoutedViaTag {
		t.Fatalf("routed via %q, want %q", via, RoutedViaTag)
	}
}

// A monitor watching a shared service carries the tag twice, and waking only
// the first of them is how the other team finds out from a customer.
func TestAMonitorMayNameSeveralSeats(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	routed := parse(t, p, alert(map[string]any{
		"tags": "crewlet:backend-lead,service:checkout,crewlet:payments-lead",
	}))

	want := []string{"backend-lead", "payments-lead"}
	if got := handlesOf(routed); !slices.Equal(got, want) {
		t.Fatalf("woke %v, want %v", got, want)
	}
}

// One seat tagged twice is woken once. A monitor carrying a duplicate tag is
// ordinary, and two copies of one alert is two turns doing the same work.
func TestADuplicateTagWakesOneSeatOnce(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	routed := parse(t, p, alert(map[string]any{
		"tags": "crewlet:backend-lead,crewlet:backend-lead",
	}))
	if got := handlesOf(routed); !slices.Equal(got, []string{"backend-lead"}) {
		t.Fatalf("woke %v, want one copy", got)
	}
}

// THE FLOOR. An alert whose monitor names nobody reaches the company's
// fallback, because an alerting integration whose alerts reach nobody is
// worse than one that is switched off: it looks exactly like coverage.
func TestAnUntaggedMonitorReachesTheFallback(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	routed := parse(t, p, alert(map[string]any{
		"title": "[Triggered] Disk", "tags": "env:prod,service:api",
	}))

	if got := handlesOf(routed); !slices.Equal(got, []string{"sre-lead"}) {
		t.Fatalf("woke %v, want the fallback seat", got)
	}
	if via := routed[0].Metadata[RoutedViaField]; via != RoutedViaFallback {
		t.Fatalf("routed via %q, want %q", via, RoutedViaFallback)
	}
}

// A monitor with no tags at all is the same case as one whose tags name
// nobody. A template that dropped $TAGS must not silently stop routing.
func TestAMonitorWithNoTagsReachesTheFallback(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	routed := parse(t, p, alert(map[string]any{"title": "[Triggered] Disk"}))
	if got := handlesOf(routed); !slices.Equal(got, []string{"sre-lead"}) {
		t.Fatalf("woke %v, want the fallback seat", got)
	}
}

// A RECOVERY reaches the same seat as the alert it recovers from. The seat
// woken to investigate is the one that has to be told to stand down, and a
// company whose recoveries were dropped would have agents chasing incidents
// that ended hours ago.
func TestARecoveryWakesTheSameSeat(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	routed := parse(t, p, alert(map[string]any{
		"tags": "crewlet:backend-lead", "alert_transition": "Recovered",
	}))
	if got := handlesOf(routed); !slices.Equal(got, []string{"backend-lead"}) {
		t.Fatalf("woke %v, want the tagged seat", got)
	}
}

// The tag key is configurable, because it becomes a tag on the operator's own
// monitors beside their existing conventions.
func TestTheHandleTagKeyIsConfigurable(t *testing.T) {
	p := NewParser(ParserOptions{HandleTag: "owner", Fallback: "sre-lead"})
	routed := parse(t, p, alert(map[string]any{"tags": "owner:backend-lead,crewlet:ignored"}))
	if got := handlesOf(routed); !slices.Equal(got, []string{"backend-lead"}) {
		t.Fatalf("woke %v, want the seat named by the configured key", got)
	}
}

// Datadog lowercases tag keys and values on ingestion, so a monitor tagged
// Crewlet:CEO in the UI arrives lowercased. A comparison that respected case
// would match neither what the operator typed nor what the seat is called.
func TestTagMatchingIgnoresCase(t *testing.T) {
	p := NewParser(ParserOptions{HandleTag: "Crewlet", Fallback: "sre-lead"})
	routed := parse(t, p, alert(map[string]any{"tags": "CREWLET:Backend-Lead"}))
	if got := handlesOf(routed); !slices.Equal(got, []string{"backend-lead"}) {
		t.Fatalf("woke %v, want the tagged seat", got)
	}
}

// A company with no fallback is only reachable through a config that skipped
// validation, and it wakes nobody rather than inventing a recipient.
func TestNoTagAndNoFallbackWakesNobody(t *testing.T) {
	p := NewParser(ParserOptions{})
	if routed := parse(t, p, alert(map[string]any{"title": "x"})); len(routed) != 0 {
		t.Fatalf("woke %v with no tag and no fallback", handlesOf(routed))
	}
}

// A template written with $PRIORITY unquoted arrives as a JSON number. Both
// are things an operator writes, and refusing one would drop a firing monitor
// over a pair of quotes.
func TestPriorityIsReadQuotedOrNot(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"quoted":   {"priority": "2"},
		"unquoted": {"priority": float64(2)},
		"prefixed": {"priority": "P2"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := decode(alert(body)).Priority; got != "2" {
				t.Fatalf("priority is %q, want %q", got, "2")
			}
		})
	}
}

// An unknown transition counts as TRIGGERED. Datadog has added transition
// names before, and the two ways of being wrong are not symmetric: reading a
// new alert state as a recovery tells a seat to stand down from a page nobody
// has looked at.
func TestUnknownTransitionsCountAsTriggered(t *testing.T) {
	for _, transition := range []string{"Triggered", "Re-Triggered", "Warn", "No Data", "", "Escalated"} {
		if !(Alert{Transition: transition}).Triggered() {
			t.Errorf("%q was read as a recovery", transition)
		}
	}
	for _, transition := range []string{"Recovered", "recovered", " OK "} {
		if (Alert{Transition: transition}).Triggered() {
			t.Errorf("%q was read as a trigger", transition)
		}
	}
}

// THE DUPLICATED RULE. The prompt cannot call Alert.Triggered: it is handed a
// notify.Inbound whose metadata the parser wrote, because a notification is
// re-rendered from the event store long after the delivery is gone. So the
// rule is stated twice, and this is what holds the two equal.
func TestTheRecoveryRuleIsStatedTheSameWayTwice(t *testing.T) {
	for _, transition := range []string{
		"Triggered", "Re-Triggered", "Recovered", "recovered", "OK", "ok",
		"Warn", "No Data", "", "Escalated", " Recovered ",
	} {
		fromAlert := !(Alert{Transition: transition}).Triggered()
		fromPrompt := isRecovery(transition)
		if fromAlert != fromPrompt {
			t.Errorf("%q: Alert.Triggered says recovery=%v, isRecovery says %v",
				transition, fromAlert, fromPrompt)
		}
	}
}

// The event type sits beside the other third-party apps' in the event store rather
// than beside nothing.
func TestEventTypeIsNormalised(t *testing.T) {
	cases := map[string]string{
		"Triggered": "monitor.triggered",
		"No Data":   "monitor.no_data",
		"":          "monitor",
	}
	for transition, want := range cases {
		if got := eventType(Alert{Transition: transition}); got != want {
			t.Errorf("%q became %q, want %q", transition, got, want)
		}
	}
}

// A template somebody edited can leave the title out, and a subject naming
// the host is far better than an empty one in an operator's feed.
func TestSubjectFallsBackToTheScope(t *testing.T) {
	if got := subject(Alert{Scope: "host:web-3"}); !strings.Contains(got, "host:web-3") {
		t.Fatalf("subject is %q, want it to name the scope", got)
	}
	if got := subject(Alert{}); got == "" {
		t.Fatal("an alert with no title and no scope produced an empty subject")
	}
}

// The parser stamps NO actor. Every other surface stamps who caused the event
// so the spine can suppress waking them for their own action. A monitor has
// no author, and stamping the nearest thing would suppress the alert for
// exactly the person who owns the monitor.
func TestNoActorIsStamped(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	routed := parse(t, p, alert(map[string]any{"tags": "crewlet:ceo"}))
	if actor, stamped := routed[0].Metadata[notify.ActorField]; stamped {
		t.Fatalf("an actor %q was stamped on a monitor alert", actor)
	}
}

// A monitor's trigger and its recovery are ONE conversation, so a seat sees
// that this is the fourth time tonight rather than four unrelated pages.
func TestAMonitorIsTheConversation(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	fired := parse(t, p, alert(map[string]any{
		"title": "[Triggered] API latency", "tags": "crewlet:ceo",
		"alert_transition": "Triggered",
	}))[0]
	recovered := parse(t, p, alert(map[string]any{
		"title": "[Triggered] API latency", "tags": "crewlet:ceo",
		"alert_transition": "Recovered",
	}))[0]

	var prompt Prompt
	firedKey := prompt.ConversationKey(fired.Metadata, fired.Subject)
	recoveredKey := prompt.ConversationKey(recovered.Metadata, recovered.Subject)
	if firedKey == "" {
		t.Fatal("a monitor alert derived no conversation key, so it merges with nothing")
	}
	if firedKey != recoveredKey {
		t.Fatalf("the trigger keys on %q and the recovery on %q", firedKey, recoveredKey)
	}
}

// ONLY AN OWNER IS ADDRESSED. A tag naming the seat is the company saying the
// monitor is its, and that turn may not end in silence; the fallback is the
// alert landing somewhere rather than on somebody, and a seat obliged to
// answer every untagged monitor would post on each one whether or not it had
// anything to say.
func TestOnlyATaggedOwnerIsAddressed(t *testing.T) {
	addressed := func(via string) bool {
		return (Prompt{}).Addressed(notify.Inbound{
			Source:   Backend,
			Metadata: map[string]string{RoutedViaField: via},
		})
	}
	if !addressed(RoutedViaTag) {
		t.Error("a monitor tagged as the seat's does not address it")
	}
	for _, via := range []string{RoutedViaFallback, ""} {
		if addressed(via) {
			t.Errorf("%q addresses the seat and is the alert landing somewhere, not an ask", via)
		}
	}
}

// The prompt asks a tagged owner and a fallback seat for different things.
// Telling a seat reached by fallback to "investigate and mitigate" starts it
// on work it may have no context for.
func TestThePromptTailorsToTheRoutingReason(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	owner := parse(t, p, alert(map[string]any{"tags": "crewlet:ceo", "title": "x"}))[0]
	fallback := parse(t, p, alert(map[string]any{"title": "x"}))[0]

	var prompt Prompt
	ownerText := prompt.Build(owner.Inbound, nil)
	fallbackText := prompt.Build(fallback.Inbound, nil)

	if ownerText == fallbackText {
		t.Fatal("an owner and a fallback seat were asked for the same thing")
	}
	if !strings.Contains(fallbackText, "belongs to you") {
		t.Fatalf("the fallback prompt does not ask whether this is theirs:\n%s", fallbackText)
	}
	if strings.Contains(ownerText, "belongs to you") {
		t.Fatalf("the owner prompt asks whether this is theirs:\n%s", ownerText)
	}
}

// A recovery and an alert ask for different things too. A recovery is not a
// no-op and it is not an incident either.
func TestThePromptTailorsToTheTransition(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	fired := parse(t, p, alert(map[string]any{
		"tags": "crewlet:ceo", "title": "x", "alert_transition": "Triggered",
	}))[0]
	recovered := parse(t, p, alert(map[string]any{
		"tags": "crewlet:ceo", "title": "x", "alert_transition": "Recovered",
	}))[0]

	var prompt Prompt
	firedText := prompt.Build(fired.Inbound, nil)
	recoveredText := prompt.Build(recovered.Inbound, nil)
	if firedText == recoveredText {
		t.Fatal("a firing monitor and a recovered one were asked for the same thing")
	}
	if !strings.Contains(recoveredText, "recovered") {
		t.Fatalf("the recovery prompt does not say it recovered:\n%s", recoveredText)
	}
}

// The prompt renders the link and the scope when they are there, because they
// are what let a seat look rather than guess.
func TestThePromptCarriesWhereToLook(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: "sre-lead"})
	routed := parse(t, p, alert(map[string]any{
		"tags": "crewlet:ceo", "title": "[Triggered] API latency",
		"link": "https://app.datadoghq.com/event/1", "scope": "host:web-3",
		"body": "p99 above 2s",
	}))[0]

	text := Prompt{}.Build(routed.Inbound, nil)
	for _, want := range []string{"API latency", "https://app.datadoghq.com/event/1", "host:web-3", "p99 above 2s"} {
		if !strings.Contains(text, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, text)
		}
	}
}

// Every alert keeps its own message in a digest. A monitor that triggers,
// recovers and re-triggers within one window is telling a story whose middle
// is the part that matters, and collapsing to the latest would render "it
// recovered" and drop that it had already gone off twice.
func TestEveryAlertKeepsItsBodyInADigest(t *testing.T) {
	if got := (Prompt{}).DigestBody("monitor.triggered", "p99 above 2s"); got != "p99 above 2s" {
		t.Fatalf("digest body is %q, want the message kept", got)
	}
}

// The source name is one word in four places (the route, the config key, the
// parser and integration.KindDatadog) and none of them may disagree: a parser
// registered under a name the route does not publish is one nothing reaches,
// and the failure is silent because an unrouted delivery is still verified,
// stored and counted.
func TestSourceMatchesTheBackendName(t *testing.T) {
	if got := NewParser(ParserOptions{}).Source(); got != Backend {
		t.Fatalf("the parser answers for %q, want %q", got, Backend)
	}
	if got := (Prompt{}).Source(); got != Backend {
		t.Fatalf("the prompt answers for %q, want %q", got, Backend)
	}
	if Backend != "datadog" {
		t.Fatalf("the backend name is %q, which is not the webhook route's", Backend)
	}
}

// DISMISSING AN UNOWNED ALERT IS AN ANSWER, not a blank.
//
// A company may want only the monitors it has labelled to wake anybody, and
// every other alert to stay with whatever Datadog already does about it.
// That is a decision, and it is a different thing from leaving the field
// empty: empty is a question nobody answered, and an alert reaching nobody
// through it is a silent hole in the coverage this integration exists to
// provide. Two values, because they are two states.
func TestNoneDismissesAnAlertNobodyOwns(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: config.DatadogIgnore})
	routed := parse(t, p, alert(map[string]any{"tags": "service:checkout"}))
	if len(routed) != 0 {
		t.Fatalf("woke %v, want the alert dismissed", handlesOf(routed))
	}
}

// AND A TAGGED MONITOR STILL WAKES ITS SEAT. Dismissing the unowned ones is
// not switching the integration off: it is what the tags are for.
func TestNoneStillWakesATaggedSeat(t *testing.T) {
	p := NewParser(ParserOptions{Fallback: config.DatadogIgnore})
	routed := parse(t, p, alert(map[string]any{"tags": "crewlet:backend-lead"}))
	if got := handlesOf(routed); !slices.Equal(got, []string{"backend-lead"}) {
		t.Fatalf("woke %v, want the tagged seat", got)
	}
}

// The tag key is offered on the form, above the fallback seat, and offered
// with its default filled in.
//
// ORDER IS ASSERTED because the two fields answer one question in sequence —
// which tag names an owner, and who is woken when none does — and a form that
// asks the second first reads as though the fallback were the whole of the
// routing.
func TestTheFormOffersTheTagKeyAboveTheFallbackSeat(t *testing.T) {
	reqs := Requirements(&config.Datadog{Enabled: true}, func(string) (string, bool) {
		return "", false
	})

	tag, route := -1, -1
	for i, r := range reqs {
		switch r.Field {
		case "handle_tag":
			tag = i
		case "route_to":
			route = i
		}
	}
	if tag < 0 || route < 0 {
		t.Fatalf("want both handle_tag and route_to on the form, got %d and %d", tag, route)
	}
	if tag > route {
		t.Errorf("handle_tag is at %d, below route_to at %d", tag, route)
	}
	if reqs[tag].Required {
		t.Error("handle_tag is required; a company that says nothing gets the default")
	}
	if got := reqs[tag].Default; got != DefaultHandleTag {
		t.Errorf("handle_tag default = %q, want %q", got, DefaultHandleTag)
	}
}

// A tag key the operator chose is what the form reports back, so reopening
// settings shows what is in force rather than the default underneath it.
func TestTheFormReportsAChosenTagKey(t *testing.T) {
	reqs := Requirements(
		&config.Datadog{Enabled: true, HandleTag: "owner"},
		func(string) (string, bool) { return "", false },
	)
	for _, r := range reqs {
		if r.Field != "handle_tag" {
			continue
		}
		if !r.Present || r.Stored != "owner" {
			t.Fatalf("handle_tag present=%v stored=%q, want true and %q", r.Present, r.Stored, "owner")
		}
		return
	}
	t.Fatal("no handle_tag requirement")
}
