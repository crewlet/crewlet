package webhooks_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/store"
)

// rowOf files one delivery's record exactly as the node's publish listener
// does, so these cases assert the ROW a listing returns rather than the record
// on its way to becoming one.
func rowOf(t *testing.T, route, source, label, handle, key string, raw []byte) store.EventRecord {
	t.Helper()
	ev := events.New(webhooks.DeliveryRecordForTest(route, label, handle, key, raw),
		events.TraceContext{})
	ev.Source = source
	row, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a delivery's record is not filed as a row at all")
	}
	return row
}

// A DELIVERY'S ROW SAYS WHO IT WAS FOR.
//
// The row is what a listing returns and the payload deliberately is not, so
// without the tag a deliveries screen could say a delivery arrived and not
// which seat it was addressed to — and answering that for a page of rows meant
// one payload fetch per row.
func TestADeliveryRowNamesTheSeatItWasFor(t *testing.T) {
	t.Parallel()
	got := rowOf(t, "slack", "slack", "webhook:message", "agent-swe", "d-42", []byte(`{"a":1}`)).Tags
	// `recipient` is one of the four keys the event store indexes as a
	// PARTY, so this is also what makes `events?agent=agent-swe` return
	// what reached that seat from outside.
	if got["recipient"] != "agent-swe" {
		t.Errorf("tags = %v, want the seat under the party key", got)
	}
	if got["delivery_key"] != "d-42" {
		t.Errorf("tags = %v, want the provider's own delivery id", got)
	}
}

// AND A DELIVERY ADDRESSED TO NOBODY CARRIES NO EMPTY TAG.
//
// An empty tag is not an absent one: a row carrying `recipient: ""` reads as a
// delivery addressed to a seat whose handle went missing, and it would join
// the party index under the empty string.
func TestADeliveryAddressedToNobodyCarriesNoEmptyTag(t *testing.T) {
	t.Parallel()
	got := rowOf(t, "gitlab", "gitlab", "webhook:push", "", "", []byte(`{}`)).Tags
	for _, key := range []string{"recipient", "delivery_key"} {
		if _, present := got[key]; present {
			t.Errorf("tags = %v, want no %s key at all", got, key)
		}
	}
	got = rowOf(t, "gitlab", "gitlab", "webhook:push", "", "d-7", []byte(`{}`)).Tags
	if _, present := got["recipient"]; present {
		t.Errorf("tags = %v, want no recipient key at all", got)
	}
	if got["delivery_key"] != "d-7" {
		t.Errorf("tags = %v, want the key that was present", got)
	}
}

// A HASH THIS EDGE DERIVED IS NOT THE PROVIDER'S ID. The Forge relay, Datadog
// and an Atlassian build without the identifier header send no id, and the edge
// dedupes on a hash of the bytes instead; tagged as the provider's id, that
// hash drew under "Provider id" on the deliveries panel as though the provider
// had minted it.
func TestABodyHashIsNotTheProvidersID(t *testing.T) {
	t.Parallel()
	got := rowOf(t, "forge", "jira", "forge:avi:jira:created:issue", "", "body:0123abcd", []byte(`{}`)).Tags
	if _, present := got["delivery_key"]; present {
		t.Errorf("tags = %v, want no delivery_key: a body hash is the edge's dedupe key", got)
	}
}

// THE ROW IS THE DELIVERY AS IT HAS ALWAYS BEEN — filed under the provider's
// own label, with the provider's exact bytes as its payload — and it names the
// ROUTE that authenticated it, which on the Forge relay is not its source.
func TestADeliveryRowIsFiledUnderItsLabelWithTheProvidersBytes(t *testing.T) {
	t.Parallel()
	raw := []byte(`{ "issue" : {"key":"ENG-1"} }`)
	row := rowOf(t, "forge", "jira", "forge:avi:jira:created:issue", "", "", raw)
	if row.Type != "forge:avi:jira:created:issue" {
		t.Errorf("row type = %q, want the delivery's label", row.Type)
	}
	if string(row.Payload) != string(raw) {
		t.Errorf("row payload = %s, want the provider's exact bytes %s", row.Payload, raw)
	}
	if row.Source != "jira" || row.Tags["route"] != "forge" {
		t.Errorf("source %q, route %q: want the payload's integration and the relay that "+
			"authenticated it", row.Source, row.Tags["route"])
	}
	if row.Category != events.WebhookCategory {
		t.Errorf("category = %q, want %q", row.Category, events.WebhookCategory)
	}
}
