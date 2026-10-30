package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
// one person, the founder, named by an account nobody there holds. It asks
// this engine to provision its bots, under an administrator token nothing
// resolves, so a pass reaches the reconcile only with a token handed in.
func ghostCompany(t *testing.T, url string) *Company {
	t.Helper()
	return ghostCompanyProvisioning(t, url, `
    provisioning:
      admin_token: "${GHOST_MATTERMOST_ADMIN_TOKEN}"`)
}

// ghostCompanyProvisioning is ghostCompany with provisioning as its
// Mattermost block's provisioning lines: empty for a company whose bots were
// made by hand.
func ghostCompanyProvisioning(t *testing.T, url, provisioning string) *Company {
	t.Helper()
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
    team: eng%s
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
`, url, provisioning)))
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
		case strings.HasSuffix(r.URL.Path, "/users/me") &&
			r.Header.Get("Authorization") == "Bearer admin-token":
			// THE PROVISIONING TOKEN IS A SYSTEM ADMIN, so the pass's
			// preflight, which runs whether or not it has a seat to
			// provision, has nothing to report about it.
			w.Write([]byte(`{"id":"admin","username":"admin","roles":"system_admin system_user"}`))
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
	// A FLEET STORE, which every engine [New] builds has: the pass reads the
	// seat access ledger once the Mattermost vendor has opened.
	e := &Engine{backends: &Backends{Fleet: memory.NewFleet()}}
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

	sink, err := provision.NewEnvFileSink(filepath.Join(t.TempDir(), "minted.env"))
	if err != nil {
		t.Fatalf("NewEnvFileSink: %v", err)
	}
	for _, tc := range []struct {
		name string
		in   setup.PassInput
		// want is every kind the pass reports, in order: the return's own,
		// then the person's.
		want []integration.FindingKind
	}{{
		name: "no administrator token",
		want: []integration.FindingKind{
			integration.FindingCredentialMissing, integration.FindingContactUnknown,
		},
	}, {
		// AN ADMINISTRATOR TOKEN AND A SINK, so the pass reaches the
		// reconcile and returns what it found. The CEO's bot token is
		// written by hand, so there is nothing to mint and the reconcile
		// answers with no finding of its own: the person is the whole of
		// what that return reports.
		name: "the reconcile's return",
		in:   setup.PassInput{Operator: "admin-token", Sink: sink},
		want: []integration.FindingKind{integration.FindingContactUnknown},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := (&mattermostPass{engine: e}).Run(t.Context(), tc.in)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var kinds []integration.FindingKind
			var person *integration.Finding
			for i, f := range findings {
				kinds = append(kinds, f.Kind)
				if f.Kind == integration.FindingContactUnknown {
					person = &findings[i]
				}
			}
			if !slices.Equal(kinds, tc.want) {
				t.Fatalf("findings = %+v, want the kinds %v", findings, tc.want)
			}
			if person.Subject != "founder" || !strings.Contains(person.Detail, `"ghost"`) {
				t.Fatalf("the finding reads %+v", person)
			}
		})
	}
}

// A TOKEN HANDED IN MINTS NO BOT FOR A COMPANY THAT PROVISIONS NONE. Its bots
// were made by hand, so no seat's tool server waits on this pass
// ([seatMinter]); a run that reconciled anyway would mint tokens for servers
// that were never told one was coming. The setup route refuses that run
// before it starts, and the pass refuses it again where it reads its plan.
func TestAnAdministratorTokenMintsNoBotForACompanyThatProvisionsNone(t *testing.T) {
	t.Parallel()
	chat := ghostServer(t)
	e := &Engine{}
	e.epoch.current.Store(ghostCompanyProvisioning(t, chat.URL, ""))
	sink, err := provision.NewEnvFileSink(filepath.Join(t.TempDir(), "minted.env"))
	if err != nil {
		t.Fatalf("NewEnvFileSink: %v", err)
	}
	findings, err := (&mattermostPass{engine: e}).Run(t.Context(),
		setup.PassInput{Operator: "admin-token", Sink: sink})
	if !errors.Is(err, integration.ErrNotConfigured) {
		t.Fatalf("Run = %+v, %v; want the pass to refuse to reconcile a company "+
			"that provisions no bots", findings, err)
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
	e.notify.registry = notify.NewRegistry(company.Org, nil)

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

// A NAME REPORTED AS MOVED IS ANSWERED BY AN APPLY, AND BY NOTHING ELSE.
//
// The party registry is rebuilt on two triggers: an apply, and the identity
// directory moving under the same company. Only the first states who anybody is,
// so a person's name found holding another account stays reported through any
// number of directory rebuilds — a suspension of somebody else is no answer to
// it — and the next apply clears it. Mutation: pass true for every rebuild and
// the report is gone after the first directory rebuild.
func TestOnlyAnApplyAnswersAMovedName(t *testing.T) {
	t.Parallel()
	var holder atomic.Value
	holder.Store("u-jane")
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/me") &&
			r.Header.Get("Authorization") == "Bearer admin-token":
			// THE PROVISIONING TOKEN IS A SYSTEM ADMIN, so the pass's
			// preflight, which runs whether or not it has a seat to
			// provision, has nothing to report about it.
			w.Write([]byte(`{"id":"admin","username":"admin","roles":"system_admin system_user"}`))
		case strings.HasSuffix(r.URL.Path, "/users/me"):
			w.Write([]byte(`{"id":"bot-ceo","username":"ceo","is_bot":true}`))
		case strings.HasSuffix(r.URL.Path, "/users/usernames"):
			fmt.Fprintf(w, `[{"id":%q,"username":"jane"}]`, holder.Load())
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(chat.Close)
	doc := []byte(fmt.Sprintf(`
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
        bot_token: tok-ceo
  - name: Founder
    kind: human
    contact:
      mattermost_user_id: jane
`, chat.URL))
	company := func() *Company {
		cfg, err := config.ParseCompany(doc)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		c, err := NewCompany(cfg)
		if err != nil {
			t.Fatalf("NewCompany: %v", err)
		}
		return c
	}
	running := company()
	e := &Engine{}
	e.rebuildPartiesLocked(running, notify.Standing{})

	tr, err := mattermost.NewTransport(mattermost.TransportOptions{
		Config: mattermost.Config{
			URL: chat.URL, Team: "eng",
			Seats:  mattermost.SeatsFrom(running.Org, nil),
			People: mattermost.PeopleFrom(running.Org, nil),
		},
		Publisher: nopPublisher{},
		Claims:    memory.NewFleet(),
		Registry:  e.Registry,
		// Never on its own, so every ask is one a rebuild made.
		PeopleRefresh: time.Hour,
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
	moved := func() bool {
		got := tr.PeopleFindings()
		return len(got) == 1 && got[0].Kind == integration.FindingContactMoved
	}

	// THE NAME COMES TO HOLD ANOTHER ACCOUNT, found by the ask the first
	// directory rebuild makes.
	holder.Store("u-mallory")
	e.rebuildPartiesLocked(running, notify.Standing{})
	deadline := time.Now().Add(10 * time.Second)
	for !moved() {
		if time.Now().After(deadline) {
			t.Fatalf("the move was never reported: %+v", tr.PeopleFindings())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// ANOTHER DIRECTORY REBUILD, for the same company, states nothing.
	e.rebuildPartiesLocked(running, notify.Standing{})
	if !moved() {
		t.Fatalf("a directory rebuild withdrew the report: %+v", tr.PeopleFindings())
	}

	// THE APPLY does.
	e.rebuildPartiesLocked(company(), notify.Standing{})
	if got := tr.PeopleFindings(); len(got) != 0 {
		t.Fatalf("the apply left %+v", got)
	}
}
