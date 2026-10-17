package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/setup"
)

// A PERSON THE CHART NAMES BY AN ACCOUNT NOBODY HOLDS IS IN THE INTEGRATION'S
// STATUS, which is where an operator looks — and on the pass's every return,
// the one that cannot provision included: whether a person's name holds an
// account is no part of provisioning, and a company whose bots were made by
// hand has people the agents may not recognise all the same.
func TestTheMattermostPassReportsAPersonNobodyHolds(t *testing.T) {
	t.Parallel()
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/me"):
			w.Write([]byte(`{"id":"bot-ceo","username":"ceo","is_bot":true}`))
		case strings.HasSuffix(r.URL.Path, "/users/usernames"):
			// NOBODY HOLDS THE NAME.
			w.Write([]byte(`[]`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(chat.Close)

	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
integrations:
  mattermost:
    enabled: true
    url: %q
    team: eng
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      mattermost:
        bot_token: tok-ceo
  - name: Founder
    kind: human
    contact:
      mattermost_user_id: ghost
`, chat.URL)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	e := &Engine{}
	e.epoch.current.Store(company)

	tr, err := mattermost.NewTransport(mattermost.TransportOptions{
		Config: mattermost.Config{
			URL: chat.URL, Team: "eng",
			Seats:  mattermost.SeatsFrom(company.Org, nil),
			People: mattermost.PeopleFrom(company.Org, nil),
		},
		Publisher: nopPublisher{},
		Claims:    memory.NewFleet(),
		Registry: func() *notify.Registry {
			return notify.NewRegistry(company.Org, nil)
		},
		Connect: func(ctx context.Context, _ mattermost.Seat, _ *mattermost.Client) (mattermost.Socket, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { tr.Stop(context.Background()) })
	e.notify.mattermost = tr

	findings, err := (&mattermostPass{engine: e}).Run(t.Context(), setup.PassInput{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var person *integration.Finding
	for i, f := range findings {
		if f.Kind == integration.FindingContactUnknown {
			person = &findings[i]
		}
	}
	if person == nil {
		t.Fatalf("findings = %+v, want the founder named by an account nobody holds", findings)
	}
	if person.Subject != "founder" || !strings.Contains(person.Detail, `"ghost"`) {
		t.Fatalf("the finding reads %+v", person)
	}
}

// nopPublisher queues nothing: no post reaches this transport.
type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, string, *events.Event) error { return nil }
