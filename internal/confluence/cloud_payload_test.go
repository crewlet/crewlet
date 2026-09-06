package confluence_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// The two payload shapes Confluence CLOUD actually delivers, captured from a
// real site rather than written. Neither carries an event name; the route
// stamps it from the registered path before the parser sees the body.
const (
	cloudPageUpdated = `{"page":{"idAsString":"41746440","creatorAccountId":"712020:actor","spaceKey":"ENG","spaceId":28672112,"modificationDate":1788716539150,"lastModifierAccountId":"712020:actor","self":"https://example.atlassian.net/wiki/spaces/ENG/pages/41746440/x","id":"41746440","title":"crewlet webhook variants","creationDate":1788716535483,"contentType":"page","version":2},"userAccountId":"712020:actor","timestamp":1788716539192,"accountType":"customer","updateTrigger":"edit_page","suppressNotifications":false}`

	cloudCommentCreated = `{"comment":{"idAsString":"41713685","creatorAccountId":"712020:actor","spaceKey":"ENG","spaceId":28672112,"parent":{"idAsString":"41746440","creatorAccountId":"712020:actor","spaceKey":"ENG","spaceId":28672112,"modificationDate":1788716539150,"lastModifierAccountId":"712020:actor","self":"https://example.atlassian.net/wiki/spaces/ENG/pages/41746440/x","id":"41746440","title":"crewlet webhook variants","creationDate":1788716535483,"contentType":"page","version":2},"modificationDate":1788716577732,"lastModifierAccountId":"712020:actor","self":"https://example.atlassian.net/wiki/spaces/ENG/pages/41746440/x?focusedCommentId=41713685","id":"41713685","creationDate":1788716577732,"contentType":"comment","version":1},"accountType":"customer","timestamp":1788716577732,"userAccountId":"712020:actor"}`
)

func cloudDelivery(t *testing.T, event, payload string) types.RawWebhook {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		t.Fatal(err)
	}
	// What the Cloud route does before publishing.
	body["event"] = event
	return types.RawWebhook{Body: body}
}

// A CLOUD PAGE EVENT ROUTES with no Cloud branch in the parser: the actor is
// read from the top-level account id, the space from spaceKey, the page from
// its id and title, exactly as the Data Center shape is. The one input Cloud
// lacks, the event name, is what the route stamps.
func TestCloudPageUpdatedRoutesToTheSpaceLead(t *testing.T) {
	t.Parallel()
	p := confluence.NewParser(confluence.ParserOptions{
		SiteURL: "https://example.atlassian.net/wiki",
		Leads:   map[string]string{"ENG": "eng-lead"},
	})
	routed, err := p.Parse(context.Background(), cloudDelivery(t, "page_updated", cloudPageUpdated), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(routed) != 1 || routed[0].To.Handle != "eng-lead" {
		t.Fatalf("routed = %+v, want the ENG lead", routed)
	}
	in := routed[0].Inbound
	if in.Metadata[notify.ActorField] != "712020:actor" {
		t.Errorf("actor = %q, want the top-level userAccountId", in.Metadata[notify.ActorField])
	}
	if in.Metadata["space"] != "ENG" || in.Metadata["page_id"] != "41746440" {
		t.Errorf("space/page = %q/%q", in.Metadata["space"], in.Metadata["page_id"])
	}
	if in.Metadata["event_type"] != "page_updated" {
		t.Errorf("event_type = %q, want the stamped event", in.Metadata["event_type"])
	}
}

// A CLOUD COMMENT carries its page under "parent" rather than "page", and the
// parser reads the page it is on from there.
func TestCloudCommentCreatedRoutes(t *testing.T) {
	t.Parallel()
	p := confluence.NewParser(confluence.ParserOptions{
		SiteURL: "https://example.atlassian.net/wiki",
		Leads:   map[string]string{"ENG": "eng-lead"},
	})
	routed, err := p.Parse(context.Background(), cloudDelivery(t, "comment_created", cloudCommentCreated), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(routed) == 0 {
		t.Fatal("a Cloud comment routed to nobody")
	}
	if got := routed[0].Inbound.Metadata["space"]; got != "ENG" {
		t.Errorf("space = %q", got)
	}
}

// WITHOUT THE STAMP, A CLOUD PAYLOAD ROUTES NOWHERE, which is the state every
// Cloud delivery was in before the route existed and the reason the route
// stamps rather than trusting the body.
func TestCloudPayloadWithoutAnEventRoutesNowhere(t *testing.T) {
	t.Parallel()
	p := confluence.NewParser(confluence.ParserOptions{Leads: map[string]string{"ENG": "eng-lead"}})
	var body map[string]any
	_ = json.Unmarshal([]byte(cloudPageUpdated), &body)
	routed, err := p.Parse(context.Background(), types.RawWebhook{Body: body}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(routed) != 0 {
		t.Fatalf("an unstamped Cloud payload routed to %+v", routed)
	}
}
