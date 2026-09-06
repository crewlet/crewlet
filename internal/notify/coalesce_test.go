package notify_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

var t0 = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// tracker is a vendor whose field updates re-emit stale state and carry no
// signal of their own — the shape the supersede rule exists for.
type tracker struct{}

func (tracker) Source() string { return "tracker" }

// Build front-loads triage boilerplate the way a real prompt does — around
// 1.5k characters of it on a chat backend. The scaffolding is the whole
// reason the salient body is carried separately: a worker filtering on a
// prefix of the rendered trigger sees only text identical on every turn.
func (tracker) Build(n notify.Inbound, _ notify.Parties) string {
	return "## How to triage this\n\n" + n.Body
}

// RequiresRecon: this vendor's webhooks name a thing-that-changed rather
// than carrying it, EXCEPT a plain message, which is self-contained. Both
// branches exist so a hardcoded answer is visible.
func (tracker) RequiresRecon(n notify.Inbound) bool { return n.EventType != "message" }

// Addressed: this vendor's assignments name the seat as the one who has to
// act; everything else it emits is news about something the seat follows.
// Both branches exist so a hardcoded answer is visible.
func (tracker) Addressed(n notify.Inbound) bool { return n.EventType == "assigned" }

// A pipeline result reports the outcome of the actor's OWN push, so it
// reaches them; everything else this vendor emits, they already know about.
func (tracker) WakesActor(eventType string) bool { return eventType == "pipeline_failed" }
func (tracker) ConversationKey(m map[string]string, _ string) string {
	return m["issue_id"]
}
func (tracker) DigestBody(eventType, body string) string {
	if eventType == "issue_updated" {
		return ""
	}
	return body
}

func note(sender, body, eventType string, opts ...func(*types.ExternalNotification)) types.ExternalNotification {
	salient := body
	n := types.ExternalNotification{
		NotificationSource: "tracker", SourceEventType: eventType,
		Sender: sender, Subject: "ENG-42", Body: "SCAFFOLDING\n\n" + body,
		SalientBody: &salient,
		Metadata:    map[string]string{"issue_id": "u-1", "event_type": eventType},
	}
	for _, opt := range opts {
		opt(&n)
	}
	return n
}

func at(offsets ...int) []time.Time {
	out := make([]time.Time, 0, len(offsets))
	for _, o := range offsets {
		out = append(out, t0.Add(time.Duration(o)*time.Minute))
	}
	return out
}

func prompts() notify.Prompts { return notify.NewPrompts(tracker{}) }

// A partition of one must stay EXACTLY the pre-coalescing path: one that went
// through the merge would gain a digest header describing one event.
func TestASingleEventIsReturnedUntouched(t *testing.T) {
	original := note("ana", "please look at this", "comment")
	merged, ok := notify.Coalesce(prompts(), []types.ExternalNotification{original}, at(0))
	if !ok {
		t.Fatal("Coalesce refused a single event")
	}
	if merged.Body != original.Body {
		t.Fatalf("a single event was rewritten:\n%s", merged.Body)
	}
	if len(merged.Messages) != 0 {
		t.Fatal("a single event gained a constituent list, so every consumer now reads it as a digest")
	}
}

func TestNoEventsCoalesceToNothing(t *testing.T) {
	if _, ok := notify.Coalesce(prompts(), nil, nil); ok {
		t.Fatal("Coalesce invented an event out of an empty partition")
	}
}

// The latest constituent renders IN FULL and last, so the vendor's own prompt
// scaffolding appears exactly once, from the most recent state.
func TestTheLatestConstituentRendersInFullAfterTheDigest(t *testing.T) {
	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "first", "comment"),
		note("bo", "second", "comment"),
		note("cy", "third and latest", "comment"),
	}, at(0, 1, 2))

	if strings.Count(merged.Body, "SCAFFOLDING") != 1 {
		t.Fatalf("the vendor's boilerplate rendered %d times:\n%s",
			strings.Count(merged.Body, "SCAFFOLDING"), merged.Body)
	}
	digest, full, found := strings.Cut(merged.Body, "\n---\n")
	if !found {
		t.Fatalf("no separator between the digest and the latest event:\n%s", merged.Body)
	}
	for _, want := range []string{"ana", "first", "bo", "second"} {
		if !strings.Contains(digest, want) {
			t.Fatalf("the digest is missing %q:\n%s", want, digest)
		}
	}
	if !strings.Contains(full, "third and latest") {
		t.Fatalf("the latest event did not render in full:\n%s", full)
	}
	if strings.Contains(digest, "third and latest") {
		t.Fatal("the latest event rendered twice — once in the digest and once in full")
	}
}

// The flat fields mirror the latest constituent, whose metadata carries the
// pointers a reply needs.
func TestTheFlatFieldsMirrorTheLatestConstituent(t *testing.T) {
	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "first", "comment", func(n *types.ExternalNotification) {
			n.Metadata = map[string]string{"issue_id": "u-1", "comment_id": "c-1"}
		}),
		note("cy", "latest", "comment", func(n *types.ExternalNotification) {
			n.Metadata = map[string]string{"issue_id": "u-1", "comment_id": "c-9"}
		}),
	}, at(0, 1))

	if merged.Sender != "cy" {
		t.Fatalf("sender = %q, want the latest", merged.Sender)
	}
	if merged.Metadata["comment_id"] != "c-9" {
		t.Fatalf("metadata = %v, want the latest constituent's pointers", merged.Metadata)
	}
}

// The order is the caller's fact, and a merge that sorted by anything else
// would rewrite the conversation.
func TestConstituentsAreOrderedByTheirTimestamps(t *testing.T) {
	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("cy", "third", "comment"),
		note("ana", "first", "comment"),
		note("bo", "second", "comment"),
	}, at(2, 0, 1))

	if merged.Sender != "cy" {
		t.Fatalf("the latest was resolved as %q, not the one with the newest timestamp", merged.Sender)
	}
	var order []string
	for _, m := range merged.Messages {
		order = append(order, m.Sender)
	}
	if strings.Join(order, ",") != "ana,bo,cy" {
		t.Fatalf("constituents are ordered %v, want chronological", order)
	}
}

// A vendor emitting a burst routinely stamps one timestamp on several events;
// re-ordering them on a tie would rewrite the conversation for nothing.
//
// TWENTY of them, deliberately. Go's sort falls back to insertion sort below a
// small threshold, which happens to be stable — so a three-element version of
// this test passes against an UNSTABLE sort too, and pins nothing. Twenty is
// past that threshold, and a person typing a burst produces far more than
// three messages anyway.
func TestATieKeepsTheDeliveredOrder(t *testing.T) {
	// The SHAPE here is the whole test, and it is not obvious.
	//
	// A burst that is the ENTIRE input pins nothing, at any size: Go's
	// pdqsort checks for an already-ordered run first, and an all-equal
	// slice IS one, so it returns before partitioning and an unstable sort
	// gives the same answer as a stable one. Measured — twenty, a hundred
	// and a thousand tied elements all come back untouched.
	//
	// What separates the two sorts is a tie sitting inside input that
	// genuinely has to be partitioned. That is also the real delivery: a
	// broker partition arrives in whatever order it arrives in — sorting it
	// is the point — so a burst of same-stamped messages is bracketed by
	// events that are out of chronological order around it.
	const burst = 18
	evs := []types.ExternalNotification{note("straggler", "delivered first, happened last", "comment")}
	when := []time.Time{t0.Add(2 * time.Hour)}
	want := []string{"opener"}
	for i := range burst {
		sender := "burst-" + strconv.Itoa(i)
		evs = append(evs, note(sender, "message "+strconv.Itoa(i), "comment"))
		when = append(when, t0) // every one of them at the same instant
		want = append(want, sender)
	}
	evs = append(evs, note("opener", "delivered last, happened first", "comment"))
	when = append(when, t0.Add(-2*time.Hour))
	want = append(want, "straggler")

	merged, _ := notify.Coalesce(prompts(), evs, when)

	var order []string
	for _, m := range merged.Messages {
		order = append(order, m.Sender)
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("a burst sharing one timestamp was reordered:\n got %v\nwant %v", order, want)
	}
	// And the LATEST is the one that happened last, not the one delivered
	// last — the flat fields a reply reads come from that constituent.
	if merged.Sender != "straggler" {
		t.Fatalf("the latest constituent resolved to %q, want %q", merged.Sender, "straggler")
	}
}

// A source whose intermediate bodies carry no signal collapses them, so the
// one line that changed is not buried under N copies of the description.
func TestASupersededBodyCollapsesToItsLead(t *testing.T) {
	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "the whole stale description", "issue_updated"),
		note("bo", "the actual question", "comment"),
	}, at(0, 1))

	digest, _, _ := strings.Cut(merged.Body, "\n---\n")
	if strings.Contains(digest, "the whole stale description") {
		t.Fatalf("a superseded body rendered anyway:\n%s", digest)
	}
	if !strings.Contains(digest, "superseded by later state") {
		t.Fatalf("the digest does not say why the line is empty:\n%s", digest)
	}
}

// A code host re-emits the whole description on every lifecycle action.
func TestARepeatedBodyFromOneSenderRendersOnce(t *testing.T) {
	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("bot", "the full unchanged description", "comment"),
		note("bot", "the full unchanged description", "comment"),
		note("ana", "the one real comment", "comment"),
	}, at(0, 1, 2))

	digest, _, _ := strings.Cut(merged.Body, "\n---\n")
	if strings.Count(digest, "the full unchanged description") != 1 {
		t.Fatalf("the repeated body rendered %d times:\n%s",
			strings.Count(digest, "the full unchanged description"), digest)
	}
	if !strings.Contains(digest, "identical to a later update") {
		t.Fatalf("the collapsed line does not say why:\n%s", digest)
	}
}

// Two people each saying "+1" are two facts, not one repeated.
func TestTwoSendersSayingTheSameThingBothSurvive(t *testing.T) {
	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "+1", "comment"),
		note("bo", "+1", "comment"),
		note("cy", "shipping it", "comment"),
	}, at(0, 1, 2))

	digest, _, _ := strings.Cut(merged.Body, "\n---\n")
	if strings.Count(digest, "+1") != 2 {
		t.Fatalf("one of two independent agreements was collapsed:\n%s", digest)
	}
}

// A merge must not launder a constraint: a digest containing one bare pointer
// is still a trigger the seat has to go and look behind.
func TestReconIsRequiredWhenAnyConstituentRequiredIt(t *testing.T) {
	quiet := func(n *types.ExternalNotification) { n.ContextRequiresRecon = false }
	pointer := func(n *types.ExternalNotification) { n.ContextRequiresRecon = true }

	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "first", "comment", pointer),
		note("bo", "latest", "comment", quiet),
	}, at(0, 1))
	if !merged.ContextRequiresRecon {
		t.Fatal("recon was laundered by the merge — the latest constituent's flag won")
	}

	merged, _ = notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "first", "comment", quiet),
		note("bo", "latest", "comment", quiet),
	}, at(0, 1))
	if merged.ContextRequiresRecon {
		t.Fatal("recon was required by a merge of events that each said it was not")
	}
}

// The same laundering rule for the delivery obligation: one direct ask inside
// a burst of broadcasts is still somebody waiting for an answer, and a merge
// that dropped it would let a coalesced turn end in silence on a request.
func TestAddressedSurvivesAMergeWithUnaddressedTraffic(t *testing.T) {
	passing := func(n *types.ExternalNotification) { n.Addressed = false }
	asked := func(n *types.ExternalNotification) { n.Addressed = true }

	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "first", "comment", asked),
		note("bo", "latest", "comment", passing),
	}, at(0, 1))
	if !merged.Addressed {
		t.Fatal("the ask was laundered by the merge — the latest constituent's flag won")
	}
	// The counterfactual: without it the assertion above passes for a merge
	// that hardcodes true.
	merged, _ = notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "first", "comment", passing),
		note("bo", "latest", "comment", passing),
	}, at(0, 1))
	if merged.Addressed {
		t.Fatal("a merge of events that each addressed nobody produced an ask")
	}
	// And each constituent keeps its OWN flag, so a worker reasoning per
	// message can still tell the ask from the traffic around it.
	merged, _ = notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "first", "comment", asked),
		note("bo", "latest", "comment", passing),
	}, at(0, 1))
	if len(merged.Messages) != 2 || !merged.Messages[0].Addressed || merged.Messages[1].Addressed {
		t.Fatalf("per-constituent flags lost: %+v", merged.Messages)
	}
}

// A worker that observed only the last sender would never see the four people
// who spoke before them.
func TestEveryConstituentSurvivesAtFullFidelity(t *testing.T) {
	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "first", "comment"),
		note("bo", "second", "comment"),
		note("cy", "third", "comment"),
	}, at(0, 1, 2))

	if len(merged.Messages) != 3 {
		t.Fatalf("%d constituents survived, want 3", len(merged.Messages))
	}
	for _, m := range merged.Messages {
		if m.Sender == "" || m.Body == "" || m.Timestamp.IsZero() {
			t.Fatalf("a constituent lost its identity: %+v", m)
		}
		// The SALIENT body, not the scaffolding-laden one.
		if strings.Contains(m.Body, "SCAFFOLDING") {
			t.Fatalf("a constituent carries prompt scaffolding: %q", m.Body)
		}
	}
}

// What the learning workers embed, so it carries the same supersede rule the
// digest does.
func TestTheMergedSalientTextMatchesWhatThePlannerSees(t *testing.T) {
	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "stale description", "issue_updated"),
		note("bo", "the real question", "comment"),
	}, at(0, 1))

	if merged.SalientBody == nil {
		t.Fatal("a merge produced no salient body, so a worker falls back to the digest scaffolding")
	}
	salient := *merged.SalientBody
	if strings.Contains(salient, "stale description") {
		t.Fatalf("the salient text kept a body the digest superseded: %q", salient)
	}
	if !strings.Contains(salient, "bo: the real question") {
		t.Fatalf("the salient text is not sender-attributed: %q", salient)
	}
	if strings.Contains(salient, "SCAFFOLDING") {
		t.Fatalf("the salient text carries prompt scaffolding: %q", salient)
	}
}

// nil means "no distinct raw message, use the body"; "" means a genuinely
// contentless message that must NOT fall back to the scaffolding.
func TestTheSalientBodyContractIsHonoured(t *testing.T) {
	empty := ""
	merged, _ := notify.Coalesce(prompts(), []types.ExternalNotification{
		// No salient body at all: the body IS the message.
		note("ana", "first", "comment", func(n *types.ExternalNotification) {
			n.SalientBody = nil
			n.Body = "an extension's plain body"
		}),
		// A salient body that is genuinely empty.
		note("bo", "second", "comment", func(n *types.ExternalNotification) {
			n.SalientBody = &empty
		}),
		note("cy", "latest", "comment"),
	}, at(0, 1, 2))

	digest, _, _ := strings.Cut(merged.Body, "\n---\n")
	if !strings.Contains(digest, "an extension's plain body") {
		t.Fatalf("a nil salient body did not fall back to the body:\n%s", digest)
	}
	if strings.Contains(digest, "SCAFFOLDING") {
		t.Fatalf("an empty salient body fell back to the scaffolding:\n%s", digest)
	}
}

// A source nobody wrote a prompt for merges by the conservative rules.
func TestAnUnknownSourceStillCoalesces(t *testing.T) {
	unknown := func(sender, body string) types.ExternalNotification {
		salient := body
		return types.ExternalNotification{
			NotificationSource: "something-new", SourceEventType: "thing",
			Sender: sender, Body: body, SalientBody: &salient,
		}
	}
	merged, ok := notify.Coalesce(notify.NewPrompts(), []types.ExternalNotification{
		unknown("ana", "first"), unknown("bo", "second"),
	}, at(0, 1))
	if !ok {
		t.Fatal("an unknown source did not coalesce")
	}
	// Nothing is superseded, because the fallback has no idea which of this
	// vendor's bodies are state and which are messages.
	if !strings.Contains(merged.Body, "first") {
		t.Fatalf("the generic fallback dropped a body:\n%s", merged.Body)
	}
}

// A missing timestamp list must not reorder or crash the merge.
func TestMissingTimestampsDoNotBreakTheMerge(t *testing.T) {
	merged, ok := notify.Coalesce(prompts(), []types.ExternalNotification{
		note("ana", "first", "comment"), note("bo", "second", "comment"),
	}, nil)
	if !ok || len(merged.Messages) != 2 {
		t.Fatalf("Coalesce with no timestamps = %v, %d constituents", ok, len(merged.Messages))
	}
	// With no clock to sort by, the delivered order stands.
	if merged.Messages[0].Sender != "ana" {
		t.Fatalf("the delivered order was not preserved: %+v", merged.Messages)
	}
}
