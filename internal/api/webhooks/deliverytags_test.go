package webhooks_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/webhooks"
)

// A DELIVERY'S ROW SAYS WHO IT WAS FOR.
//
// The row is what a listing returns and the payload deliberately is not, so
// without the tag a deliveries screen could say a delivery arrived and not
// which seat it was addressed to — and answering that for a page of rows meant
// one payload fetch per row.
func TestADeliveryRowNamesTheSeatItWasFor(t *testing.T) {
	t.Parallel()
	got := webhooks.DeliveryTagsForTest("agent-swe", "d-42")
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
	if got := webhooks.DeliveryTagsForTest("", ""); got != nil {
		t.Errorf("tags = %v, want none at all", got)
	}
	got := webhooks.DeliveryTagsForTest("", "d-7")
	if _, present := got["recipient"]; present {
		t.Errorf("tags = %v, want no recipient key at all", got)
	}
	if got["delivery_key"] != "d-7" {
		t.Errorf("tags = %v, want the key that was present", got)
	}
}
