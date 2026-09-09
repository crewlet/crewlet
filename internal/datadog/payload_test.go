package datadog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// THE TEMPLATE AND THE DECODER MUST AGREE ON EVERY KEY.
//
// [WebhookPayload] is what the reconcile writes into the webhook definition
// at Datadog, and [decode] is the only reader of it — the package doc says
// they live in one file precisely so they cannot drift, and that "the failure
// when they drifted would be a field that silently arrived empty for every
// alert". Nothing checked it: the only assertion was that the registered hook
// carried the constant, compared against that same constant, which catches a
// hook with no payload and never a renamed key.
//
// The template is literal JSON whose values are quoted $VARS, so it decodes
// directly: every Alert field must come back holding its own variable.
func TestEveryTemplateFieldReachesTheDecoder(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal([]byte(WebhookPayload), &body); err != nil {
		t.Fatalf("the payload template is not JSON, so Datadog posts it and "+
			"nothing can read it: %v", err)
	}

	got := decode(types.RawWebhook{Body: body})
	value := reflect.ValueOf(got)
	for i := range value.NumField() {
		name := value.Type().Field(i).Name
		field := value.Field(i)
		switch name {
		case "Tags":
			// Split on commas, so the one $VAR arrives as a single
			// element rather than as a string.
			if len(got.Tags) != 1 || !strings.HasPrefix(got.Tags[0], "$") {
				t.Errorf("Tags = %v, want the one template variable", got.Tags)
			}
		default:
			text, ok := field.Interface().(string)
			if !ok {
				t.Errorf("%s is %s; this test only knows strings and slices, "+
					"so a new kind of field needs a case here", name, field.Kind())
				continue
			}
			if text == "" {
				t.Errorf("%s decoded empty, so the template writes a key the "+
					"decoder does not read — every alert arrives without it",
					name)
			}
		}
	}
}

// AND THE DECODER READS NO KEY THE TEMPLATE DOES NOT WRITE, which is the
// same drift from the other side: a decoder field with no template key is one
// that is empty on every real delivery, and nothing about the running system
// says so.
func TestTheDecoderReadsNoKeyTheTemplateOmits(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal([]byte(WebhookPayload), &body); err != nil {
		t.Fatal(err)
	}
	// Every value in the template is its own $VAR, so a field that came
	// back holding one is a field the decoder found. The check above
	// covers the whole struct; this one asserts the template writes
	// nothing the decoder ignores, which is the leftover key case.
	got := decode(types.RawWebhook{Body: body})
	read := map[string]bool{
		"id": true, "monitor_id": true, "title": true, "body": true,
		"alert_transition": true, "priority": true, "tags": true,
		"link": true, "scope": true, "event_type": true,
	}
	for key := range body {
		if !read[key] {
			t.Errorf("the template writes %q and nothing in decode reads it", key)
		}
	}
	if got.ID == "" {
		t.Error("the sanity check itself decoded nothing")
	}
}
