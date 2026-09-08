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
	cloudPageUpdated = `{"page":{"idAsString":"10000001","creatorAccountId":"712020:actor","spaceKey":"ENG","spaceId":20000001,"modificationDate":1788716539150,"lastModifierAccountId":"712020:actor","self":"https://example.atlassian.net/wiki/spaces/ENG/pages/10000001/x","id":"10000001","title":"crewlet webhook variants","creationDate":1788716535483,"contentType":"page","version":2},"userAccountId":"712020:actor","timestamp":1788716539192,"accountType":"customer","updateTrigger":"edit_page","suppressNotifications":false}`

	cloudCommentCreated = `{"comment":{"idAsString":"10000002","creatorAccountId":"712020:actor","spaceKey":"ENG","spaceId":20000001,"parent":{"idAsString":"10000001","creatorAccountId":"712020:actor","spaceKey":"ENG","spaceId":20000001,"modificationDate":1788716539150,"lastModifierAccountId":"712020:actor","self":"https://example.atlassian.net/wiki/spaces/ENG/pages/10000001/x","id":"10000001","title":"crewlet webhook variants","creationDate":1788716535483,"contentType":"page","version":2},"modificationDate":1788716577732,"lastModifierAccountId":"712020:actor","self":"https://example.atlassian.net/wiki/spaces/ENG/pages/10000001/x?focusedCommentId=10000002","id":"10000002","creationDate":1788716577732,"contentType":"comment","version":1},"accountType":"customer","timestamp":1788716577732,"userAccountId":"712020:actor"}`
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
	if in.Metadata["space"] != "ENG" || in.Metadata["page_id"] != "10000001" {
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

// A CLOUD REPLY names its page in NEITHER place a top-level comment does: its
// parent is the comment being replied to, and the page is one level further
// up. This variant carries NO self URL on the reply, so the parent chain is
// the only thing that can answer and the walk is what the test pins.
const cloudReplyCreated = `{"comment":{"idAsString":"10000003","creatorAccountId":"712020:actor","spaceKey":"ENG","spaceId":20000001,"parent":{"idAsString":"10000002","spaceKey":"ENG","contentType":"comment","id":"10000002","parent":{"idAsString":"10000001","spaceKey":"ENG","self":"https://example.atlassian.net/wiki/spaces/ENG/pages/10000001/x","id":"10000001","title":"crewlet webhook variants","contentType":"page","version":2}},"id":"10000003","contentType":"comment","version":1},"accountType":"customer","timestamp":1788716599000,"userAccountId":"712020:actor"}`

// And this variant is the reply as delivered when the parent comment names no
// parent of its own: the self URL's /pages/<id>/ segment is then the only
// place the page appears at all, so it pins the fallback rather than the walk.
const cloudReplyBareParent = `{"comment":{"idAsString":"10000003","creatorAccountId":"712020:actor","spaceKey":"ENG","spaceId":20000001,"parent":{"idAsString":"10000002","spaceKey":"ENG","contentType":"comment","id":"10000002"},"self":"https://example.atlassian.net/wiki/spaces/ENG/pages/10000001/x?focusedCommentId=10000003","id":"10000003","contentType":"comment","version":1},"accountType":"customer","timestamp":1788716599000,"userAccountId":"712020:actor"}`

// A REPLY REACHES SOMEBODY, and reaches them on the PAGE's key.
//
// Refusing a comment parent outright, which is what a guard written against
// mis-keying did, dropped every Cloud reply before mentions, watchers or the
// space lead were ever considered: replies are most of a wiki thread, so a
// seat asked in one was never woken at all. Keying on the parent COMMENT is
// the other half of the same bug, since the reply then coalesces with nothing
// and never joins the thread it belongs to.
func TestACloudReplyRoutesOnItsPagesKey(t *testing.T) {
	t.Parallel()
	for name, payload := range map[string]string{
		"through the parent chain": cloudReplyCreated,
		"through the self URL":     cloudReplyBareParent,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := confluence.NewParser(confluence.ParserOptions{
				SiteURL: "https://example.atlassian.net/wiki",
				Leads:   map[string]string{"ENG": "eng-lead"},
			})
			routed, err := p.Parse(context.Background(),
				cloudDelivery(t, "comment_created", payload), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(routed) == 0 {
				t.Fatal("a Cloud reply routed to nobody")
			}
			if got := routed[0].Inbound.Metadata["page_id"]; got != "10000001" {
				t.Fatalf("page_id = %q, want the page's; the parent comment's "+
					"10000002 keys the reply away from its own thread", got)
			}
			if got := routed[0].Inbound.Metadata["space"]; got != "ENG" {
				t.Errorf("space = %q", got)
			}
		})
	}
}

// AND A REPLY COALESCES WITH THE THREAD IT IS IN. The page is the
// conversation for this third-party app, so a comment and a reply to it are one
// trigger rather than two turns.
func TestACloudReplyAndItsParentShareAConversation(t *testing.T) {
	t.Parallel()
	p := confluence.NewParser(confluence.ParserOptions{
		SiteURL: "https://example.atlassian.net/wiki",
		Leads:   map[string]string{"ENG": "eng-lead"},
	})
	parse := func(payload string) map[string]string {
		routed, err := p.Parse(context.Background(),
			cloudDelivery(t, "comment_created", payload), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(routed) == 0 {
			t.Fatal("routed to nobody")
		}
		return routed[0].Inbound.Metadata
	}
	top := (confluence.Prompt{}).ConversationKey(parse(cloudCommentCreated), "")
	reply := (confluence.Prompt{}).ConversationKey(parse(cloudReplyCreated), "")
	if top == "" || top != reply {
		t.Fatalf("a comment keys on %q and a reply to it on %q", top, reply)
	}
}

// The two bodies docs/integrations/confluence.md tells an operator to paste
// into a Confluence Automation rule, with the smart values rendered. Kept
// here so the recipe is pinned by the parser rather than by a reader trying
// it: a body the docs recommend that routes to nobody is a silent outage the
// operator has no way to attribute.
const (
	automationPageBody    = `{"page":{"id":"10000001","title":"Deploy runbook","version":{"number":"3"}},"space":{"key":"ENG"},"userAccountId":"712020:actor"}`
	automationCommentBody = `{"comment":{"id":"5001","parent":{"id":"10000001","title":"Deploy runbook","contentType":"page"}},"space":{"key":"ENG"},"userAccountId":"712020:actor"}`
)

func TestTheDocumentedAutomationBodiesRoute(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ event, body string }{
		"a page":    {"page_updated", automationPageBody},
		"a comment": {"comment_created", automationCommentBody},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := confluence.NewParser(confluence.ParserOptions{
				SiteURL: "https://example.atlassian.net/wiki",
				Leads:   map[string]string{"ENG": "eng-lead"},
			})
			routed, err := p.Parse(context.Background(), cloudDelivery(t, c.event, c.body), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(routed) != 1 || routed[0].To.Handle != "eng-lead" {
				t.Fatalf("routed = %+v, want the ENG lead", routed)
			}
			if got := routed[0].Inbound.Metadata["page_id"]; got != "10000001" {
				t.Errorf("page_id = %q", got)
			}
		})
	}
}
