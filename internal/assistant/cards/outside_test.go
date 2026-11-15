package cards_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// A STEP OUTSIDE CREWLET IS DRAWN ONLY AT AN ABSOLUTE HTTPS ADDRESS, and the
// model reads its words and host, never the address. Mutation: draw any
// address, or hand the model the URL, and this fails.
func TestAStepOutsideCrewletIsAnHTTPSAddressTheModelNeverReads(t *testing.T) {
	t.Parallel()
	spec := cards.Spec{Entity: &cards.Entity{ID: "handle", Title: "name",
		Facts: []cards.Fact{{Label: "Add", Path: "step.add", Type: wire.ColumnText}}},
		Outside: &cards.Outside{Path: "step.url", Label: "step.label"}}
	answer := func(url string) []byte {
		return []byte(`{"handle":"sre","name":"SRE","step":{"label":"Add it at Cloudflare",` +
			`"add":["Origin Rules: Edit"],"url":"` + url + `"}}`)
	}
	built, err := cards.Build(spec, answer("https://dash.cloudflare.com/profile/api-tokens"), evals.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if o := built.Card.Entity.Outside; o == nil || o.URL != "https://dash.cloudflare.com/profile/api-tokens" {
		t.Fatalf("outside = %+v, want the https address", o)
	}
	said, err := cards.SummaryOf(built, cards.ModelBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(said), "dash.cloudflare.com") || strings.Contains(string(said), "/profile/api-tokens") {
		t.Errorf("the model reads %s, want the host and never the address", said)
	}
	for _, url := range []string{"http://dash.cloudflare.com/x", "javascript:alert(1)",
		"https://user@evil.example/", "/settings"} {
		built, err := cards.Build(spec, answer(url), evals.Epoch)
		if err != nil || built.Card.Entity.Outside != nil {
			t.Errorf("%s drew %+v (%v), want no link", url, built.Card.Entity.Outside, err)
		}
	}
}
