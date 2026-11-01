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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/notify"
)

// A SUSPENSION WITHDRAWS THE ACCOUNT A PERSON'S USERNAME NAMES, through the
// directory's own trigger and with no configuration change.
//
// The account is registered by the Mattermost transport rather than by the
// human reconcile, so it reaches a registry by another road than every other
// contact identity — and the directory's rebuild has to reach it there too:
// the registry it builds for the same company is handed to the transport,
// which judges each person by that registry's reading. Suspended, the
// founder's posts reach the agents from a stranger and nothing is reported;
// reinstated, the next registry has their account again from the last answer.
//
// Mutations: let the transport judge people by no registry, and the suspended
// founder keeps their account; skip handing the transport the rebuilt
// registry, and the reinstated founder never gets it back.
func TestASuspensionWithdrawsAPersonsMattermostAccount(t *testing.T) {
	t.Parallel()
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/me"):
			w.Write([]byte(`{"id":"bot-ceo","username":"ceo","is_bot":true}`))
		case strings.HasSuffix(r.URL.Path, "/users/usernames"):
			w.Write([]byte(`[{"id":"u-jane","username":"jane"}]`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(chat.Close)

	e := &Engine{directoryNudge: make(chan struct{}, 1)}
	stage := iam.StageActive
	e.useDirectory(directoryFunc(func(context.Context) ([]notify.Holder, error) {
		return []notify.Holder{{Seat: "founder", Stage: stage}}, nil
	}), nil)
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
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
        enabled: false
        bot_token: tok-ceo
  - name: Founder
    kind: human
    contact:
      mattermost_user_id: jane
`, chat.URL)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("NewCompanyWith: %v", err)
	}
	e.refreshParties(t.Context(), company)

	tr, err := mattermost.NewTransport(mattermost.TransportOptions{
		Config: mattermost.Config{
			URL: chat.URL, Team: "eng",
			Seats:  mattermost.SeatsFrom(company.Org, nil),
			People: mattermost.PeopleFrom(company.Org, nil),
		},
		Publisher: nopPublisher{},
		Claims:    memory.NewFleet(),
		Registry:  e.Registry,
		Connect: func(ctx context.Context, _ mattermost.Seat, _ *mattermost.Client) (mattermost.Socket, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	e.notify.mattermost = tr
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { tr.Stop(context.Background()) })
	founder := func() (notify.Party, bool) {
		return e.Registry().ByExternalID(mattermost.Backend, "u-jane")
	}

	// THE CONTROL: an active founder's account is theirs.
	if p, ok := founder(); !ok || p.Handle != "founder" {
		t.Fatalf("an active founder's post names %+v, want the founder", p)
	}

	stage = iam.StageSuspended
	e.refreshDirectory(t.Context(), true)
	if why, ok := e.Registry().Withholding("founder"); !ok || why != notify.WithheldSuspended {
		t.Fatalf("the rebuilt registry reports the founder as %q/%v, want suspended", why, ok)
	}
	if p, ok := founder(); ok {
		t.Fatalf("a suspended founder's post names %+v, want a stranger", p)
	}
	if got := tr.PeopleFindings(); len(got) != 0 {
		t.Fatalf("findings = %+v, want none about a founder nobody may reach", got)
	}

	stage = iam.StageActive
	e.refreshDirectory(t.Context(), true)
	if p, ok := founder(); !ok || p.Handle != "founder" {
		t.Fatalf("a reinstated founder's post names %+v, want the founder", p)
	}
}
