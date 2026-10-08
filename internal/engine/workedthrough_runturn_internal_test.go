package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/slack"
	"github.com/crewlet/crewlet/internal/tools"
)

// A TURN REPORTS THE WAITING MESSAGES ITS THREAD BLOCK SHOWED IT, through the
// real [Engine.runTurn].
//
// The dispatcher records [turn.Result.WorkedThrough] beside the turn's own
// triggers, so a failed message that comes round behind the newer one whose
// turn already answered the thread is dropped instead of answered twice (see
// workedthrough.go). Nothing but runTurn fills that field — the loop never
// knew what a prompt showed — so every dispatcher test that sets it by hand
// would keep passing with the one line that fills it gone. This drives the
// whole frame: a Slack thread in which the seat replied, then Ana asked
// something (M1, waiting), then Ana asked again (M2, the trigger). The turn
// woken by M2 is shown M1 waiting, and reports it — and only it: not the
// trigger, not the root, not anything up to the seat's own reply.
func TestRunTurnReportsTheWaitingMessagesItsThreadShowedIt(t *testing.T) {
	e := threadedEngine(t, []map[string]string{
		{"user": "U0ANA", "ts": "1.0", "text": "the api test is flaking"},
		{"user": "U0BOT", "ts": "1.05", "text": "looking at it now"},
		{"user": "U0ANA", "ts": "1.1", "text": "it loops on /login"},
		{"user": "U0ANA", "ts": "1.2", "text": "and on /logout"},
	})
	trigger := events.New(types.ExternalNotification{
		NotificationSource: slack.Backend, SourceEventType: "message",
		Sender: "ana", Subject: "a message", Body: "and on /logout",
		Metadata: map[string]string{
			notify.TransportField: slack.Backend, "channel": "C0ENG",
			"thread_ts": "1.0", "ts": "1.2", "channel_type": "channel",
		},
	}, events.TraceContext{})
	trigger.Source = "notify." + slack.Backend

	// PAST THE DEPTH GUARD, so the turn ends before any model call — after
	// the prefetch that read the thread, which is the half under test.
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-thread", Depth: 3, WorkSince: time.Now().UTC(),
		Events: []*events.Event{trigger},
	})
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	want := []string{chatMessageKey(slack.Backend, "C0ENG", "1.1")}
	if !slices.Equal(res.WorkedThrough, want) {
		t.Fatalf("the turn reported %v as worked through, want only M1 (%v): the dispatch "+
			"records exactly this, and a retried M1 is dropped on it", res.WorkedThrough, want)
	}
}

// threadedEngine is an engine whose one seat is on a Slack workspace that
// answers every thread read with the given messages, oldest first — and whose
// company refuses every turn at its depth guard, so runTurn runs its prefetch
// and its bookkeeping without a model call (see [indicating]).
func threadedEngine(t *testing.T, thread []map[string]string) *Engine {
	t.Helper()
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.TrimPrefix(r.URL.Path, "/api/") != "conversations.replies" {
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"U0BOT","team_id":"T0ACME"}`))
			return
		}
		var b strings.Builder
		b.WriteString(`{"ok":true,"messages":[`)
		for i, m := range thread {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"user":"` + m["user"] + `","ts":"` + m["ts"] + `","text":"` + m["text"] + `"}`)
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(ws.Close)
	transport, err := slack.NewTransport(slack.TransportOptions{
		Config: slack.Config{
			Status: notify.StatusAddressed,
			Seats:  []slack.SeatConfig{{Handle: "swe", Token: "xoxb-swe"}},
		},
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, ws)},
	})
	if err != nil {
		t.Fatalf("slack.NewTransport: %v", err)
	}
	if err := transport.Start(t.Context()); err != nil {
		t.Fatalf("the transport did not come up: %v", err)
	}
	t.Cleanup(func() { transport.Stop(context.Background()) })

	seat := &org.Role{Name: "SWE", DeclaredHandle: "swe", LLM: org.ProviderKeys{"only"}}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{seat}}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: refusingProvider{}}})
	if err != nil {
		t.Fatalf("phase.NewRegistry: %v", err)
	}
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	e := &Engine{backends: &Backends{Queue: q, Fleet: coordmem.NewFleet()}}
	e.epoch.current.Store(&Company{
		Org: organization, Models: models, Tools: tools.NewRegistry(),
		Config: &config.Company{Name: "Acme", TurnEngine: config.TurnEngine{
			MaxIterations: 1, DelegationDepthLimit: 1, MaxToolRounds: 3,
		}},
	})
	e.notify.registry = notify.NewRegistry(organization)
	e.notify.slack = transport
	return e
}
