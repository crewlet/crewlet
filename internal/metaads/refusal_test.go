package metaads_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/metaads"
)

// META'S DEFINITE ANSWER IS A REFUSAL IN ITS OWN WORDS FOR A PERSON, and Meta
// not serving the call (a 5xx, a rate limit, a transient error) is none.
//
// Mutation: answer a refusal for a transient error, and a person is sent to
// fix what clears on its own.
func TestMetasRefusalSaysWhatMetaWroteForAPerson(t *testing.T) {
	t.Parallel()
	r := (&metaads.APIError{Status: 400, Message: "(#200) Permissions error", UserTitle: "Accept the terms",
		UserMessage: "The ad account owner must accept the terms."}).VendorRefusal()
	if r == nil || r.Said != "Meta refused the call: Accept the terms. The ad account owner must accept the terms." {
		t.Fatalf("refusal %+v", r)
	}
	if r := (&metaads.APIError{Status: 403, Message: "Permissions error."}).VendorRefusal(); r == nil ||
		r.Said != "Meta refused the call: Permissions error." {
		t.Fatalf("refusal with Meta's message alone %+v", r)
	}
	for _, e := range []*metaads.APIError{{Status: 500}, {Status: 429}, {Status: 400, Transient: true}} {
		if r := e.VendorRefusal(); r != nil {
			t.Errorf("Meta not serving (%+v) read as a refusal: %+v", e, r)
		}
	}
}
