package googlegrowth_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// GOOGLE'S DEFINITE ANSWER IS A REFUSAL IN GOOGLE'S WORDS; a call Google did
// not answer, or one it was too busy for, is none.
//
// Mutation: answer a refusal for an unavailable Google, and a person is sent
// to fix an outage.
func TestGooglesRefusalSaysWhatGoogleAnswered(t *testing.T) {
	t.Parallel()
	api := &googleauth.APIError{Status: 403, Code: "PERMISSION_DENIED", Message: "The caller does not have permission"}
	r := (&googlegrowth.CallError{Failure: growth.FailureVendorRefused, Err: api}).VendorRefusal()
	if r == nil || r.Said != "Google refused the call: The caller does not have permission." {
		t.Fatalf("refusal %+v", r)
	}
	for _, e := range []*googlegrowth.CallError{
		{Failure: growth.FailureVendorUnavailable, Err: &googleauth.APIError{Status: 503}},
		{Failure: growth.FailureTimedOut, Err: errors.New("deadline")},
	} {
		if r := e.VendorRefusal(); r != nil {
			t.Errorf("Google not answering (%s) read as a refusal: %+v", e.Failure, r)
		}
	}
}
