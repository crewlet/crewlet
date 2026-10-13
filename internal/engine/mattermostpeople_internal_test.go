package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provision"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/setup"
)

// ghostCompany is a company with one agent on a Mattermost server at url and
// one person, the founder, named by an account nobody there holds.
func ghostCompany(t *testing.T, url string) *Company {
	t.Helper()
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
`, url)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return company
}

// ghostServer is a Mattermost server where the CEO's bot is the only account
// and nobody holds a person's name.
func ghostServer(t *testing.T) *httptest.Server {
	t.Helper()
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/me"):
			w.Write([]byte(`{"id":"bot-ceo","username":"ceo","is_bot":true}`))
		case strings.HasSuffix(r.URL.Path, "/users/usernames"):
			// NOBODY HOLDS THE NAME.
			w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/config/client"):
			w.Write([]byte(`{}`))
		default:
			// The websocket among them: the seat keeps reconnecting in
			// the background, which is no part of what is asserted.
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"not here"}`))
		}
	}))
	t.Cleanup(chat.Close)
	return chat
}

// A PERSON THE CHART NAMES BY AN ACCOUNT NOBODY HOLDS IS IN THE INTEGRATION'S
// STATUS, which is where an operator looks — and on the pass's every return,
// the one that cannot provision and the one that reconciles alike: whether a
// person's name holds an account is no part of provisioning, and a company
// whose bots were made by hand has people the agents may not recognise all
// the same.
func TestTheMattermostPassReportsAPersonNobodyHolds(t *testing.T) {
	t.Parallel()
	chat := ghostServer(t)
	company := ghostCompany(t, chat.URL)
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
			return notify.NewRegistry(company.Org)
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

	for _, tc := range []struct {
		name string
		in   setup.PassInput
		// beside is the reconcile's own finding the person sits beside.
		beside integration.Finding
	}{{
		name:   "no administrator token",
		beside: integration.Finding{Kind: integration.FindingCredentialMissing},
	}, {
		// AN ADMINISTRATOR TOKEN, so the pass reaches the reconcile and
		// returns what it found. A node with no keyring is the reconcile's
		// answer that needs no server: it cannot seal what it would mint.
		name:   "the reconcile's own findings",
		in:     setup.PassInput{Operator: "admin-token", Sink: provision.ReadOnly()},
		beside: integration.Finding{Kind: integration.FindingCredentialMissing, Subject: "secrets.keys"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := (&mattermostPass{engine: e}).Run(t.Context(), tc.in)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var person, beside *integration.Finding
			for i, f := range findings {
				switch {
				case f.Kind == integration.FindingContactUnknown:
					person = &findings[i]
				case f.Kind == tc.beside.Kind && f.Subject == tc.beside.Subject:
					beside = &findings[i]
				}
			}
			if beside == nil {
				t.Fatalf("findings = %+v, want the pass's own %+v", findings, tc.beside)
			}
			if person == nil {
				t.Fatalf("findings = %+v, want the founder named by an account nobody holds", findings)
			}
			if person.Subject != "founder" || !strings.Contains(person.Detail, `"ghost"`) {
				t.Fatalf("the finding reads %+v", person)
			}
		})
	}
}

// A CHANGE IN WHO THE PEOPLE ARE BRINGS THE STATUS FORWARD. The reconcile loop
// reads a settled surface again only on its own cadence — ten minutes at the
// least, an hour for a fix the operator owes — so the transport tells it when
// its people findings change, and the next tick runs the Mattermost pass
// however recently it last ran.
func TestAPeopleChangeBringsTheMattermostStatusForward(t *testing.T) {
	t.Parallel()
	chat := ghostServer(t)
	company := ghostCompany(t, chat.URL)
	fleet := memory.NewFleet()
	e := &Engine{backends: &Backends{Queue: queuememory.New(), Fleet: fleet}}
	e.epoch.current.Store(company)
	e.notify.registry = notify.NewRegistry(company.Org)

	pass := &countingReconciler{}
	store, err := integration.NewCoordStore(fleet)
	if err != nil {
		t.Fatalf("NewCoordStore: %v", err)
	}
	worker, err := integration.New(integration.Options{
		Registrations: []integration.Registration{{Reconciler: pass}},
		Store:         store,
	})
	if err != nil {
		t.Fatalf("integration.New: %v", err)
	}
	e.integrations = worker

	// SETTLED: the first tick runs the pass and the second, on its cadence,
	// does not.
	worker.Tick(t.Context())
	worker.Tick(t.Context())
	if got := pass.count(); got != 1 {
		t.Fatalf("a settled surface ran %d passes, want 1", got)
	}

	// THE TRANSPORT STARTS, and its first answer is that nobody holds the
	// founder's name: a change in what the status should say.
	tr, err := e.startMattermost(t.Context(), company, company.Config.Integrations.Mattermost)
	if err != nil {
		t.Fatalf("startMattermost: %v", err)
	}
	t.Cleanup(func() { tr.Stop(context.Background()) })
	if got := tr.PeopleFindings(); len(got) != 1 {
		t.Fatalf("the transport found %+v, want the founder", got)
	}
	worker.Tick(t.Context())
	if got := pass.count(); got != 2 {
		t.Fatalf("the Mattermost pass ran %d times after the people changed, want "+
			"a second pass on the next tick", got)
	}
}

// countingReconciler converges the Mattermost surface and counts its passes.
type countingReconciler struct {
	mu     sync.Mutex
	passes int
}

func (*countingReconciler) Kind() integration.Kind { return integration.KindMattermost }

func (p *countingReconciler) Reconcile(context.Context) ([]integration.Finding, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.passes++
	return nil, nil
}

func (p *countingReconciler) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.passes
}

// nopPublisher queues nothing: no post reaches this transport.
type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, string, *events.Event) error { return nil }
