package expo_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/expo"
)

// EXPO'S DEFINITE ANSWER IS A REFUSAL IN EXPO'S WORDS, and Expo not serving
// the call (a 5xx, a rate limit) is none.
//
// Mutation: answer nil for a 4xx, and its words reach no person.
func TestExposRefusalSaysWhatExpoAnswered(t *testing.T) {
	t.Parallel()
	r := (&expo.APIError{Op: "CreateRobot", Status: 403, Detail: "The account is not on a plan with robots."}).VendorRefusal()
	if r == nil || r.Said != "Expo refused the call: The account is not on a plan with robots." {
		t.Fatalf("refusal %+v", r)
	}
	for _, status := range []int{500, 429} {
		if r := (&expo.APIError{Status: status}).VendorRefusal(); r != nil {
			t.Errorf("Expo not serving (%d) read as a refusal: %+v", status, r)
		}
	}
}
