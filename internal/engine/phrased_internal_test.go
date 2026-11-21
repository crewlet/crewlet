package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/named"
)

// A WAKE WHOSE LINE NAMES WHAT IT IS ABOUT is still read as its line when it
// carries no body: a payload that writes a phrase (events.Phraser) rather than
// a summary hands the turn the same words, never a bare type.
//
// Mutation: read only events.Summarizer here, and the ask is
// "(external_notification)".
func TestABodilessWakeIsReadAsItsPhrase(t *testing.T) {
	t.Parallel()
	wake := events.New(types.ExternalNotification{Sender: "ana", Subject: "web!42: Fix login",
		SubjectRefs: []named.Ref{{Kind: named.Change, ID: "web!42", Text: "web!42"}}}, events.TraceContext{})
	if got, want := DescribeTrigger([]*events.Event{wake}, ""), "Message from ana: web!42: Fix login"; got != want {
		t.Errorf("the woken turn's task = %q, want %q", got, want)
	}
}
